package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// --- fixtures ---

// fakeSource is a repository reader: one default branch, files by path.
type fakeSource struct {
	mu     sync.Mutex
	branch string
	files  map[string]string // path → content
	fail   error
	calls  int
	refs   map[string]bool
	ids    map[int64]bool
}

func newFakeSource() *fakeSource {
	return &fakeSource{branch: "trunk", files: map[string]string{}, refs: map[string]bool{}, ids: map[int64]bool{}}
}

func (f *fakeSource) set(fn func(*fakeSource)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSource) DefaultBranch(_ context.Context, projectID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.ids[projectID] = true
	if f.fail != nil {
		return "", f.fail
	}
	return f.branch, nil
}

func (f *fakeSource) GetFile(_ context.Context, projectID int64, p, ref string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.refs[ref] = true
	if f.fail != nil {
		return nil, false, f.fail
	}
	c, ok := f.files[p]
	if !ok {
		return nil, false, nil
	}
	return []byte(c), true, nil
}

func (f *fakeSource) ListTree(_ context.Context, projectID int64, dir, ref string) ([]forge.TreeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.refs[ref] = true
	if f.fail != nil {
		return nil, f.fail
	}
	var out []forge.TreeEntry
	for p := range f.files {
		if path.Dir(p) == dir {
			out = append(out, forge.TreeEntry{Name: path.Base(p), Path: p, Type: "blob"})
		}
	}
	return out, nil
}

const (
	baseSHA = "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
	// llmKey and the others are values the API must never show.
	llmKey        = "llm-key-SECRET"
	reviewToken   = "glpat-review-SECRET"
	approveToken  = "glpat-approve-SECRET"
	webhookSecret = "webhook-SECRET"
	deployToken   = "deploy-token-SECRET"
	ghAppKey      = "-----BEGIN RSA PRIVATE KEY-----app-key-SECRET"
)

func head(c byte) string { return strings.Repeat(string(c), 40) }

// withSecrets adds an LLM, bot tokens, webhook secrets and a token approver
// to the config, with their values in the environment.
func withSecrets(t *testing.T) func(*config.Config) {
	return func(cfg *config.Config) {
		t.Setenv("PERGENT_TEST_LLM_KEY", llmKey)
		t.Setenv("PERGENT_TEST_REVIEW", reviewToken)
		t.Setenv("PERGENT_TEST_APPROVE", approveToken)
		t.Setenv("PERGENT_TEST_HOOK", webhookSecret)
		t.Setenv("PERGENT_TEST_APP_KEY", ghAppKey)
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "claude-sonnet-4", APIKeyEnv: "PERGENT_TEST_LLM_KEY",
			BaseURL: "https://llm.example.com/v1"}
		cfg.GitLab[0].ReviewTokenEnv = "PERGENT_TEST_REVIEW"
		cfg.GitLab[0].ApproveTokenEnv = "PERGENT_TEST_APPROVE"
		cfg.GitLab[0].WebhookSecretEnv = "PERGENT_TEST_HOOK"
		cfg.GitHub[0].ReviewApp = config.App{AppID: 1, PrivateKeyEnv: "PERGENT_TEST_APP_KEY"}
		cfg.GitHub[0].ApproveTokenEnv = "PERGENT_TEST_APPROVE"
		cfg.GitHub[0].WebhookSecretEnv = "PERGENT_TEST_HOOK"
	}
}

// signInGitLab signs alice in on main, able to see the given projects (as
// a Reporter) and an admin when admin is true.
func (e *env) signInGitLab(admin bool, visible ...string) *http.Cookie {
	e.t.Helper()
	for _, p := range visible {
		e.gl.setProject(p, gitlabProject("private", 20, 0))
	}
	e.gl.setMember("acme/platform", admin)
	return e.signIn("gitlab", "main")
}

// signInGitHub signs alice in on gh, able to see the given repositories.
func (e *env) signInGitHub(admin bool, visible ...string) *http.Cookie {
	e.t.Helper()
	for _, p := range visible {
		e.gh.setRepo(p, githubRepo(true, true))
	}
	if admin {
		e.gh.setTeam("acme/platform", "active")
	}
	return e.signIn("github", "gh")
}

func (e *env) get(c *http.Cookie, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	var cookies []*http.Cookie
	if c != nil {
		cookies = []*http.Cookie{c}
	}
	return e.do("GET", target, cookies, nil)
}

// getJSON GETs target, wants 200 and decodes the body into a T.
func getJSON[T any](e *env, c *http.Cookie, target string) T {
	e.t.Helper()
	rec := e.get(c, target)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		e.t.Errorf("GET %s Content-Type = %q", target, ct)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		e.t.Fatalf("GET %s body %s: %v", target, rec.Body, err)
	}
	return v
}

func (e *env) wantStatus(c *http.Cookie, target string, status int) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := e.get(c, target)
	if rec.Code != status {
		e.t.Errorf("GET %s = %d %s; want %d", target, rec.Code, rec.Body, status)
	}
	return rec
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// runFix is a run to store. Zero fields take defaults: instance main,
// target main, published, created an hour before the clock, a 2-minute Job.
type runFix struct {
	instance, project  string
	projectID, number  int64
	head, target       string
	state              store.RunState
	result, failure    string
	title, webURL      string
	at                 time.Time
	duration           time.Duration
	approved           bool
	undispatched       bool
	sourceBranch, user string
}

