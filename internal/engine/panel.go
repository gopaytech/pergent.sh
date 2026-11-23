package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// reasonInputsChanged fails a member whose dispatch would change what its run
// was dispatched with: a member never writes its run.
const reasonInputsChanged = "the review's inputs changed while the panel ran"

// errInputsChanged is how subset says it would write to a member's run.
var errInputsChanged = errors.New(reasonInputsChanged)

// reasonNoPanelImage fails a review whose merge, Job P, can't fetch its
// members' results: pergent.sh's own image is not set.
const reasonNoPanelImage = "a review panel needs pergent.sh's own image (PERGENT_SH_IMAGE, set by the chart) to merge its members"

// reasonMemberError fails a member whose result says its review failed and
// gives no message.
const reasonMemberError = "the review failed"

// reasonMemberRefused, then why (chain.ReasonWords), fails an API review's
// member whose provider refused its model: it does not fall back.
const reasonMemberRefused = "the provider refused the model: "

// settleBatch is how many reported members one settle pass takes, as
// publishBatch is for runs.
const settleBatch = 20

// settleMembers decides up to settleBatch reported panel members, then releases each
// review whose members are all final. It runs in PublishOnce before the runs
// are listed, so a review whose members both failed is published as failed
// in the same pass. Nothing is posted for a member: no comment, note,
// thread, approval or status. A member it can't settle now is tried again on
// the next pass.
func (e *Engine) settleMembers(ctx context.Context) error {
	ms, err := e.Store.MembersToSettle(ctx, settleBatch)
	if err != nil {
		return fmt.Errorf("list the reported panel members: %w", err)
	}
	for _, m := range ms {
		if err := e.settleMember(ctx, m); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e.log().Warn("couldn't settle a panel member; trying again on the next pass",
				"run", m.RunID, "member", m.ID, "role", m.Role, "error", e.redact(err.Error()))
		}
	}
	// Every waiting review with no live member, not only those settled
	// above: a pass that stopped after a settle releases on the next one.
	roots, err := e.Store.PanelsToRelease(ctx)
	if err != nil {
		return fmt.Errorf("list the panels to release: %w", err)
	}
	for _, root := range roots {
		log := e.log().With("run", root)
		released, failed, err := e.Store.ReleasePanel(ctx, root, e.now())
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("couldn't release the review panel; trying again on the next pass", "error", e.redact(err.Error()))
		case released:
			// The dispatcher starts the merge.
			log.Info("the review panel's members are done; the review is queued for its merge")
			e.Wake()
		case failed:
			// The publisher publishes the failure, as for any run.
			log.Warn("both review panel members failed; the review fails")
			e.Wake()
		}
	}
	return nil
}

// settleMember decides the reported member m. Its stored result is checked
// again as the result handler checks it (callback.CheckStored), since the
// handler stores a result before it checks it: a result that fails the
// check, or says its review failed, fails the member. A result whose
// provider refused the member's entry falls back along the member's chain,
// as a run does (fallBack), on a merge request's review; on an API review
// the entry is marked and the member fails (markOnly). Otherwise the
// member is done with its result, or failed with its failure.
func (e *Engine) settleMember(ctx context.Context, m store.PanelMember) error {
	log := e.log().With("run", m.RunID, "member", m.ID, "role", m.Role)
	run, found, err := e.Store.PanelRun(ctx, m.RunID)
	if err != nil {
		return fmt.Errorf("load the panel's run: %w", err)
	}
	if !found {
		// A push or a close took the run: the sweep supersedes the member.
		return nil
	}
	log = log.With("instance", run.MR.Instance, "project", run.ProjectPath, "mr", run.MR.IID, "head", run.HeadSHA)
	if m.Result != nil {
		res, f, ok := callback.CheckStored(m.Result, run)
		if ok && res.Status != "completed" {
			f, ok = store.Failure{Reason: reasonMemberError}, false
			if res.Error != nil && res.Error.Message != "" {
				f.Reason = res.Error.Message
			}
		}
		if !ok {
			// ErrNotReported: the handler's own check failed it first.
			if err := e.Store.ReplaceMemberWithFailure(ctx, m.ID, f); err != nil && !errors.Is(err, store.ErrNotReported) {
				return fmt.Errorf("fail the member's result: %w", err)
			}
			log.Warn("the panel member's result can't be used; the member fails", "step", f.Step)
		} else if mark, refused := e.refusedMark(store.EntryRef{Source: m.Entry.Source, ID: m.Entry.ID}, m.Entry.Length, m.Result); refused {
			if run.Kind == store.KindMR {
				return e.fallBackMember(ctx, run, m, mark, log)
			}
			// An API review's member does not fall back: its entry is
			// marked as the review's would be, and it fails, which leaves
			// Job P the other member. The audit entry is written once, by
			// the settle that fails the member: a settle tried again after
			// an error marks the entry again, and audits nothing more.
			mlog := log.With("model_entry", m.Entry.ID, "class", mark.Class)
			if err := e.markOnly(ctx, run, m.Entry.Name, mark, false, mlog); err != nil {
				return err
			}
			f := store.Failure{Reason: reasonMemberRefused + chain.ReasonWords(mark.Class)}
			switch err := e.Store.ReplaceMemberWithFailure(ctx, m.ID, f); {
			case err == nil:
				mlog.Warn("the provider refused the review's model entry; it is skipped until the mark ends")
				e.auditMark(ctx, run, m.Entry.Name, mark.Class, mlog)
			case !errors.Is(err, store.ErrNotReported):
				return fmt.Errorf("fail the member's result: %w", err)
			}
		}
	}
	state, err := e.Store.SettleMember(ctx, m.ID)
	if err != nil {
		return fmt.Errorf("settle the member: %w", err)
	}
	if state != "" {
		log.Info("panel member settled", "state", state)
	}
	return nil
}

