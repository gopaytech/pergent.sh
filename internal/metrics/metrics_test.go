package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Errorf("escapeLabel = %q", got)
	}
	if got := escapeLabel("ok\xffbad"); got != "ok�bad" {
		t.Errorf("escapeLabel of invalid UTF-8 = %q, want the replacement character", got)
	}
}

func labels(kv ...string) []Label {
	var out []Label
	for i := 0; i < len(kv); i += 2 {
		out = append(out, Label{kv[i], kv[i+1]})
	}
	return out
}

func TestWrite(t *testing.T) {
	s := Snapshot{Metrics: []Metric{
		{Name: "b_total", Help: "Second.", Samples: []Sample{
			{labels("repository", "z/y", "kind", "output"), 7},
			{labels("repository", "a/b", "kind", "input"), 12},
			{labels("repository", "a/b", "kind", "input"), 3}, // the same series: added
			{labels("repository", `we"ird\path`+"\n", "kind", "input"), 1},
		}},
		{Name: "a_total", Help: "First, with a backslash \\ and a\nnewline.", Samples: []Sample{
			{nil, 5},
		}},
		{Name: "c_total", Help: "Nothing yet."},
	}}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	want := `# HELP a_total First, with a backslash \\ and a\nnewline.
# TYPE a_total counter
a_total 5
# HELP b_total Second.
# TYPE b_total counter
b_total{repository="a/b",kind="input"} 15
b_total{repository="we\"ird\\path\n",kind="input"} 1
b_total{repository="z/y",kind="output"} 7
# HELP c_total Nothing yet.
# TYPE c_total counter
`
	if got := buf.String(); got != want {
		t.Errorf("Write =\n%s\nwant\n%s", got, want)
	}
}

func TestSnapshotAdd(t *testing.T) {
	s := New()
	s.Add(TokensTotal, 4, labels("platform_instance", "main")...)
	if len(s.Metrics) != len(names) {
		t.Fatalf("New declares %d metrics, want %d", len(s.Metrics), len(names))
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, name := range names {
		typ := "counter"
		if name == FindingsDismissed {
			typ = "gauge"
		}
		if strings.Count(out, "# TYPE "+name+" "+typ+"\n") != 1 {
			t.Errorf("%s has no single # TYPE line:\n%s", name, out)
		}
		if strings.Count(out, "# HELP "+name+" ") != 1 {
			t.Errorf("%s has no single # HELP line:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "pergent_tokens_total{platform_instance=\"main\"} 4\n") {
		t.Errorf("the added sample is missing:\n%s", out)
	}
}

func TestHandlerServesTheText(t *testing.T) {
	h := Handler(func(context.Context) (Snapshot, error) {
		s := New()
		s.Add(FindingsDismissed, 2, labels("platform_instance", "main", "repository", "acme/widgets", "skill", "security")...)
		return s, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), `pergent_findings_dismissed{platform_instance="main",repository="acme/widgets",skill="security"} 2`+"\n") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestHandlerErrorIs500WithoutDetail(t *testing.T) {
	var logged bytes.Buffer
	h := Handler(func(context.Context) (Snapshot, error) {
		return Snapshot{}, errors.New("password=hunter2 in the query")
	}, slog.New(slog.NewTextHandler(&logged, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want none", rec.Body.String())
	}
	if !strings.Contains(logged.String(), "hunter2") {
		t.Errorf("the error was not logged: %q", logged.String())
	}
}

func TestHandlerGivesTheSourceATimeout(t *testing.T) {
	var left time.Duration
	h := Handler(func(ctx context.Context) (Snapshot, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("the source's context has no deadline")
		}
		left = time.Until(d)
		return New(), nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if left <= 5*time.Second || left > 10*time.Second {
		t.Errorf("time left = %v, want about 10s", left)
	}
}

// The dismissed findings go down when one is restored, so the page types them
// as a gauge and every other metric as a counter.
func TestFindingsDismissedIsAGauge(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, New()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "# TYPE pergent_findings_dismissed gauge\n") || strings.Contains(out, "dismissals_total") {
		t.Errorf("the dismissed findings are not a gauge:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE pergent_tokens_total counter\n") {
		t.Errorf("the token metric is not a counter:\n%s", out)
	}
}

// A scrape that arrives while another runs gets a 503 with an empty body, and
// the next one, once the first is done, is served.
func TestHandlerRunsOneScrapeAtATime(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	h := Handler(func(context.Context) (Snapshot, error) {
		started <- struct{}{}
		<-release
		return New(), nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		close(done)
	}()
	<-started
	busy := httptest.NewRecorder()
	second := make(chan struct{})
	go func() {
		h.ServeHTTP(busy, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Error("a scrape while one runs waited for it instead of being refused")
		close(release)
		<-second
		<-done
		return
	}
	if busy.Code != http.StatusServiceUnavailable || busy.Body.Len() != 0 {
		t.Errorf("a scrape while one runs = %d %q, want 503 with no body", busy.Code, busy.Body.String())
	}
	close(release)
	<-done
	if first.Code != http.StatusOK {
		t.Errorf("the first scrape = %d, want 200", first.Code)
	}
	again := httptest.NewRecorder()
	h.ServeHTTP(again, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if again.Code != http.StatusOK {
		t.Errorf("a scrape after the first finished = %d, want 200", again.Code)
	}
}