// addRun stores f and returns the run's ID.
func (e *env) addRun(f runFix) string {
	e.t.Helper()
	if f.instance == "" {
		f.instance = "main"
	}
	if f.target == "" {
		f.target = "main"
	}
	if f.state == "" {
		f.state = store.Published
	}
	if f.at.IsZero() {
		f.at = e.clock.now().Add(-time.Hour)
	}
	if f.duration == 0 {
		f.duration = 2 * time.Minute
	}
	if f.projectID == 0 {
		f.projectID = 7
	}
	mr := store.MRKey{Instance: f.instance, ProjectID: f.projectID, IID: f.number}
	id, _, err := e.st.EnqueueRun(ctx, mr, f.project, f.head, f.target)
	if err != nil {
		e.t.Fatalf("EnqueueRun: %v", err)
	}
	var dispatched, reported *time.Time
	if f.state != store.Queued && !f.undispatched {
		d := f.at.Add(10 * time.Second)
		dispatched = &d
	}
	switch f.state {
	case store.Reported, store.Published, store.PublishFailed:
		r := f.at.Add(10*time.Second + f.duration)
		reported = &r
	}
	var result *string
	if f.result != "" {
		result = &f.result
	}
	e.exec(`UPDATE runs SET state = $2, result = $3::jsonb, failure_reason = $4, base_sha = $5, created_at = $6,
			dispatched_at = $7, reported_at = $8, title = $9, web_url = $10, source_branch = $11, author = $12
		WHERE id = $1::uuid`,
		id, string(f.state), result, f.failure, baseSHA, f.at, dispatched, reported, f.title, f.webURL,
		f.sourceBranch, f.user)
	if f.approved {
		if err := e.st.SetApprovedSHA(ctx, mr, f.head, f.target); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func skillJSON(name, verdict string, findings ...map[string]any) map[string]any {
	if findings == nil {
		findings = []map[string]any{}
	}
	return map[string]any{
		"name": name, "verdict": verdict, "findings": findings,
		"files":  map[string]any{"reviewed": []string{"api/handler.go"}, "skipped": []any{}, "not_reviewed": []any{}},
		"tokens": map[string]any{"input": 100, "output": 10},
	}
}

func resultJSON(h string, skills ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"schema_version": 1, "status": "completed", "pergent_version": "0.1.0", "base": baseSHA, "head": h,
		"skills": skills, "comment": map[string]any{"markdown": "<!-- pergent -->\nreview", "findings_block": ""},
	})
	return string(b)
}

func cleanResult(h string) string {
	return resultJSON(h, skillJSON("code-review", "no_blocking_issues"))
}

func blockingResult(h string) string {
	return resultJSON(h, skillJSON("code-review", "has_blocking_issues", map[string]any{
		"id": "a1b2c3", "fingerprint": "a1b2c3d4e5f6", "file": "api/handler.go", "line": 42, "severity": "blocking",
		"title": "Request body is used before validation", "explanation": "It is not validated.",
		"suggested_fix": "Validate it."}))
}

// --- tests ---

