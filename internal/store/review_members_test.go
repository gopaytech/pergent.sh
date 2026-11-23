package store

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// frozenOf is a frozen chain of global entries with the given names.
func frozenOf(names ...string) FrozenChain {
	c := FrozenChain{Scope: "repo"}
	for _, n := range names {
		c.Entries = append(c.Entries, FrozenEntry{Source: SourceGlobal, ID: "id-" + n, Name: n,
			Provider: "anthropic", Model: "model-" + n})
	}
	return c
}

// multiInputs are the inputs a test multi review records at its split.
var multiInputs = RunInputs{
	Model: "gpt-a", MaxTurns: 40, Sources: map[string]string{"security": "library"},
	Versions: map[string]int{"security": 3}, Hashes: map[string]string{"security": "h1"},
}

// multiSplit is the split of a test multi review with reviewers 1 to n.
// Reviewer k's frozen chain is R<k> then R<k>b. skipped holds why a
// reviewer can't start.
func multiSplit(n int, skipped map[int]string) MultiReviewSplit {
	sp := MultiReviewSplit{BaseSHA: "base-sha", StartSHA: "start-sha", Inputs: multiInputs,
		Judge: frozenOf("Judge", "Judge2")}
	for k := 1; k <= n; k++ {
		name := "R" + strconv.Itoa(k)
		sp.Reviewers = append(sp.Reviewers, SplitReviewer{Number: k, Chain: frozenOf(name, name+"b"), Skipped: skipped[k]})
	}
	return sp
}

// multiRun gives a claimed run of head on mr made a multi review with
// split: it waits for its members.
func multiRun(t *testing.T, s *Store, mr MRKey, head string, split MultiReviewSplit) string {
	t.Helper()
	id := enqueue(t, s, mr, head)
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartMultiReview(ctx, id, split, t0)
	if err != nil || !started {
		t.Fatalf("StartMultiReview = %v, %v", started, err)
	}
	return id
}

// reviewerOf returns the newest attempt of reviewer n of the review whose
// first run is root.
func reviewerOf(t *testing.T, s *Store, root string, n int) ReviewMember {
	t.Helper()
	ms, err := s.ReviewMembers(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var out ReviewMember
	for _, m := range ms {
		if m.Reviewer == n && m.State != MemberOff {
			out = m
		}
	}
	if out.ID == "" {
		t.Fatalf("no reviewer %d of %s in %+v", n, root, ms)
	}
	return out
}

// dispatchReviewer claims the queued members and records a Job for member
// id with the token, valid for an hour after t0.
func dispatchReviewer(t *testing.T, s *Store, id, token string) {
	t.Helper()
	if _, err := s.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkReviewMemberDispatched(ctx, id, "job-"+token, hash(token), t0.Add(time.Hour), t0); err != nil {
		t.Fatalf("MarkReviewMemberDispatched: %v", err)
	}
}

// reportReviewer takes member id through dispatch to a stored result.
func reportReviewer(t *testing.T, s *Store, id, body string) {
	t.Helper()
	dispatchReviewer(t, s, id, "tok-"+id)
	if out, err := s.StoreReviewMemberResult(ctx, reviewRootOf(t, s, id), id, "tok-"+id, []byte(body), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreReviewMemberResult = %v, %v", out, err)
	}
}

// doneReviewer takes member id through dispatch to a stored result and settles it.
func doneReviewer(t *testing.T, s *Store, id, body string) {
	t.Helper()
	reportReviewer(t, s, id, body)
	if st, err := s.SettleReviewMember(ctx, id); err != nil || st != MemberDone {
		t.Fatalf("SettleReviewMember = %q, %v; want done", st, err)
	}
}

// failedReviewer takes member id through dispatch to a failure and settles it.
func failedReviewer(t *testing.T, s *Store, id, reason string) {
	t.Helper()
	dispatchReviewer(t, s, id, "tok-"+id)
	if ok, err := s.ReportReviewMemberFailure(ctx, id, Failure{Reason: reason, Step: "review"}, t0.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("ReportReviewMemberFailure = %v, %v", ok, err)
	}
	if st, err := s.SettleReviewMember(ctx, id); err != nil || st != MemberFailed {
		t.Fatalf("SettleReviewMember = %q, %v; want failed", st, err)
	}
}

func reviewerStates(t *testing.T, s *Store, root string) map[int][]MemberState {
	t.Helper()
	ms, err := s.ReviewMembers(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int][]MemberState{}
	for _, m := range ms {
		out[m.Reviewer] = append(out[m.Reviewer], m.State)
	}
	return out
}

// reviewRootOf is the review's first run of member id.
func reviewRootOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	var root string
	if err := s.pool.QueryRow(ctx, `SELECT run_id::text FROM review_members WHERE id = $1::uuid`, id).Scan(&root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestStartMultiReviewWaitsAndQueues(t *testing.T) {
	s := newStore(t)
	split := multiSplit(3, map[int]string{3: "no different model left"})
	id := multiRun(t, s, mr1, "a1", split)
	r := getRun(t, s, id)
	if r.State != Waiting || r.SkipReason != WaitReviewers || !r.DispatchedAt.Equal(t0) || r.JobName != "" {
		t.Errorf("run = %s %q dispatched %v job %q; want waiting for its members, dispatched at t0", r.State, r.SkipReason, r.DispatchedAt, r.JobName)
	}
	if r.BaseSHA != "base-sha" || r.StartSHA != "start-sha" {
		t.Errorf("run base %q start %q; want the split's", r.BaseSHA, r.StartSHA)
	}
	if r.SkillSources["security"] != "library" || r.SkillVersions["security"] != 3 || r.SkillHashes["security"] != "h1" ||
		r.Model == nil || *r.Model != "gpt-a" || r.MaxTurns == nil || *r.MaxTurns != 40 {
		t.Errorf("run inputs = %v %v %v %v %v; want the split's", r.Model, r.MaxTurns, r.SkillSources, r.SkillVersions, r.SkillHashes)
	}
	ms, err := s.ReviewMembers(ctx, id)
	if err != nil || len(ms) != 3 {
		t.Fatalf("ReviewMembers = %+v, %v; want 3", ms, err)
	}
	for i, m := range ms {
		want := MemberQueued
		reason := ""
		if i == 2 {
			want, reason = MemberSkipped, "no different model left"
		}
		if m.Reviewer != i+1 || m.RunID != id || m.State != want || m.FailureReason != reason || m.AttemptOf != nil || m.JobName != "" {
			t.Errorf("member %d = %+v; want reviewer %d %s %q, of the run, a first attempt", i, m, i+1, want, reason)
		}
		if !reflect.DeepEqual(m.Chain, split.Reviewers[i].Chain) {
			t.Errorf("member %d chain = %+v; want %+v", i, m.Chain, split.Reviewers[i].Chain)
		}
	}
	judge, found, err := s.JudgeChainOf(ctx, id)
	if err != nil || !found || !reflect.DeepEqual(judge, split.Judge) {
		t.Errorf("JudgeChainOf = %+v, %v, %v; want %+v", judge, found, err, split.Judge)
	}
	if judge, err := s.IsJudgeRun(ctx, id); err != nil || !judge {
		t.Errorf("IsJudgeRun = %v, %v; want true", judge, err)
	}
	// The previous release never claims a waiting run.
	if got, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Errorf("ClaimQueued = %v, %v; want nothing", runIDs(got), err)
	}
	// A skipped reviewer is never claimed.
	got, err := s.ClaimQueuedReviewMembers(ctx, 10, t0)
	if err != nil || len(got) != 2 || got[0].Reviewer != 1 || got[1].Reviewer != 2 {
		t.Errorf("ClaimQueuedReviewMembers = %+v, %v; want reviewers 1 and 2", got, err)
	}
	// A run that is no multi review has no judge chain and is no judge run.
	other := enqueue(t, s, mr2, "b1")
	if _, found, err := s.JudgeChainOf(ctx, other); err != nil || found {
		t.Errorf("JudgeChainOf(single review) = %v, %v; want none", found, err)
	}
	if judge, err := s.IsJudgeRun(ctx, other); err != nil || judge {
		t.Errorf("IsJudgeRun(single review) = %v, %v; want false", judge, err)
	}
	if _, found, err := s.JudgeChainOf(ctx, "bad"); err != nil || found {
		t.Errorf("JudgeChainOf(bad) = %v, %v; want none", found, err)
	}
}

// A judge chain with no entries is stored as it is: the review runs with
// --no-judge.
func TestStartMultiReviewEmptyJudge(t *testing.T) {
	s := newStore(t)
	split := multiSplit(2, nil)
	split.Judge = FrozenChain{Scope: "repo"}
	id := multiRun(t, s, mr1, "a1", split)
	judge, found, err := s.JudgeChainOf(ctx, id)
	if err != nil || !found || len(judge.Entries) != 0 || judge.Scope != "repo" {
		t.Errorf("JudgeChainOf = %+v, %v, %v; want the empty chain", judge, found, err)
	}
}

// A split must list reviewer 1 first, queued, then the others in number
// order, at most 5. A bad split writes nothing.
func TestStartMultiReviewRefusesBadSplit(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	skipped1 := multiSplit(2, map[int]string{1: "no usable entry"})
	gap := multiSplit(3, nil)
	gap.Reviewers = []SplitReviewer{gap.Reviewers[0], gap.Reviewers[2]}
	for name, sp := range map[string]MultiReviewSplit{
		"no reviewer":        {BaseSHA: "b", StartSHA: "s"},
		"reviewer 1 skipped": skipped1,
		"a gap":              gap,
		"6 reviewers":        multiSplit(6, nil),
	} {
		if started, err := s.StartMultiReview(ctx, id, sp, t0); err == nil || started {
			t.Errorf("%s: StartMultiReview = %v, %v; want an error", name, started, err)
		}
	}
	if r := getRun(t, s, id); r.State != Running || r.SkipReason != "" || !r.DispatchedAt.IsZero() {
		t.Errorf("run = %s %q dispatched %v; want still claimed", r.State, r.SkipReason, r.DispatchedAt)
	}
	if ms, err := s.ReviewMembers(ctx, id); err != nil || len(ms) != 0 {
		t.Errorf("ReviewMembers = %+v, %v; want none", ms, err)
	}
	if _, found, err := s.JudgeChainOf(ctx, id); err != nil || found {
		t.Errorf("JudgeChainOf = %v, %v; want none", found, err)
	}
}

// A review is split once: a second call, a superseded run, a run with a Job
// and a run released for the judge (dispatched_at kept) are not split.
func TestStartMultiReviewOnce(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	if started, err := s.StartMultiReview(ctx, id, multiSplit(3, nil), t0); err != nil || started {
		t.Errorf("second StartMultiReview = %v, %v; want false", started, err)
	}
	if ms, err := s.ReviewMembers(ctx, id); err != nil || len(ms) != 2 {
		t.Errorf("ReviewMembers = %d, %v; want the first split's 2", len(ms), err)
	}

	gone := enqueue(t, s, mr2, "b1")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr2, "b2")
	if started, err := s.StartMultiReview(ctx, gone, multiSplit(2, nil), t0); err != nil || started {
		t.Errorf("StartMultiReview of a superseded run = %v, %v; want false", started, err)
	}
	if ms, err := s.ReviewMembers(ctx, gone); err != nil || len(ms) != 0 {
		t.Errorf("ReviewMembers = %+v, %v; want none", ms, err)
	}
	if _, found, err := s.JudgeChainOf(ctx, gone); err != nil || found {
		t.Errorf("JudgeChainOf = %v, %v; want none", found, err)
	}
	d := dispatched(t, s, mr3, "c1", "tok")
	if started, err := s.StartMultiReview(ctx, d, multiSplit(2, nil), t0); err != nil || started {
		t.Errorf("StartMultiReview of a dispatched run = %v, %v; want false", started, err)
	}
	if started, err := s.StartMultiReview(ctx, "not-a-uuid", multiSplit(2, nil), t0); err != nil || started {
		t.Errorf("StartMultiReview of a bad id = %v, %v; want false", started, err)
	}

	// Released for the judge and claimed again: dispatched_at is kept.
	doneReviewer(t, s, reviewerOf(t, s, id, 1).ID, `{}`)
	failedReviewer(t, s, reviewerOf(t, s, id, 2).ID, "R2 broke")
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	if got, err := s.ClaimQueued(ctx, 10, t0); err != nil || len(got) != 1 {
		t.Fatal(runIDs(got), err)
	}
	if started, err := s.StartMultiReview(ctx, id, multiSplit(2, nil), t0.Add(time.Minute)); err != nil || started {
		t.Errorf("StartMultiReview of a released run = %v, %v; want not started", started, err)
	}
	if r := getRun(t, s, id); r.State != Running || r.SkipReason != "" || !r.DispatchedAt.Equal(t0) {
		t.Errorf("run = %s %q dispatched %v; want still claimed for the judge", r.State, r.SkipReason, r.DispatchedAt)
	}
	if got := reviewerStates(t, s, id); !slices.Equal(got[1], []MemberState{MemberDone}) || !slices.Equal(got[2], []MemberState{MemberFailed}) {
		t.Errorf("states = %v; want the first split only", got)
	}
}

