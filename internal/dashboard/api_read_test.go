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
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
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
	// gate, when not nil, holds DefaultBranch until it is closed;
	// entered is told each time one starts waiting.
	gate    chan struct{}
	entered chan struct{}
	// change is what GetChange answers (changeErr: an error instead);
	// changeCalls counts the calls.
	change      forge.Change
	changeErr   error
	changeCalls int
	// users answers UserByName by lower-cased name (userErr: an error).
	users   map[string]forge.User
	userErr error
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

// GetChange answers with the change a test set (its Number is the one asked
// for), or its error. It is not counted in callCount.
func (f *fakeSource) GetChange(_ context.Context, _, number int64) (forge.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changeCalls++
	if f.changeErr != nil {
		return forge.Change{}, f.changeErr
	}
	c := f.change
	c.Number = number
	return c, nil
}

func (f *fakeSource) DefaultBranch(_ context.Context, projectID int64) (string, error) {
	f.mu.Lock()
	gate, entered := f.gate, f.entered
	f.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
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

// userOn is alice's session as a viewer of the given repositories of
// instance (a team of exactly those), or, with admin, as an admin, who sees
// every repository of every instance.
func (e *env) userOn(instance string, admin bool, visible ...string) *http.Cookie {
	e.t.Helper()
	if admin {
		e.ssoAdmin(aliceEmail)
		return e.aliceSession()
	}
	repos := make([]store.TeamRepo, len(visible))
	for i, p := range visible {
		repos[i] = store.TeamRepo{Instance: instance, Project: p}
	}
	e.team("viewers of "+instance, []store.TeamMember{{AccountID: e.alice(), Role: store.RoleViewer}}, repos...)
	return e.aliceSession()
}

// userOnMain is alice's session on main: see userOn.
func (e *env) userOnMain(admin bool, visible ...string) *http.Cookie {
	e.t.Helper()
	return e.userOn("main", admin, visible...)
}

// userOnGH is alice's session on gh: see userOn.
func (e *env) userOnGH(admin bool, visible ...string) *http.Cookie {
	e.t.Helper()
	return e.userOn("gh", admin, visible...)
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
// target main, published, created an hour before the clock, a 2-minute Job,
// the project ID tests give its path (testID).
// outcome, reason, status and approved are the publisher's recorded
// decision (status defaults from the outcome); model and sources (JSON)
// what the dispatcher recorded.
type runFix struct {
	instance, project       string
	projectID, number       int64
	head, target            string
	state                   store.RunState
	result, failure         string
	title, webURL           string
	at                      time.Time
	duration                time.Duration
	outcome, reason, status string
	approved                bool
	undispatched            bool
	sourceBranch, user      string
	model, sources          string
	provider                string
	maxTurns                *int
	// versions (JSON) and skipReason are what a run records of the library
	// skills it used and of why it was skipped.
	versions, skipReason string
	// incremental marks a run that reviewed only some files.
	incremental bool
	// quiet marks a run started quiet.
	quiet bool
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
		f.projectID = testID(f.project)
	}
	// The project reconcile would have stored (a session's repositories get
	// their project ID from here), unless a test stored it already.
	e.exec(`INSERT INTO projects (instance, project_id, path) VALUES ($1, $2, $3) ON CONFLICT (instance, project_id) DO NOTHING`,
		f.instance, f.projectID, f.project)
	mr := store.MRKey{Instance: f.instance, ProjectID: f.projectID, IID: f.number}
	id, _, err := e.st.EnqueueRun(ctx, mr, f.project, f.head, f.target)
	if err != nil {
		e.t.Fatalf("EnqueueRun: %v", err)
	}
	if f.quiet {
		// Only a queued or running run takes it, so before the state is set.
		if err := e.st.SetRunQuiet(ctx, id, true); err != nil {
			e.t.Fatalf("SetRunQuiet: %v", err)
		}
	}
	var dispatched, reported *time.Time
	if f.state != store.Queued && !f.undispatched {
		d := f.at.Add(10 * time.Second)
		dispatched = &d
	}
	switch f.state {
	case store.Reported, store.Published, store.PublishFailed, store.Skipped:
		r := f.at.Add(10*time.Second + f.duration)
		reported = &r
	}
	var result, model, sources, versions, provider *string
	if f.result != "" {
		result = &f.result
	}
	if f.model != "" {
		model = &f.model
	}
	if f.provider != "" {
		provider = &f.provider
	}
	if f.sources != "" {
		sources = &f.sources
	}
	if f.versions != "" {
		versions = &f.versions
	}
	if f.outcome != "" && f.status == "" {
		f.status = "success"
		if f.outcome == OutcomeBlocking || f.outcome == OutcomeFailed {
			f.status = "failed"
		}
	}
	if f.reason == "" {
		switch f.outcome {
		case OutcomeBlocking:
			f.reason = "Blocking issues found"
		case OutcomeFailed:
			f.reason = f.failure
		}
	}
	e.exec(`UPDATE runs SET state = $2, result = $3::jsonb, failure_reason = $4, base_sha = $5, created_at = $6,
			dispatched_at = $7, reported_at = $8, title = $9, web_url = $10, source_branch = $11, author = $12,
			outcome = $13, outcome_reason = $14, decision_status = $15, approved = $16, model = $17,
			skill_sources = $18::jsonb, skill_versions = $19::jsonb, skip_reason = $20, max_turns = $21, incremental = $22, provider = $23
		WHERE id = $1::uuid`,
		id, string(f.state), result, f.failure, baseSHA, f.at, dispatched, reported, f.title, f.webURL,
		f.sourceBranch, f.user, f.outcome, f.reason, f.status, f.approved, model, sources, versions, f.skipReason, f.maxTurns, f.incremental, provider)
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
	e.addRun(runFix{project: "acme/widgets", number: 1, head: head('1'), result: cleanResult(head('1')),
		outcome: OutcomeApproved, approved: true, title: "Add login", webURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/1", at: now.Add(-6 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 2, head: head('2'), result: blockingResult(head('2')),
		outcome: OutcomeBlocking, title: "Refactor", at: now.Add(-5 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 3, head: head('3'), failure: "the review Job ran out of memory (OOMKilled)",
		outcome: OutcomeFailed, at: now.Add(-4 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 4, head: head('4'), state: store.Running, at: now.Add(-3 * time.Hour)})
	e.addRun(runFix{project: "acme/widgets", number: 6, head: head('6'), result: cleanResult(head('6')),
		outcome: OutcomeApproved, approved: true, at: now.Add(-10 * 24 * time.Hour)})
	e.addRun(runFix{project: "acme/payments", projectID: 8, number: 5, head: head('5'), result: blockingResult(head('5')),
		outcome: OutcomeBlocking, title: "Payments secret change", at: now.Add(-2 * time.Hour)})
	e.addRun(runFix{instance: "gh", project: "acme/widgets", projectID: 900, number: 7, head: head('7'),
		result: blockingResult(head('7')), outcome: OutcomeBlocking, title: "GitHub change", at: now.Add(-time.Hour)})

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
	c := e.userOnMain(false, "acme/widgets")

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
	c := e.userOnMain(true, "acme/widgets")
	ov := getJSON[OverviewView](e, c, "/api/overview")
	var kinds []string
	for _, a := range ov.Attention {
		kinds = append(kinds, a.Instance+"/"+a.Project+"/"+a.Kind)
	}
	// An admin sees the setup problems of every instance.
	want := "gh/acme/widgets/approver_unreachable,main//token_expiry,main/acme/payments/out_of_memory,main/acme/widgets/single_approval"
	if strings.Join(kinds, ",") != want {
		t.Errorf("admin attention = %v", kinds)
	}
}

// Setup problems are shown only for repositories switched on, even when a
// team still names one.
func TestAttentionOnlyForActiveRepos(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.UpsertDashboardRepo(ctx, store.DashboardRepo{Instance: "main", Project: "acme/tools"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetAttention(ctx, "main", "acme/tools", "out_of_memory", "tools ran out of memory"); err != nil {
		t.Fatal(err)
	}
	c := e.userOnMain(false, "acme/widgets", "acme/tools")
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
	// Older than the default 90 days.
	e.addRun(runFix{project: "acme/widgets", number: 8, head: head('8'), outcome: OutcomeApproved, approved: true,
		at: e.clock.now().Add(-100 * 24 * time.Hour)})
	c := e.userOnMain(false, "acme/widgets")

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
		{"?days=365", "4,3,2,1,6,8"},
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
	for i := 1; i <= 110; i++ {
		f := runFix{project: "acme/widgets", number: int64(i), head: head('a'), at: now.Add(-time.Duration(i) * time.Minute),
			result: cleanResult(head('a')), outcome: OutcomeApproved, approved: true}
		if i%2 == 0 {
			f.result, f.outcome, f.approved = blockingResult(head('a')), OutcomeBlocking, false
		}
		e.addRun(f)
	}
	c := e.userOnMain(false, "acme/widgets")

	var all []int64
	cursor := ""
	for page := 0; ; page++ {
		v := getJSON[ReviewListView](e, c, "/api/reviews?cursor="+url.QueryEscape(cursor))
		if want := min(50, 110-50*page); len(v.Items) != want {
			t.Fatalf("page %d: %d items, want %d", page, len(v.Items), want)
		}
		for _, r := range v.Items {
			all = append(all, r.Number)
		}
		if cursor = v.NextCursor; cursor == "" {
			break
		}
	}
	for i, n := range all {
		if n != int64(i+1) {
			t.Fatalf("paged numbers = %v; want 1 to 110 in order", all)
		}
	}

	// The outcome filter pages the same way: 50 matches, then the rest.
	first := getJSON[ReviewListView](e, c, "/api/reviews?outcome=blocking")
	if len(first.Items) != 50 || first.NextCursor == "" || first.Items[0].Number != 2 || first.Items[49].Number != 100 {
		t.Fatalf("blocking page 1: %d items, cursor %q", len(first.Items), first.NextCursor)
	}
	second := getJSON[ReviewListView](e, c, "/api/reviews?outcome=blocking&cursor="+url.QueryEscape(first.NextCursor))
	if len(second.Items) != 5 || second.NextCursor != "" || second.Items[0].Number != 102 {
		t.Fatalf("blocking page 2: %+v", second)
	}
	for _, r := range append(first.Items, second.Items...) {
		if r.Outcome != OutcomeBlocking {
			t.Errorf("blocking filter returned %+v", r)
		}
	}
}

func TestReviewDetail(t *testing.T) {
	e := newEnv(t, nil)
	now := e.clock.now()
	first := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: blockingResult(head('a')),
		outcome: OutcomeBlocking, at: now.Add(-3 * time.Hour), title: "Old title"})
	push := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('b'), result: cleanResult(head('b')),
		outcome: OutcomeNoBlocking, reason: "No blocking issues; not approved: the change was closed, or its head or target branch moved, before it could be approved",
		at: now.Add(-2 * time.Hour)})
	retarget := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('b'), target: "develop",
		result: cleanResult(head('b')), outcome: OutcomeApproved, approved: true, at: now.Add(-time.Hour), duration: 90 * time.Second,
		title: "Add login", webURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/3",
		sourceBranch: "feature/login", user: "bob"})
	c := e.userOnMain(false, "acme/widgets")

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
	if r := v.Runs[1]; !strings.Contains(r.Reason, "target branch moved") {
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

// Outcomes are the ones the publisher recorded: settings changed since, an
// approval withdrawn since (the change merged) change nothing. Runs not
// published show their state; nothing is guessed.
func TestReviewOutcomes(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		off := false
		cfg.Repositories[0].Approve = &off // turned off after the reviews below
	})
	now := e.clock.now()
	add := func(f runFix) {
		f.project = "acme/widgets"
		f.head = head(byte('0' + f.number%10))
		f.at = now.Add(-time.Duration(f.number) * time.Minute)
		e.addRun(f)
	}
	// Approved, then merged: the sweep cleared the recorded approval.
	add(runFix{number: 1, outcome: OutcomeApproved, approved: true})
	add(runFix{number: 2, outcome: OutcomeNoBlocking,
		reason: "No blocking issues; not approved: the change was closed, or its head or target branch moved, before it could be approved"})
	add(runFix{number: 3, outcome: OutcomeDraft, reason: "No blocking issues; not approved: draft"})
	add(runFix{number: 4}) // published before outcomes were recorded
	add(runFix{number: 5, state: store.Queued})
	add(runFix{number: 6, state: store.Running})
	add(runFix{number: 7, state: store.Superseded})
	add(runFix{number: 8, state: store.PublishFailed})
	add(runFix{number: 9, state: store.Reported})
	add(runFix{number: 10, state: store.Reported, failure: "the review Job ran out of memory (OOMKilled)"})
	add(runFix{number: 11, state: store.Superseded, reason: store.SupersededDraft})
	// Published, then superseded by a review of the same head again.
	add(runFix{number: 12, state: store.Superseded, reason: "Blocking issues found"})
	c := e.userOnMain(false, "acme/widgets")

	byNumber := map[int64]ReviewSummary{}
	for _, r := range getJSON[ReviewListView](e, c, "/api/reviews").Items {
		byNumber[r.Number] = r
	}
	for n, want := range map[int64]struct{ outcome, reason string }{
		1:  {OutcomeApproved, ""},
		2:  {OutcomeNoBlocking, "closed"},
		3:  {OutcomeDraft, "draft"},
		4:  {OutcomeUnknown, "before pergent.sh recorded"},
		5:  {OutcomeRunning, "Waiting"},
		6:  {OutcomeRunning, "Reviewing"},
		7:  {OutcomeSuperseded, "newer push"},
		8:  {OutcomeFailed, "couldn't post"},
		9:  {OutcomeRunning, "Posting"},
		10: {OutcomeFailed, "OOMKilled"},
		11: {OutcomeSuperseded, "Not reviewed: the change is a draft and drafts are not reviewed here."},
		12: {OutcomeSuperseded, "newer push"},
	} {
		r := byNumber[n]
		if r.Outcome != want.outcome || !strings.Contains(r.Reason, want.reason) || (want.reason == "" && r.Reason != "") {
			t.Errorf("#%d = %s %q; want %s containing %q", n, r.Outcome, r.Reason, want.outcome, want.reason)
		}
	}
	if v := getJSON[ReviewListView](e, c, "/api/reviews?outcome=unknown"); len(v.Items) != 1 || v.Items[0].Number != 4 {
		t.Errorf("unknown filter = %+v", v.Items)
	}
	// The approved-then-merged review counts as approved.
	if ov := getJSON[OverviewView](e, c, "/api/overview"); ov.Stats.Approved != 1 || ov.Stats.Failed != 2 || ov.Stats.Reviews != 9 {
		t.Errorf("stats = %+v; want 9 reviews, 1 approved, 2 failed", ov.Stats)
	}
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); repos[0].Last7 != (Last7View{Reviews: 9, Approved: 1, Failed: 2}) {
		t.Errorf("last 7 days = %+v", repos[0].Last7)
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
	// code-review is a library skill, but this repository replaced it
	// with its own when the review ran.
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: string(b), duration: time.Minute,
		outcome: OutcomeBlocking, provider: "anthropic", model: "claude-sonnet-4", maxTurns: ptr(40), sources: `{"code-review":"repo","team-rules":"repo"}`})
	failed := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), outcome: OutcomeFailed,
		failure: "the review Job ran out of memory (OOMKilled)"})
	// A run that recorded no model or skill sources.
	older := e.addRun(runFix{project: "acme/widgets", number: 5, head: head('c'), result: cleanResult(head('c')),
		outcome: OutcomeApproved, approved: true})
	e.cfg.LLM.Model = "a-newer-model" // changed since: the runs keep theirs
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[RunDetailView](e, c, "/api/runs/"+id)
	if v.Run.ID != id || v.Run.Outcome != OutcomeBlocking || v.Run.Trigger != "first" || v.Instance != "main" ||
		v.Project != "acme/widgets" || v.Number != 3 {
		t.Errorf("run = %+v", v)
	}
	if v.Status != "failed" || v.Approved || v.ApprovalReason == "" || v.PergentVersion != "0.1.0" ||
		v.Model == nil || *v.Model != "claude-sonnet-4" || v.Provider == nil || *v.Provider != "anthropic" {
		t.Errorf("status %q approved %v reason %q version %q provider %v model %v", v.Status, v.Approved, v.ApprovalReason, v.PergentVersion, v.Provider, v.Model)
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
	if cr.Name != "code-review" || cr.Verdict != "has_blocking_issues" || cr.Source == nil || *cr.Source != "repo" || len(cr.Findings) != 2 {
		t.Errorf("code-review = %+v", cr)
	}
	wantFinding := FindingView{ID: "a1b2c3", Fingerprint: "a1b2c3d4e5f6", Severity: "blocking", Title: "Request body is used before validation",
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
	if tr := v.Skills[1]; tr.Source == nil || *tr.Source != "repo" || tr.Verdict != "failed" || tr.Findings == nil || tr.Checks == nil {
		t.Errorf("team-rules = %+v", tr)
	}

	f := getJSON[RunDetailView](e, c, "/api/runs/"+failed)
	if f.Run.Outcome != OutcomeFailed || f.Run.Reason != "the review Job ran out of memory (OOMKilled)" ||
		f.Status != "failed" || len(f.Skills) != 0 || f.Files.Reviewed == nil || f.Model != nil || f.Provider != nil {
		t.Errorf("failed run = %+v", f)
	}

	o := getJSON[RunDetailView](e, c, "/api/runs/"+older)
	if o.Model != nil || o.Provider != nil || len(o.Skills) != 1 || o.Skills[0].Source != nil || !o.Approved || o.ApprovalReason != "" || o.Status != "success" {
		t.Errorf("run without recorded inputs = %+v", o)
	}
	raw := e.get(c, "/api/runs/"+older).Body.String()
	if !strings.Contains(raw, `"model":null`) || !strings.Contains(raw, `"provider":null`) || !strings.Contains(raw, `"source":null`) || !strings.Contains(raw, `"maxTurns":null`) {
		t.Errorf("body = %s; want null model, maxTurns and source", raw)
	}
	// The step limit is the one the run was dispatched with.
	if v.MaxTurns == nil || *v.MaxTurns != 40 || f.MaxTurns != nil || o.MaxTurns != nil {
		t.Errorf("maxTurns = %v, %v, %v; want 40, null, null", v.MaxTurns, f.MaxTurns, o.MaxTurns)
	}
	if body := e.get(c, "/api/runs/"+id).Body.String(); !strings.Contains(body, `"maxTurns":40`) {
		t.Errorf("body = %s; want maxTurns 40", body)
	}

	for _, target := range []string{"/api/runs/not-a-uuid", "/api/runs/00000000-0000-0000-0000-000000000000"} {
		e.wantStatus(c, target, http.StatusNotFound)
	}
}

// A re-run of failed skills stores the skills it kept with its own, so the
// result counts the kept skills' tokens again. The page shows what the run
// used itself, from the usage table, and the sum of the result only until
// that is filled in.
func TestRunDetailTokensComeFromUsage(t *testing.T) {
	e := newEnv(t, nil)
	kept := skillJSON("kept", "no_blocking_issues")
	own := skillJSON("own", "no_blocking_issues")
	own["tokens"] = map[string]any{"input": 5, "output": 1}
	f := runFix{project: "acme/widgets", number: 3, head: head('a'), result: resultJSON(head('a'), kept, own), outcome: OutcomeApproved, approved: true}
	c := e.userOnMain(false, "acme/widgets")

	id := e.addRun(f)
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+id); v.Tokens != (TokensView{In: 105, Out: 11}) {
		t.Errorf("tokens before usage is recorded = %+v; want the sum of the result", v.Tokens)
	}
	if err := e.st.RecordUsage(ctx, id, []store.SkillUsage{usageSkill("own", "no_blocking_issues", 5, 1)}); err != nil {
		t.Fatal(err)
	}
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+id); v.Tokens != (TokensView{In: 5, Out: 1}) {
		t.Errorf("tokens = %+v; want the run's own, 5 and 1", v.Tokens)
	}
}