// The standard fixture: alice sees acme/widgets on main but not
// acme/payments; acme/widgets on gh is another instance.
func overviewFixture(e *env) {
	now := e.clock.now()
	e.addRun(runFix{project: "acme/widgets", number: 1, head: head('1'), result: cleanResult(head('1')), approved: true,
		title: "Add login", webURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/1", at: now.Add(-6 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 2, head: head('2'), result: blockingResult(head('2')),
		title: "Refactor", at: now.Add(-5 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 3, head: head('3'), failure: "the review Job ran out of memory (OOMKilled)",
		at: now.Add(-4 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 4, head: head('4'), state: store.Running, at: now.Add(-3 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 6, head: head('6'), result: cleanResult(head('6')), approved: true,
		at: now.Add(-10 * 24 * time.Hour)})
	e.addRun(runFix{project: "acme/payments", projectID: 8, number: 5, head: head('5'), result: blockingResult(head('5')),
		title: "Payments secret change", at: now.Add(-2 * time.Hour)})
	e.addRun(runFix{instance: "gh", project: "acme/widgets", projectID: 900, number: 7, head: head('7'),
		result: blockingResult(head('7')), title: "GitHub change", at: now.Add(-time.Hour)})

	for _, a := range []struct{ instance, project, kind string }{
		{"main", "acme/widgets", "single_approval"},
		{"main", "acme/payments", "out_of_memory"},
		{"main", "", "token_expiry"},
		{"gh", "acme/widgets", "approver_unreachable"},
	} {
		if err := e.st.SetAttention(ctx, a.instance, a.project, a.kind, "message for "+a.kind); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestOverview(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	c := e.signInGitLab(false, "acme/widgets")

	ov := getJSON[OverviewView](e, c, "/api/overview")
	if want := (StatsView{Reviews: 4, Approved: 1, Blocking: 1, Failed: 1}); ov.Stats != want {
		t.Errorf("stats = %+v, want %+v", ov.Stats, want)
	}
	var got []string
	for _, r := range ov.Recent {
		got = append(got, fmt.Sprintf("%s %s #%d %s %s", r.Instance, r.Project, r.Number, r.Platform, r.Outcome))
	}
	want := []string{
		"main acme/widgets #4 gitlab running",
		"main acme/widgets #3 gitlab failed",
		"main acme/widgets #2 gitlab blocking",
		"main acme/widgets #1 gitlab approved",
		"main acme/widgets #6 gitlab approved",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("recent =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := ov.Recent[3]; r.Title != "Add login" || r.WebURL != "https://gitlab.example.com/acme/widgets/-/merge_requests/1" ||
		r.Reason != "" || r.UpdatedAt.IsZero() {
		t.Errorf("recent #1 = %+v", r)
	}
	if r := ov.Recent[1]; r.Reason != "the review Job ran out of memory (OOMKilled)" {
		t.Errorf("failed reason = %q", r.Reason)
	}
	if len(ov.Attention) != 1 || ov.Attention[0].Project != "acme/widgets" || ov.Attention[0].Kind != "single_approval" ||
		ov.Attention[0].Message != "message for single_approval" {
		t.Errorf("attention = %+v; want only acme/widgets' item", ov.Attention)
	}

	// A longer period counts the older review too.
	if ov := getJSON[OverviewView](e, c, "/api/overview?days=30"); ov.Stats.Reviews != 5 || ov.Stats.Approved != 2 {
		t.Errorf("30-day stats = %+v", ov.Stats)
	}
	for _, bad := range []string{"0", "91", "x", "-1"} {
		e.wantStatus(c, "/api/overview?days="+bad, http.StatusBadRequest)
	}
}

func TestOverviewInstanceItemsOnlyForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	c := e.signInGitLab(true, "acme/widgets")
	ov := getJSON[OverviewView](e, c, "/api/overview")
	var kinds []string
	for _, a := range ov.Attention {
		kinds = append(kinds, a.Instance+"/"+a.Project+"/"+a.Kind)
	}
	if strings.Join(kinds, ",") != "main//token_expiry,main/acme/widgets/single_approval" {
		t.Errorf("admin attention = %v", kinds)
	}
}

// Setup problems are shown only for repositories switched on, even when the
// session's access list (refreshed every 5 minutes) still names one.
func TestAttentionOnlyForActiveRepos(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.UpsertDashboardRepo(ctx, store.DashboardRepo{Instance: "main", Project: "acme/tools"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAttention(ctx, "main", "acme/tools", "out_of_memory", "tools ran out of memory"); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(false, "acme/widgets", "acme/tools")
	if ov := getJSON[OverviewView](e, c, "/api/overview"); len(ov.Attention) != 1 {
		t.Fatalf("attention while switched on = %+v", ov.Attention)
	}
	if err := e.st.DeleteDashboardRepo(ctx, "main", "acme/tools"); err != nil {
		t.Fatal(err)
	}
	if ov := getJSON[OverviewView](e, c, "/api/overview"); len(ov.Attention) != 0 {
		t.Errorf("attention after switching off = %+v", ov.Attention)
	}
	e.wantStatus(c, "/api/repositories/main/acme/tools", http.StatusNotFound)
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); len(repos) != 1 || repos[0].Project != "acme/widgets" {
		t.Errorf("repositories = %+v", repos)
	}
}

func TestReviewsList(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	c := e.signInGitLab(false, "acme/widgets")

	numbers := func(v ReviewListView) string {
		var out []string
		for _, r := range v.Items {
			out = append(out, fmt.Sprint(r.Number))
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ query, want string }{
		{"", "4,3,2,1,6"},
		{"?outcome=blocking", "2"},
		{"?outcome=approved", "1,6"},
		{"?outcome=approved&days=7", "1"},
		{"?project=ACME/widgets", "4,3,2,1,6"},
		{"?project=acme/payments", ""},
		{"?project=acme/nothing", ""},
		{"?outcome=draft", ""},
	} {
		v := getJSON[ReviewListView](e, c, "/api/reviews"+tc.query)
		if got := numbers(v); got != tc.want || v.NextCursor != "" {
			t.Errorf("reviews%s = %s (cursor %q); want %s", tc.query, got, v.NextCursor, tc.want)
		}
	}
	for _, bad := range []string{"?outcome=bogus", "?days=0", "?days=366", "?cursor=@@@", "?cursor=" + url.QueryEscape("eyJ4IjoxfQ")} {
		e.wantStatus(c, "/api/reviews"+bad, http.StatusBadRequest)
	}
}

func TestReviewsListPages(t *testing.T) {
	e := newEnv(t, nil)
	now := e.clock.now()
	for i := 1; i <= 55; i++ {
		f := runFix{project: "acme/widgets", number: int64(i), head: head('a'), at: now.Add(-time.Duration(i) * time.Minute)}
		if i%2 == 0 {
			f.result = blockingResult(head('a'))
		} else {
			f.result = cleanResult(head('a'))
		}
		e.addRun(f)
	}
	c := e.signInGitLab(false, "acme/widgets")

	first := getJSON[ReviewListView](e, c, "/api/reviews")
	if len(first.Items) != 50 || first.NextCursor == "" || first.Items[0].Number != 1 || first.Items[49].Number != 50 {
		t.Fatalf("first page: %d items, cursor %q", len(first.Items), first.NextCursor)
	}
	second := getJSON[ReviewListView](e, c, "/api/reviews?cursor="+url.QueryEscape(first.NextCursor))
	if len(second.Items) != 5 || second.NextCursor != "" || second.Items[0].Number != 51 {
		t.Fatalf("second page: %+v", second)
	}

	// Filtered pages hold 50 matches each.
	blocking := getJSON[ReviewListView](e, c, "/api/reviews?outcome=blocking")
	if len(blocking.Items) != 27 || blocking.NextCursor != "" {
		t.Errorf("blocking: %d items, cursor %q", len(blocking.Items), blocking.NextCursor)
	}
	for _, r := range blocking.Items {
		if r.Outcome != OutcomeBlocking {
			t.Errorf("blocking filter returned %+v", r)
		}
	}
}

func TestReviewDetail(t *testing.T) {
	e := newEnv(t, nil)
	now := e.clock.now()
	first := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: blockingResult(head('a')),
		at: now.Add(-3 * time.Hour), title: "Old title"})
	push := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('b'), result: cleanResult(head('b')),
		at: now.Add(-2 * time.Hour)})
	retarget := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('b'), target: "develop",
		result: cleanResult(head('b')), approved: true, at: now.Add(-time.Hour), duration: 90 * time.Second,
		title: "Add login", webURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/3",
		sourceBranch: "feature/login", user: "bob"})
	c := e.signInGitLab(false, "acme/widgets")

	v := getJSON[ReviewDetailView](e, c, "/api/reviews/main/Acme/Widgets/3")
	wantHeader := ReviewHeader{Instance: "main", Project: "acme/widgets", Platform: "gitlab", Number: 3,
		Title: "Add login", SourceBranch: "feature/login", TargetBranch: "develop", Author: "bob",
		WebURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/3"}
	if v.Header != wantHeader {
		t.Errorf("header = %+v\nwant %+v", v.Header, wantHeader)
	}
	var got []string
	for _, r := range v.Runs {
		got = append(got, r.ID+" "+r.Trigger+" "+r.Outcome)
	}
	want := []string{retarget + " retarget approved", push + " push no_blocking", first + " first blocking"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("runs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := v.Runs[0]; r.Head != head('b') || r.DurationSeconds == nil || *r.DurationSeconds != 90 ||
		!r.CreatedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("newest run = %+v", r)
	}
	if r := v.Runs[1]; !strings.Contains(r.Reason, "newer review") {
		t.Errorf("replaced run reason = %q", r.Reason)
	}

	for _, target := range []string{
		"/api/reviews/main/acme/widgets/4",   // no such MR
		"/api/reviews/main/acme/widgets/x",   // not a number
		"/api/reviews/main/acme/widgets",     // no number
		"/api/reviews/gh/acme/widgets/3",     // another instance
		"/api/reviews/main/acme/payments/3",  // can't see it
		"/api/reviews/other/acme/widgets/3",  // no such instance
		"/api/reviews/main/acme/widgets/3/x", // not a project
	} {
		e.wantStatus(c, target, http.StatusNotFound)
	}
}

func TestReviewOutcomes(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		off := false
		cfg.Repositories[0].Approve = &off // acme/widgets on main: approval off
		cfg.GitHub[0].ApproveTokenEnv = "PERGENT_TEST_APPROVE"
	})
	now := e.clock.now()
	add := func(f runFix) {
		f.at = now.Add(-time.Duration(f.number) * time.Minute)
		e.addRun(f)
	}
	clean := func(h string) string { return cleanResult(h) }
	errorResult := `{"schema_version":1,"status":"error","base":"` + baseSHA + `","head":"` + head('e') +
		`","error":{"code":"commit_missing","message":"commit is not in the checkout"},"comment":{"markdown":"x"}}`
	failedSkill := resultJSON(head('f'), skillJSON("code-review", "failed"))
	skipped := resultJSON(head('s'), map[string]any{"name": "code-review", "verdict": "no_blocking_issues",
		"files": map[string]any{"reviewed": []string{}, "skipped": []any{map[string]any{"path": "go.sum", "rule": "lockfile"}}}})
	// acme/widgets (approval off)
	add(runFix{project: "acme/widgets", number: 1, head: head('1'), result: clean(head('1'))})
	add(runFix{project: "acme/widgets", number: 2, head: head('e'), result: errorResult})
	add(runFix{project: "acme/widgets", number: 3, head: head('f'), result: failedSkill})
	add(runFix{project: "acme/widgets", number: 4, head: head('s'), result: skipped})
	add(runFix{project: "acme/widgets", number: 5, head: head('5'), result: clean(head('x'))}) // another head's result
	add(runFix{project: "acme/widgets", number: 6, head: head('6'), state: store.Queued})
	add(runFix{project: "acme/widgets", number: 7, head: head('7'), state: store.Superseded})
	add(runFix{project: "acme/widgets", number: 8, head: head('8'), state: store.PublishFailed, result: clean(head('8'))})
	add(runFix{project: "acme/widgets", number: 9, head: head('9'), state: store.Reported, result: clean(head('9'))})
	// acme/payments (approval on)
	add(runFix{project: "acme/payments", projectID: 8, number: 11, head: head('1'), result: clean(head('1'))})
	add(runFix{project: "acme/payments", projectID: 8, number: 12, head: head('2'), result: clean(head('2')), approved: true})
	c := e.signInGitLab(false, "acme/widgets", "acme/payments")

	byNumber := map[int64]ReviewSummary{}
	for _, r := range getJSON[ReviewListView](e, c, "/api/reviews").Items {
		byNumber[r.Number] = r
	}
	for n, want := range map[int64]struct{ outcome, reason string }{
		1:  {OutcomeNoBlocking, "approval is turned off"},
		2:  {OutcomeFailed, "commit is not in the checkout"},
		3:  {OutcomeFailed, "Review failed for 1 of 1 skills"},
		4:  {OutcomeNoBlocking, "every changed file is skipped"},
		5:  {OutcomeFailed, "couldn't use"},
		6:  {OutcomeRunning, "Waiting"},
		7:  {OutcomeSuperseded, "newer push"},
		8:  {OutcomeFailed, "couldn't post"},
		9:  {OutcomeRunning, "Posting"},
		11: {OutcomeDraft, "draft"},
		12: {OutcomeApproved, ""},
	} {
		r := byNumber[n]
		if r.Outcome != want.outcome || !strings.Contains(r.Reason, want.reason) || (want.reason == "" && r.Reason != "") {
			t.Errorf("#%d = %s %q; want %s containing %q", n, r.Outcome, r.Reason, want.outcome, want.reason)
		}
	}
}

func TestRunDetail(t *testing.T) {
	e := newEnv(t, nil)
	res := map[string]any{
		"schema_version": 1, "status": "completed", "pergent_version": "0.1.0", "base": baseSHA, "head": head('a'),
		"skills": []any{
			map[string]any{"name": "code-review", "verdict": "has_blocking_issues",
				"findings": []any{
					map[string]any{"id": "a1b2c3", "fingerprint": "a1b2c3d4e5f6", "file": "api/handler.go", "line": 42,
						"severity": "blocking", "title": "Request body is used before validation",
						"explanation": "Not validated.", "suggested_fix": "Call validate(req)."},
					map[string]any{"id": "d4e5f6", "file": "api/handler.go", "severity": "suggestion",
						"title": "Error ignored", "explanation": "Dropped."},
				},
				"checks": []any{map[string]any{"batch": 1, "checks": []any{
					map[string]any{"section": "Input", "name": "Body validated", "status": "fail", "detail": "line 42"}}}},
				"files": map[string]any{"reviewed": []string{"api/handler.go", "api/handler_test.go"},
					"skipped": []any{map[string]any{"path": "go.sum", "rule": "lockfile"}}, "not_reviewed": []any{}},
				"tokens": map[string]any{"input": 52310, "output": 1840}},
			map[string]any{"name": "team-rules", "verdict": "failed", "findings": []any{},
				"files": map[string]any{"reviewed": []string{"api/handler.go"}, "skipped": []any{map[string]any{"path": "go.sum", "rule": "lockfile"}},
					"not_reviewed": []any{map[string]any{"path": "api/router.go", "cause": "Timed out"}}},
				"tokens": map[string]any{"input": 100, "output": 10}},
		},
		"comment": map[string]any{"markdown": "x"},
	}
	b, _ := json.Marshal(res)
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: string(b), duration: time.Minute})
	failed := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), failure: "the review Job ran out of memory (OOMKilled)"})
	e.cfg.LLM.Model = "claude-sonnet-4"
	c := e.signInGitLab(false, "acme/widgets")

	v := getJSON[RunDetailView](e, c, "/api/runs/"+id)
	if v.Run.ID != id || v.Run.Outcome != OutcomeBlocking || v.Run.Trigger != "first" || v.Instance != "main" ||
		v.Project != "acme/widgets" || v.Number != 3 {
		t.Errorf("run = %+v", v)
	}
	if v.Status != "failed" || v.Approved || v.ApprovalReason == "" || v.PergentVersion != "0.1.0" || v.Model != "claude-sonnet-4" {
		t.Errorf("status %q approved %v reason %q version %q model %q", v.Status, v.Approved, v.ApprovalReason, v.PergentVersion, v.Model)
	}
	if v.Tokens != (TokensView{In: 52410, Out: 1850}) {
		t.Errorf("tokens = %+v", v.Tokens)
	}
	if strings.Join(v.Files.Reviewed, ",") != "api/handler.go,api/handler_test.go" ||
		len(v.Files.Skipped) != 1 || v.Files.Skipped[0] != (SkippedFile{Path: "go.sum", Rule: "lockfile"}) ||
		len(v.Files.NotReviewed) != 1 || v.Files.NotReviewed[0] != (NotReviewedFile{Path: "api/router.go", Cause: "Timed out"}) {
		t.Errorf("files = %+v", v.Files)
	}
	if len(v.Skills) != 2 {
		t.Fatalf("skills = %+v", v.Skills)
	}
	cr := v.Skills[0]
	if cr.Name != "code-review" || cr.Verdict != "has_blocking_issues" || cr.Source != "library" || len(cr.Findings) != 2 {
		t.Errorf("code-review = %+v", cr)
	}
	wantFinding := FindingView{ID: "a1b2c3", Severity: "blocking", Title: "Request body is used before validation",
		File: "api/handler.go", Line: 42, Explanation: "Not validated.", SuggestedFix: "Call validate(req)."}
	if cr.Findings[0] != wantFinding {
		t.Errorf("finding = %+v\nwant %+v", cr.Findings[0], wantFinding)
	}
	if cr.Findings[1].Line != 0 || cr.Findings[1].SuggestedFix != "" {
		t.Errorf("finding without line = %+v", cr.Findings[1])
	}
	if len(cr.Checks) != 1 || cr.Checks[0] != (CheckView{Batch: 1, Section: "Input", Name: "Body validated", Status: "fail", Detail: "line 42"}) {
		t.Errorf("checks = %+v", cr.Checks)
	}
	if tr := v.Skills[1]; tr.Source != "repo" || tr.Verdict != "failed" || tr.Findings == nil || tr.Checks == nil {
		t.Errorf("team-rules = %+v", tr)
	}

	f := getJSON[RunDetailView](e, c, "/api/runs/"+failed)
	if f.Run.Outcome != OutcomeFailed || f.Run.Reason != "the review Job ran out of memory (OOMKilled)" ||
		f.Status != "failed" || len(f.Skills) != 0 || f.Files.Reviewed == nil {
		t.Errorf("failed run = %+v", f)
	}

	for _, target := range []string{"/api/runs/not-a-uuid", "/api/runs/00000000-0000-0000-0000-000000000000"} {
		e.wantStatus(c, target, http.StatusNotFound)
	}
}

