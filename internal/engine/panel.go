package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
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
)

// reasonInputsChanged fails a member whose dispatch would change what its run
// was dispatched with: a member never writes its run.
const reasonInputsChanged = "the review's inputs changed while the panel ran"

// errInputsChanged is how subset says it would write to a member's run.
var errInputsChanged = errors.New(reasonInputsChanged)

// reasonAPIPanel fails a member of an API review, which has no panel yet.
const reasonAPIPanel = "an API review has no review panel yet"

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
func (d *dispatcher) panel(ctx context.Context, mr forge.Change, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (split bool, err error) {
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
	if repoUsedUp {
		return false, d.panelOff(ctx, panelOffQuota)
	}
	if dec := d.resolveB(in, instanceUsedUp); !dec.Runs() {
		return false, d.panelOff(ctx, panelOffReason(dec))
	}
	// The run keeps the merge request's base: both members review it.
	started, err := e.Store.StartPanel(ctx, d.run.ID, mr.BaseSHA, mr.StartSHA, e.now())
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
// run). The quota is the reason when only it and A's model stopped the
// entries, A's model when only it did; anything else is no usable entry.
func panelOffReason(dec chain.Decision) string {
	quota, same := false, false
	for _, s := range dec.Skips {
		switch s.Reason {
		case chain.ReasonInstanceQuota:
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
		return d.fail(ctx, reasonAPIPanel)
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
// ran, and no hold for the instance quota.
func (d *dispatcher) resolveA(ctx context.Context, repo settings.Effective, ms []store.PanelMember, instanceUsedUp bool) (chain.Decision, chain.Input, error) {
	in, err := d.e.chainFor(ctx, d.run, repo)
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
		in.InstanceQuotaUsedUp = false
		dec = chain.Resolve(in)
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

// memberInputsMatch reports whether the member's fitted inputs are what its
// run recorded when it was dispatched: the rules, whether it is incremental
// and since which run, and the linked repositories.
func (d *dispatcher) memberInputsMatch(in *inputs.Inputs, last store.Run, links linkPlan) bool {
	run := d.run
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