func TestRunLog(t *testing.T) {
	e := newEnv(t, nil)
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: cleanResult(head('a'))})
	none := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), result: cleanResult(head('b'))})
	if err := e.st.PutRunLog(ctx, id, "cloning...\ntoken [REDACTED]\n"); err != nil {
		t.Fatal(err)
	}
	c := e.userOnMain(false, "acme/widgets")
	if v := getJSON[LogView](e, c, "/api/runs/"+id+"/log"); v.Text != "cloning...\ntoken [REDACTED]\n" {
		t.Errorf("log = %q", v.Text)
	}
	e.wantStatus(c, "/api/runs/"+none+"/log", http.StatusNotFound)
	e.wantStatus(c, "/api/runs/bad/log", http.StatusNotFound)
}

// rawKeys decodes the body of GET target into its top-level keys.
func rawKeys(e *env, c *http.Cookie, target string) map[string]json.RawMessage {
	e.t.Helper()
	rec := e.get(c, target)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		e.t.Fatalf("GET %s body %s: %v", target, rec.Body, err)
	}
	return m
}

// setJob records the Job fields the engine would have set on the run.
func (e *env) setJob(id, job, pod, detail string) {
	e.t.Helper()
	e.exec(`UPDATE runs SET job_name = $2, pod_name = $3, failure_step = 'starting', failure_detail = $4
		WHERE id = $1::uuid`, id, job, pod, detail)
}