// fallBackMember moves member m to the next entry of its own chain, as
// fallBack does for a run, with the same mark and audit entry: m is
// fell_back and its role's next attempt is queued, in one transaction. The
// next attempt is a new Job with the full review timeout; B still skips the
// model A runs on (resolveMember). Nothing is posted, not even a status.
func (e *Engine) fallBackMember(ctx context.Context, run store.Run, m store.PanelMember, mark store.Mark, log *slog.Logger) error {
	log = log.With("model_entry", m.Entry.ID, "class", mark.Class)
	next, created, err := e.Store.EnqueueMemberAttempt(ctx, m, mark)
	if err != nil {
		return fmt.Errorf("queue the member's next attempt: %w", err)
	}
	if !created {
		// Superseded by a push meanwhile, or settled already.
		log.Info("not falling back: the member is no longer waiting to be settled")
		return nil
	}
	log.Warn("the provider refused the panel member's model entry; trying the next entry of its chain", "next_member", next)
	e.auditMark(ctx, run, m.Entry.Name, mark.Class, log)
	e.Wake()
	return nil
}

// mergePlan is what Job P runs with: the entry each role judges with (a role
// missing judges with no key), the models it names as A and B (a role that
// does not judge still names its member's), and whether no judge runs.
type mergePlan struct {
	judges  map[string]judge
	a, b    config.LLM
	noJudge bool
}

// judge is the entry one role of Job P judges with, and what panel_judges
// records of it.
type judge struct {
	model instances.Model
	entry store.RunEntry
}

// key is the key role judges with, "" when it does not judge.
func (p mergePlan) key(role string) string { return p.judges[role].model.Key }

// resolveMerge decides what Job P runs with (d.merge) from the review's
// members (d.panelMembers). Each role with a done member judges with that
// member's final entry when it still has a key, has no mark and, for B, is
// not the model A ran on; else with the next usable entry of the role's
// chain, skipping what earlier attempts of P fell back from on that role; a
// role with no done member, or no entry left, does not judge, and pergent's
// merge goes on without that half. No judge runs (--panel-no-judge) when no
// role can judge, when the repository's token quota (an API client's
// monthly budget) is used up, or when the instance's is used up and a judge
// entry is global. Limits are not checked: its tokens count on its entries all
// the same. d.entry names model A and its key for the Job. err is a store error.
func (d *dispatcher) resolveMerge(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) error {
	done := map[string]*store.PanelMember{}
	for i := range d.panelMembers {
		if m := &d.panelMembers[i]; m.State == store.MemberDone {
			done[m.Role] = m
		}
	}
	tried, err := d.judgedBefore(ctx)
	if err != nil {
		return err
	}
	inA, err := d.chainA(ctx, repo)
	if err != nil {
		return err
	}
	inB, err := chain.LoadSet(ctx, d.e.Store, d.e.registry(), d.e.now(), d.run, repo, store.ChainB)
	if err != nil {
		return err
	}
	inA.Tried, inB.Tried = tried[kube.PanelRoleA], tried[kube.PanelRoleB]
	// The judge ends a review that has started: no limit stops it.
	inA.IgnoreLimits()
	inB.IgnoreLimits()
	if a := done[kube.PanelRoleA]; a != nil {
		inB.Avoid = []chain.SameModel{{Provider: a.Provider, Model: a.Model}}
	}
	plan := mergePlan{judges: map[string]judge{}}
	for role, in := range map[string]chain.Input{kube.PanelRoleA: inA, kube.PanelRoleB: inB} {
		if j, ok := pickJudge(in, done[role]); ok {
			plan.judges[role] = j
		}
	}
	plan.a, plan.b = d.roleLLM(plan, kube.PanelRoleA, inA.Entries), d.roleLLM(plan, kube.PanelRoleB, inB.Entries)
	if plan.a.Provider == "" {
		plan.a = plan.b
	}
	if plan.b.Provider == "" {
		plan.b = plan.a
	}
	global := false
	for _, j := range plan.judges {
		global = global || j.entry.Source == store.SourceGlobal
	}
	var why string
	switch {
	case len(plan.judges) == 0:
		why = "no judge entry can be used"
	case repoUsedUp && d.api:
		why = "the client's monthly token budget is used up"
	case repoUsedUp:
		why = "the repository's token quota is used up"
	case instanceUsedUp && global:
		why = "the instance's token quota is used up"
	}
	if why != "" {
		plan.noJudge, plan.judges = true, nil
	}
	d.merge = plan
	d.entry = chain.Decision{Entry: instances.Model{LLM: plan.a, Key: plan.key(kube.PanelRoleA)}}
	d.log.Info("review panel merge", "judge_a", plan.judges[kube.PanelRoleA].entry.Name,
		"judge_b", plan.judges[kube.PanelRoleB].entry.Name, "no_judge", why)
	return nil
}