func TestRunLog(t *testing.T) {
	e := newEnv(t, nil)
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: cleanResult(head('a'))})
	none := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), result: cleanResult(head('b'))})
	if err := e.st.PutRunLog(ctx, id, "cloning...\ntoken [REDACTED]\n"); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(false, "acme/widgets")
	if v := getJSON[LogView](e, c, "/api/runs/"+id+"/log"); v.Text != "cloning...\ntoken [REDACTED]\n" {
		t.Errorf("log = %q", v.Text)
	}
	e.wantStatus(c, "/api/runs/"+none+"/log", http.StatusNotFound)
	e.wantStatus(c, "/api/runs/bad/log", http.StatusNotFound)
}

func TestRepositories(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	if err := e.st.UpsertDashboardRepo(ctx, store.DashboardRepo{Instance: "main", Project: "acme/tools"}); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(false, "acme/widgets", "acme/tools")

	repos := getJSON[[]RepositoryView](e, c, "/api/repositories")
	want := []RepositoryView{
		{Instance: "main", Project: "acme/widgets", Platform: "gitlab", Source: "config", On: true,
			Last7: Last7View{Reviews: 4, Approved: 1, Failed: 1}, Health: "attention"},
		{Instance: "main", Project: "acme/tools", Platform: "gitlab", Source: "dashboard", On: true, Health: "ok"},
	}
	if len(repos) != len(want) {
		t.Fatalf("repositories = %+v", repos)
	}
	for i := range want {
		if repos[i] != want[i] {
			t.Errorf("repository %d = %+v\nwant %+v", i, repos[i], want[i])
		}
	}
}