// belowAdmin are sessions on acme/widgets with every role below RoleAdmin: a
// viewer, a maintainer of the repository, and a maintainer of every
// repository of the instance. A team role never reaches RoleAdmin (roleOf),
// so only an admin account does.
func belowAdmin(e *env) map[string]*http.Cookie {
	e.t.Helper()
	repo := e.sso("repo-maintainer@example.com")
	e.team("widgets maintainers", []store.TeamMember{{AccountID: repo, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	inst := e.sso("instance-maintainer@example.com")
	e.team("main maintainers", []store.TeamMember{{AccountID: inst, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main"})
	below := map[string]*http.Cookie{
		"viewer":                e.userOnMain(false, "acme/widgets"),
		"repository maintainer": e.sessionFor(repo),
		"instance maintainer":   e.sessionFor(inst),
	}
	for who, want := range map[string]Role{"viewer": RoleViewer, "repository maintainer": RoleMaintainer, "instance maintainer": RoleMaintainer} {
		if _, se := e.session(below[who]); se.Role("main", "acme/widgets") != want {
			e.t.Fatalf("%s has role %v on acme/widgets; want %v", who, se.Role("main", "acme/widgets"), want)
		}
	}
	return below
}

func TestRunKubernetesAdminOnly(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Jobs.Namespace = "pergent-jobs" })
	failed := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), outcome: OutcomeFailed,
		failure: "The review Job's pod could not be scheduled."})
	e.setJob(failed, "pergent-abc123", "pergent-abc123-xyz", "0/3 nodes are available: insufficient memory.")
	// A running run shows its Job as soon as it has one, with no pod yet.
	running := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), state: store.Running})
	e.exec(`UPDATE runs SET job_name = 'pergent-def456' WHERE id = $1::uuid`, running)
	// No Job yet: no block, for an admin too.
	queued := e.addRun(runFix{project: "acme/widgets", number: 5, head: head('c'), state: store.Queued})

	admin := e.sessionFor(e.ssoAdmin("root@example.com"))
	v := getJSON[RunDetailView](e, admin, "/api/runs/"+failed)
	want := &KubernetesView{Namespace: "pergent-jobs", Job: "pergent-abc123", Pod: "pergent-abc123-xyz",
		Detail: "0/3 nodes are available: insufficient memory."}
	if v.Kubernetes == nil || *v.Kubernetes != *want {
		t.Errorf("admin kubernetes = %+v; want %+v", v.Kubernetes, want)
	}
	v = getJSON[RunDetailView](e, admin, "/api/runs/"+running)
	if v.Kubernetes == nil || *v.Kubernetes != (KubernetesView{Namespace: "pergent-jobs", Job: "pergent-def456"}) {
		t.Errorf("running admin kubernetes = %+v; want the Job alone", v.Kubernetes)
	}
	if _, ok := rawKeys(e, admin, "/api/runs/"+queued)["kubernetes"]; ok {
		t.Error("a run with no Job has a kubernetes key")
	}

	// Below admin (a viewer, a maintainer of the repository, a maintainer of
	// the whole instance) the JSON has no kubernetes key, and nothing of the
	// Job or the detail anywhere in the body.
	below := belowAdmin(e)
	for who, c := range below {
		for _, id := range []string{failed, running} {
			rec := e.wantStatus(c, "/api/runs/"+id, http.StatusOK)
			if _, ok := rawKeys(e, c, "/api/runs/"+id)["kubernetes"]; ok {
				t.Errorf("%s's run %s has a kubernetes key", who, id)
			}
			for _, secret := range []string{"pergent-abc123", "pergent-def456", "pergent-jobs", "insufficient memory"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("%s's run %s shows %q: %s", who, id, secret, rec.Body)
				}
			}
		}
	}
	// The review page's run list carries none of it either.
	for _, target := range []string{"/api/reviews/main/acme/widgets/3", "/api/reviews"} {
		for _, c := range []*http.Cookie{admin, below["viewer"], below["repository maintainer"], below["instance maintainer"]} {
			if body := e.get(c, target).Body.String(); strings.Contains(body, "insufficient memory") ||
				strings.Contains(body, "pergent-abc123") {
				t.Errorf("GET %s shows the Job or its detail: %s", target, body)
			}
		}
	}
}