// A fallback attempt keeps the reviewer's frozen chain and points at its
// first attempt.
func TestReviewMemberChainCopiedOnFallback(t *testing.T) {
	s := newStore(t)
	split := multiSplit(3, nil)
	id := multiRun(t, s, mr1, "a1", split)
	b := reviewerOf(t, s, id, 2)
	// Only a reported member falls back.
	if _, created, err := s.EnqueueReviewMemberAttempt(ctx, b, quotaMark("id-R2")); err != nil || created {
		t.Errorf("EnqueueReviewMemberAttempt of a queued member = %v, %v; want nothing", created, err)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 0 {
		t.Errorf("marks = %+v; want none", marks)
	}
	dispatchReviewer(t, s, b.ID, "tok-b")
	if err := s.SetReviewMemberEntry(ctx, b.ID, RunEntry{Source: SourceGlobal, ID: "id-R2", Name: "R2", Position: 1, Length: 2}, "anthropic", "model-R2"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportReviewMemberFailure(ctx, b.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b = reviewerOf(t, s, id, 2)
	if b.Entry.ID != "id-R2" || b.Entry.Name != "R2" || b.Entry.Length != 2 || b.Provider != "anthropic" || b.Model != "model-R2" {
		t.Errorf("member = %+v; want its entry", b)
	}
	if err := s.SetReviewMemberEntry(ctx, b.ID, RunEntry{Source: SourceGlobal, ID: "x"}, "x", "y"); err == nil {
		t.Error("SetReviewMemberEntry on a reported member succeeded")
	}
	next, created, err := s.EnqueueReviewMemberAttempt(ctx, b, quotaMark("id-R2"))
	if err != nil || !created || next == "" || next == b.ID {
		t.Fatalf("EnqueueReviewMemberAttempt = %q, %v, %v", next, created, err)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 1 || marks[0].Ref.ID != "id-R2" {
		t.Errorf("marks = %+v; want id-R2", marks)
	}
	n := reviewerOf(t, s, id, 2)
	if n.ID != next || n.State != MemberQueued || n.AttemptOf == nil || *n.AttemptOf != b.ID || n.RunID != id ||
		n.Entry.ID != "" || !reflect.DeepEqual(n.Chain, split.Reviewers[1].Chain) {
		t.Errorf("next attempt = %+v; want queued, pointing at the first, with the frozen chain", n)
	}
	if got := reviewerStates(t, s, id); !slices.Equal(got[2], []MemberState{MemberFellBack, MemberQueued}) ||
		!slices.Equal(got[1], []MemberState{MemberRunning}) || !slices.Equal(got[3], []MemberState{MemberRunning}) {
		t.Errorf("states = %v; want 2 fell back and queued again, 1 and 3 still running", got)
	}
	if _, created, err := s.EnqueueReviewMemberAttempt(ctx, b, quotaMark("id-R2")); err != nil || created {
		t.Errorf("second EnqueueReviewMemberAttempt = %v, %v; want nothing", created, err)
	}
	// The third attempt still points at the first, with the same chain.
	dispatchReviewer(t, s, next, "tok-n")
	if ok, err := s.ReportReviewMemberFailure(ctx, next, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	third, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 2), quotaMark("id-R2b"))
	if err != nil || !created {
		t.Fatal(third, created, err)
	}
	if n := reviewerOf(t, s, id, 2); n.ID != third || n.AttemptOf == nil || *n.AttemptOf != b.ID ||
		!reflect.DeepEqual(n.Chain, split.Reviewers[1].Chain) {
		t.Errorf("third attempt = %+v; want attempt_of the first, the same chain", n)
	}
	// With the run gone, nothing is queued.
	a := reviewerOf(t, s, id, 1)
	if ok, err := s.ReportReviewMemberFailure(ctx, a.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	enqueue(t, s, mr1, "a2")
	if _, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 1), quotaMark("id-R1")); err != nil || created {
		t.Errorf("EnqueueReviewMemberAttempt with the run gone = %v, %v; want nothing", created, err)
	}
	if _, _, err := s.EnqueueReviewMemberAttempt(ctx, ReviewMember{ID: "bad"}, quotaMark("x")); err == nil {
		t.Error("EnqueueReviewMemberAttempt of a bad id succeeded")
	}
}

func TestReleaseMultiReviewOneDone(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, nil))
	failedReviewer(t, s, reviewerOf(t, s, id, 1).ID, "R1 broke")
	doneReviewer(t, s, reviewerOf(t, s, id, 2).ID, `{}`)
	failedReviewer(t, s, reviewerOf(t, s, id, 3).ID, "R3 broke")
	setRun(t, s, id, `not_before = $2`, t0.Add(time.Hour))
	released, failed, err := s.ReleaseMultiReview(ctx, id, t0.Add(time.Minute))
	if err != nil || !released || failed {
		t.Fatalf("ReleaseMultiReview = %v, %v, %v; want released", released, failed, err)
	}
	r := getRun(t, s, id)
	if r.State != Queued || r.SkipReason != "" || !r.NotBefore.IsZero() || r.FailureReason != "" {
		t.Errorf("run = %s %q not before %v; want queued again, due now", r.State, r.SkipReason, r.NotBefore)
	}
	if released, failed, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || released || failed {
		t.Errorf("second ReleaseMultiReview = %v, %v, %v; want nothing", released, failed, err)
	}
	if got, err := s.ClaimQueued(ctx, 10, t0); err != nil || len(got) != 1 || got[0].ID != id {
		t.Errorf("ClaimQueued = %v, %v; want the released run", runIDs(got), err)
	}
	if released, failed, err := s.ReleaseMultiReview(ctx, "bad", t0); err != nil || released || failed {
		t.Errorf("ReleaseMultiReview(bad) = %v, %v, %v; want nothing", released, failed, err)
	}
}