func TestRepositoryDetail(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		off := false
		cfg.Repositories[0].Approve = &off
		cfg.Repositories[0].Skills = []string{"code-review"}
	})
	if err := e.st.SetAttention(ctx, "main", "acme/widgets", "single_approval", "One approval is enough."); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(false, "acme/widgets")

	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/ACME/widgets")
	if v.Repo.Project != "acme/widgets" || v.Repo.Source != "config" || v.Repo.Health != "attention" {
		t.Errorf("repo = %+v", v.Repo)
	}
	settings, _ := json.Marshal(v.Settings)
	want := `{"approve":{"value":false,"overridden":true},"failureStatus":{"value":"failed","overridden":false},` +
		`"jobMemory":{"value":"","overridden":false},"jobTimeoutSeconds":{"value":600,"overridden":false},` +
		`"reviewDrafts":{"value":true,"overridden":false},"skills":{"value":["code-review"],"overridden":true}}`
	if string(settings) != want {
		t.Errorf("settings = %s\nwant %s", settings, want)
	}
	if len(v.Attention) != 1 || v.Attention[0].Kind != "single_approval" {
		t.Errorf("attention = %+v", v.Attention)
	}
	for _, target := range []string{"/api/repositories/main/acme/payments", "/api/repositories/main/acme/none",
		"/api/repositories/gh/acme/widgets", "/api/repositories/main", "/api/reviews/main", "/api/nothing"} {
		e.wantStatus(c, target, http.StatusNotFound)
	}
}

