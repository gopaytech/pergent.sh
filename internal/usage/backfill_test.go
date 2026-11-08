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
