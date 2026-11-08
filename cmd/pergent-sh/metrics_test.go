package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/metrics"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func metricsTestLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startMetrics serves src on 127.0.0.1:0 and returns its address; the server
// is stopped when the test ends.
func startMetrics(t *testing.T, src func(context.Context) (metrics.Snapshot, error)) (addr string, done <-chan struct{}, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		serveMetrics(ln, metricsHandler(src, metricsTestLog()), metricsTestLog())(ctx)
		close(finished)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	return ln.Addr().String(), finished, cancel
}

func metricsGet(t *testing.T, method, url string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// The metrics port answers GET /metrics and nothing else.
func TestMetricsServerServesOnlyMetrics(t *testing.T) {
	addr, _, _ := startMetrics(t, func(context.Context) (metrics.Snapshot, error) {
		s := metrics.New()
		s.Add(metrics.ReviewsNoResultTotal, 3, metrics.Label{Name: "instance", Value: "main"}, metrics.Label{Name: "repository", Value: "acme/widgets"})
		return s, nil
	})
	code, body, header := metricsGet(t, http.MethodGet, "http://"+addr+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", code)
	}
	if got := header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.Contains(body, "# TYPE pergent_reviews_no_result_total counter\n") ||
		!strings.Contains(body, `pergent_reviews_no_result_total{instance="main",repository="acme/widgets"} 3`+"\n") {
		t.Errorf("body = %q", body)
	}
	for _, p := range []string{"/healthz", "/api/me", "/api/", "/", "/metrics/", "/runs/1/result", "/webhooks/gitlab/main"} {
		if code, _, _ := metricsGet(t, http.MethodGet, "http://"+addr+p); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if code, _, _ := metricsGet(t, m, "http://"+addr+"/metrics"); code != http.StatusMethodNotAllowed {
			t.Errorf("%s /metrics = %d, want 405", m, code)
		}
	}
}

// A source that fails gives a 500 with nothing in the body.
func TestMetricsServerErrorIs500(t *testing.T) {
	addr, _, _ := startMetrics(t, func(context.Context) (metrics.Snapshot, error) {
		return metrics.Snapshot{}, errors.New("relation run_usage does not exist")
	})
	code, body, _ := metricsGet(t, http.MethodGet, "http://"+addr+"/metrics")
	if code != http.StatusInternalServerError || body != "" {
		t.Errorf("GET /metrics = %d %q, want 500 with no body", code, body)
	}
}

// The loop returns, and the port is released, when its context ends.
func TestMetricsServerStopsWithItsContext(t *testing.T) {
	addr, done, stop := startMetrics(t, func(context.Context) (metrics.Snapshot, error) { return metrics.New(), nil })
	if code, _, _ := metricsGet(t, http.MethodGet, "http://"+addr+"/metrics"); code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", code)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the metrics loop did not return after its context ended")
	}
	if _, err := http.Get("http://" + addr + "/metrics"); err == nil {
		t.Error("the metrics port still answers after shutdown")
	}
}

// A port that is taken stops start-up with an error that names metrics_listen.
func TestListenMetricsPortTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	_, err = listenMetrics(taken.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "metrics_listen") {
		t.Errorf("listenMetrics on a taken port = %v, want an error naming metrics_listen", err)
	}
	if ln, err := listenMetrics(""); ln != nil || err != nil {
		t.Errorf("listenMetrics(\"\") = %v, %v; want nil, nil", ln, err)
	}
}

func metricsEff(instance, project string, skills, repoSkills []string) settings.Effective {
	return settings.Effective{Instance: instance, Project: project, Skills: skills, RepoSkills: repoSkills}
}