func TestRunDiagnosticsMaskedForViewer(t *testing.T) {
	e := newEnv(t, nil)
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), outcome: OutcomeFailed, failure: "clone failed"})
	if err := e.st.PutRunDiagnostics(ctx, id, store.Diagnostics{
		CloneLog: "fatal: unable to access https://gitlab.example.com/acme/private-lib.git", CloneLogMasked: "fatal: unable to access [linked repository]",
		PodState: "phase: Pending\nPodScheduled: False"}); err != nil {
		t.Fatal(err)
	}
	// Masked text may be empty (the engine fails closed): the viewer gets "".
	empty := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), outcome: OutcomeFailed, failure: "clone failed"})
	if err := e.st.PutRunDiagnostics(ctx, empty, store.Diagnostics{CloneLog: "secret path", PodState: "state"}); err != nil {
		t.Fatal(err)
	}

	below := belowAdmin(e)
	viewer := below["viewer"]
	target := "/api/runs/" + id + "/diagnostics"
	for who, c := range below {
		if v := getJSON[DiagnosticsView](e, c, target); v.CloneLog != "fatal: unable to access [linked repository]" || v.PodState != nil {
			t.Errorf("%s diagnostics = %+v; want the masked log and no pod state", who, v)
		}
		keys := rawKeys(e, c, target)
		if _, ok := keys["podState"]; ok {
			t.Errorf("%s's JSON has a podState key: %v", who, keys)
		}
		if rec := e.get(c, target); strings.Contains(rec.Body.String(), "private-lib") {
			t.Errorf("%s sees the linked repository: %s", who, rec.Body)
		}
	}
	v := getJSON[DiagnosticsView](e, viewer, "/api/runs/"+empty+"/diagnostics")
	if v.CloneLog != "" || v.PodState != nil {
		t.Errorf("viewer diagnostics with no masked log = %+v; want an empty log and no pod state", v)
	}
	if _, ok := rawKeys(e, viewer, "/api/runs/"+empty+"/diagnostics")["cloneLog"]; !ok {
		t.Error("viewer's JSON has no cloneLog key")
	}

	admin := e.sessionFor(e.ssoAdmin("root@example.com"))
	got := getJSON[DiagnosticsView](e, admin, target)
	if got.CloneLog != "fatal: unable to access https://gitlab.example.com/acme/private-lib.git" ||
		got.PodState == nil || *got.PodState != "phase: Pending\nPodScheduled: False" {
		t.Errorf("admin diagnostics = %+v; want the full log and pod state", got)
	}
}

func TestRunDiagnosticsNotFound(t *testing.T) {
	e := newEnv(t, nil)
	none := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), result: cleanResult(head('b'))})
	hidden := e.addRun(runFix{project: "acme/payments", projectID: 8, number: 9, head: head('9'), result: cleanResult(head('9'))})
	if err := e.st.PutRunDiagnostics(ctx, hidden, store.Diagnostics{CloneLog: "log", CloneLogMasked: "log", PodState: "state"}); err != nil {
		t.Fatal(err)
	}
	c := e.userOnMain(false, "acme/widgets")
	e.wantStatus(c, "/api/runs/"+none+"/diagnostics", http.StatusNotFound)
	e.wantStatus(c, "/api/runs/bad/diagnostics", http.StatusNotFound)
	// A run the caller can't see answers as an unknown one.
	rec := e.wantStatus(c, "/api/runs/"+hidden+"/diagnostics", http.StatusNotFound)
	if strings.Contains(rec.Body.String(), "payments") {
		t.Errorf("the 404 names the hidden repository: %s", rec.Body)
	}
	e.wantStatus(nil, "/api/runs/"+none+"/diagnostics", http.StatusUnauthorized)
}

func TestRepositories(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	if err := e.st.UpsertDashboardRepo(ctx, store.DashboardRepo{Instance: "main", Project: "acme/tools"}); err != nil {
		t.Fatal(err)
	}
	c := e.userOnMain(false, "acme/widgets", "acme/tools")

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
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/ACME/widgets")
	if v.Repo.Project != "acme/widgets" || v.Repo.Source != "config" || v.Repo.Health != "attention" {
		t.Errorf("repo = %+v", v.Repo)
	}
	settings, _ := json.Marshal(v.Settings)
	want := `{"approve":{"value":false,"overridden":true},"commandsAuthorAndMaintainersOnly":{"value":true,"overridden":false},` +
		`"commentLayout":{"value":["summary:open","findings:collapsed","dismissed:collapsed","checks:open","files:collapsed"],"overridden":false},` +
		`"failureStatus":{"value":"failed","overridden":false},` +
		`"incrementalReviews":{"value":true,"overridden":false},"inlineFindings":{"value":true,"overridden":false},"jobMemory":{"value":"","overridden":false},"jobTimeoutSeconds":{"value":600,"overridden":false},` +
		`"learnFromDismissals":{"value":true,"overridden":false},"linkedRepos":{"value":[],"overridden":false},"linkedReposEnabled":{"value":false,"overridden":false},` +
		`"maxTurns":{"value":40,"overridden":false},"model":{"value":null,"overridden":false},"quietReviews":{"value":false,"overridden":false},"repoSkills":{"value":[],"overridden":false},` +
		`"rerunLimitPerMR":{"value":10,"overridden":false},"rerunLimitPerRepo":{"value":50,"overridden":false},` +
		`"reviewDrafts":{"value":true,"overridden":false},"skills":{"value":["code-review"],"overridden":true},` +
		`"trustedAuthors":{"value":[],"overridden":false},"trustedAuthorsOnly":{"value":"follow_visibility","overridden":false}}`
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
	e.skill("code-review", "---\nname: code-review\n---\nReview the code.\n")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	src := e.src["main"]
	src.set(func(f *fakeSource) {
		f.files[".pergent/skills/team-rules.md"] = "---\nname: team-rules\n---\nOur rules.\n"
		f.files[".pergent/skills/style.md"] = "---\nname: style\n---\nOur style.\n"
		f.files["AGENTS.md"] = "Be kind."
		f.files[".pergent/instructions.md"] = "Check migrations."
	})
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	if v.Branch != "trunk" {
		t.Errorf("branch = %q", v.Branch)
	}
	var got []string
	for _, s := range v.Skills {
		got = append(got, s.Name+":"+s.Source+":"+strings.TrimSpace(s.Content[strings.LastIndex(s.Content, "---")+3:]))
	}
	// style isn't in the library, so the repository's own is a repository
	// skill; its source is the folder.
	if want := "code-review:library:Review the code.,style:.pergent/skills:Our style.,team-rules:.pergent/skills:Our rules."; strings.Join(got, ",") != want {
		t.Errorf("skills = %v\nwant %s", got, want)
	}
	if !strings.Contains(v.Instructions, "Be kind.") || !strings.Contains(v.Instructions, "Check migrations.") {
		t.Errorf("instructions = %q", v.Instructions)
	}
	src.mu.Lock()
	refs, ids := src.refs, src.ids
	src.mu.Unlock()
	if len(refs) != 1 || !refs["trunk"] || len(ids) != 1 || !ids[testID("acme/widgets")] {
		t.Errorf("read refs %v of projects %v; want trunk of 7", refs, ids)
	}

	// Kept 5 minutes.
	calls := src.callCount()
	getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	if src.callCount() != calls {
		t.Errorf("a second view read the repository again")
	}
	e.clock.advance(6 * time.Minute)
	src.set(func(f *fakeSource) { f.fail = errors.New("gitlab: status 500: internal " + reviewToken) })
	rec := e.wantStatus(c, "/api/repositories/main/acme/widgets/inputs", http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "500") || strings.Contains(rec.Body.String(), reviewToken) {
		t.Errorf("502 body = %s; want a generic message", rec.Body)
	}
	if src.callCount() == calls {
		t.Errorf("after 6 minutes the repository wasn't read again")
	}
}