func TestRepositoryInputs(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].Skills = []string{"code-review"} })
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: 7, Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	src := e.src["main"]
	src.set(func(f *fakeSource) {
		f.files[".pergent/skills/team-rules.md"] = "---\nname: team-rules\n---\nOur rules.\n"
		f.files[".pergent/skills/style.md"] = "---\nname: style\n---\nOur style.\n"
		f.files["AGENTS.md"] = "Be kind."
		f.files[".pergent/instructions.md"] = "Check migrations."
	})
	c := e.signInGitLab(false, "acme/widgets")

	v := getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	if v.Branch != "trunk" {
		t.Errorf("branch = %q", v.Branch)
	}
	var got []string
	for _, s := range v.Skills {
		got = append(got, s.Name+":"+s.Source+":"+strings.TrimSpace(s.Content[strings.LastIndex(s.Content, "---")+3:]))
	}
	// style isn't picked from the library, so the repository's own is a
	// repository skill.
	if want := "code-review:library:Review the code.,style:repo:Our style.,team-rules:repo:Our rules."; strings.Join(got, ",") != want {
		t.Errorf("skills = %v\nwant %s", got, want)
	}
	if !strings.Contains(v.Instructions, "Be kind.") || !strings.Contains(v.Instructions, "Check migrations.") {
		t.Errorf("instructions = %q", v.Instructions)
	}
	src.mu.Lock()
	refs, ids := src.refs, src.ids
	src.mu.Unlock()
	if len(refs) != 1 || !refs["trunk"] || len(ids) != 1 || !ids[7] {
		t.Errorf("read refs %v of projects %v; want trunk of 7", refs, ids)
	}

	// Kept 5 minutes.
	calls := src.callCount()
	getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	if src.callCount() != calls {
		t.Errorf("a second view read the repository again")
	}
	e.clock.advance(6 * time.Minute)
	e.gl.setProject("acme/widgets", gitlabProject("private", 20, 0))
	src.set(func(f *fakeSource) { f.fail = errors.New("gitlab: status 500: internal " + reviewToken) })
	rec := e.wantStatus(c, "/api/repositories/main/acme/widgets/inputs", http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "500") || strings.Contains(rec.Body.String(), reviewToken) {
		t.Errorf("502 body = %s; want a generic message", rec.Body)
	}
	if src.callCount() == calls {
		t.Errorf("after 6 minutes the repository wasn't read again")
	}
}

