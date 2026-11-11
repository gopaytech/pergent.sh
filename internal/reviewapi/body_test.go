package reviewapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// parse reads an inline result, a completed one with the given skills.
func parse(t *testing.T, skills ...string) *result.Result {
	t.Helper()
	res, err := result.Parse([]byte(`{"schema_version":1,"status":"completed","comment":{"markdown":"x"},"skills":[` +
		strings.Join(skills, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// skillJSON is one skill: its verdict, the files it reviewed, and its findings.
func skillJSON(name, verdict string, reviewed []string, findings ...string) string {
	r, _ := json.Marshal(reviewed)
	return `{"name":"` + name + `","verdict":"` + verdict + `","findings":[` + strings.Join(findings, ",") +
		`],"files":{"reviewed":` + string(r) + `,"skipped":[],"not_reviewed":[]}}`
}

func findingJSON(fp, severity string) string {
	return `{"fingerprint":"` + fp + `","file":"a.go","line":3,"severity":"` + severity +
		`","title":"t ` + fp + `","explanation":"e","suggested_fix":"f"}`
}

var one = []string{"a.go"}

func TestVerdictOrder(t *testing.T) {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	block := findingJSON("aaaa", "blocking")
	cases := []struct {
		name       string
		skills     []string
		dismissals []DismissalRef
		want       string
	}{
		{"a failed skill beats a blocking one", []string{
			skillJSON("a", "failed", nil), skillJSON("b", "has_blocking_issues", one, block)}, nil, VerdictFailed},
		{"every skill reviewed nothing", []string{
			skillJSON("a", "no_blocking_issues", nil), skillJSON("b", "no_blocking_issues", []string{})}, nil, VerdictNothingReviewed},
		{"a blocking finding", []string{skillJSON("a", "has_blocking_issues", one, block)}, nil, VerdictHasBlockingIssues},
		{"nothing wrong", []string{skillJSON("a", "no_blocking_issues", one)}, nil, VerdictNoBlockingIssues},
		{"its blocking dismissal", []string{skillJSON("a", "has_blocking_issues", one, block)},
			[]DismissalRef{{Fingerprint: "aaaa", Severity: "blocking", Reason: "intended", At: at}}, VerdictNoBlockingIssues},
		{"a suggestion dismissal does not cover a blocking finding", []string{skillJSON("a", "has_blocking_issues", one, block)},
			[]DismissalRef{{Fingerprint: "aaaa", Severity: "suggestion", Reason: "meh", At: at}}, VerdictHasBlockingIssues},
		{"another fingerprint covers nothing", []string{skillJSON("a", "has_blocking_issues", one, block)},
			[]DismissalRef{{Fingerprint: "bbbb", Severity: "blocking", Reason: "x", At: at}}, VerdictHasBlockingIssues},
		{"one of two blocking findings dismissed", []string{skillJSON("a", "has_blocking_issues", one, block, findingJSON("cccc", "blocking"))},
			[]DismissalRef{{Fingerprint: "aaaa", Severity: "blocking", Reason: "x", At: at}}, VerdictHasBlockingIssues},
		{"an unknown skill verdict never passes", []string{skillJSON("a", "passed", one)}, nil, VerdictFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := Map(parse(t, c.skills...), nil, c.dismissals, nil)
			if got != c.want {
				t.Fatalf("verdict = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDismissedFindingStays(t *testing.T) {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	res := parse(t, skillJSON("a", "has_blocking_issues", one, findingJSON("aaaa", "blocking"), findingJSON("bbbb", "suggestion")))
	_, skills, _ := Map(res, nil, []DismissalRef{
		{Fingerprint: "aaaa", Severity: "suggestion", Reason: "too weak", At: at},
		{Fingerprint: "bbbb", Severity: "blocking", Reason: "covers a suggestion", At: at},
	}, nil)
	fs := skills[0].Findings
	if len(fs) != 2 {
		t.Fatalf("findings = %d, want both kept", len(fs))
	}
	if fs[0].Dismissed != nil {
		t.Fatalf("blocking finding dismissed by a suggestion dismissal: %+v", fs[0].Dismissed)
	}
	if d := fs[1].Dismissed; d == nil || d.Reason != "covers a suggestion" || !d.At.Equal(at) {
		t.Fatalf("suggestion finding dismissed = %+v", d)
	}
	if fs[0].ID == "" || fs[0].ID == fs[1].ID {
		t.Fatalf("ids = %q, %q", fs[0].ID, fs[1].ID)
	}
	if fs[0].Fix != "f" || fs[0].Line != 3 {
		t.Fatalf("finding = %+v", fs[0])
	}
}

func TestFirstDismissalWins(t *testing.T) {
	res := parse(t, skillJSON("a", "has_blocking_issues", one, findingJSON("aaaa", "blocking")))
	_, skills, _ := Map(res, nil, []DismissalRef{
		{Fingerprint: "aaaa", Severity: "suggestion", Reason: "weak"},
		{Fingerprint: "aaaa", Severity: "blocking", Reason: "first that covers"},
		{Fingerprint: "aaaa", Severity: "blocking", Reason: "later"},
	}, nil)
	if d := skills[0].Findings[0].Dismissed; d == nil || d.Reason != "first that covers" {
		t.Fatalf("dismissed = %+v", d)
	}
}

func TestSkillNothingReviewed(t *testing.T) {
	res := parse(t,
		skillJSON("empty", "no_blocking_issues", nil),
		skillJSON("real", "has_blocking_issues", one, findingJSON("aaaa", "blocking")))
	verdict, skills, _ := Map(res, nil, nil, nil)
	if skills[0].Verdict != VerdictNothingReviewed {
		t.Fatalf("empty skill = %q", skills[0].Verdict)
	}
	if skills[1].Verdict != VerdictHasBlockingIssues {
		t.Fatalf("real skill = %q", skills[1].Verdict)
	}
	if verdict != VerdictHasBlockingIssues {
		t.Fatalf("verdict = %q", verdict)
	}
	if skills[0].Findings == nil || skills[0].Files.Reviewed == nil || skills[0].Files.Skipped == nil || skills[0].Files.NotReviewed == nil {
		t.Fatalf("empty lists must marshal as [], not null: %+v", skills[0])
	}
}

func TestMapOfErrorResultNeverPasses(t *testing.T) {
	data, err := os.ReadFile("../result/testdata/error.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := result.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := Map(res, nil, nil, nil); v != VerdictFailed {
		t.Fatalf("verdict = %q", v)
	}
	if v, _, _ := Map(nil, nil, nil, nil); v != VerdictFailed {
		t.Fatalf("nil result verdict = %q", v)
	}
	res = parse(t, skillJSON("a", "no_blocking_issues", one))
	res.Skills = nil
	if v, _, _ := Map(res, nil, nil, nil); v != VerdictNothingReviewed {
		t.Fatalf("no skills verdict = %q", v)
	}
}

func TestSkipRulesMapped(t *testing.T) {
	rules := map[string]string{
		"deleted": "deleted", "binary": "binary", "no content changes": "no_content_changes",
		"lockfile": "lockfile", "vendored": "vendored", "generated": "generated",
		"out of scope": "out_of_scope", "excluded by skill": "excluded_by_skill", "new rule": "other", "": "other",
	}
	var skipped []string
	for rule := range rules {
		skipped = append(skipped, `{"path":"`+rule+`.txt","rule":"`+rule+`"}`)
	}
	res := parse(t, `{"name":"a","verdict":"no_blocking_issues","findings":[],"files":{"reviewed":["a.go"],"skipped":[`+
		strings.Join(skipped, ",")+`],"not_reviewed":[]}}`)
	_, skills, _ := Map(res, nil, nil, nil)
	got := skills[0].Files.Skipped
	if len(got) != len(rules) {
		t.Fatalf("skipped = %d, want %d", len(got), len(rules))
	}
	for _, s := range got {
		in := strings.TrimSuffix(s.Path, ".txt")
		if s.Rule != rules[in] {
			t.Errorf("rule %q -> %q, want %q", in, s.Rule, rules[in])
		}
	}
}

func TestNotReviewedCarriesCause(t *testing.T) {
	res := parse(t, `{"name":"a","verdict":"failed","findings":[],"files":{"reviewed":["b.go"],"skipped":[],
		"not_reviewed":[{"path":"big.go","cause":"too large for one batch"}]}}`)
	verdict, skills, _ := Map(res, nil, nil, nil)
	nr := skills[0].Files.NotReviewed
	if len(nr) != 1 || nr[0].Path != "big.go" || nr[0].Reason != "too large for one batch" {
		t.Fatalf("not_reviewed = %+v", nr)
	}
	if skills[0].Verdict != VerdictFailed || verdict != VerdictFailed {
		t.Fatalf("verdicts = %q, %q", skills[0].Verdict, verdict)
	}
	// A skill that says it passed but lists a file it could not review is failed.
	res = parse(t, `{"name":"a","verdict":"no_blocking_issues","findings":[],"files":{"reviewed":["b.go"],"skipped":[],
		"not_reviewed":[{"path":"big.go","cause":"x"}]}}`)
	if verdict, skills, _ = Map(res, nil, nil, nil); skills[0].Verdict != VerdictFailed || verdict != VerdictFailed {
		t.Fatalf("verdicts = %q, %q", skills[0].Verdict, verdict)
	}
}

func TestMapRedacts(t *testing.T) {
	secret := func(s string) string { return strings.ReplaceAll(s, "sk-secret", "***") }
	f := `{"fingerprint":"aaaa","file":"sk-secret/a.go","severity":"suggestion","title":"sk-secret","explanation":"leaks sk-secret",
		"suggested_fix":"drop sk-secret","suggestion":"x := sk-secret"}`
	res := parse(t, `{"name":"a","verdict":"failed","findings":[`+f+`],"files":{"reviewed":["sk-secret.go"],
		"skipped":[{"path":"sk-secret.lock","rule":"lockfile"}],"not_reviewed":[{"path":"sk-secret.big","cause":"sk-secret"}]}}`)
	_, skills, _ := Map(res, nil, []DismissalRef{{Fingerprint: "aaaa", Severity: "blocking", Reason: "sk-secret"}}, secret)
	out, _ := json.Marshal(skills)
	if strings.Contains(string(out), "sk-secret") {
		t.Fatalf("unredacted: %s", out)
	}
	if got := skills[0].Findings[0].Explanation; got != "leaks ***" {
		t.Fatalf("explanation = %q", got)
	}
}

func TestMapUsage(t *testing.T) {
	res := parse(t, skillJSON("a", "no_blocking_issues", one), skillJSON("b", "no_blocking_issues", one))
	_, _, u := Map(res, []store.SkillUsage{{Skill: "a", In: 10, Out: 2}, {Skill: "b", In: 5, Out: 1}}, nil, nil)
	if u.TokensIn != 15 || u.TokensOut != 3 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestMapSample(t *testing.T) {
	data, err := os.ReadFile("../result/testdata/completed.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := result.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	verdict, skills, _ := Map(res, nil, nil, nil)
	if verdict != VerdictFailed || len(skills) != 2 {
		t.Fatalf("verdict = %q, skills = %d", verdict, len(skills))
	}
	if skills[0].Verdict != VerdictHasBlockingIssues || len(skills[0].Findings) != 2 {
		t.Fatalf("first skill = %+v", skills[0])
	}
	if r := skills[0].Files.Skipped[0]; r.Path != "go.sum" || r.Rule != "lockfile" {
		t.Fatalf("skipped = %+v", r)
	}
	if skills[1].Verdict != VerdictFailed || len(skills[1].Files.NotReviewed) != 1 {
		t.Fatalf("second skill = %+v", skills[1])
	}
}

func apiReview(state store.RunState, job string, result string) store.APIReview {
	var res []byte
	if result != "" {
		res = []byte(result)
	}
	return store.APIReview{Run: store.Run{ID: "r1", State: state, JobName: job, Result: res}}
}

func TestStatusOf(t *testing.T) {
	cases := []struct {
		name string
		r    store.APIReview
		want string
	}{
		{"queued", apiReview(store.Queued, "", ""), StatusQueued},
		{"running with no job is still queued", apiReview(store.Running, "", ""), StatusQueued},
		{"running with a job", apiReview(store.Running, "job-1", ""), StatusRunning},
		{"reported", apiReview(store.Reported, "job-1", `{"status":"completed"}`), StatusRunning},
		{"reported without a job", apiReview(store.Reported, "", ""), StatusRunning},
		{"superseded", apiReview(store.Superseded, "", ""), StatusSuperseded},
		{"published completed", apiReview(store.Published, "job-1", `{"status":"completed"}`), StatusCompleted},
		{"published error result", apiReview(store.Published, "job-1", `{"status":"error"}`), StatusFailed},
		{"published without a result", apiReview(store.Published, "job-1", ""), StatusFailed},
		{"published with an unreadable result", apiReview(store.Published, "job-1", "not json"), StatusFailed},
		{"publish failed", apiReview(store.PublishFailed, "job-1", `{"status":"completed"}`), StatusFailed},
		{"waiting is not finished", apiReview(store.Waiting, "", ""), StatusQueued},
		{"skipped never reads as reviewed", apiReview(store.Skipped, "", ""), StatusFailed},
		{"a state nobody knows", apiReview("odd", "", ""), StatusFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StatusOf(c.r); got != c.want {
				t.Fatalf("status = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFailureOf(t *testing.T) {
	failed := apiReview(store.Published, "job-1", "")
	failed.Run.FailureStep, failed.Run.FailureReason = "review", "the review job ran out of memory (OOMKilled)"
	if f := FailureOf(failed); f == nil || f.Step != StepReview || f.Reason != failed.Run.FailureReason {
		t.Fatalf("failure = %+v", f)
	}
	for _, r := range []store.APIReview{
		apiReview(store.Queued, "", ""), apiReview(store.Running, "j", ""), apiReview(store.Superseded, "", ""),
		apiReview(store.Published, "j", `{"status":"completed"}`),
	} {
		if f := FailureOf(r); f != nil {
			t.Fatalf("%s: failure = %+v, want none", r.Run.State, f)
		}
	}
	pf := apiReview(store.PublishFailed, "j", `{"status":"completed"}`)
	pf.Run.FailureReason = "some internal text"
	if f := FailureOf(pf); f == nil || f.Step != StepUnknown || f.Reason != "pergent.sh couldn't finish the review" {
		t.Fatalf("publish_failed failure = %+v", f)
	}
	// An error result carries no failure_step: the Job ran, so pergent failed.
	er := apiReview(store.Published, "j", `{"status":"error","error":{"code":"x","message":"a file name"}}`)
	f := FailureOf(er)
	if f == nil || f.Step != StepReview || strings.Contains(f.Reason, "file name") || f.Reason == "" {
		t.Fatalf("error result failure = %+v", f)
	}
	// Failed with no reason at all still says something.
	bare := apiReview(store.Published, "j", "")
	if f := FailureOf(bare); f == nil || f.Reason == "" || f.Step != StepStarting {
		t.Fatalf("bare failure = %+v", f)
	}
}

func TestFailureStep(t *testing.T) {
	for in, want := range map[string]string{
		"": StepStarting, "clone": StepStarting, "starting": StepStarting,
		"review": StepReview, "delivery": StepDelivery, "unknown": StepUnknown, "odd": StepUnknown,
	} {
		if got := stepOf(in); got != want {
			t.Errorf("stepOf(%q) = %q, want %q", in, got, want)
		}
	}
	if got := strings.Join([]string{stepOf(""), stepOf("clone"), stepOf("review"), stepOf("delivery"), stepOf("odd")}, " "); got != "starting starting review delivery unknown" {
		t.Fatalf("steps = %q", got)
	}
}

func TestPendingBody(t *testing.T) {
	r := store.APIReview{
		Run:    store.Run{ID: "11111111-1111-1111-1111-111111111111", State: store.Queued},
		Tenant: "acme", Subject: "route:1",
		CreatedAt: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC),
	}
	got, err := json.Marshal(Pending(r))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"11111111-1111-1111-1111-111111111111","subject":"route:1","tenant":"acme","title":"","status":"queued","created_at":"2026-11-11T09:00:00Z"}`
	if string(got) != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
}

func TestPendingOfAFailedReview(t *testing.T) {
	r := apiReview(store.PublishFailed, "j", "")
	r.CreatedAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	r.FinishedAt = r.CreatedAt.Add(time.Minute)
	b := Pending(r)
	if b.Status != StatusFailed || b.Verdict != VerdictFailed || b.Failure == nil || b.FinishedAt == nil {
		t.Fatalf("body = %+v", b)
	}
}
