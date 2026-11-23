package usage

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/store/storetest"
)

type fakeRuns struct {
	pending  [][]store.Run
	recorded map[string][]store.SkillUsage
	judged   map[string]bool // recorded with RecordJudgeUsage
	judges   map[string]bool // IsJudgeRun's answers
	limits   []int
}

func (f *fakeRuns) RunsWithoutUsage(ctx context.Context, limit int) ([]store.Run, error) {
	f.limits = append(f.limits, limit)
	if len(f.pending) == 0 {
		return nil, nil
	}
	page := f.pending[0]
	f.pending = f.pending[1:]
	return page, nil
}

func (f *fakeRuns) RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error {
	f.recorded[id] = skills
	return nil
}

func (f *fakeRuns) RecordJudgeUsage(ctx context.Context, id string, skills []store.SkillUsage) error {
	f.recorded[id] = skills
	if f.judged == nil {
		f.judged = map[string]bool{}
	}
	f.judged[id] = true
	return nil
}

func (f *fakeRuns) IsJudgeRun(ctx context.Context, id string) (bool, error) {
	return f.judges[id], nil
}

func TestBackfillRecordsEveryPageAndMarksOddResults(t *testing.T) {
	f := &fakeRuns{recorded: map[string][]store.SkillUsage{}, pending: [][]store.Run{
		{{ID: "a", Result: []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":1,"output":1}}]}`)}},
		{{ID: "b", Result: []byte(`not json`)}},
	}}
	n := Backfill(context.Background(), f, slog.New(slog.DiscardHandler))
	if n != 2 || len(f.recorded["a"]) != 1 || f.recorded["b"] != nil {
		t.Errorf("n = %d, recorded = %+v", n, f.recorded)
	}
	// A page holds results of up to 10 MB each, in a pod of 512Mi.
	for _, l := range f.limits {
		if l > 5 {
			t.Errorf("a page of %d results; want at most 5", l)
		}
	}
	if _, ok := f.recorded["b"]; !ok {
		t.Error("an odd result wasn't marked, so it would be read again on every start")
	}
}

// A multi review's judge run is filled with RecordJudgeUsage, so its
// tokens add to the rows its reviewers made; any other run with RecordUsage
// and no model rows, as today.
func TestBackfillJudgeRunRoute(t *testing.T) {
	body := []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":3,"output":1}}]}`)
	f := &fakeRuns{recorded: map[string][]store.SkillUsage{}, judges: map[string]bool{"j": true},
		pending: [][]store.Run{{{ID: "j", Result: body}, {ID: "r", Result: body}}}}
	if n := Backfill(context.Background(), f, slog.New(slog.DiscardHandler)); n != 2 {
		t.Fatalf("n = %d", n)
	}
	if !f.judged["j"] || f.judged["r"] || len(f.recorded["j"]) != 1 || f.recorded["j"][0].In != 3 || len(f.recorded["r"]) != 1 {
		t.Errorf("judged = %v, recorded = %+v; want j by RecordJudgeUsage, r by RecordUsage", f.judged, f.recorded)
	}
}

type fakeMembers struct {
	pending  [][]store.ReviewMember
	runs     map[string]store.Run
	recorded map[string][]store.SkillUsage
}

func (f *fakeMembers) ReviewMembersWithoutUsage(ctx context.Context, limit int) ([]store.ReviewMember, error) {
	if len(f.pending) == 0 {
		return nil, nil
	}
	page := f.pending[0]
	f.pending = f.pending[1:]
	return page, nil
}

func (f *fakeMembers) Run(ctx context.Context, id string) (store.Run, bool, error) {
	r, ok := f.runs[id]
	return r, ok, nil
}

func (f *fakeMembers) RecordReviewMemberUsage(ctx context.Context, id string, skills []store.SkillUsage) error {
	f.recorded[id] = skills
	return nil
}

// A reviewer result whose usage write was lost is filled at start, with the
// skills its review's run ran; an odd result, or one whose run is gone, is
// marked with none.
func TestBackfillReviewMembers(t *testing.T) {
	body := []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":4,"output":1}},
		{"name":"t","verdict":"failed","tokens":{"input":9,"output":9}}]}`)
	f := &fakeMembers{recorded: map[string][]store.SkillUsage{},
		runs: map[string]store.Run{"r": {ID: "r", Trigger: store.TriggerRerunFailed, SkillSubset: []string{"s"}}},
		pending: [][]store.ReviewMember{
			{{ID: "a", RunID: "r", Result: body}, {ID: "b", RunID: "r", Result: []byte(`not json`)}},
			{{ID: "c", RunID: "gone", Result: body}},
		}}
	if n := BackfillReviewMembers(context.Background(), f, slog.New(slog.DiscardHandler)); n != 3 {
		t.Fatalf("n = %d", n)
	}
	if got := f.recorded["a"]; len(got) != 1 || got[0].Skill != "s" || got[0].In != 4 {
		t.Errorf("a = %+v; want the run's subset only", got)
	}
	for _, id := range []string{"b", "c"} {
		if got, ok := f.recorded[id]; !ok || got != nil {
			t.Errorf("%s = %+v, %v; want marked with none", id, got, ok)
		}
	}
}

// newDBStore opens a migrated store on a database of its own, which goes
// back to the pool when the test ends. It skips when no test database is set.
func newDBStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t, store.OpenInSchema)
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// A judge run whose usage write was lost is filled at start with
// RecordJudgeUsage: its tokens are added to the split run's rows, which
// hold its reviewers' tokens, and it gets one judge row per skill. With the
// single RecordUsage the reviewers' row would be kept and the judge's
// tokens lost.
func TestBackfillJudgeRun(t *testing.T) {
	t.Parallel()
	st := newDBStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	mr := store.MRKey{Instance: "gitlab", ProjectID: 7, IID: 1}
	id, _, err := st.EnqueueRun(ctx, mr, "acme/widgets", "head-sha", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	frozen := func(name string) store.FrozenChain {
		return store.FrozenChain{Scope: "repo", Entries: []store.FrozenEntry{{Source: store.SourceGlobal, ID: "id-" + name,
			Name: name, Provider: "prov", Model: "model-" + name}}}
	}
	split := store.MultiReviewSplit{BaseSHA: "base-sha", StartSHA: "start-sha", Judge: frozen("judge"),
		Inputs: store.RunInputs{Model: "model-r1", MaxTurns: 40, Sources: map[string]string{"security": "library"},
			Versions: map[string]int{"security": 1}, Hashes: map[string]string{"security": "h1"}},
		Reviewers: []store.SplitReviewer{{Number: 1, Chain: frozen("r1")}, {Number: 2, Chain: frozen("r2")}}}
	if started, err := st.StartMultiReview(ctx, id, split, t0); err != nil || !started {
		t.Fatalf("StartMultiReview = %v, %v", started, err)
	}
	ms, err := st.ReviewMembers(ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("ReviewMembers = %+v, %v", ms, err)
	}
	if _, err := st.ClaimQueuedReviewMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		name := "r" + string(rune('1'+i))
		e := store.RunEntry{Source: store.SourceGlobal, ID: "id-" + name, Name: name, Position: 1, Length: 1}
		if err := st.SetReviewMemberEntry(ctx, m.ID, e, "prov", "model-"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := st.MarkReviewMemberDispatched(ctx, m.ID, "job-"+name, tokenHash("tok-"+name), t0.Add(time.Hour), t0); err != nil {
			t.Fatal(err)
		}
		if out, err := st.StoreReviewMemberResult(ctx, id, m.ID, "tok-"+name, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != store.ResultStored {
			t.Fatalf("StoreReviewMemberResult = %v, %v", out, err)
		}
		if s, err := st.SettleReviewMember(ctx, m.ID); err != nil || s != store.MemberDone {
			t.Fatalf("SettleReviewMember = %q, %v", s, err)
		}
		// The callback's own write of each reviewer's usage.
		if err := st.RecordReviewMemberUsage(ctx, m.ID, []store.SkillUsage{{Skill: "security", In: int64(100 * (i + 1)), Out: 10}}); err != nil {
			t.Fatal(err)
		}
	}
	if released, _, err := st.ReleaseMultiReview(ctx, id, t0.Add(2*time.Minute)); err != nil || !released {
		t.Fatalf("ReleaseMultiReview = %v, %v", released, err)
	}
	if _, err := st.ClaimQueued(ctx, 10, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	judge := store.RunEntry{Source: store.SourceGlobal, ID: "id-judge", Name: "judge", Position: 1, Length: 1}
	if err := st.SetJudge(ctx, id, store.JudgeRow{Reviewers: []int{1, 2}, Provider: "prov", Model: "model-judge", Entry: judge}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDispatched(ctx, id, "job-j", "base-sha", "start-sha", tokenHash("tok-j"), t0.Add(time.Hour), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"skills":[{"name":"security","verdict":"has_blocking_issues","findings":[{"severity":"blocking"}],` +
		`"tokens":{"input":20,"output":2}}]}`)
	if out, err := st.StoreResult(ctx, id, "tok-j", body, t0.Add(3*time.Minute)); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	// The judge's usage write was lost: the fill at start records it.
	if n := Backfill(ctx, st, slog.New(slog.DiscardHandler)); n != 1 {
		t.Fatalf("Backfill marked %d runs; want the judge run", n)
	}
	if in, out, found, err := st.RunUsage(ctx, id); err != nil || !found || in != 100+200+20 || out != 10+10+2 {
		t.Errorf("RunUsage = %d in, %d out, %v, %v; want both reviewers and the judge", in, out, found, err)
	}
	costs, err := st.ReviewCosts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var judges []store.ReviewCost
	for _, c := range costs {
		if c.Part == "judge" {
			judges = append(judges, c)
		}
	}
	if len(costs) != 3 || len(judges) != 1 || judges[0].RunID != id || judges[0].Entry != "judge" ||
		judges[0].Model != "model-judge" || judges[0].In != 20 || judges[0].Out != 2 {
		t.Errorf("ReviewCosts = %+v; want 2 reviewer rows and the judge's row of 20 in, 2 out", costs)
	}
	// Marked: a second fill adds nothing.
	if n := Backfill(ctx, st, slog.New(slog.DiscardHandler)); n != 0 {
		t.Errorf("a second Backfill marked %d runs; want none", n)
	}
	if in, _, _, _ := st.RunUsage(ctx, id); in != 320 {
		t.Errorf("RunUsage after a second fill = %d; want 320", in)
	}
}
