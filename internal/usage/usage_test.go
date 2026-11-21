package usage

import (
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

func TestModelRows(t *testing.T) {
	body := []byte(`{"merged":true,"skills":[
	 {"name":"security","tokens_by_model":[
	  {"role":"a","provider":"p1","model":"m1","tokens":{"input":10,"output":2,"reasoning":1,"cache_read":4,"cache_write":3}},
	  {"role":"b","provider":"p2","model":"m2","tokens":{"input":7,"output":1}}]},
	 {"name":"style","tokens_by_model":[
	  {"role":"a","provider":"p1","model":"m1","tokens":{"input":5,"output":5}},
	  {"role":"b","provider":"p2","model":"m2","tokens":{"input":6,"output":6}}]}]}`)
	got, err := ModelRows(body, store.Run{Trigger: store.TriggerPush})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Skill != "security" || got[0].Role != "a" || got[1].Role != "b" || got[2].Skill != "style" {
		t.Fatalf("rows = %+v", got)
	}
	a, b := got[0], got[1]
	if a.Provider != "p1" || a.Model != "m1" || a.In != 10 || a.Out != 2 ||
		!sameCount(a.Reasoning, i64(1)) || !sameCount(a.CacheRead, i64(4)) || !sameCount(a.CacheWrite, i64(3)) {
		t.Errorf("a = %+v", a)
	}
	if b.Reasoning != nil || b.CacheRead != nil || b.CacheWrite != nil {
		t.Errorf("b = %+v, want unknown counts", b)
	}
	sub, _ := ModelRows(body, store.Run{Trigger: store.TriggerRerunFailed, SkillSubset: []string{"style"}})
	if len(sub) != 2 || sub[0].Skill != "style" {
		t.Errorf("subset rows = %+v", sub)
	}
}

func TestModelRowsNone(t *testing.T) {
	got, err := ModelRows([]byte(`{"skills":[{"name":"a","tokens":{"input":1,"output":1}}]}`), store.Run{})
	if err != nil || got != nil {
		t.Errorf("got %+v, %v; want nil", got, err)
	}
}

func TestModelRowsNegativeDropped(t *testing.T) {
	body := []byte(`{"skills":[{"name":"a","tokens_by_model":[
	 {"role":"a","model":"m","tokens":{"input":-1,"output":0}},
	 {"role":"b","model":"m","tokens":{"input":1,"output":0,"cache_read":-5}},
	 {"role":"b","model":"n","tokens":{"input":2,"output":0}}]}]}`)
	got, err := ModelRows(body, store.Run{})
	if err != nil || len(got) != 1 || got[0].Model != "n" {
		t.Errorf("got %+v, %v; want only the valid row", got, err)
	}
}