// A skill removed from the library since a repository named it is shown with
// its problem, as a review fails on it: never dropped, and never replaced by
// the other library skills.
func TestRepositoryInputsSkillRemovedFromLibrary(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("code-review", "Review the code.")
	e.skill("gone", "x")
	e.skill("other", "y")
	for _, project := range []string{"acme/tools", "acme/old"} {
		if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID(project), Path: project}); err != nil {
			t.Fatal(err)
		}
	}
	e.dashboardRepo("main", "acme/tools", func(r *store.DashboardRepo) { r.Skills = []string{"code-review", "gone"} })
	e.dashboardRepo("main", "acme/old", func(r *store.DashboardRepo) { r.Skills = []string{"gone"} })
	// The store refuses to delete a skill in use; raw SQL stands for a
	// removal that raced a save.
	e.exec(`DELETE FROM library_skills WHERE name = 'gone'`)

	c := e.userOnMain(false, "acme/tools", "acme/old")
	for project, want := range map[string]string{
		"acme/tools": `code-review:Review the code.:,gone::skill "gone" is not in the skill library`,
		"acme/old":   `gone::skill "gone" is not in the skill library`,
	} {
		v := getJSON[InputsView](e, c, "/api/repositories/main/"+project+"/inputs")
		var got []string
		for _, s := range v.Skills {
			got = append(got, s.Name+":"+s.Content+":"+s.Problem)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: skills = %q, want %q", project, got, want)
		}
	}
}

func TestRepositoryInputsHidden(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/payments"), Path: "acme/payments"}); err != nil {
		t.Fatal(err)
	}
	c := e.userOnMain(false, "acme/widgets")
	e.wantStatus(c, "/api/repositories/main/acme/payments/inputs", http.StatusNotFound)
	if n := e.src["main"].callCount(); n != 0 {
		t.Errorf("%d reads of a repository the user can't see", n)
	}
	// A repository covered but not stored yet by reconcile: try again soon.
	rec := e.wantStatus(c, "/api/repositories/main/acme/widgets/inputs", http.StatusServiceUnavailable)
	if !strings.Contains(rec.Body.String(), "try again in a minute") {
		t.Errorf("503 body = %s", rec.Body)
	}
	if n := e.src["main"].callCount(); n != 0 {
		t.Errorf("%d reads of a repository that is not stored yet", n)
	}
}

// Requests for the same repository while its inputs are being read share
// that one read.
func TestRepositoryInputsReadOnce(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	src := e.src["main"]
	gate, entered := make(chan struct{}), make(chan struct{}, 10)
	src.set(func(f *fakeSource) { f.gate, f.entered = gate, entered })
	c := e.userOnMain(false, "acme/widgets")

	const n = 5
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- e.get(c, "/api/repositories/main/acme/widgets/inputs").Code
		}()
	}
	<-entered // the first read is under way
	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("status = %d", code)
		}
	}
	src.mu.Lock()
	reads := len(entered)
	src.mu.Unlock()
	if reads != 0 {
		t.Errorf("%d more default-branch reads; want one for all %d requests", reads, n)
	}
}

func TestAdminSettings(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	c := e.userOnMain(true, "acme/widgets")
	v := getJSON[AdminSettingsView](e, c, "/api/admin/settings")
	// The built-in layout, spelled out: the server builds it from pergent's
	// render.DefaultLayout and a change there should show here.
	layout := []string{"summary:open", "findings:collapsed", "dismissed:collapsed", "checks:open", "files:collapsed"}
	want := AdminSettingsView{
		Defaults: DefaultsView{Approve: true, ReviewDrafts: true, InlineFindings: true, CommandsAuthorAndMaintainersOnly: true, IncrementalReviews: true, LearnFromDismissals: true, CommentLayout: layout, FailureStatus: "failed",
			JobTimeoutSeconds: 600, MaxTurns: 40,
			RerunLimitPerMR: 10, RerunLimitPerRepo: 50, Skills: []string{},
			Config: DefaultValues{Approve: true, ReviewDrafts: true, InlineFindings: true, CommandsAuthorAndMaintainersOnly: true, IncrementalReviews: true, LearnFromDismissals: true, CommentLayout: layout, FailureStatus: "failed",
				JobTimeoutSeconds: 600, MaxTurns: 40, RerunLimitPerMR: 10, RerunLimitPerRepo: 50},
			Pinned: map[string]bool{"approve": false, "reviewDrafts": false, "inlineFindings": false, "commandsAuthorAndMaintainersOnly": false,
				"incrementalReviews": false, "learnFromDismissals": false, "quietReviews": false, "commentLayout": false, "linkedReposEnabled": false, "failureStatus": false, "jobTimeoutSeconds": false,
				"maxTurns": false, "jobMemory": false, "rerunLimitPerMR": false, "rerunLimitPerRepo": false}},
		LLM: LLMView{Provider: "anthropic", Model: "claude-sonnet-4"}}
	got, _ := json.Marshal(v)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Errorf("admin settings = %s\nwant %s", got, wantJSON)
	}
}

func TestAdminEndpointsNotFoundForDevelopers(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	c := e.userOnMain(false, "acme/widgets")
	for _, target := range []string{"/api/admin/settings"} {
		rec := e.wantStatus(c, target, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "anthropic") || strings.Contains(rec.Body.String(), "acme/widgets") {
			t.Errorf("404 body of %s = %s", target, rec.Body)
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
		"/api/reviews/main/acme/widgets/1", "/api/runs/" + id, "/api/runs/" + id + "/log", "/api/runs/" + id + "/diagnostics",
		"/api/repositories", "/api/repositories/main/acme/widgets", "/api/repositories/main/acme/widgets/inputs",
		"/api/skills", "/api/admin/settings", "/api/me",
	}
}

// A repository the user's teams don't cover never shows in a list, and its
// own URLs are 404.
func TestReadAPIHidesInvisibleRepos(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	hidden := e.addRun(runFix{project: "acme/payments", projectID: 8, number: 9, head: head('9'),
		result: blockingResult(head('9')), title: "Payments secret change"})
	if err := e.st.PutRunLog(ctx, hidden, "payments log"); err != nil {
		t.Fatal(err)
	}
	// MR 20 moved from acme/widgets into acme/payments: its later
	// run is hidden.
	moved := e.addRun(runFix{project: "acme/widgets", number: 20, head: head('a'), outcome: OutcomeBlocking,
		at: e.clock.now().Add(-2 * time.Hour)})
	movedHidden := e.addRun(runFix{project: "acme/payments", number: 20, head: head('b'), outcome: OutcomeApproved,
		approved: true, title: "Moved payments change"})
	for _, p := range []store.Project{{Instance: "main", ProjectID: 7, Path: "acme/widgets", HookID: 1},
		{Instance: "main", ProjectID: 8, Path: "acme/payments", HookID: 2}} {
		if err := e.st.UpsertProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	c := e.userOnMain(false, "acme/widgets")

	for _, target := range []string{"/api/overview", "/api/overview?days=90", "/api/reviews",
		"/api/reviews?project=acme/payments", "/api/repositories"} {
		rec := e.get(c, target)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", target, rec.Code)
		}
		if body := strings.ToLower(rec.Body.String()); strings.Contains(body, "payments") || strings.Contains(body, hidden) {
			t.Errorf("GET %s shows acme/payments: %s", target, rec.Body)
		}
	}
	for _, target := range []string{
		"/api/reviews/main/acme/payments/9", "/api/reviews/main/acme/payments/5",
		"/api/runs/" + hidden, "/api/runs/" + hidden + "/log", "/api/runs/" + hidden + "/diagnostics", "/api/runs/" + movedHidden,
		"/api/reviews/main/acme/payments/20",
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
	// The moved MR's visible run is shown, its hidden one isn't.
	v := getJSON[ReviewDetailView](e, c, "/api/reviews/main/acme/widgets/20")
	if len(v.Runs) != 1 || v.Runs[0].ID != moved {
		t.Errorf("moved MR runs = %+v; want only %s", v.Runs, moved)
	}
	body := e.get(c, "/api/reviews/main/acme/widgets/20").Body.String()
	if strings.Contains(body, movedHidden) || strings.Contains(strings.ToLower(body), "payments") {
		t.Errorf("moved MR shows its hidden run: %s", body)
	}
	if d := getJSON[RunDetailView](e, c, "/api/runs/"+moved); d.Run.ID != moved {
		t.Errorf("moved MR's visible run = %+v", d.Run)
	}
}

// No response of the read API holds a token, key or secret.
func TestNoSecretInTheReadAPI(t *testing.T) {
	for _, instance := range []string{"main", "gh"} {
		t.Run(instance, func(t *testing.T) {
			e := newEnv(t, withSecrets(t))
			overviewFixture(e)
			id := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('1'), result: cleanResult(head('1'))})
			if err := e.st.PutRunLog(ctx, id, "token [REDACTED]"); err != nil {
				t.Fatal(err)
			}
			if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets", HookID: 3,
				DeployTokenID: 4, DeployTokenUsername: "deploy-user", DeployTokenSealed: []byte(deployToken)}); err != nil {
				t.Fatal(err)
			}
			if err := e.st.UpsertGitHubProject(ctx, store.Project{Instance: "gh", ProjectID: testID("acme/widgets"), Path: "acme/widgets",
				InstallationID: 55, ApproveAvailable: true}); err != nil {
				t.Fatal(err)
			}
			c := e.userOnMain(true)
			secrets := []string{llmKey, reviewToken, approveToken, webhookSecret, deployToken, ghAppKey,
				e.key, c.Value, "deploy-user", "llm.example.com"}

			targets := readEndpoints(id)
			targets = append(targets, "/api/reviews/gh/acme/widgets/7", "/api/repositories/gh/acme/widgets",
				"/api/repositories/gh/acme/widgets/inputs",
				"/api/admin/instances", "/api/admin/models", "/api/admin/defaults", "/api/admin/comment-footer")
			for _, target := range targets {
				rec := e.get(c, target)
				if rec.Code >= 500 && rec.Code != http.StatusBadGateway {
					t.Errorf("GET %s = %d %s", target, rec.Code, rec.Body)
				}
				for _, s := range secrets {
					// The model's address is public to everyone on the read API
					// and shown to admins, by design, on their models page: no key,
					// token or secret may be in it either way.
					if s == "llm.example.com" && target == "/api/admin/models" {
						continue
					}
					if s != "" && strings.Contains(rec.Body.String(), s) {
						t.Errorf("GET %s shows the secret %q: %s", target, s, rec.Body)
					}
				}
				if strings.Contains(rec.Body.String(), "SECRET") {
					t.Errorf("GET %s shows a secret: %s", target, rec.Body)
				}
			}
		})
	}
}