func metricsText(t *testing.T, s metrics.Snapshot) string {
	t.Helper()
	var b strings.Builder
	if err := metrics.Write(&b, s); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Repositories not switched on and skills nobody lists collapse into one
// label each, series that collapse are added, and nothing a person wrote
// reaches a label.
func TestSnapshotOfBoundsTheLabels(t *testing.T) {
	zero := store.TokenSum{}
	totals := store.MetricsTotals{
		Tokens: []store.TokenTotal{
			{Instance: "main", Project: "Acme/Widgets", Skill: "security", Model: "m", Tokens: store.TokenSum{In: 10, Out: 2, CacheRead: 5, Reasoning: 1}},
			// The same repository under another spelling and an older path: one series each way.
			{Instance: "main", Project: "acme/widgets", Skill: "security", Model: "m", Tokens: store.TokenSum{In: 1, Out: 1}},
			{Instance: "main", Project: "acme/gone", Skill: "security", Model: "m", Tokens: store.TokenSum{In: 100}},
			{Instance: "main", Project: "acme/old-name", Skill: "security", Model: "m", Tokens: store.TokenSum{In: 200}},
			{Instance: "main", Project: "acme/widgets", Skill: "typo-skill", Model: "m", Tokens: store.TokenSum{In: 3}},
			{Instance: "main", Project: "acme/widgets", Skill: "another-skill", Model: "m", Tokens: store.TokenSum{In: 4}},
			{Instance: "main", Project: "acme/widgets", Skill: "own", Model: "m", Tokens: store.TokenSum{Out: 6}},
			{Instance: "main", Project: "acme/widgets", Skill: "from-repo", Model: "m", Tokens: store.TokenSum{Out: 7}},
			{Instance: "other", Project: "acme/widgets", Skill: "security", Model: "m", Tokens: zero}, // another instance's repository
		},
		Reviews: []store.ReviewTotal{
			{Instance: "main", Project: "acme/widgets", Trigger: "push", Incremental: true, Reviews: 2},
			{Instance: "main", Project: "acme/gone", Trigger: "push", Incremental: true, Reviews: 1},
			{Instance: "main", Project: "acme/old-name", Trigger: "push", Incremental: true, Reviews: 4},
		},
		Unfinished: []store.UnfinishedTotal{{Instance: "main", Project: "acme/gone", NoResult: 2, Superseded: 1}},
		SkillRuns:  []store.SkillRunTotal{{Instance: "main", Project: "acme/widgets", Skill: "security", Verdict: "failed", Runs: 5}},
		Findings:   []store.FindingTotal{{Instance: "main", Project: "acme/widgets", Skill: "security", Blocking: 1, Suggestions: 3}},
		Dismissals: []store.DismissalTotal{
			{Instance: "main", Project: "acme/widgets", Skill: "security", Dismissals: 2},
			{Instance: "main", Project: "", Skill: "", Dismissals: 1},
		},
	}
	on := []settings.Effective{
		metricsEff("main", "acme/Widgets", []string{"security"}, []string{"from-repo"}),
		metricsEff("main", "acme/other", []string{"own"}, nil),
	}
	library := []store.LibrarySkill{{Name: "security"}, {Name: "another-skill"}}
	// "another-skill" is in the library, "own" is a repository's own list,
	// "typo-skill" is in neither.
	out := metricsText(t, snapshotOf(totals, on, library))

	for _, want := range []string{
		// Two spellings of one switched-on repository are one series; the label is the spelling of the list.
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="security",model="m",kind="input"} 11`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="security",model="m",kind="output"} 3`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="security",model="m",kind="cache_read"} 5`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="security",model="m",kind="reasoning"} 1`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="security",model="m",kind="cache_write"} 0`,
		// Two repositories not switched on are one.
		`pergent_tokens_total{instance="main",repository="(switched off)",skill="security",model="m",kind="input"} 300`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="(other)",model="m",kind="input"} 3`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="another-skill",model="m",kind="input"} 4`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="own",model="m",kind="output"} 6`,
		`pergent_tokens_total{instance="main",repository="acme/Widgets",skill="from-repo",model="m",kind="output"} 7`,
		// The same path on another instance is not switched on there.
		`pergent_tokens_total{instance="other",repository="(switched off)",skill="security",model="m",kind="input"} 0`,
		`pergent_reviews_total{instance="main",repository="acme/Widgets",trigger="push",incremental="true"} 2`,
		`pergent_reviews_total{instance="main",repository="(switched off)",trigger="push",incremental="true"} 5`,
		`pergent_reviews_no_result_total{instance="main",repository="(switched off)"} 2`,
		`pergent_reviews_superseded_total{instance="main",repository="(switched off)"} 1`,
		`pergent_skill_runs_total{instance="main",repository="acme/Widgets",skill="security",verdict="failed"} 5`,
		`pergent_findings_total{instance="main",repository="acme/Widgets",skill="security",severity="blocking"} 1`,
		`pergent_findings_total{instance="main",repository="acme/Widgets",skill="security",severity="suggestion"} 3`,
		`pergent_dismissals_total{instance="main",repository="acme/Widgets",skill="security"} 2`,
		`pergent_dismissals_total{instance="main",repository="(switched off)",skill="(other)"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s\nin:\n%s", want, out)
		}
	}
	for _, bad := range []string{"acme/gone", "acme/old-name", "typo-skill", `repository="acme/widgets"`} {
		if strings.Contains(out, bad) {
			t.Errorf("the page names %q:\n%s", bad, out)
		}
	}
	// A series is listed once: no line repeats.
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "pergent_") {
			continue
		}
		series := line[:strings.LastIndex(line, " ")]
		if seen[series] {
			t.Errorf("series listed twice: %s", series)
		}
		seen[series] = true
	}
}

type fakeMetricsStore struct {
	totals  store.MetricsTotals
	library []store.LibrarySkill
	err     error
}

func (f fakeMetricsStore) MetricsTotals(context.Context) (store.MetricsTotals, error) {
	return f.totals, f.err
}
func (f fakeMetricsStore) LibrarySkills(context.Context) ([]store.LibrarySkill, error) {
	return f.library, f.err
}

type fakeRepos struct {
	on  []settings.Effective
	err error
}

func (f fakeRepos) List(context.Context) ([]settings.Effective, error) { return f.on, f.err }

func TestMetricsSourceReadsAllThree(t *testing.T) {
	st := fakeMetricsStore{
		totals:  store.MetricsTotals{Dismissals: []store.DismissalTotal{{Instance: "main", Project: "acme/widgets", Skill: "security", Dismissals: 1}}},
		library: []store.LibrarySkill{{Name: "security"}},
	}
	snap, err := metricsSource(st, fakeRepos{on: []settings.Effective{metricsEff("main", "acme/widgets", nil, nil)}})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out := metricsText(t, snap); !strings.Contains(out, `pergent_dismissals_total{instance="main",repository="acme/widgets",skill="security"} 1`+"\n") {
		t.Errorf("page = %s", out)
	}
	boom := errors.New("boom")
	if _, err := metricsSource(fakeMetricsStore{err: boom}, fakeRepos{})(context.Background()); !errors.Is(err, boom) {
		t.Errorf("a failing store gave %v, want its error", err)
	}
	if _, err := metricsSource(st, fakeRepos{err: boom})(context.Background()); !errors.Is(err, boom) {
		t.Errorf("a failing repository list gave %v, want its error", err)
	}
}
