package usage

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

type fakeRuns struct {
	pending  [][]store.Run
	recorded map[string][]store.SkillUsage
	models   map[string][]store.ModelUsage
	merges   map[string]bool
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

func (f *fakeRuns) RecordUsage(ctx context.Context, id string, skills []store.SkillUsage, models []store.ModelUsage) error {
	f.recorded[id] = skills
	if f.models != nil {
		f.models[id] = models
	}
	return nil
}

func (f *fakeRuns) IsPanelMerge(ctx context.Context, id string) (bool, error) {
	return f.merges[id], nil
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

// A panel's merge (Job P) is filled with its model rows, never nil, so the
// store adds its tokens to the members'; any other run with none, as today.
func TestBackfillPanelMerge(t *testing.T) {
	withModels := []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":3,"output":1},
		"tokens_by_model":[{"role":"b","provider":"p","model":"m","tokens":{"input":1,"output":0}},
			{"role":"a","provider":"p","model":"n","tokens":{"input":2,"output":1}}]}]}`)
	plain := []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":1,"output":1}}]}`)
	f := &fakeRuns{recorded: map[string][]store.SkillUsage{}, models: map[string][]store.ModelUsage{},
		merges: map[string]bool{"p": true, "q": true}, pending: [][]store.Run{
			{{ID: "p", Result: withModels}, {ID: "q", Result: plain}, {ID: "r", Result: plain}},
		}}
	if n := Backfill(context.Background(), f, slog.New(slog.DiscardHandler)); n != 3 {
		t.Fatalf("n = %d", n)
	}
	if m := f.models["p"]; len(m) != 2 || m[0].Role != "a" || m[1].Role != "b" {
		t.Errorf("models of p = %+v; want A then B", m)
	}
	if m := f.models["q"]; m == nil || len(m) != 0 {
		t.Errorf("models of q = %#v; want empty, not nil: it is a panel's merge", m)
	}
	if m := f.models["r"]; m != nil {
		t.Errorf("models of r = %#v; want nil", m)
	}
}

type fakeMembers struct {
	pending  [][]store.PanelMember
	runs     map[string]store.Run
	recorded map[string][]store.SkillUsage
}

func (f *fakeMembers) MembersWithoutUsage(ctx context.Context, limit int) ([]store.PanelMember, error) {
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

func (f *fakeMembers) RecordMemberUsage(ctx context.Context, id string, skills []store.SkillUsage) error {
	f.recorded[id] = skills
	return nil
}

// A member result whose usage write was lost is filled at start, with the
// skills its review's run ran; an odd result, or one whose run is gone, is
// marked with none.
func TestBackfillMembers(t *testing.T) {
	body := []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":4,"output":1}},
		{"name":"t","verdict":"failed","tokens":{"input":9,"output":9}}]}`)
	f := &fakeMembers{recorded: map[string][]store.SkillUsage{},
		runs: map[string]store.Run{"r": {ID: "r", Trigger: store.TriggerRerunFailed, SkillSubset: []string{"s"}}},
		pending: [][]store.PanelMember{
			{{ID: "a", RunID: "r", Result: body}, {ID: "b", RunID: "r", Result: []byte(`not json`)}},
			{{ID: "c", RunID: "gone", Result: body}},
		}}
	if n := BackfillMembers(context.Background(), f, slog.New(slog.DiscardHandler)); n != 3 {
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