// A path can pass from one project to another (deleted and created again,
// renamed, transferred): a user who can see only the project at the path
// now never sees the earlier one's runs, findings or logs.
func TestReusedPathHidesEarlierProject(t *testing.T) {
	e := newEnv(t, nil)
	oldRun := e.addRun(runFix{project: "acme/widgets", projectID: 7, number: 1, head: head('1'),
		result: blockingResult(head('1')), outcome: OutcomeBlocking, title: "Old project's change"})
	if err := e.st.PutRunLog(ctx, oldRun, "old project log"); err != nil {
		t.Fatal(err)
	}
	newRun := e.addRun(runFix{project: "acme/widgets", number: 2, head: head('2'),
		result: cleanResult(head('2')), outcome: OutcomeApproved, approved: true, title: "New project's change"})
	c := e.userOnMain(false, "acme/widgets") // the project at the path now: testID("acme/widgets")

	if _, se := e.session(c); se.Visible[RepoKey{"main", "acme/widgets"}] != testID("acme/widgets") {
		t.Fatalf("visible = %v", se.Visible)
	}
	list := getJSON[ReviewListView](e, c, "/api/reviews")
	if len(list.Items) != 1 || list.Items[0].Number != 2 {
		t.Errorf("reviews = %+v; want only the new project's", list.Items)
	}
	ov := getJSON[OverviewView](e, c, "/api/overview")
	if ov.Stats.Reviews != 1 || ov.Stats.Blocking != 0 || len(ov.Recent) != 1 {
		t.Errorf("overview = %+v; want only the new project's run", ov)
	}
	for _, target := range []string{"/api/runs/" + oldRun, "/api/runs/" + oldRun + "/log", "/api/reviews/main/acme/widgets/1"} {
		rec := e.wantStatus(c, target, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "Old project") || strings.Contains(rec.Body.String(), "old project log") {
			t.Errorf("%s shows the earlier project: %s", target, rec.Body)
		}
	}
	e.wantStatus(c, "/api/runs/"+newRun, http.StatusOK)
	e.wantStatus(c, "/api/reviews/main/acme/widgets/2", http.StatusOK)
	repos := getJSON[[]RepositoryView](e, c, "/api/repositories")
	for _, r := range repos {
		if strings.EqualFold(r.Project, "acme/widgets") && r.Last7.Reviews != 1 {
			t.Errorf("acme/widgets last 7 days = %+v; want only the new project's run", r.Last7)
		}
	}
}

// --- skills library views ---

// A fresh install has nothing set up: every item is false. Each turns true
// on its own, and the checklist is gone once all four are done.
func TestSetupChecklist(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		cfg.GitLab, cfg.GitHub, cfg.Repositories = nil, nil, nil
	})
	root := e.signInOperator()
	setup := func() *SetupView { return getJSON[OverviewView](e, root, "/api/overview").Setup }

	if got := setup(); got == nil || *got != (SetupView{}) {
		t.Fatalf("fresh install setup = %+v, want all four false", got)
	}
	if body := e.get(root, "/api/overview").Body.String(); !strings.Contains(body, `"setup":{"llm":false,"instance":false,"skill":false,"repository":false}`) {
		t.Errorf("body = %s", body)
	}

	t.Setenv("PERGENT_TEST_LLM_KEY", llmKey)
	e.cfg.LLM = config.LLM{Provider: "anthropic", Model: "claude-sonnet-4", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	if got := setup(); got == nil || *got != (SetupView{LLM: true}) {
		t.Errorf("with an LLM = %+v", got)
	}
	e.cfg.GitLab = []config.Instance{{Name: "main", URL: glURL}}
	if got := setup(); got == nil || *got != (SetupView{LLM: true, Instance: true}) {
		t.Errorf("with an instance = %+v", got)
	}
	// A skill of a repository doesn't count: the library must hold one.
	e.src["main"].set(func(f *fakeSource) { f.files[".pergent/skills/mine.md"] = "Mine." })
	if got := setup(); got == nil || got.Skill {
		t.Errorf("a repository skill counted: %+v", got)
	}
	e.skill("code-review", "Review the code.")
	if got := setup(); got == nil || *got != (SetupView{LLM: true, Instance: true, Skill: true}) {
		t.Errorf("with a skill = %+v", got)
	}
	e.cfg.Repositories = []config.Repository{{GitLab: "main", Project: "acme/widgets"}}
	if got := setup(); got != nil {
		t.Errorf("all four done: setup = %+v, want null", got)
	}
	if body := e.get(root, "/api/overview").Body.String(); !strings.Contains(body, `"setup":null`) {
		t.Errorf("body = %s; want setup null", body)
	}
}

func TestSetupChecklistOnlyForAdmins(t *testing.T) {
	e := newEnv(t, nil) // no LLM, no skill: not set up
	viewer := e.userOnMain(false, "acme/widgets")
	if ov := getJSON[OverviewView](e, viewer, "/api/overview"); ov.Setup != nil {
		t.Errorf("viewer setup = %+v, want null", ov.Setup)
	}
	e.ssoAdmin(aliceEmail) // the same session, now an admin's
	ov := getJSON[OverviewView](e, viewer, "/api/overview")
	if ov.Setup == nil || ov.Setup.LLM || ov.Setup.Skill || !ov.Setup.Instance || !ov.Setup.Repository {
		t.Errorf("admin setup = %+v", ov.Setup)
	}
}

// A usage table that can't be read leaves the page as it was: the sum of the
// result, and a warning in the log.
func TestRunDetailKeepsThePageWhenUsageCannotBeRead(t *testing.T) {
	e := newEnv(t, nil)
	own := skillJSON("own", "no_blocking_issues")
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: resultJSON(head('a'), own),
		outcome: OutcomeApproved, approved: true})
	c := e.userOnMain(false, "acme/widgets")
	e.exec(`DROP TABLE run_usage`)

	rec := e.get(c, "/api/runs/"+id)
	wantCode(t, rec, "a run whose usage can't be read", http.StatusOK)
	var v RunDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Tokens != (TokensView{In: 100, Out: 10}) || len(v.Skills) != 1 {
		t.Errorf("tokens = %+v, skills = %d; want the sum of the result and its skill", v.Tokens, len(v.Skills))
	}
	if !strings.Contains(e.logs.String(), "reading a run's usage failed") {
		t.Errorf("log lacks the warning:\n%s", e.logs.String())
	}
}