// pickJudge is the entry the role whose chain is in judges with: its done
// member's final entry while it is still in the chain and the chain's rules
// let it run (a key, no mark, not the model to avoid), else the chain's
// first usable entry. ok is false with no done member or no usable entry.
// The instance quota is left to resolveMerge.
func pickJudge(in chain.Input, done *store.PanelMember) (judge, bool) {
	if done == nil {
		return judge{}, false
	}
	ref := store.EntryRef{Source: done.Entry.Source, ID: done.Entry.ID}
	if ref.ID != "" && slices.Contains(in.Refs, ref) && chain.EntryReason(in, ref) == "" {
		m := in.Entries[ref]
		return judge{model: m, entry: store.RunEntry{Source: ref.Source, ID: ref.ID, Name: m.Name, TeamID: m.TeamID,
			Position: slices.Index(in.Refs, ref) + 1, Length: len(in.Refs)}}, true
	}
	dec := chain.Resolve(in)
	if !dec.Runs() {
		return judge{}, false
	}
	return judge{model: dec.Entry, entry: runEntry(dec)}, true
}

// roleLLM is the model Job P names for role: its judge's, else that of the
// role's newest member with an entry (entries are the entries known now;
// a removed one keeps the provider and model the member ran on), else none.
// pergent runs no model of a role that has no key.
func (d *dispatcher) roleLLM(plan mergePlan, role string, entries map[store.EntryRef]instances.Model) config.LLM {
	if j, ok := plan.judges[role]; ok {
		return j.model.LLM
	}
	for i := len(d.panelMembers) - 1; i >= 0; i-- {
		m := d.panelMembers[i]
		if m.Role != role || m.State == store.MemberOff || m.Entry.ID == "" {
			continue
		}
		if e, ok := entries[store.EntryRef{Source: m.Entry.Source, ID: m.Entry.ID}]; ok {
			return e.LLM
		}
		return config.LLM{Provider: m.Provider, Model: m.Model}
	}
	return config.LLM{}
}

// judgedBefore is, by role, the entries earlier attempts of this Job P fell
// back from: the judge entry of the role its provider error named. That
// judge does not judge with them again, even once their mark ends; the
// other role's judge is not held to it.
func (d *dispatcher) judgedBefore(ctx context.Context) (map[string]map[store.EntryRef]bool, error) {
	out := map[string]map[store.EntryRef]bool{}
	if d.run.AttemptOf == nil {
		return out, nil
	}
	as, err := d.e.Store.Attempts(ctx, *d.run.AttemptOf)
	if err != nil {
		return nil, fmt.Errorf("read the review's attempts: %w", err)
	}
	for _, a := range as {
		pe := providerError(a.Result)
		if a.ID == d.run.ID || a.Outcome != store.OutcomeFellBack || pe == nil || pe.Role == "" {
			continue
		}
		js, err := d.e.Store.PanelJudges(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("read the judges of an earlier attempt: %w", err)
		}
		if e, ok := js[pe.Role]; ok {
			if out[pe.Role] == nil {
				out[pe.Role] = map[store.EntryRef]bool{}
			}
			out[pe.Role][store.EntryRef{Source: e.Source, ID: e.ID}] = true
		}
	}
	return out, nil
}

// recordJudges records the entry each role of Job P judges with, and drops
// those of an earlier try of this run. No judge (--panel-no-judge) records
// none.
func (d *dispatcher) recordJudges(ctx context.Context) error {
	if err := d.e.Store.ClearPanelJudges(ctx, d.run.ID); err != nil {
		return fmt.Errorf("clear the review panel's judges: %w", err)
	}
	for _, role := range []string{kube.PanelRoleA, kube.PanelRoleB} {
		j, ok := d.merge.judges[role]
		if !ok {
			continue
		}
		if err := d.e.Store.SetPanelJudge(ctx, d.run.ID, role, j.entry, j.model.LLM.Provider, j.model.LLM.Model); err != nil {
			return fmt.Errorf("record the review panel's judge %s: %w", role, err)
		}
	}
	return nil
}

// deref is *s, "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
