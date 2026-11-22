package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Why a panel review runs as one review (the 'off' row's reason, after
// "panel off: " in the footer).
const (
	panelOffImage = "the review image does not support it"
	panelOffQuota = "the token quota stopped model B"
	panelOffSame  = "model B is the model A runs on"
	panelOffNone  = "no entry of chain B can be used"
	panelOffRead  = "chain B could not be read"
	// panelOffFetch: Job P fetches the members' results with pergent.sh's
	// own image (PERGENT_SH_IMAGE, set by the chart).
	panelOffFetch = "pergent.sh's own image is not set"
)

// reasonInputsChanged fails a member whose dispatch would change what its run
// was dispatched with: a member never writes its run.
const reasonInputsChanged = "the review's inputs changed while the panel ran"

// errInputsChanged is how subset says it would write to a member's run.
var errInputsChanged = errors.New(reasonInputsChanged)

// reasonNoPanelImage fails a review whose merge, Job P, can't fetch its
// members' results: pergent.sh's own image is not set.
const reasonNoPanelImage = "a review panel needs pergent.sh's own image (PERGENT_SH_IMAGE, set by the chart) to merge its members"

// panel decides whether the run is a panel review, after every check a
// review passes and the run's writes, before its token. split is true when
// the dispatch ends here: StartPanel made the run a panel (its members get
// their Jobs apart), or the run was gone. With no chain B in effect nothing
// is written and split is false: the review is today's. When B can't start,
// the reason goes in the review's one 'off' row and split is false: the
// review runs A only, as today. repoUsedUp and instanceUsedUp are the token
// quotas (quota.Check); a run under a used-up repository quota is here only
// because somebody asked for it, and B does not start then. err is a store
// error; the caller retries.
func (d *dispatcher) panel(ctx context.Context, mr forge.Change, fitted *inputs.Inputs, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (split bool, err error) {
	e := d.e
	if !d.run.DispatchedAt.IsZero() {
		// Split before, and released: its members are done. It is never
		// split again, and its off row (none) is not written.
		return false, nil
	}
	in, err := chain.LoadSet(ctx, e.Store, e.registry(), e.now(), d.run, repo, store.ChainB)
	if err != nil {
		// Never the review's failure, and never a silent change to it.
		d.log.Error("the review runs without a panel: chain B could not be read", "error", d.redact(err.Error()))
		return false, d.panelOff(ctx, panelOffRead)
	}
	if len(in.Refs) == 0 {
		return false, nil
	}
	if ok, why := config.PanelImage(e.Config.Jobs.Image); !ok {
		d.log.Info("the review runs without a panel: the review image does not support it", "image_check", why)
		return false, d.panelOff(ctx, panelOffImage)
	}
	if e.ScanImage == "" {
		d.log.Info("the review runs without a panel: pergent.sh's own image (PERGENT_SH_IMAGE) is not set")
		return false, d.panelOff(ctx, panelOffFetch)
	}
	if repoUsedUp {
		return false, d.panelOff(ctx, panelOffQuota)
	}
	if dec := d.resolveB(in, instanceUsedUp); !dec.Runs() {
		return false, d.panelOff(ctx, panelOffReason(dec))
	}
	// The run keeps the merge request's base, which both members review,
	// and its skills as a review records them, which their results are
	// checked against.
	sources, versions, hashes := d.runInputs(fitted)
	started, err := e.Store.StartPanel(ctx, d.run.ID, store.PanelSplit{
		BaseSHA: mr.BaseSHA, StartSHA: mr.StartSHA,
		Inputs: store.RunInputs{Model: d.model, MaxTurns: repo.MaxTurns,
			Sources: sources, Versions: versions, Hashes: hashes},
	}, e.now())
	if err != nil {
		return false, err
	}
	if !started {
		// The run is no longer claimed: superseded, or split by another
		// dispatch, meanwhile.
		d.log.Info("the review panel did not start: the run is no longer claimed")
		return true, nil
	}
	d.log.Info("review panel started", "chain_b", in.Scope)
	e.Wake()
	return true, nil
}

// panelOff records why the review runs A only.
func (d *dispatcher) panelOff(ctx context.Context, reason string) error {
	d.log.Info("the review runs without a panel", "reason", reason)
	if err := d.e.Store.RecordPanelOff(ctx, d.run.ID, reason); err != nil {
		return fmt.Errorf("record why the review has no panel: %w", err)
	}
	return nil
}

// panelOffReason is why B can't run on any entry of chain B (dec does not
// run). The quota is the reason when only it, the limits and A's model stopped
// the entries, A's model when only it did; anything else is no usable entry.
func panelOffReason(dec chain.Decision) string {
	quota, same := false, false
	for _, s := range dec.Skips {
		switch s.Reason {
		case chain.ReasonInstanceQuota, chain.ReasonModelLimit, chain.ReasonTeamLimit:
			quota = true
		case chain.ReasonSameModel:
			same = true
		default:
			return panelOffNone
		}
	}
	switch {
	case quota:
		return panelOffQuota
	case same:
		return panelOffSame
	}
	return panelOffNone
}

// isMember reports whether the dispatch starts a panel member's Job.
func (d *dispatcher) isMember() bool { return d.stage == stageMember }

// dispatchMember starts the Job of the claimed panel member m, with the same
// dispatch as a review, in stageMember: its run is the one waiting for m. A
// member whose run no longer waits goes back to the queue, where nothing
// claims it, and the sweep supersedes it.
func (e *Engine) dispatchMember(ctx context.Context, m store.PanelMember) error {
	d := &dispatcher{e: e, stage: stageMember, member: m, run: store.Run{ID: m.RunID},
		log: e.log().With("run", m.RunID, "member", m.ID, "role", m.Role)}
	run, found, err := e.Store.PanelRun(ctx, m.RunID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load the panel's run: %w", err))
	}
	if !found {
		d.log.Info("the panel's run no longer waits for its members; the member is left to the sweep")
		return d.postpone(ctx, e.now())
	}
	d.run = run
	d.log = e.log().With("run", run.ID, "member", m.ID, "role", m.Role, "instance", run.MR.Instance,
		"project", run.ProjectPath, "mr", run.MR.IID, "head", run.HeadSHA)
	if run.Kind == store.KindAPI {
		return d.dispatchAPI(ctx)
	}
	return d.dispatch(ctx)
}

// resolveMember picks the member's entry. Member A reads chain A as its run
// did, but the instance quota never holds it: the run passed the quota hold.
// Member B reads chain B and never runs on the model A runs on; the instance
// quota stops B's global entries, as at the split. Each skips the entries
// its role's earlier attempts ran.
func (d *dispatcher) resolveMember(ctx context.Context, repo settings.Effective, instanceUsedUp bool) (chain.Decision, error) {
	ms, err := d.e.Store.PanelMembers(ctx, d.member.RunID)
	if err != nil {
		return chain.Decision{}, fmt.Errorf("read the panel's members: %w", err)
	}
	if d.member.Role == kube.PanelRoleA {
		dec, in, err := d.resolveA(ctx, repo, ms, instanceUsedUp)
		if err != nil {
			return chain.Decision{}, err
		}
		d.attempt = len(in.Tried) + 1
		d.entry, d.scope = dec, in.Scope
		return dec, nil
	}
	in, err := chain.LoadSet(ctx, d.e.Store, d.e.registry(), d.e.now(), d.run, repo, store.ChainB)
	if err != nil {
		return chain.Decision{}, err
	}
	in.Tried = triedBy(ms, d.member)
	if in.Avoid, err = d.modelOfA(ctx, repo, ms, instanceUsedUp); err != nil {
		return chain.Decision{}, err
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
	d.attempt = len(in.Tried) + 1
	d.entry, d.scope = dec, in.Scope
	return dec, nil
}

// resolveA resolves chain A for member A of the review whose members are ms:
// the run's chain and tried entries, the entries member A's earlier attempts
// ran, and no hold: it goes past the instance quota and the admin limits (pastAdmin).
func (d *dispatcher) resolveA(ctx context.Context, repo settings.Effective, ms []store.PanelMember, instanceUsedUp bool) (chain.Decision, chain.Input, error) {
	in, err := d.chainA(ctx, repo)
	if err != nil {
		return chain.Decision{}, chain.Input{}, err
	}
	a := store.PanelMember{Role: kube.PanelRoleA}
	if d.member.Role == kube.PanelRoleA {
		a = d.member
	}
	for ref := range triedBy(ms, a) {
		if in.Tried == nil {
			in.Tried = map[store.EntryRef]bool{}
		}
		in.Tried[ref] = true
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
	if dec.Hold {
		dec = pastAdmin(in)
	}
	return dec, in, nil
}

// modelOfA is the model member A runs on, which B avoids: that of A's newest
// attempt with an entry, else the entry chain A resolves to now (none when it
// resolves to nothing).
func (d *dispatcher) modelOfA(ctx context.Context, repo settings.Effective, ms []store.PanelMember, instanceUsedUp bool) (chain.SameModel, error) {
	var newest *store.PanelMember
	for i := range ms {
		if ms[i].Role == kube.PanelRoleA && ms[i].Entry.ID != "" {
			newest = &ms[i]
		}
	}
	if newest != nil {
		return chain.SameModel{Provider: newest.Provider, Model: newest.Model}, nil
	}
	dec, _, err := d.resolveA(ctx, repo, ms, instanceUsedUp)
	if err != nil || !dec.Runs() {
		return chain.SameModel{}, err
	}
	return chain.SameModel{Provider: dec.Entry.LLM.Provider, Model: dec.Entry.LLM.Model}, nil
}

// triedBy is the entries the other attempts of m's role ran (ms are the
// review's members).
func triedBy(ms []store.PanelMember, m store.PanelMember) map[store.EntryRef]bool {
	var out map[store.EntryRef]bool
	for _, o := range ms {
		if o.Role != m.Role || o.ID == m.ID || o.Entry.ID == "" {
			continue
		}
		if out == nil {
			out = map[store.EntryRef]bool{}
		}
		out[store.EntryRef{Source: o.Entry.Source, ID: o.Entry.ID}] = true
	}
	return out
}

// memberCannotRun acts on a member whose chain gives it no entry (dec does
// not run). Member A with no entry at all waits, as a run does; with none
// usable it fails as a run does. Member B fails: without B the review goes on
// with A alone.
func (d *dispatcher) memberCannotRun(ctx context.Context, dec chain.Decision) error {
	switch {
	case d.member.Role == kube.PanelRoleA && dec.Wait:
		d.e.warnEvery("no-llm", msgNoLLM)
		return d.postpone(ctx, d.e.now().Add(waitDelay))
	case d.member.Role == kube.PanelRoleA:
		return d.failExhausted(ctx, dec)
	case dec.Wait:
		return d.fail(ctx, panelOffNone)
	case dec.Hold:
		return d.fail(ctx, panelOffQuota)
	}
	return d.fail(ctx, chain.FailureText(dec))
}

// memberInputsMatch reports whether the fitted inputs of a member, or of Job
// P, are what its run recorded when it was dispatched: the rules, the skills (where each
// came from and its content's hash), whether it is incremental and since
// which run, and the linked repositories. A member whose skills changed
// stops here, before its Job, as its result would fail its check.
func (d *dispatcher) memberInputsMatch(in *inputs.Inputs, last store.Run, links linkPlan) bool {
	run := d.run
	if run.SkillSources != nil {
		sources, _, hashes := d.runInputs(in)
		if !maps.Equal(sources, run.SkillSources) || (run.SkillHashes != nil && !maps.Equal(hashes, run.SkillHashes)) {
			d.log.Info("the review's skills changed since its panel started")
			return false
		}
	}
	if run.RulesHash != nil && *run.RulesHash != d.rulesHash {
		d.log.Info("the review's rules changed since its panel started")
		return false
	}
	incremental := len(in.Carried) > 0
	if incremental != run.Incremental || (incremental && (run.BaseRunID == nil || *run.BaseRunID != last.ID)) {
		d.log.Info("the files the review carries changed since its panel started")
		return false
	}
	if !slices.EqualFunc(links.Matched, run.LinkedRepos, sameLink) {
		d.log.Info("the review's linked repositories changed since its panel started")
		return false
	}
	return true
}

// sameLink reports whether a and b are one link (a nil and an empty watched
// list are the same).
func sameLink(a, b store.Link) bool {
	return a.Target == b.Target && a.CanBlock == b.CanBlock && slices.Equal(a.Watched, b.Watched)
}

// requeueMember puts the member back in the queue with an attempt counted. A
// member that is no longer queued or running (superseded meanwhile) can't be,
// and that is not an error.
func (d *dispatcher) requeueMember(ctx context.Context, notBefore time.Time) error {
	err := d.e.Store.RequeueMember(ctx, d.member.ID, notBefore)
	if err == nil {
		return nil
	}
	ms, lerr := d.e.Store.PanelMembers(ctx, d.member.RunID)
	if lerr == nil {
		for _, m := range ms {
			if m.ID == d.member.ID && m.State != store.MemberQueued && m.State != store.MemberRunning {
				return nil
			}
		}
	}
	return fmt.Errorf("requeue the member: %w", err)
}

// memberRunningText is the running status the review's first member Job
// sets: the run's own, which says a backup for an attempt after the first.
func (d *dispatcher) memberRunningText(ctx context.Context) string {
	run := d.run
	if run.AttemptOf == nil {
		return runningDescription
	}
	as, err := d.e.Store.Attempts(ctx, *run.AttemptOf)
	if err != nil {
		d.log.Info("couldn't read the review's attempts for its status", "error", d.redact(err.Error()))
		return runningDescription
	}
	for i, a := range as {
		if a.ID == run.ID && i+1 <= run.ChainLength {
			return fmt.Sprintf(backupDescription, i+1, run.ChainLength)
		}
	}
	return runningDescription
}

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
// members (d.members). Each role with a done member judges with that
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
	for i := range d.members {
		if m := &d.members[i]; m.State == store.MemberDone {
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
		inB.Avoid = chain.SameModel{Provider: a.Provider, Model: a.Model}
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
	for i := len(d.members) - 1; i >= 0; i-- {
		m := d.members[i]
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