func TestRunDetailSkillVersionAndFolder(t *testing.T) {
	e := newEnv(t, nil)
	res := resultJSON(head('a'),
		skillJSON("code-review", "no_blocking_issues"),
		skillJSON("team-rules", "no_blocking_issues"),
		skillJSON("old-run", "no_blocking_issues"),
		skillJSON("unrecorded", "no_blocking_issues"))
	id := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('a'), result: res, outcome: OutcomeApproved,
		approved: true, sources: `{"code-review":"library","team-rules":".agents/skills","old-run":"repo"}`,
		versions: `{"code-review":3}`})
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[RunDetailView](e, c, "/api/runs/"+id)
	if len(v.Skills) != 4 {
		t.Fatalf("skills = %+v", v.Skills)
	}
	type row struct {
		source  string
		version int
	}
	want := map[string]row{
		"code-review": {"library", 3},
		"team-rules":  {".agents/skills", 0}, // a repository skill has no library version
		"old-run":     {"repo", 0},           // a run from before the folders, still shown
		"unrecorded":  {"", 0},
	}
	for _, s := range v.Skills {
		got := row{version: s.Version}
		if s.Source != nil {
			got.source = *s.Source
		}
		if got != want[s.Name] {
			t.Errorf("%s = %+v, want %+v", s.Name, got, want[s.Name])
		}
	}
	if raw := e.get(c, "/api/runs/"+id).Body.String(); !strings.Contains(raw, `"version":3`) {
		t.Errorf("body = %s; want version 3", raw)
	}
}

// An incremental run's view says what it covered; a full run's note is empty
// (and still present, as "").
func TestRunDetailIncrementalNote(t *testing.T) {
	e := newEnv(t, nil)
	var res map[string]any
	if err := json.Unmarshal([]byte(cleanResult(head('a'))), &res); err != nil {
		t.Fatal(err)
	}
	res["incremental"] = map[string]any{"reviewed": 2, "carried": 28, "from": "abc1234def5678abc1234def5678abc1234def56"}
	b, _ := json.Marshal(res)
	incremental := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('a'), result: string(b), outcome: OutcomeApproved, approved: true, incremental: true})
	full := e.addRun(runFix{project: "acme/widgets", number: 2, head: head('b'), result: cleanResult(head('b')), outcome: OutcomeApproved, approved: true})
	// An incremental run whose result was never merged with the last review
	// (a push superseded it, or the pod restarted) is a few files only.
	unmerged := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('c'), result: cleanResult(head('c')), state: store.Superseded, incremental: true})
	// Not yet merged is no reason to say so: a reported run waits for the
	// publisher to merge it, and an error result has no files to speak of.
	waiting := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('d'), result: cleanResult(head('d')), state: store.Reported, incremental: true})
	errored := e.addRun(runFix{project: "acme/widgets", number: 5, head: head('e'), state: store.Superseded, incremental: true,
		result: `{"schema_version":1,"status":"error","base":"` + baseSHA + `","head":"` + head('e') + `","comment":{"markdown":"x"}}`})
	c := e.userOnMain(false, "acme/widgets")

	for name, id := range map[string]string{"a reported run": waiting, "an error result": errored} {
		if v := getJSON[RunDetailView](e, c, "/api/runs/"+id); v.IncrementalNote != "" {
			t.Errorf("incrementalNote = %q for %s, want none", v.IncrementalNote, name)
		}
	}
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+incremental); v.IncrementalNote != "Re-reviewed 2 files · 28 carried over from `abc1234`" {
		t.Errorf("incrementalNote = %q", v.IncrementalNote)
	}
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+unmerged); v.IncrementalNote != "Part of an incremental review that was never merged." {
		t.Errorf("incrementalNote = %q for an unmerged partial", v.IncrementalNote)
	}
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+full); v.IncrementalNote != "" {
		t.Errorf("incrementalNote = %q for a full review, want none", v.IncrementalNote)
	}
	if raw := e.get(c, "/api/runs/"+full).Body.String(); !strings.Contains(raw, `"incrementalNote":""`) {
		t.Errorf("body = %s; want an empty incrementalNote", raw)
	}
}

// A run's view says whether it was started quiet: true for a run that
// recorded it, false for one whose column is NULL (and present as false).
func TestRunDetailQuiet(t *testing.T) {
	e := newEnv(t, nil)
	quiet := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('a'), result: cleanResult(head('a')), outcome: OutcomeApproved, approved: true, quiet: true})
	plain := e.addRun(runFix{project: "acme/widgets", number: 2, head: head('b'), result: cleanResult(head('b')), outcome: OutcomeApproved, approved: true})
	c := e.userOnMain(false, "acme/widgets")

	if v := getJSON[RunDetailView](e, c, "/api/runs/"+quiet); !v.Quiet {
		t.Errorf("quiet = false for a quiet run")
	}
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+plain); v.Quiet {
		t.Errorf("quiet = true for a run that never recorded it")
	}
	if raw := e.get(c, "/api/runs/"+plain).Body.String(); !strings.Contains(raw, `"quiet":false`) {
		t.Errorf("body = %s; want quiet false", raw)
	}
}

// A run skipped for lack of skills is not a failure and not counted as a
// review; its reason says so.
func TestSkippedRunOutcome(t *testing.T) {
	e := newEnv(t, nil)
	now := e.clock.now()
	skipped := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('1'), state: store.Skipped, undispatched: true,
		skipReason: "this repository has no skills", at: now.Add(-time.Minute)})
	e.addRun(runFix{project: "acme/widgets", number: 2, head: head('2'), outcome: OutcomeFailed,
		failure: "the review Job ran out of memory (OOMKilled)", at: now.Add(-2 * time.Minute)})
	c := e.userOnMain(false, "acme/widgets")

	const reason = "Not reviewed: this repository has no skills."
	byNumber := map[int64]ReviewSummary{}
	for _, r := range getJSON[ReviewListView](e, c, "/api/reviews").Items {
		byNumber[r.Number] = r
	}
	if r := byNumber[1]; r.Outcome != OutcomeSkipped || r.Reason != reason {
		t.Errorf("skipped review = %s %q, want skipped %q", r.Outcome, r.Reason, reason)
	}
	if v := getJSON[ReviewListView](e, c, "/api/reviews?outcome=skipped"); len(v.Items) != 1 || v.Items[0].Number != 1 {
		t.Errorf("skipped filter = %+v, want only the skipped review", v.Items)
	}
	if n := len(getJSON[ReviewListView](e, c, "/api/reviews?outcome=superseded").Items); n != 0 {
		t.Errorf("superseded filter lists %d reviews, want none: a skipped run is not superseded", n)
	}
	if n := len(getJSON[ReviewListView](e, c, "/api/reviews?outcome=failed").Items); n != 1 {
		t.Errorf("failed filter lists %d reviews, want only the real failure", n)
	}
	ov := getJSON[OverviewView](e, c, "/api/overview")
	if ov.Stats.Failed != 1 || ov.Stats.Reviews != 1 {
		t.Errorf("stats = %+v; want the skipped run left out of the count", ov.Stats)
	}
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); repos[0].Last7.Failed != 1 || repos[0].Last7.Reviews != 1 {
		t.Errorf("last 7 days = %+v", repos[0].Last7)
	}
	d := getJSON[RunDetailView](e, c, "/api/runs/"+skipped)
	if d.Run.Outcome != OutcomeSkipped || d.Run.Reason != reason || d.Approved || d.Status != "" || len(d.Skills) != 0 {
		t.Errorf("run detail = %+v", d)
	}
	rv := getJSON[ReviewDetailView](e, c, "/api/reviews/main/acme/widgets/1")
	if len(rv.Runs) != 1 || rv.Runs[0].Reason != reason {
		t.Errorf("review runs = %+v", rv.Runs)
	}
}

func skillFile(name string) string { return "---\nname: " + name + "\n---\nText of " + name + ".\n" }

func TestRepoSkillURLGitLab(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].RepoSkills = []string{"agent-rules"} })
	e.skill("code-review", "Review the code.")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	e.src["main"].set(func(f *fakeSource) {
		f.branch = "release/1.x"
		f.files[".pergent/skills/team-rules.md"] = skillFile("team-rules")
		f.files[".agents/skills/agent-rules.md"] = skillFile("agent-rules")
	})
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	urls := map[string]string{}
	for _, s := range v.Skills {
		urls[s.Name] = s.URL
	}
	base := glURL + "/acme/widgets/-/blob/release/1.x/"
	if urls["team-rules"] != base+".pergent/skills/team-rules.md" || urls["agent-rules"] != base+".agents/skills/agent-rules.md" {
		t.Errorf("urls = %v", urls)
	}
	if urls["code-review"] != "" {
		t.Errorf("library skill url = %q, want none", urls["code-review"])
	}
}