func TestRepositoryInputsHidden(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: 8, Path: "acme/payments"}); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(false, "acme/widgets")
	e.wantStatus(c, "/api/repositories/main/acme/payments/inputs", http.StatusNotFound)
	if n := e.src["main"].callCount(); n != 0 {
		t.Errorf("%d reads of a repository the user can't see", n)
	}
	// A repository not stored yet can't be read.
	e.wantStatus(c, "/api/repositories/main/acme/widgets/inputs", http.StatusBadGateway)
}

func TestSkills(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInGitLab(false)
	got := getJSON[[]LibrarySkill](e, c, "/api/skills")
	if len(got) != 2 || got[0].Name != "code-review" || got[1].Name != "style" || got[0].Content != string(e.lib["code-review"]) {
		t.Errorf("skills = %+v", got)
	}
}

func TestIntegrations(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: 7, Path: "acme/widgets", HookID: 3,
		DeployTokenID: 4, DeployTokenUsername: "deploy-user", DeployTokenSealed: []byte(deployToken)}); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(true, "acme/widgets")
	v := getJSON[IntegrationView](e, c, "/api/integrations")
	want := IntegrationView{Instance: "main", Platform: "gitlab", Connected: true, Approver: "token",
		Repos: []IntegrationRepo{{Project: "acme/widgets", Installed: true, ApproverReachable: true}}}
	if v.Instance != want.Instance || v.Platform != want.Platform || !v.Connected || v.Approver != want.Approver ||
		v.TokenExpiry != nil || len(v.Repos) != 1 || v.Repos[0] != want.Repos[0] {
		t.Errorf("integrations = %+v\nwant %+v", v, want)
	}
}

func TestIntegrationsGitHub(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	if err := e.st.UpsertGitHubProject(ctx, store.Project{Instance: "gh", ProjectID: 900, Path: "acme/widgets",
		InstallationID: 55, ApproveAvailable: false}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAttention(ctx, "gh", "", "token_expiry", "The approve bot's token expires on 2026-11-10."); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitHub(true, "acme/widgets")
	v := getJSON[IntegrationView](e, c, "/api/integrations")
	if v.Instance != "gh" || v.Platform != "github" || v.Approver != "token" || v.TokenExpiry == nil ||
		!v.TokenExpiry.Equal(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("integrations = %+v", v)
	}
	if len(v.Repos) != 1 || v.Repos[0] != (IntegrationRepo{Project: "acme/widgets", Installed: true, ApproverReachable: false}) {
		t.Errorf("repos = %+v", v.Repos)
	}
}

func TestAdminSettings(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	c := e.signInGitLab(true, "acme/widgets")
	v := getJSON[AdminSettingsView](e, c, "/api/admin/settings")
	want := AdminSettingsView{Instance: "main",
		Defaults: DefaultsView{Approve: true, ReviewDrafts: true, FailureStatus: "failed", JobTimeoutSeconds: 600},
		LLM:      LLMView{Provider: "anthropic", Model: "claude-sonnet-4"},
		Admins:   []AdminView{{Instance: "main", Group: "acme/platform"}}}
	got, _ := json.Marshal(v)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Errorf("admin settings = %s\nwant %s", got, wantJSON)
	}
}

func TestAdminEndpointsForbiddenToDevelopers(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	c := e.signInGitLab(false, "acme/widgets")
	for _, target := range []string{"/api/integrations", "/api/admin/settings"} {
		rec := e.wantStatus(c, target, http.StatusForbidden)
		if strings.Contains(rec.Body.String(), "anthropic") || strings.Contains(rec.Body.String(), "acme/widgets") {
			t.Errorf("403 body of %s = %s", target, rec.Body)
		}
	}
}

func TestReadAPIRequiresSignIn(t *testing.T) {
	e := newEnv(t, nil)
	for _, target := range readEndpoints("00000000-0000-0000-0000-000000000000") {
		e.wantStatus(nil, target, http.StatusUnauthorized)
	}
}

