package usage

import (
	"context"
	"errors"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func i64(v int64) *int64 { return &v }

func TestFromResultCountsEverySkill(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","skills":[
	 {"name":"security","verdict":"has_blocking_issues","findings":[{"severity":"blocking"},{"severity":"suggestion"}],
	  "tokens":{"input":100,"output":20,"reasoning":3,"cache_read":400,"cache_write":7}},
	 {"name":"style","verdict":"failed","findings":[],"tokens":{"input":5,"output":1}}]}`)
	got, err := FromResult(body, store.Run{Trigger: store.TriggerPush})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.SkillUsage{
		{Skill: "security", Verdict: "has_blocking_issues", In: 100, Out: 20, Reasoning: i64(3), CacheRead: i64(400), CacheWrite: i64(7), Findings: 2, Blocking: 1},
		{Skill: "style", Verdict: "failed", In: 5, Out: 1},
	}
	if !equal(got, want) {
		t.Errorf("FromResult = %+v\nwant %+v", got, want)
	}
}

// A re-run of failed skills is stored merged with the skills it kept, and
// those carry the earlier run's tokens: only its own skills count.
func TestFromResultRerunFailedCountsOnlyItsSkills(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","merged":true,"skills":[
	 {"name":"security","verdict":"no_blocking_issues","tokens":{"input":100,"output":20}},
	 {"name":"style","verdict":"no_blocking_issues","tokens":{"input":5,"output":1}}]}`)
	got, _ := FromResult(body, store.Run{Trigger: store.TriggerRerunFailed, SkillSubset: []string{"style"}})
	if len(got) != 1 || got[0].Skill != "style" {
		t.Errorf("FromResult = %+v, want only style", got)
	}
}

// A merged incremental result can carry the base run's impact skill as it
// was, tokens included: it isn't counted.
func TestFromResultMergedIncrementalSkipsImpact(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","merged":true,"incremental":{"reviewed":[],"carried":[],"from":"abc"},"skills":[
	 {"name":"impact","verdict":"no_blocking_issues","tokens":{"input":9,"output":9}},
	 {"name":"security","verdict":"no_blocking_issues","tokens":{"input":1,"output":1}}]}`)
	got, _ := FromResult(body, store.Run{Trigger: store.TriggerPush, Incremental: true})
	if len(got) != 1 || got[0].Skill != "security" {
		t.Errorf("FromResult = %+v, want only security", got)
	}
}

// The same body as posted live isn't merged, so its impact skill is the
// run's own.
func TestFromResultLiveIncrementalKeepsImpact(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","incremental":{"reviewed":[],"carried":[],"from":"abc"},"skills":[
	 {"name":"impact","verdict":"no_blocking_issues","tokens":{"input":9,"output":9}}]}`)
	got, _ := FromResult(body, store.Run{Trigger: store.TriggerPush, Incremental: true})
	if len(got) != 1 || got[0].Skill != "impact" {
		t.Errorf("FromResult = %+v, want impact", got)
	}
}