// Every reviewer failed: the review fails with each reviewer's reason and
// name, in number order. A failed reviewer that kept a partial result and a
// skipped reviewer are failed too: never a pass.
func TestReleaseMultiReviewAllFailed(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(4, map[int]string{3: "no usable entry"}))

	// Reviewer 1 falls back from R1 to R1b, then fails: its name is R1b.
	r1 := reviewerOf(t, s, id, 1)
	dispatchReviewer(t, s, r1.ID, "tok-r1")
	if err := s.SetReviewMemberEntry(ctx, r1.ID, RunEntry{Source: SourceGlobal, ID: "id-R1", Name: "R1", Position: 1, Length: 2}, "anthropic", "model-R1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportReviewMemberFailure(ctx, r1.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	next, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 1), quotaMark("id-R1"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	dispatchReviewer(t, s, next, "tok-r1b")
	if err := s.SetReviewMemberEntry(ctx, next, RunEntry{Source: SourceGlobal, ID: "id-R1b", Name: "R1b", Position: 2, Length: 2}, "anthropic", "model-R1b"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportReviewMemberFailure(ctx, next, Failure{Reason: "R1 broke"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if st, err := s.SettleReviewMember(ctx, next); err != nil || st != MemberFailed {
		t.Fatal(st, err)
	}
	// Reviewer 2 delivered a partial result, then its chain had no entry left.
	r2 := reviewerOf(t, s, id, 2)
	reportReviewer(t, s, r2.ID, `{"partial":true}`)
	if ok, err := s.FailReviewMemberKeepingResult(ctx, r2.ID, "out of tokens, no entry left"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Reviewer 4 never started.
	r4 := reviewerOf(t, s, id, 4)
	if ok, err := s.ReportReviewMemberFailure(ctx, r4.ID, Failure{Reason: strings.Repeat("x", 3000)}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if st, err := s.SettleReviewMember(ctx, r4.ID); err != nil || st != MemberFailed {
		t.Fatal(st, err)
	}

	released, failed, err := s.ReleaseMultiReview(ctx, id, t0.Add(time.Minute))
	if err != nil || released || !failed {
		t.Fatalf("ReleaseMultiReview = %v, %v, %v; want failed", released, failed, err)
	}
	r := getRun(t, s, id)
	want := "every reviewer failed: reviewer 1 (R1b): R1 broke; reviewer 2 (R2): out of tokens, no entry left; " +
		"reviewer 3 (R3): no usable entry; reviewer 4 (R4): " + strings.Repeat("x", maxChangeDetail)
	if r.State != Reported || r.FailureReason != want || !r.ReportedAt.Equal(t0.Add(time.Minute)) || r.Result != nil {
		t.Errorf("run = %s %q reported %v result %s; want reported with every reason", r.State, r.FailureReason, r.ReportedAt, r.Result)
	}
	if due, err := s.RunsToPublish(ctx, t0.Add(time.Minute), 10); err != nil || len(due) != 1 || due[0].ID != id {
		t.Errorf("RunsToPublish = %v, %v; want the failed run", runIDs(due), err)
	}
}

// Reviewer 1 failed and reviewer 2 was skipped: no reviewer is done, so the
// review fails.
func TestReleaseMultiReviewSkippedOnly(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, map[int]string{2: "no different model left"}))
	failedReviewer(t, s, reviewerOf(t, s, id, 1).ID, "R1 broke")
	released, failed, err := s.ReleaseMultiReview(ctx, id, t0.Add(time.Minute))
	if err != nil || released || !failed {
		t.Fatalf("ReleaseMultiReview = %v, %v, %v; want failed", released, failed, err)
	}
	want := "every reviewer failed: reviewer 1 (R1): R1 broke; reviewer 2 (R2): no different model left"
	if r := getRun(t, s, id); r.State != Reported || r.FailureReason != want {
		t.Errorf("run = %s %q; want reported with %q", r.State, r.FailureReason, want)
	}
}

func TestReleaseMultiReviewWaitsForLive(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, map[int]string{3: "the token quota"}))
	check := func(when string) {
		t.Helper()
		if released, failed, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || released || failed {
			t.Errorf("%s: ReleaseMultiReview = %v, %v, %v; want nothing", when, released, failed, err)
		}
		if r := getRun(t, s, id); r.State != Waiting || r.SkipReason != WaitReviewers {
			t.Errorf("%s: run = %s %q; want still waiting", when, r.State, r.SkipReason)
		}
	}
	check("both queued")
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	doneReviewer(t, s, a.ID, `{}`)
	check("2 queued")
	dispatchReviewer(t, s, b.ID, "tok-b")
	check("2 running")
	if ok, err := s.ReportReviewMemberFailure(ctx, b.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	check("2 reported, not settled")
	next, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 2), quotaMark("id-R2"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	check("2 fell back, its next attempt queued")
	doneReviewer(t, s, next, `{}`)
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Errorf("ReleaseMultiReview with 1 and 2 done, 3 skipped = %v, %v; want released", released, err)
	}
	if ms, err := s.ReviewMembersToSettle(ctx, 20); err != nil || len(ms) != 0 {
		t.Errorf("ReviewMembersToSettle = %+v, %v; want none", ms, err)
	}
}

// A multi review is to release once none of its members is live, and no
// longer once it is released. Another review with live members, and a held
// run, are not listed.
func TestMultiReviewsToRelease(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	other := multiRun(t, s, mr2, "b1", multiSplit(2, nil))
	heldRun(t, s, MRKey{Instance: mr1.Instance, ProjectID: 9, IID: 9}, "c1", "ci")
	want := func(when string, roots ...string) {
		t.Helper()
		got, err := s.MultiReviewsToRelease(ctx)
		if err != nil || !slices.Equal(got, roots) {
			t.Errorf("%s: MultiReviewsToRelease = %v, %v; want %v", when, got, err, roots)
		}
	}
	want("all queued")
	doneReviewer(t, s, reviewerOf(t, s, id, 1).ID, `{}`)
	b := reviewerOf(t, s, id, 2)
	dispatchReviewer(t, s, b.ID, "tok-b")
	if ok, err := s.ReportReviewMemberFailure(ctx, b.ID, Failure{Reason: "broke"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	want("2 reported, not settled")
	if st, err := s.SettleReviewMember(ctx, b.ID); err != nil || st != MemberFailed {
		t.Fatal(st, err)
	}
	want("both settled", id)
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	want("released")
	if r := getRun(t, s, other); r.State != Waiting {
		t.Errorf("the other review = %s; want still waiting", r.State)
	}
}

func TestReviewMembersToSettle(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	dispatchReviewer(t, s, a.ID, "tok-a")
	if _, err := s.MarkReviewMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreReviewMemberResult(ctx, a.RunID, a.ID, "tok-a", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	if st, err := s.SettleReviewMember(ctx, b.ID); err != nil || st != "" {
		t.Errorf("SettleReviewMember of a running member = %q, %v; want nothing", st, err)
	}
	ms, err := s.ReviewMembersToSettle(ctx, 20)
	if err != nil || len(ms) != 1 || ms[0].ID != a.ID {
		t.Fatalf("ReviewMembersToSettle = %+v, %v; want reviewer 1", ms, err)
	}
	if err := s.ReplaceReviewMemberWithFailure(ctx, a.ID, Failure{Reason: "bad result", Step: "check"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceReviewMemberWithFailure(ctx, a.ID, Failure{Reason: "again"}); !errors.Is(err, ErrNotReported) {
		t.Errorf("second ReplaceReviewMemberWithFailure = %v; want ErrNotReported", err)
	}
	if st, err := s.SettleReviewMember(ctx, a.ID); err != nil || st != MemberFailed {
		t.Errorf("SettleReviewMember = %q, %v; want failed", st, err)
	}
	if got := reviewerOf(t, s, id, 1); got.Result != nil || got.FailureReason != "bad result" || got.FailureStep != "check" {
		t.Errorf("reviewer 1 = %+v; want the failure", got)
	}
}

// ReviewMembersToSettle takes at most limit members, the oldest reports first.
func TestReviewMembersToSettleLimit(t *testing.T) {
	s := newStore(t)
	var want []string
	for i, mr := range []MRKey{mr1, mr2, mr3} {
		id := multiRun(t, s, mr, "h"+strconv.Itoa(i), multiSplit(2, nil))
		a := reviewerOf(t, s, id, 1)
		dispatchReviewer(t, s, a.ID, "tok-"+a.ID)
		if ok, err := s.ReportReviewMemberFailure(ctx, a.ID, Failure{Reason: "broke"}, t0.Add(time.Duration(i)*time.Minute)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		want = append(want, a.ID)
	}
	ms, err := s.ReviewMembersToSettle(ctx, 2)
	if err != nil || len(ms) != 2 || ms[0].ID != want[0] || ms[1].ID != want[1] {
		t.Fatalf("ReviewMembersToSettle(2) = %+v, %v; want the 2 oldest reports", ms, err)
	}
	if ms, err := s.ReviewMembersToSettle(ctx, 10); err != nil || len(ms) != 3 {
		t.Errorf("ReviewMembersToSettle(10) = %d, %v; want 3", len(ms), err)
	}
}

// A member is settled, and its result turned into a failure, only while its
// run still waits for it.
func TestSettleReviewMemberNeedsWaitingRun(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, nil))
	for n := 1; n <= 3; n++ {
		reportReviewer(t, s, reviewerOf(t, s, id, n).ID, `{}`)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'superseded' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if st, err := s.SettleReviewMember(ctx, reviewerOf(t, s, id, 1).ID); err != nil || st != "" {
		t.Errorf("SettleReviewMember = %q, %v; want nothing: the run no longer waits", st, err)
	}
	if err := s.ReplaceReviewMemberWithFailure(ctx, reviewerOf(t, s, id, 2).ID, Failure{Reason: "bad"}); !errors.Is(err, ErrNotReported) {
		t.Errorf("ReplaceReviewMemberWithFailure = %v; want ErrNotReported", err)
	}
	if ok, err := s.FailReviewMemberKeepingResult(ctx, reviewerOf(t, s, id, 3).ID, "no entry left"); err != nil || ok {
		t.Errorf("FailReviewMemberKeepingResult = %v, %v; want false: the run no longer waits", ok, err)
	}
	for n := 1; n <= 3; n++ {
		if got := reviewerOf(t, s, id, n); got.State != MemberReported || got.Result == nil || got.FailureReason != "" {
			t.Errorf("reviewer %d = %s %q; want reported with its result", n, got.State, got.FailureReason)
		}
	}
}

func TestFailReviewMemberKeepingResult(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	if ok, err := s.FailReviewMemberKeepingResult(ctx, b.ID, "x"); err != nil || ok {
		t.Errorf("FailReviewMemberKeepingResult of a queued member = %v, %v; want false", ok, err)
	}
	reportReviewer(t, s, a.ID, `{"partial":true}`)
	if ok, err := s.FailReviewMemberKeepingResult(ctx, a.ID, "out of tokens, no entry left"); err != nil || !ok {
		t.Fatalf("FailReviewMemberKeepingResult = %v, %v; want true", ok, err)
	}
	got := reviewerOf(t, s, id, 1)
	if got.State != MemberFailed || got.FailureReason != "out of tokens, no entry left" || string(got.Result) != `{"partial": true}` {
		t.Errorf("reviewer 1 = %s %q %s; want failed, the reason, the result kept", got.State, got.FailureReason, got.Result)
	}
	if ok, err := s.FailReviewMemberKeepingResult(ctx, a.ID, "again"); err != nil || ok {
		t.Errorf("second FailReviewMemberKeepingResult = %v, %v; want false", ok, err)
	}
	if ok, err := s.FailReviewMemberKeepingResult(ctx, "bad", "x"); err != nil || ok {
		t.Errorf("FailReviewMemberKeepingResult(bad) = %v, %v; want false", ok, err)
	}
	// A member failed keeping its result is not done: it does not release
	// the review alone.
	failedReviewer(t, s, b.ID, "R2 broke")
	if released, failed, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || released || !failed {
		t.Errorf("ReleaseMultiReview = %v, %v, %v; want failed", released, failed, err)
	}
}

func TestCountRunningCountsReviewMembers(t *testing.T) {
	s := newStore(t)
	dispatched(t, s, mr2, "b1", "tok")
	id := multiRun(t, s, mr1, "a1", multiSplit(3, map[int]string{3: "no usable entry"}))
	if n, err := s.CountRunning(ctx); err != nil || n != 1 {
		t.Fatalf("CountRunning = %d, %v; want 1 (the waiting run is not running)", n, err)
	}
	dispatchReviewer(t, s, reviewerOf(t, s, id, 1).ID, "tok-a")
	if n, err := s.CountRunning(ctx); err != nil || n != 3 {
		t.Errorf("CountRunning = %d, %v; want 3 (a run and 2 members, not the skipped one)", n, err)
	}
	// A panel member still counts too, until the panel tables go.
	p := panelRun(t, s, mr3, "c1")
	dispatchMember(t, s, memberOf(t, s, p, "a").ID, "tok-p")
	if n, err := s.CountRunning(ctx); err != nil || n != 5 {
		t.Errorf("CountRunning = %d, %v; want 5 (with 2 panel members)", n, err)
	}
}

// A claim takes at most limit members; a member whose run no longer waits,
// a skipped one, or one of a skipped instance, is never claimed.
func TestClaimQueuedReviewMembersSkipsUnconnected(t *testing.T) {
	s := newStore(t)
	other := MRKey{Instance: "https://other.example.com", ProjectID: 1, IID: 1}
	id := multiRun(t, s, other, "a1", multiSplit(3, map[int]string{3: "no usable entry"}))
	if got, err := s.ClaimQueuedReviewMembers(ctx, 10, t0, other.Instance); err != nil || len(got) != 0 {
		t.Fatalf("ClaimQueuedReviewMembers skipping the instance = %+v, %v; want none", got, err)
	}
	a := reviewerOf(t, s, id, 1)
	if err := s.PostponeReviewMember(ctx, a.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimQueuedReviewMembers(ctx, 10, t0)
	if err != nil || len(got) != 1 || got[0].Reviewer != 2 || got[0].State != MemberRunning {
		t.Fatalf("ClaimQueuedReviewMembers = %+v, %v; want reviewer 2 only, running", got, err)
	}
	got, err = s.ClaimQueuedReviewMembers(ctx, 10, t0.Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("ClaimQueuedReviewMembers later = %+v, %v; want reviewer 1", got, err)
	}
	if got, err := s.ClaimQueuedReviewMembers(ctx, 10, t0.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Errorf("third claim = %+v, %v; want none", got, err)
	}
	if got, err := s.ClaimQueuedReviewMembers(ctx, 0, t0); err != nil || got != nil {
		t.Errorf("claim of 0 = %+v, %v; want none", got, err)
	}
}

// 4 dispatchers never claim the same member, nor one whose run is gone.
func TestClaimQueuedReviewMembersOnce(t *testing.T) {
	s := newStore(t)
	gone := multiRun(t, s, mr1, "a1", multiSplit(3, nil))
	enqueue(t, s, mr1, "a2") // supersedes the waiting run
	for i := 0; i < 4; i++ {
		multiRun(t, s, MRKey{Instance: mr2.Instance, ProjectID: 9, IID: int64(i + 1)}, "h", multiSplit(3, nil))
	}
	var wg sync.WaitGroup
	got := make([][]ReviewMember, 4)
	errs := make([]error, 4)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = s.ClaimQueuedReviewMembers(ctx, 4, t0)
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range got {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for _, m := range got[i] {
			if seen[m.ID] {
				t.Errorf("member %s claimed twice", m.ID)
			}
			if m.RunID == gone {
				t.Errorf("member of a superseded run claimed")
			}
			seen[m.ID] = true
		}
	}
	if len(seen) != 12 {
		t.Errorf("claimed %d members; want 12", len(seen))
	}
}

// One review's 3 queued members and limit 1 give one.
func TestClaimQueuedReviewMembersLimit(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, nil))
	if got, err := s.ClaimQueuedReviewMembers(ctx, 1, t0); err != nil || len(got) != 1 {
		t.Fatalf("ClaimQueuedReviewMembers(1) = %d members, %v; want 1", len(got), err)
	}
	running := 0
	for _, st := range reviewerStates(t, s, id) {
		if st[0] == MemberRunning {
			running++
		}
	}
	if running != 1 {
		t.Errorf("running members = %d; want 1", running)
	}
	if n, err := s.CountRunning(ctx); err != nil || n != 1 {
		t.Errorf("CountRunning = %d, %v; want 1", n, err)
	}
}

func TestMarkReviewMemberDispatchedFirst(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, nil))
	a, b, c := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2), reviewerOf(t, s, id, 3)
	if _, err := s.MarkReviewMemberDispatched(ctx, a.ID, "job-a", hash("ta"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("MarkReviewMemberDispatched of a queued member = %v; want ErrNotRunning", err)
	}
	if _, err := s.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	for i, m := range []ReviewMember{b, a, c} {
		first, err := s.MarkReviewMemberDispatched(ctx, m.ID, "job-"+m.ID, hash("t"+m.ID), t0.Add(time.Hour), t0.Add(time.Second))
		if err != nil || first != (i == 0) {
			t.Errorf("MarkReviewMemberDispatched %d = %v, %v; want first %v", i, first, err, i == 0)
		}
	}
	got := reviewerOf(t, s, id, 2)
	if got.JobName != "job-"+b.ID || !got.TokenExpiresAt.Equal(t0.Add(time.Hour)) || !got.DispatchedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("reviewer 2 = %+v; want its Job recorded", got)
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM review_members WHERE id = $1::uuid`, b.ID).Scan(&stored); err != nil ||
		!bytes.Equal(stored, hash("t"+b.ID)) {
		t.Errorf("token_hash = %x, %v; want the hash only", stored, err)
	}
	other := multiRun(t, s, mr2, "b1", multiSplit(2, nil))
	oa := reviewerOf(t, s, other, 1)
	if _, err := s.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr2, "b2")
	if _, err := s.MarkReviewMemberDispatched(ctx, oa.ID, "job", hash("t"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("MarkReviewMemberDispatched with the run gone = %v; want ErrNotRunning", err)
	}
	if _, err := s.MarkReviewMemberDispatched(ctx, "bad", "job", hash("t"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("MarkReviewMemberDispatched of a bad id = %v; want ErrNotRunning", err)
	}
}

func TestReviewMemberResultToken(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	dispatchReviewer(t, s, a.ID, "tok-a")
	if _, err := s.MarkReviewMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, id, token string
		at              time.Time
	}{
		{"wrong token", a.ID, "nope", t0},
		{"other member's token", a.ID, "tok-b", t0},
		{"no token", a.ID, "", t0},
		{"expired", a.ID, "tok-a", t0.Add(2 * time.Hour)},
		{"unknown member", "00000000-0000-0000-0000-000000000000", "tok-a", t0},
		{"bad id", "x", "tok-a", t0},
	} {
		if out, err := s.StoreReviewMemberResult(ctx, id, c.id, c.token, []byte(`{}`), c.at); err != nil || out != ResultUnauthorized {
			t.Errorf("%s: StoreReviewMemberResult = %v, %v; want unauthorized", c.name, out, err)
		}
	}
	other := multiRun(t, s, mr2, "b1", multiSplit(2, nil))
	for _, run := range []string{other, "x"} {
		if out, err := s.StoreReviewMemberResult(ctx, run, a.ID, "tok-a", []byte(`{}`), t0); err != nil || out != ResultUnauthorized {
			t.Errorf("run %s: StoreReviewMemberResult = %v, %v; want unauthorized", run, out, err)
		}
	}
	// A panel member's token is no review member's.
	p := panelRun(t, s, mr3, "c1")
	pa := memberOf(t, s, p, "a")
	dispatchMember(t, s, pa.ID, "tok-pa")
	if out, err := s.StoreReviewMemberResult(ctx, p, pa.ID, "tok-pa", []byte(`{}`), t0); err != nil || out != ResultUnauthorized {
		t.Errorf("panel member: StoreReviewMemberResult = %v, %v; want unauthorized", out, err)
	}
	if out, err := s.StoreReviewMemberResult(ctx, a.RunID, a.ID, "tok-a", []byte(`not json`), t0); !errors.Is(err, ErrInvalidResult) {
		t.Errorf("bad body = %v, %v; want ErrInvalidResult", out, err)
	}
	if out, err := s.StoreReviewMemberResult(ctx, a.RunID, a.ID, "tok-a", []byte(`{"x":1}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreReviewMemberResult = %v, %v; want stored", out, err)
	}
	got := reviewerOf(t, s, id, 1)
	if got.State != MemberReported || string(got.Result) != `{"x": 1}` || !got.ReportedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("member = %s %s %v; want reported with the result", got.State, got.Result, got.ReportedAt)
	}
	if out, err := s.StoreReviewMemberResult(ctx, a.RunID, a.ID, "tok-a", []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultConflict {
		t.Errorf("second post = %v, %v; want conflict", out, err)
	}
	if ok, err := s.ReportReviewMemberFailure(ctx, b.ID, Failure{Reason: "boom"}, t0); err != nil || !ok {
		t.Fatalf("ReportReviewMemberFailure = %v, %v", ok, err)
	}
	if out, err := s.StoreReviewMemberResult(ctx, b.RunID, b.ID, "tok-b", []byte(`{}`), t0); err != nil || out != ResultConflict {
		t.Errorf("post after a failure = %v, %v; want conflict", out, err)
	}
	if ok, err := s.ReportReviewMemberFailure(ctx, a.ID, Failure{Reason: "late"}, t0); err != nil || ok {
		t.Errorf("ReportReviewMemberFailure on a reported member = %v, %v; want false", ok, err)
	}
	if r := getRun(t, s, id); r.State != Waiting || r.Result != nil || r.FailureReason != "" {
		t.Errorf("run = %s %s %q; want still waiting", r.State, r.Result, r.FailureReason)
	}
}

func TestReviewMemberResultDiscardedAfterSupersede(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	dispatchReviewer(t, s, a.ID, "tok-a")
	if _, err := s.MarkReviewMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr1, "a2")
	if out, err := s.StoreReviewMemberResult(ctx, a.RunID, a.ID, "tok-a", []byte(`{}`), t0); err != nil || out != ResultDiscarded {
		t.Errorf("StoreReviewMemberResult with the run gone = %v, %v; want discarded", out, err)
	}
	if n, err := s.SupersedeOrphanReviewMembers(ctx); err != nil || n != 2 {
		t.Fatalf("SupersedeOrphanReviewMembers = %d, %v; want 2", n, err)
	}
	if out, err := s.StoreReviewMemberResult(ctx, b.RunID, b.ID, "tok-b", []byte(`{}`), t0); err != nil || out != ResultDiscarded {
		t.Errorf("StoreReviewMemberResult of a superseded member = %v, %v; want discarded", out, err)
	}
	if got := reviewerOf(t, s, id, 1); got.Result != nil || got.State != MemberSuperseded {
		t.Errorf("reviewer 1 = %s %s; want superseded, no result", got.State, got.Result)
	}
}

func TestSupersedeMultiReviewSupersedesMembers(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, map[int]string{3: "no usable entry"}))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	doneReviewer(t, s, a.ID, `{}`)
	dispatchReviewer(t, s, b.ID, "tok-b")
	if err := s.SupersedeMultiReview(ctx, id, SupersededDraft); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.State != Superseded || r.OutcomeReason != SupersededDraft {
		t.Errorf("run = %s %q; want superseded as a draft", r.State, r.OutcomeReason)
	}
	if got := reviewerStates(t, s, id); !slices.Equal(got[1], []MemberState{MemberDone}) ||
		!slices.Equal(got[2], []MemberState{MemberSuperseded}) || !slices.Equal(got[3], []MemberState{MemberSkipped}) {
		t.Errorf("states = %v; want 1 done, 2 superseded, 3 skipped", got)
	}
	clean, err := s.ReviewMembersToCleanUp(ctx)
	if err != nil || len(clean) != 1 || clean[0].ID != b.ID || clean[0].JobName != "job-tok-b" {
		t.Fatalf("ReviewMembersToCleanUp = %+v, %v; want reviewer 2", clean, err)
	}
	if err := s.MarkReviewMemberCleanedUp(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if clean, err := s.ReviewMembersToCleanUp(ctx); err != nil || len(clean) != 0 {
		t.Errorf("ReviewMembersToCleanUp after = %+v, %v; want none", clean, err)
	}
	if err := s.MarkReviewMemberCleanedUp(ctx, "bad"); err == nil {
		t.Error("MarkReviewMemberCleanedUp of a bad id succeeded")
	}
	other := enqueue(t, s, mr2, "b1")
	if err := s.SupersedeMultiReview(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, other); r.State != Queued {
		t.Errorf("queued run = %s; want left alone", r.State)
	}
	// A panel's waiting run is not a multi review's.
	p := panelRun(t, s, mr3, "c1")
	if err := s.SupersedeMultiReview(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, p); r.State != Waiting {
		t.Errorf("panel run = %s; want left alone", r.State)
	}
	if err := s.SupersedeMultiReview(ctx, "bad", ""); err != nil {
		t.Errorf("SupersedeMultiReview of a bad id = %v; want nil", err)
	}
}