// readEndpoints lists every read endpoint, about acme/widgets on main and
// the run id.
func readEndpoints(id string) []string {
	return []string{
		"/api/overview", "/api/reviews", "/api/reviews?project=acme/widgets",
		"/api/reviews/main/acme/widgets/1", "/api/runs/" + id, "/api/runs/" + id + "/log",
		"/api/repositories", "/api/repositories/main/acme/widgets", "/api/repositories/main/acme/widgets/inputs",
		"/api/skills", "/api/integrations", "/api/admin/settings", "/api/me",
	}
}

// A repository the user can't see never shows in a list, and its own URLs
// are 404, for a developer and for an admin.
func TestReadAPIHidesInvisibleRepos(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint("admin=", admin), func(t *testing.T) {
			e := newEnv(t, nil)
			overviewFixture(e)
			hidden := e.addRun(runFix{project: "acme/payments", projectID: 8, number: 9, head: head('9'),
				result: blockingResult(head('9')), title: "Payments secret change"})
			if err := e.st.PutRunLog(ctx, hidden, "payments log"); err != nil {
				t.Fatal(err)
			}
			for _, p := range []store.Project{{Instance: "main", ProjectID: 7, Path: "acme/widgets", HookID: 1},
				{Instance: "main", ProjectID: 8, Path: "acme/payments", HookID: 2}} {
				if err := e.st.UpsertProject(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			c := e.signInGitLab(admin, "acme/widgets")

			for _, target := range []string{"/api/overview", "/api/overview?days=90", "/api/reviews",
				"/api/reviews?project=acme/payments", "/api/repositories", "/api/integrations"} {
				rec := e.get(c, target)
				if target == "/api/integrations" && !admin {
					continue
				}
				if rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d", target, rec.Code)
				}
				if body := strings.ToLower(rec.Body.String()); strings.Contains(body, "payments") || strings.Contains(body, hidden) {
					t.Errorf("GET %s shows acme/payments: %s", target, rec.Body)
				}
			}
			for _, target := range []string{
				"/api/reviews/main/acme/payments/9", "/api/reviews/main/acme/payments/5",
				"/api/runs/" + hidden, "/api/runs/" + hidden + "/log",
				"/api/repositories/main/acme/payments", "/api/repositories/main/acme/payments/inputs",
				// Another instance's repository with the same path.
				"/api/reviews/gh/acme/widgets/7", "/api/repositories/gh/acme/widgets",
			} {
				rec := e.wantStatus(c, target, http.StatusNotFound)
				if strings.Contains(strings.ToLower(rec.Body.String()), "payments") {
					t.Errorf("404 body of %s = %s", target, rec.Body)
				}
			}
			if n := e.src["main"].callCount(); n != 0 {
				t.Errorf("%d repository reads for hidden repositories", n)
			}
		})
	}
}

// No response of the read API holds a token, key or secret: not the
// integrations' nor any other.
func TestNoSecretInIntegrations(t *testing.T) {
	for _, platform := range []string{"gitlab", "github"} {
		t.Run(platform, func(t *testing.T) {
			e := newEnv(t, withSecrets(t))
			overviewFixture(e)
			id := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('1'), result: cleanResult(head('1'))})
			if err := e.st.PutRunLog(ctx, id, "token [REDACTED]"); err != nil {
				t.Fatal(err)
			}
			if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: 7, Path: "acme/widgets", HookID: 3,
				DeployTokenID: 4, DeployTokenUsername: "deploy-user", DeployTokenSealed: []byte(deployToken)}); err != nil {
				t.Fatal(err)
			}
			if err := e.st.UpsertGitHubProject(ctx, store.Project{Instance: "gh", ProjectID: 900, Path: "acme/widgets",
				InstallationID: 55, ApproveAvailable: true}); err != nil {
				t.Fatal(err)
			}
			var c *http.Cookie
			if platform == "gitlab" {
				c = e.signInGitLab(true, "acme/widgets")
			} else {
				c = e.signInGitHub(true, "acme/widgets")
			}
			secrets := append([]string{llmKey, reviewToken, approveToken, webhookSecret, deployToken, ghAppKey,
				glClientSecret, ghClientSecret, e.key, c.Value, "deploy-user", "llm.example.com"},
				append(e.gl.tokens(), e.gh.tokens()...)...)

			targets := readEndpoints(id)
			targets = append(targets, "/api/reviews/gh/acme/widgets/7", "/api/repositories/gh/acme/widgets",
				"/api/repositories/gh/acme/widgets/inputs")
			for _, target := range targets {
				rec := e.get(c, target)
				if rec.Code >= 500 && rec.Code != http.StatusBadGateway {
					t.Errorf("GET %s = %d %s", target, rec.Code, rec.Body)
				}
				for _, s := range secrets {
					if s != "" && strings.Contains(rec.Body.String(), s) {
						t.Errorf("GET %s shows the secret %q: %s", target, s, rec.Body)
					}
				}
				if strings.Contains(rec.Body.String(), "SECRET") {
					t.Errorf("GET %s shows a secret: %s", target, rec.Body)
				}
			}
			if v := getJSON[IntegrationView](e, c, "/api/integrations"); len(v.Repos) != 1 {
				t.Errorf("integrations = %+v", v)
			}
		})
	}
}