// A re-run of failed skills that includes impact ran it for real, even when
// the merged result also carries an incremental base.
func TestFromResultRerunOfImpactKeepsIt(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","merged":true,"incremental":{"reviewed":[],"carried":[],"from":"abc"},"skills":[
	 {"name":"impact","verdict":"no_blocking_issues","tokens":{"input":9,"output":9}},
	 {"name":"security","verdict":"no_blocking_issues","tokens":{"input":1,"output":1}}]}`)
	got, _ := FromResult(body, store.Run{Trigger: store.TriggerRerunFailed, SkillSubset: []string{"impact"}})
	if len(got) != 1 || got[0].Skill != "impact" {
		t.Errorf("FromResult = %+v, want only impact", got)
	}
}

// Counts the Job didn't report stay nil, and a reported zero stays zero.
func TestFromResultOlderPergentLeavesCountsUnknown(t *testing.T) {
	body := []byte(`{"skills":[{"name":"a","tokens":{"input":1,"output":2}},{"name":"b","tokens":{"input":1,"output":2,"reasoning":0,"cache_read":0,"cache_write":0}},{"name":"c"}]}`)
	got, err := FromResult(body, store.Run{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Reasoning != nil || got[0].CacheRead != nil || got[0].CacheWrite != nil {
		t.Errorf("a = %+v, want unknown counts", got[0])
	}
	if got[1].Reasoning == nil || got[1].CacheRead == nil || got[1].CacheWrite == nil {
		t.Errorf("b = %+v, want counts of zero", got[1])
	}
	if len(got) != 3 || got[2].In != 0 || got[2].Out != 0 {
		t.Errorf("c = %+v, want a row of zeros", got)
	}
}

func TestFromResultOddShapes(t *testing.T) {
	for _, body := range []string{`not json`, `{"skills":"x"}`, `{"skills":[{"name":""}]}`, `{"skills":[{"name":"a","tokens":{"input":-1,"output":0}}]}`, `{"skills":[{"name":"a","tokens":{"input":1,"output":0,"cache_read":-5}}]}`} {
		got, err := FromResult([]byte(body), store.Run{})
		if err == nil && len(got) != 0 {
			t.Errorf("FromResult(%s) = %+v, want nothing", body, got)
		}
	}
}

func equal(a, b []store.SkillUsage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Skill != y.Skill || x.Verdict != y.Verdict || x.In != y.In || x.Out != y.Out ||
			x.Findings != y.Findings || x.Blocking != y.Blocking ||
			!sameCount(x.CacheRead, y.CacheRead) || !sameCount(x.CacheWrite, y.CacheWrite) || !sameCount(x.Reasoning, y.Reasoning) {
			return false
		}
	}
	return true
}

// sameCount compares two optional counts by value, nil meaning unknown.
func sameCount(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Rows reads what each skill used (FromResult) and says whether the run is
// a judge run. A judge result's own tokens are the judge's (D8): a legacy
// tokens_by_model list is not read. An unreadable result is an error with
// no skills, judge or not.
func TestRowsJudge(t *testing.T) {
	body := []byte(`{"skills":[{"name":"security","tokens":{"input":10,"output":2},
	  "tokens_by_model":[{"role":"a","provider":"p1","model":"m1","tokens":{"input":9,"output":9}}]}]}`)
	for _, judge := range []bool{true, false} {
		u, err := Rows(body, store.Run{Trigger: store.TriggerPush}, judge)
		if err != nil || u.Judge != judge || len(u.Skills) != 1 || u.Skills[0].In != 10 || u.Skills[0].Out != 2 {
			t.Errorf("judge %v: Rows = %+v, %v; want the skill's own tokens", judge, u, err)
		}
		bad, err := Rows([]byte(`not json`), store.Run{}, judge)
		if err == nil || bad.Skills != nil || bad.Judge != judge {
			t.Errorf("judge %v: Rows of an odd body = %+v, %v; want an error with no skills", judge, bad, err)
		}
	}
}

// recorder records which store call Usage.Record made.
type recorder struct {
	single, judge []string
}

func (r *recorder) RecordUsage(_ context.Context, id string, _ []store.SkillUsage, models []store.ModelUsage) error {
	if models != nil {
		return errors.New("model rows passed")
	}
	r.single = append(r.single, id)
	return nil
}

func (r *recorder) RecordJudgeUsage(_ context.Context, id string, _ []store.SkillUsage) error {
	r.judge = append(r.judge, id)
	return nil
}

// A judge run is recorded with RecordJudgeUsage, which adds to the rows its
// reviewers made; any other run with RecordUsage and no model rows.
func TestUsageRecord(t *testing.T) {
	r := &recorder{}
	if err := (Usage{Judge: true}).Record(context.Background(), r, "j"); err != nil {
		t.Fatal(err)
	}
	if err := (Usage{}).Record(context.Background(), r, "s"); err != nil {
		t.Fatal(err)
	}
	if len(r.judge) != 1 || r.judge[0] != "j" || len(r.single) != 1 || r.single[0] != "s" {
		t.Errorf("judge %v, single %v; want j by RecordJudgeUsage, s by RecordUsage", r.judge, r.single)
	}
}