func TestMultiReviewRun(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	if r, found, err := s.MultiReviewRun(ctx, id); err != nil || !found || r.ID != id || r.State != Waiting {
		t.Fatalf("MultiReviewRun = %s %s %v, %v; want the waiting run", r.ID, r.State, found, err)
	}
	later := enqueue(t, s, mr2, "b1")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'superseded' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.MultiReviewRun(ctx, id); err != nil || found {
		t.Fatalf("MultiReviewRun of a superseded review = %v, %v; want none", found, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET attempt_of = $2::uuid, state = 'waiting', skip_reason = 'review_members'
		WHERE id = $1::uuid`, later, id); err != nil {
		t.Fatal(err)
	}
	if r, found, err := s.MultiReviewRun(ctx, id); err != nil || !found || r.ID != later {
		t.Errorf("MultiReviewRun = %s %v, %v; want the later attempt", r.ID, found, err)
	}
	p := panelRun(t, s, mr3, "c1")
	if _, found, err := s.MultiReviewRun(ctx, p); err != nil || found {
		t.Errorf("MultiReviewRun of a panel run = %v, %v; want none", found, err)
	}
	held := heldRun(t, s, mr1, "a3", WaitUntrusted)
	if _, found, err := s.MultiReviewRun(ctx, held); err != nil || found {
		t.Errorf("MultiReviewRun of a held run = %v, %v; want none", found, err)
	}
	if _, found, err := s.MultiReviewRun(ctx, "bad"); err != nil || found {
		t.Errorf("MultiReviewRun of a bad id = %v, %v; want none", found, err)
	}
}

func TestSupersedeOrphanReviewMembers(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, map[int]string{3: "no usable entry"}))
	keep := multiRun(t, s, mr2, "b1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	doneReviewer(t, s, a.ID, `{}`)
	dispatchReviewer(t, s, b.ID, "tok-b")
	if n, err := s.SupersedeOrphanReviewMembers(ctx); err != nil || n != 0 {
		t.Fatalf("SupersedeOrphanReviewMembers with live runs = %d, %v; want 0", n, err)
	}
	enqueue(t, s, mr1, "a2")
	if r := getRun(t, s, id); r.State != Superseded {
		t.Fatalf("waiting run = %s; want superseded by the new head", r.State)
	}
	if n, err := s.SupersedeOrphanReviewMembers(ctx); err != nil || n != 1 {
		t.Fatalf("SupersedeOrphanReviewMembers = %d, %v; want 1", n, err)
	}
	if got := reviewerStates(t, s, id); !slices.Equal(got[1], []MemberState{MemberDone}) ||
		!slices.Equal(got[2], []MemberState{MemberSuperseded}) || !slices.Equal(got[3], []MemberState{MemberSkipped}) {
		t.Errorf("states = %v; want 1 done, 2 superseded, 3 skipped", got)
	}
	if got := reviewerStates(t, s, keep); got[1][0] != MemberRunning && got[1][0] != MemberQueued {
		t.Errorf("other review's members = %v; want left alone", got)
	}
	if n, err := s.SupersedeOrphanReviewMembers(ctx); err != nil || n != 0 {
		t.Errorf("second SupersedeOrphanReviewMembers = %d, %v; want 0", n, err)
	}
}

// ReviewerResult serves reviewer n's newest attempt that is done, or failed
// with its result kept, to the judge run holding its live token.
func TestReviewerResult(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(5, map[int]string{5: "no usable entry"}))
	// 1: failed with no result. 2: done. 3: failed, its result kept.
	// 4: a result on its fell-back attempt only, then failed.
	failedReviewer(t, s, reviewerOf(t, s, id, 1).ID, "R1 broke")
	doneReviewer(t, s, reviewerOf(t, s, id, 2).ID, `{"reviewer":2}`)
	r3 := reviewerOf(t, s, id, 3)
	reportReviewer(t, s, r3.ID, `{"reviewer":3}`)
	if ok, err := s.FailReviewMemberKeepingResult(ctx, r3.ID, "no entry left"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	r4 := reviewerOf(t, s, id, 4)
	reportReviewer(t, s, r4.ID, `{"reviewer":4}`)
	next, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 4), quotaMark("id-R4"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	failedReviewer(t, s, next, "R4b broke")

	// The run waits: no token yet.
	if body, found, err := s.ReviewerResult(ctx, id, "tok-p", 2); err != nil || found || body != nil {
		t.Errorf("ReviewerResult before the judge = %s, %v, %v; want nothing", body, found, err)
	}
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	// The token check reads the wall clock.
	if err := s.MarkDispatched(ctx, id, "job-p", "base", "start", hash("tok-p"), time.Now().Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]string{2: `{"reviewer": 2}`, 3: `{"reviewer": 3}`} {
		if body, found, err := s.ReviewerResult(ctx, id, "tok-p", n); err != nil || !found || string(body) != want {
			t.Errorf("ReviewerResult(%d) = %s, %v, %v; want %s", n, body, found, err, want)
		}
	}
	other := dispatched(t, s, mr2, "b1", "tok-o")
	for _, c := range []struct {
		name, run, token string
		n                int
	}{
		{"failed with no result", id, "tok-p", 1},
		{"a result on a fell-back attempt only", id, "tok-p", 4},
		{"skipped", id, "tok-p", 5},
		{"reviewer 6", id, "tok-p", 6},
		{"reviewer 0", id, "tok-p", 0},
		{"wrong token", id, "tok-a", 2},
		{"no token", id, "", 2},
		{"other run's token", id, "tok-o", 2},
		{"other run", other, "tok-o", 2},
		{"unknown run", "00000000-0000-0000-0000-000000000000", "tok-p", 2},
		{"bad id", "x", "tok-p", 2},
	} {
		if body, found, err := s.ReviewerResult(ctx, c.run, c.token, c.n); err != nil || found || body != nil {
			t.Errorf("%s: ReviewerResult = %s, %v, %v; want nothing", c.name, body, found, err)
		}
	}
	if _, found, err := s.ReviewerResultAt(ctx, id, "tok-p", 2, time.Now().Add(2*time.Hour)); err != nil || found {
		t.Errorf("expired token: found %v, %v; want nothing", found, err)
	}

	// A later judge attempt reads its root's members; the first one's token
	// no longer works.
	if out, err := s.StoreResult(ctx, id, "tok-p", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	retry, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, id))
	if err != nil || !created {
		t.Fatal(retry, created, err)
	}
	reportAttemptToRunning(t, s, retry, "tok-p2")
	if body, found, err := s.ReviewerResult(ctx, retry, "tok-p2", 3); err != nil || !found || string(body) != `{"reviewer": 3}` {
		t.Errorf("later attempt: ReviewerResult = %s, %v, %v; want the root's reviewer 3", body, found, err)
	}
	if _, found, err := s.ReviewerResult(ctx, id, "tok-p", 2); err != nil || found {
		t.Errorf("superseded first attempt: found %v, %v; want nothing", found, err)
	}
}

func TestEnqueueJudgeRetry(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	doneReviewer(t, s, reviewerOf(t, s, id, 1).ID, `{}`)
	doneReviewer(t, s, reviewerOf(t, s, id, 2).ID, `{}`)
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, id)); err != nil || created {
		t.Errorf("EnqueueJudgeRetry of a running run = %v, %v; want nothing", created, err)
	}
	if err := s.MarkDispatched(ctx, id, "job-p", "base-sha", "start-sha", hash("tok-p"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, "tok-p", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	next, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, id))
	if err != nil || !created || next == "" || next == id {
		t.Fatalf("EnqueueJudgeRetry = %q, %v, %v", next, created, err)
	}
	if r := getRun(t, s, id); r.State != Superseded || r.Outcome != OutcomeFellBack || r.OutcomeReason != OutcomeJudgeFailed {
		t.Errorf("judge run = %s %q %q; want superseded, fell_back, judge_failed", r.State, r.Outcome, r.OutcomeReason)
	}
	n := getRun(t, s, next)
	if n.State != Queued || n.AttemptOf == nil || *n.AttemptOf != id || n.HeadSHA != "a1" || n.TargetBranch != "main" {
		t.Errorf("next attempt = %+v; want queued, of the review, the same head", n)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 0 {
		t.Errorf("marks = %+v; want none", marks)
	}
	if _, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, id)); err != nil || created {
		t.Errorf("second EnqueueJudgeRetry = %v, %v; want nothing", created, err)
	}
	// The members stay the review's.
	if ms, err := s.ReviewMembers(ctx, next); err != nil || len(ms) != 2 {
		t.Errorf("ReviewMembers(next) = %d, %v; want the review's 2", len(ms), err)
	}
	if _, _, err := s.EnqueueJudgeRetry(ctx, Run{ID: "bad"}); err == nil {
		t.Error("EnqueueJudgeRetry of a bad id succeeded")
	}
}

func TestSetJudgeRoundTrip(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if _, found, err := s.JudgeOf(ctx, id); err != nil || found {
		t.Errorf("JudgeOf before = %v, %v; want none", found, err)
	}
	old := JudgeRow{Reviewers: []int{1, 2}, Provider: "p", Model: "m", Entry: RunEntry{Source: SourceGlobal, ID: "old"}}
	if err := s.SetJudge(ctx, id, old); err != nil {
		t.Fatal(err)
	}
	want := JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "anthropic", Model: "sonnet",
		Entry: RunEntry{Source: SourceTeam, ID: "t1", Name: "Team", TeamID: 4, Position: 2, Length: 3}}
	if err := s.SetJudge(ctx, id, want); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.JudgeOf(ctx, id); err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Errorf("JudgeOf = %+v, %v, %v; want %+v", got, found, err, want)
	}
	noJudge := JudgeRow{Reviewers: []int{1, 2}, NoJudge: true, NoJudgeReason: "the judge chain has no entry"}
	if err := s.SetJudge(ctx, id, noJudge); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.JudgeOf(ctx, id); err != nil || !found || !reflect.DeepEqual(got, noJudge) {
		t.Errorf("JudgeOf = %+v, %v, %v; want %+v", got, found, err, noJudge)
	}
	// A run that is not queued or running is refused.
	rep := reported(t, s, mr2, "b1", `{}`)
	if err := s.SetJudge(ctx, rep, want); err == nil {
		t.Error("SetJudge of a reported run succeeded")
	}
	w := multiRun(t, s, mr3, "c1", multiSplit(2, nil))
	if err := s.SetJudge(ctx, w, want); err == nil {
		t.Error("SetJudge of a waiting run succeeded")
	}
	if err := s.SetJudge(ctx, "bad", want); err == nil {
		t.Error("SetJudge of a bad id succeeded")
	}
	if _, found, err := s.JudgeOf(ctx, "bad"); err != nil || found {
		t.Errorf("JudgeOf(bad) = %v, %v; want none", found, err)
	}
}

func TestPruneRunLogsPrunesReviewMembers(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	for _, m := range []string{a.ID, b.ID} {
		if err := s.PutReviewMemberLog(ctx, m, "log \x00"+m); err != nil {
			t.Fatal(err)
		}
		if err := s.PutReviewMemberDiagnostics(ctx, m, Diagnostics{CloneLog: "c", CloneLogMasked: "cm", PodState: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if text, found, err := s.ReviewMemberLog(ctx, a.ID); err != nil || !found || text != "log "+a.ID {
		t.Errorf("ReviewMemberLog = %q, %v, %v", text, found, err)
	}
	if d, found, err := s.ReviewMemberDiagnostics(ctx, a.ID); err != nil || !found || d != (Diagnostics{"c", "cm", "p"}) {
		t.Errorf("ReviewMemberDiagnostics = %+v, %v, %v", d, found, err)
	}
	if _, found, _ := s.RunLog(ctx, id); found {
		t.Error("the run has a log")
	}
	for _, table := range []string{"review_member_logs", "review_member_diagnostics"} {
		if _, err := s.pool.Exec(ctx, `UPDATE `+table+` SET created_at = now() - interval '31 days' WHERE member_id = $1::uuid`, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneRunLogs(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneRunLogs = %d, %v; want 2", n, err)
	}
	if _, found, _ := s.ReviewMemberLog(ctx, a.ID); found {
		t.Error("reviewer 1's old log is still there")
	}
	if _, found, _ := s.ReviewMemberDiagnostics(ctx, a.ID); found {
		t.Error("reviewer 1's old diagnostics are still there")
	}
	if _, found, _ := s.ReviewMemberLog(ctx, b.ID); !found {
		t.Error("reviewer 2's fresh log was pruned")
	}
	if _, found, _ := s.ReviewMemberDiagnostics(ctx, b.ID); !found {
		t.Error("reviewer 2's fresh diagnostics were pruned")
	}
	if _, found, err := s.ReviewMemberLog(ctx, "bad"); found || err != nil {
		t.Errorf("ReviewMemberLog of a bad id = %v, %v", found, err)
	}
	if err := s.PutReviewMemberLog(ctx, "bad", "x"); err == nil {
		t.Error("PutReviewMemberLog of a bad id succeeded")
	}
}

// ReviewMembersToLog lists the reported, done, failed and fell-back members
// with a Job and no log, reported since the given time.
func TestReviewMembersToLog(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	doneReviewer(t, s, a.ID, `{}`)
	queued := multiRun(t, s, mr2, "b1", multiSplit(2, nil))
	ids := func(since time.Time) []string {
		t.Helper()
		ms, err := s.ReviewMembersToLog(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	if got := ids(t0); !slices.Equal(got, []string{a.ID}) {
		t.Errorf("ReviewMembersToLog = %v; want reviewer 1 alone (2 is running)", got)
	}
	dispatchReviewer(t, s, b.ID, "tok-b")
	if ok, err := s.ReportReviewMemberFailure(ctx, b.ID, Failure{Reason: "x"}, t0.Add(2*time.Minute)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got := ids(t0); !slices.Equal(got, []string{a.ID, b.ID}) {
		t.Errorf("ReviewMembersToLog = %v; want 1 then 2", got)
	}
	if got := ids(t0.Add(90 * time.Second)); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("ReviewMembersToLog since after 1's report = %v; want 2 alone", got)
	}
	if err := s.PutReviewMemberLog(ctx, a.ID, "log"); err != nil {
		t.Fatal(err)
	}
	if got := ids(t0); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("ReviewMembersToLog after 1's log = %v; want 2 alone", got)
	}
	for _, n := range []int{1, 2} {
		if slices.Contains(ids(t0), reviewerOf(t, s, queued, n).ID) {
			t.Errorf("a member with no Job is listed")
		}
	}
}

func TestRecordMultiReviewOffOnce(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if _, ok, err := s.MultiReviewOffReason(ctx, id); err != nil || ok {
		t.Fatalf("MultiReviewOffReason before any = %v, %v; want none", ok, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMultiReviewOff(ctx, id, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMultiReviewOff(ctx, id, "no other reviewer can start"); err != nil {
		t.Fatalf("second RecordMultiReviewOff: %v", err)
	}
	ms, err := s.ReviewMembers(ctx, id)
	if err != nil || len(ms) != 1 || ms[0].State != MemberOff || ms[0].Reviewer != 1 ||
		ms[0].FailureReason != "no other reviewer can start" || ms[0].Chain.Entries != nil {
		t.Fatalf("ReviewMembers = %+v, %v; want one off row with the last reason", ms, err)
	}
	if got, ok, err := s.MultiReviewOffReason(ctx, id); err != nil || !ok || got != "no other reviewer can start" {
		t.Errorf("MultiReviewOffReason = %q, %v, %v; want the reason", got, ok, err)
	}
	if judge, err := s.IsJudgeRun(ctx, id); err != nil || judge {
		t.Errorf("IsJudgeRun of a review with its multi review off = %v, %v; want false", judge, err)
	}
	if got, err := s.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil || len(got) != 0 {
		t.Errorf("ClaimQueuedReviewMembers = %+v, %v; want none", got, err)
	}
	if n, err := s.SupersedeOrphanReviewMembers(ctx); err != nil || n != 0 {
		t.Errorf("SupersedeOrphanReviewMembers = %d, %v; want 0", n, err)
	}
	if err := s.RecordMultiReviewOff(ctx, "00000000-0000-0000-0000-000000000000", "x"); err == nil {
		t.Error("RecordMultiReviewOff of an unknown run succeeded")
	}
	// The run is claimed again and records nothing: the off row is stale.
	if err := s.Requeue(ctx, id, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.MultiReviewOffReason(ctx, id); err != nil || ok {
		t.Errorf("MultiReviewOffReason after a claim with no off = %q, %v, %v; want none", got, ok, err)
	}
	// A later attempt's off row is its review's.
	if err := s.MarkDispatched(ctx, id, "job", "base", "start", hash("t"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, "t", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	next, _, err := s.EnqueueAttempt(ctx, getRun(t, s, id), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMultiReviewOff(ctx, next, "the token quota is used up"); err != nil {
		t.Fatal(err)
	}
	if ms, err := s.ReviewMembers(ctx, id); err != nil || len(ms) != 1 || ms[0].RunID != id || ms[0].FailureReason != "the token quota is used up" {
		t.Errorf("ReviewMembers = %+v, %v; want the one off row of the review", ms, err)
	}
	// A multi review that starts on a later attempt replaces the off row.
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartMultiReview(ctx, next, multiSplit(2, nil), t0); err != nil || !started {
		t.Fatalf("StartMultiReview of the later attempt = %v, %v", started, err)
	}
	if got := reviewerStates(t, s, id); !slices.Equal(got[1], []MemberState{MemberQueued}) || !slices.Equal(got[2], []MemberState{MemberQueued}) {
		t.Errorf("states = %v; want 2 queued members of the review, no off row", got)
	}
	if got, err := s.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil || len(got) != 2 {
		t.Errorf("ClaimQueuedReviewMembers = %+v, %v; want both, their run being the later attempt", got, err)
	}
	if _, ok, err := s.MultiReviewOffReason(ctx, "bad"); err != nil || ok {
		t.Errorf("MultiReviewOffReason of a bad id = %v, %v; want none", ok, err)
	}
}

// A multi review started on a later attempt is read and released by that
// attempt's id as well as by the first run's.
func TestReleaseMultiReviewByLaterAttempt(t *testing.T) {
	s := newStore(t)
	root := reported(t, s, mr1, "a1", `{}`)
	next, created, err := s.EnqueueAttempt(ctx, getRun(t, s, root), quotaMark("m1"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartMultiReview(ctx, next, multiSplit(3, nil), t0); err != nil || !started {
		t.Fatalf("StartMultiReview = %v, %v", started, err)
	}
	if _, found, err := s.JudgeChainOf(ctx, next); err != nil || !found {
		t.Errorf("JudgeChainOf(later attempt) = %v, %v; want the review's", found, err)
	}
	byRoot, err := s.ReviewMembers(ctx, root)
	if err != nil || len(byRoot) != 3 {
		t.Fatalf("ReviewMembers(root) = %+v, %v; want 3", byRoot, err)
	}
	byNext, err := s.ReviewMembers(ctx, next)
	if err != nil || !reflect.DeepEqual(byNext, byRoot) {
		t.Fatalf("ReviewMembers(later attempt) = %+v, %v; want the root's members", byNext, err)
	}
	for n := 1; n <= 3; n++ {
		doneReviewer(t, s, reviewerOf(t, s, next, n).ID, `{}`)
	}
	released, failed, err := s.ReleaseMultiReview(ctx, next, t0)
	if err != nil || !released || failed {
		t.Fatalf("ReleaseMultiReview(later attempt) = %v, %v, %v; want released", released, failed, err)
	}
	if r := getRun(t, s, next); r.State != Queued || r.SkipReason != "" {
		t.Errorf("later attempt = %s %q; want queued again", r.State, r.SkipReason)
	}
	if r := getRun(t, s, root); r.State != Superseded {
		t.Errorf("first run = %s; want left superseded", r.State)
	}
}

func TestRecoverUnlaunchedReviewMembersRequeues(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	a, b := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	dispatchReviewer(t, s, a.ID, "tok-a") // claims 2 too, with no Job
	if rs, err := s.RunningReviewMembers(ctx); err != nil || len(rs) != 2 {
		t.Fatalf("RunningReviewMembers = %+v, %v; want 2", rs, err)
	}
	if n, err := s.RecoverUnlaunchedReviewMembers(ctx, t0); err != nil || n != 0 {
		t.Errorf("RecoverUnlaunchedReviewMembers before the claim = %d, %v; want 0", n, err)
	}
	if n, err := s.RecoverUnlaunchedReviewMembers(ctx, t0.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("RecoverUnlaunchedReviewMembers = %d, %v; want 1", n, err)
	}
	if got := reviewerOf(t, s, id, 2); got.State != MemberQueued || got.Attempts != 0 {
		t.Errorf("reviewer 2 = %s attempts %d; want queued, no attempt counted", got.State, got.Attempts)
	}
	if _, err := s.ClaimQueuedReviewMembers(ctx, 10, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueReviewMember(ctx, b.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := reviewerOf(t, s, id, 2); got.State != MemberQueued || got.Attempts != 1 || !got.NotBefore.Equal(t0.Add(time.Hour)) {
		t.Errorf("reviewer 2 = %s attempts %d not before %v; want queued, one attempt", got.State, got.Attempts, got.NotBefore)
	}
	if err := s.RequeueReviewMember(ctx, "bad", t0); err == nil {
		t.Error("RequeueReviewMember of a bad id succeeded")
	}
	if err := s.SetReviewMemberPod(ctx, a.ID, "pod-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceReviewMemberCheckoutSeen(ctx, a.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceReviewMemberCheckoutSeen(ctx, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := reviewerOf(t, s, id, 1); got.PodName != "pod-a" || got.CheckoutSeen != 2 {
		t.Errorf("reviewer 1 = pod %q seen %d; want pod-a, 2", got.PodName, got.CheckoutSeen)
	}
}

// 3 dispatches at once: one is first. 2 posts of a result at once: one is stored.
func TestReviewMemberDispatchAndResultRaces(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 5; i++ {
		id := multiRun(t, s, MRKey{Instance: mr1.Instance, ProjectID: 8, IID: int64(i + 1)}, "h", multiSplit(3, nil))
		ms := []ReviewMember{reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2), reviewerOf(t, s, id, 3)}
		if _, err := s.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		firsts := make([]bool, 3)
		errs := make([]error, 3)
		for j, m := range ms {
			wg.Add(1)
			go func(j int, m ReviewMember) {
				defer wg.Done()
				firsts[j], errs[j] = s.MarkReviewMemberDispatched(ctx, m.ID, "job", hash("tok-"+strconv.Itoa(m.Reviewer)), t0.Add(time.Hour), t0)
			}(j, m)
		}
		wg.Wait()
		n := 0
		for j := range firsts {
			if errs[j] != nil {
				t.Fatal(errs[j])
			}
			if firsts[j] {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("dispatches = %v; want exactly one first", firsts)
		}
		outs := make([]ResultOutcome, 2)
		for j := range outs {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				outs[j], errs[j] = s.StoreReviewMemberResult(ctx, id, ms[0].ID, "tok-1", []byte(`{}`), t0)
			}(j)
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || !slices.Contains(outs, ResultStored) || !slices.Contains(outs, ResultConflict) {
			t.Fatalf("posts = %v, %v; want one stored, one conflict", outs, errs)
		}
	}
}

// A later attempt of the judge takes what the split run recorded; a first
// run, or an attempt that is not running, is refused.
func TestInheritMultiReviewSplit(t *testing.T) {
	s := newStore(t)
	root := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET quiet_reviews = true, provider = 'openai', rules_hash = 'rh', built_in_rules_note = 'left out',
		model_source = 'team', model_entry_id = 'e1', model_entry_name = 'Team A', model_team_id = 4,
		chain_position = 1, chain_length = 2, linked_repos = '[{"target":"acme/api","can_block":true}]'::jsonb,
		title = 'Add login', author = 'alice' WHERE id = $1::uuid`, root); err != nil {
		t.Fatal(err)
	}
	doneReviewer(t, s, reviewerOf(t, s, root, 1).ID, `{}`)
	doneReviewer(t, s, reviewerOf(t, s, root, 2).ID, `{}`)
	if released, _, err := s.ReleaseMultiReview(ctx, root, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.InheritMultiReviewSplit(ctx, root); err == nil {
		t.Error("InheritMultiReviewSplit of the first run succeeded")
	}
	if err := s.MarkDispatched(ctx, root, "job-p", "base-sha", "start-sha", hash("tok-p"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, root, "tok-p", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	next, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, root))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	if err := s.InheritMultiReviewSplit(ctx, next); err == nil {
		t.Error("InheritMultiReviewSplit of a queued attempt succeeded")
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.InheritMultiReviewSplit(ctx, next); err != nil {
		t.Fatal(err)
	}
	a := getRun(t, s, next)
	if a.BaseSHA != "base-sha" || a.StartSHA != "start-sha" || !a.Quiet || a.ModelEntryID != "e1" || a.RulesHash == nil || *a.RulesHash != "rh" || a.BuiltInRulesNote != "left out" ||
		len(a.LinkedRepos) != 1 || a.SkillSources["security"] != "library" || a.Title != "Add login" {
		t.Errorf("attempt = %+v; want the split run's fields", a)
	}
}

// A multi review's run that an older release queued, then claimed, goes back
// to wait for its members. A run that is not claimed, or has a Job, is left
// alone.
func TestWaitForReviewMembers(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'queued', skip_reason = '', not_before = NULL WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if back, err := s.WaitForReviewMembers(ctx, id); err != nil || back {
		t.Errorf("WaitForReviewMembers(queued) = %v, %v; want left alone", back, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if back, err := s.WaitForReviewMembers(ctx, id); err != nil || !back {
		t.Fatalf("WaitForReviewMembers(claimed) = %v, %v; want back to wait", back, err)
	}
	if r := getRun(t, s, id); r.State != Waiting || r.SkipReason != WaitReviewers {
		t.Errorf("run = %s %q; want waiting for its members", r.State, r.SkipReason)
	}
	if back, err := s.WaitForReviewMembers(ctx, id); err != nil || back {
		t.Errorf("WaitForReviewMembers(waiting) = %v, %v; want left alone", back, err)
	}
	other := dispatched(t, s, mr2, "b1", "tok")
	if back, err := s.WaitForReviewMembers(ctx, other); err != nil || back {
		t.Errorf("WaitForReviewMembers(with a Job) = %v, %v; want left alone", back, err)
	}
	if back, err := s.WaitForReviewMembers(ctx, "nope"); err != nil || back {
		t.Errorf("WaitForReviewMembers(unknown) = %v, %v", back, err)
	}
}

// Nothing in the review member code writes panel_members: a legacy panel
// off row of the review stays as it is through a whole multi review.
func TestPanelMembersUntouched(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPanelOff(ctx, id, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMultiReviewOff(ctx, id, "no other reviewer can start"); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartMultiReview(ctx, id, multiSplit(3, map[int]string{3: "no usable entry"}), t0); err != nil || !started {
		t.Fatal(started, err)
	}
	r1, r2 := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	reportReviewer(t, s, r1.ID, `{}`)
	if err := s.PutReviewMemberLog(ctx, r1.ID, "log"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutReviewMemberDiagnostics(ctx, r1.ID, Diagnostics{CloneLog: "c"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FailReviewMemberKeepingResult(ctx, r1.ID, "no entry left"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	dispatchReviewer(t, s, r2.ID, "tok-r2")
	if ok, err := s.ReportReviewMemberFailure(ctx, r2.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	next, _, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 2), quotaMark("id-R2"))
	if err != nil {
		t.Fatal(err)
	}
	doneReviewer(t, s, next, `{}`)
	if _, err := s.SupersedeOrphanReviewMembers(ctx); err != nil {
		t.Fatal(err)
	}
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	var n int
	var reason string
	if err := s.pool.QueryRow(ctx, `SELECT count(*), max(failure_reason) FROM panel_members`).Scan(&n, &reason); err != nil {
		t.Fatal(err)
	}
	if n != 1 || reason != "legacy" {
		t.Errorf("panel_members = %d rows, reason %q; want the one legacy off row", n, reason)
	}
	for _, table := range []string{"panel_member_logs", "panel_member_diagnostics", "panel_judges"} {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows; want 0", table, n)
		}
	}
}