func TestRepoSkillURLGitHub(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[2].RepoSkills = []string{"claude-rules"} })
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "gh", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	e.src["gh"].set(func(f *fakeSource) {
		f.files[".claude/skills/claude-rules.md"] = skillFile("claude-rules")
	})
	c := e.userOnGH(false, "acme/widgets")

	v := getJSON[InputsView](e, c, "/api/repositories/gh/acme/widgets/inputs")
	// The instance's API is at <host>/api/v3, so its web address is the host.
	want := "https://github.example.com/acme/widgets/blob/trunk/.claude/skills/claude-rules.md"
	if len(v.Skills) != 1 || v.Skills[0].URL != want {
		t.Errorf("skills = %+v, want url %s", v.Skills, want)
	}

	// github.com, and the web address of an enterprise server.
	for api, web := range map[string]string{
		"":                                "https://github.com",
		"https://api.github.com":          "https://github.com",
		"https://api.github.com/":         "https://github.com",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com",
	} {
		inst := instances.Instance{Platform: config.PlatformGitHub, GitHub: &config.GitHubInstance{APIURL: api}}
		got := repoSkillURL(inst, "acme/widgets", "main", "a/b.md")
		if want := web + "/acme/widgets/blob/main/a/b.md"; got != want {
			t.Errorf("api %q: url = %q, want %q", api, got, want)
		}
	}
}

func TestRepoSkillURLEscapesPath(t *testing.T) {
	gl := instances.Instance{Platform: config.PlatformGitLab, GitLab: &config.Instance{URL: "https://gitlab.example.com/"}}
	got := repoSkillURL(gl, "group/sub group/proj", "feat/a#1?x%", ".agents/skills/my skill#2/SKILL.md")
	want := "https://gitlab.example.com/group/sub%20group/proj/-/blob/feat/a%231%3Fx%25/.agents/skills/my%20skill%232/SKILL.md"
	if got != want {
		t.Errorf("url = %q\nwant  %q", got, want)
	}
	for _, in := range []instances.Instance{{}, {Platform: config.PlatformGitLab, GitLab: &config.Instance{URL: "javascript:alert(1)"}}} {
		if got := repoSkillURL(in, "a/b", "main", "c.md"); got != "" {
			t.Errorf("url = %q for %+v, want none", got, in)
		}
	}
}

// An item with no instance is about the library as a whole: every admin
// sees it; a viewer never does.
func TestGlobalAttentionForAdmins(t *testing.T) {
	kinds := func(e *env, c *http.Cookie) string {
		var out []string
		for _, a := range getJSON[OverviewView](e, c, "/api/overview").Attention {
			out = append(out, a.Instance+"/"+a.Project+"/"+a.Kind)
		}
		return strings.Join(out, ",")
	}
	for name, tc := range map[string]struct {
		signIn func(e *env) *http.Cookie
		want   string
	}{
		"viewer":      {func(e *env) *http.Cookie { return e.userOnMain(false, "acme/widgets") }, ""},
		"sso admin":   {func(e *env) *http.Cookie { return e.userOnMain(true) }, "//library_problem,gh//token_expiry"},
		"local admin": {func(e *env) *http.Cookie { return e.signInOperator() }, "//library_problem,gh//token_expiry"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			if err := e.st.SetAttention(ctx, "", "", "library_problem", "a library skill needs a look"); err != nil {
				t.Fatal(err)
			}
			if err := e.st.SetAttention(ctx, "gh", "", "token_expiry", "another instance's"); err != nil {
				t.Fatal(err)
			}
			if got := kinds(e, tc.signIn(e)); got != tc.want {
				t.Errorf("attention = %q, want %q", got, tc.want)
			}
		})
	}
}

// The step limit is a setting of the repository like the others.
func TestSettingViewsMaxTurns(t *testing.T) {
	repo := settings.Effective{MaxTurns: 120, Overridden: map[string]bool{settings.KeyMaxTurns: true}}
	if got := settingViews(repo)["maxTurns"]; got != (SettingView{120, true}) {
		t.Errorf("maxTurns = %+v, want 120 overridden", got)
	}
	repo = settings.Effective{MaxTurns: 40}
	if got := settingViews(repo)["maxTurns"]; got != (SettingView{40, false}) {
		t.Errorf("maxTurns = %+v, want 40 not overridden", got)
	}
}

// The commands setting is a setting of the repository like the others.
func TestSettingViewsCommandsAuthorAndMaintainersOnly(t *testing.T) {
	repo := settings.Effective{CommandsAuthorAndMaintainersOnly: false, Overridden: map[string]bool{settings.KeyCommandsAuthorAndMaintainersOnly: true}}
	if got := settingViews(repo)["commandsAuthorAndMaintainersOnly"]; got != (SettingView{false, true}) {
		t.Errorf("commandsAuthorAndMaintainersOnly = %+v, want false overridden", got)
	}
	repo = settings.Effective{CommandsAuthorAndMaintainersOnly: true}
	if got := settingViews(repo)["commandsAuthorAndMaintainersOnly"]; got != (SettingView{true, false}) {
		t.Errorf("commandsAuthorAndMaintainersOnly = %+v, want true not overridden", got)
	}
}

// Incremental reviews is a setting of the repository like the others.
func TestSettingViewsIncrementalReviews(t *testing.T) {
	repo := settings.Effective{IncrementalReviews: false, Overridden: map[string]bool{settings.KeyIncrementalReviews: true}}
	if got := settingViews(repo)["incrementalReviews"]; got != (SettingView{false, true}) {
		t.Errorf("incrementalReviews = %+v, want false overridden", got)
	}
	repo = settings.Effective{IncrementalReviews: true}
	if got := settingViews(repo)["incrementalReviews"]; got != (SettingView{true, false}) {
		t.Errorf("incrementalReviews = %+v, want true not overridden", got)
	}
}

// Learn from dismissals is a setting of the repository like the others.
func TestSettingViewsLearnFromDismissals(t *testing.T) {
	repo := settings.Effective{LearnFromDismissals: false, Overridden: map[string]bool{settings.KeyLearnFromDismissals: true}}
	if got := settingViews(repo)["learnFromDismissals"]; got != (SettingView{false, true}) {
		t.Errorf("learnFromDismissals = %+v, want false overridden", got)
	}
	repo = settings.Effective{LearnFromDismissals: true}
	if got := settingViews(repo)["learnFromDismissals"]; got != (SettingView{true, false}) {
		t.Errorf("learnFromDismissals = %+v, want true not overridden", got)
	}
}

// Quiet reviews is a setting of the repository like the others, off unless
// set.
func TestSettingViewsQuietReviews(t *testing.T) {
	if got := settingViews(settings.Effective{})["quietReviews"]; got != (SettingView{false, false}) {
		t.Errorf("quietReviews = %+v, want false not overridden", got)
	}
	repo := settings.Effective{QuietReviews: true, Overridden: map[string]bool{settings.KeyQuietReviews: true}}
	if got := settingViews(repo)["quietReviews"]; got != (SettingView{true, true}) {
		t.Errorf("quietReviews = %+v, want true overridden", got)
	}
}

// The comment layout is a list of block:state texts: the built-in layout
// (from render.DefaultLayout) while the repository sets none, its own list,
// in its order, once it does.
func TestSettingViewsCommentLayout(t *testing.T) {
	text := func(repo settings.Effective) (string, bool) {
		v := settingViews(repo)["commentLayout"]
		b, err := json.Marshal(v.Value)
		if err != nil {
			t.Fatal(err)
		}
		return string(b), v.Overridden
	}
	if got, over := text(settings.Effective{}); got != `["summary:open","findings:collapsed","dismissed:collapsed","checks:open","files:collapsed"]` || over {
		t.Errorf("commentLayout = %s overridden %v; want the default, not overridden", got, over)
	}
	repo := settings.Effective{CommentLayout: []string{"checks:open", "findings:open"}, Overridden: map[string]bool{settings.KeyCommentLayout: true}}
	if got, over := text(repo); got != `["checks:open","findings:open"]` || !over {
		t.Errorf("commentLayout = %s overridden %v; want the repository's list, overridden", got, over)
	}
	// The view hands out a copy, so a caller can't change the repository's list.
	settingViews(repo)["commentLayout"].Value.([]string)[0] = "files:open"
	if repo.CommentLayout[0] != "checks:open" {
		t.Errorf("the view shares the repository's list: %q", repo.CommentLayout)
	}
}

// Inline findings is a setting of the repository like the others.
func TestSettingViewsInlineFindings(t *testing.T) {
	repo := settings.Effective{InlineFindings: false, Overridden: map[string]bool{settings.KeyInlineFindings: true}}
	if got := settingViews(repo)["inlineFindings"]; got != (SettingView{false, true}) {
		t.Errorf("inlineFindings = %+v, want false overridden", got)
	}
	repo = settings.Effective{InlineFindings: true}
	if got := settingViews(repo)["inlineFindings"]; got != (SettingView{true, false}) {
		t.Errorf("inlineFindings = %+v, want true not overridden", got)
	}
}
