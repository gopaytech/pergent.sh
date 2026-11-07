package webhook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// unreadBody fails the test when the handler reads the body.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body was read before the request could pass")
	return 0, errors.New("read")
}

// A request that can't pass costs no memory: nothing is read.
func TestGitHubRefusesBeforeReading(t *testing.T) {
	for name, header := range map[string]string{
		"no signature": "",
		"no prefix":    strings.Repeat("a", 64),
		"short":        "sha256=abcd",
		"not hex":      "sha256=" + strings.Repeat("z", 64),
		"another hash": "sha1=" + strings.Repeat("a", 40),
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitHubRig(t)
			req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", unreadBody{t})
			req.Header.Set("X-Hub-Signature-256", header)
			rec := httptest.NewRecorder()
			r.mux.ServeHTTP(rec, req)
			wantStatus(t, rec, http.StatusUnauthorized)
		})
	}
}

// Bodies held at once are bounded, whoever sends them, and a refused one is
// answered at once.
func TestBodyBudgetRefusesWhenSpent(t *testing.T) {
	r := newGitHubRig(t)
	if !bodyBudget.take(maxBufferedBytes) {
		t.Fatal("the budget is not full")
	}
	rec := r.gh(fixture(t, "github_opened.json"))
	bodyBudget.give(maxBufferedBytes)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	noQueueCalls(t, r)

	wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %+v, want 1 once there is room", r.q.enqueues)
	}
}

// Every request gives back what it took, however it ended.
func TestBodyBudgetIsGivenBack(t *testing.T) {
	gh, gl := newGitHubRig(t), newRig(t)
	gh.gh(fixture(t, "github_opened.json"))
	gh.gh([]byte("{"))
	gh.gh(fixture(t, "github_opened.json"), withSignature("sha256="+strings.Repeat("a", 64)))
	gh.gh(bytes.Repeat([]byte("x"), maxBodyBytes+1))
	gl.hook(fixture(t, "mr_open.json"))
	gl.hook(bytes.Repeat([]byte("x"), maxBodyBytes+1))
	if left := bodyBudget.left.Load(); left != maxBufferedBytes {
		t.Fatalf("budget left = %d, want %d", left, maxBufferedBytes)
	}
}

// A body that never finishes is given up on, well inside the server's limit.
func TestBodyReadTimesOut(t *testing.T) {
	old := bodyReadTimeout
	bodyReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { bodyReadTimeout = old })
	r := newGitHubRig(t)
	srv := httptest.NewServer(r.mux)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST /webhooks/github/gh HTTP/1.1\r\nHost: x\r\nX-Hub-Signature-256: sha256=%s\r\nContent-Length: 1000\r\n\r\npartial",
		strings.Repeat("a", 64))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// oldWithoutComments is the plain form of withoutComments, which pins what it
// must do.
func oldWithoutComments(line string) (string, bool) {
	for {
		start := strings.Index(line, "<!--")
		if start < 0 {
			return line, false
		}
		end := strings.Index(line[start+len("<!--"):], "-->")
		if end < 0 {
			return line[:start], true
		}
		line = line[:start] + line[start+len("<!--")+end+len("-->"):]
	}
}

func TestWithoutCommentsKeepsItsMeaning(t *testing.T) {
	for _, line := range []string{
		"", "plain", "<!-- x -->", "a<!-- x -->b", "a<!-- x -->b<!-- y -->c", "a<!-- open", "<!--", "-->", "<!---->",
		"<!<!-- x -->-- y -->z", "<<!-- x -->!-- y -->z", "<!-<!-- x -->- open", "<!--<!-- x -->", "--><!-- x -->", "<!-- a --> @bot review",
	} {
		wantRest, wantOpen := oldWithoutComments(line)
		rest, open := withoutComments(line)
		if rest != wantRest || open != wantOpen {
			t.Errorf("withoutComments(%q) = %q, %v; want %q, %v", line, rest, open, wantRest, wantOpen)
		}
	}
}

// Any commenter chooses this text, and the webhook parses it before it answers.
func TestWithoutCommentsIsOnePass(t *testing.T) {
	line := "x" + strings.Repeat("<!---->", 100_000)
	allocs := testing.AllocsPerRun(1, func() {
		if rest, open := withoutComments(line); rest != "x" || open {
			t.Fatalf("got %q, %v", rest, open)
		}
	})
	if allocs > 10 {
		t.Fatalf("%v allocations for one line: it copies per comment", allocs)
	}
}

func TestCommandMatches(t *testing.T) {
	for name, tc := range map[string]struct {
		c    store.Command
		body string
		want bool
	}{
		"same":           {store.Command{Kind: "dismiss", Arg: "abc123", Reason: "fine"}, "@bot dismiss abc123 fine", true},
		"no reason":      {store.Command{Kind: "review"}, "@bot review", true},
		"other kind":     {store.Command{Kind: "dismiss", Arg: "abc123"}, "@bot review", false},
		"no command":     {store.Command{Kind: "review"}, "looks good", false},
		"review comment": {store.Command{Kind: "review"}, "<!-- pergent -->\n@bot review", false},
		"in a thread":    {store.Command{Kind: "dismiss", Reason: "fine", Discussion: "d1"}, "@bot dismiss fine", true},
		"thread rule":    {store.Command{Kind: "dismiss", Reason: "fine"}, "@bot dismiss fine", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := CommandMatches(tc.c, tc.body, "bot"); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// projectsAt answers that an ID's project is at path.
type projectsAt struct {
	path  string
	found bool
	err   error
}

func (p projectsAt) Project(context.Context, string, int64) (store.Project, bool, error) {
	return store.Project{Path: p.path}, p.found, p.err
}

func TestSameProject(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		projects ProjectStore
		want     bool
	}{
		"not checked":  {nil, true},
		"same":         {projectsAt{path: "acme/widgets", found: true}, true},
		"other case":   {projectsAt{path: "Acme/Widgets", found: true}, true},
		"another path": {projectsAt{path: "acme/secret", found: true}, false},
		"not stored":   {projectsAt{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := sameProject(ctx, tc.projects, "main", 1, "acme/widgets"); got != tc.want || err != nil {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	boom := errors.New("database is down")
	if _, err := sameProject(ctx, projectsAt{err: boom}, "main", 1, "acme/widgets"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

// withGitHub is a GitHub rig with its handler changed.
func withGitHub(t *testing.T, change func(h *GitHubHandler)) *rig {
	t.Helper()
	r := newGitHubRig(t)
	h, _ := r.mux.Handler(httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", nil))
	change(h.(*GitHubHandler))
	return r
}

// withGitLab is a GitLab rig with its handler changed.
func withGitLab(t *testing.T, change func(h *Handler)) *rig {
	t.Helper()
	r := newRig(t)
	h, _ := r.mux.Handler(httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", nil))
	change(h.(*Handler))
	return r
}

// A payload that names one project's path and another's ID is reviewed under
// neither.
func TestProjectMismatchQueuesNothing(t *testing.T) {
	other := projectsAt{path: "acme/secret", found: true}
	gl := withGitLab(t, func(h *Handler) { h.Projects = other })
	wantStatus(t, gl.hook(fixture(t, "mr_open.json")), 200)
	noQueueCalls(t, gl)
	gh := withGitHub(t, func(h *GitHubHandler) { h.Projects = other })
	wantStatus(t, gh.gh(fixture(t, "github_opened.json")), 200)
	noQueueCalls(t, gh)
}

func TestProjectLookupFails(t *testing.T) {
	down := projectsAt{err: errors.New("database is down")}
	gl := withGitLab(t, func(h *Handler) { h.Projects = down })
	wantStatus(t, gl.hook(fixture(t, "mr_open.json")), http.StatusInternalServerError)
	noQueueCalls(t, gl)
}

// staticRepos has every repository switched on, at acme/widgets.
type staticRepos struct{}

func (staticRepos) Get(context.Context, string, string) (settings.Effective, bool, error) {
	return settings.Effective{Platform: "gitlab", Project: "acme/widgets"}, true, nil
}

type commandSpy struct{ stored int }

func (c *commandSpy) InsertCommand(context.Context, store.Command) (bool, error) {
	c.stored++
	return true, nil
}

func TestProjectMismatchStoresNoCommand(t *testing.T) {
	spy := &commandSpy{}
	d := commands{Store: spy, Repos: staticRepos{},
		Bots: func(string) (string, []string, bool) { return "bot", nil, true }}
	c := comment{Platform: "gitlab", Instance: "main", Project: "acme/widgets", ProjectID: 1, IID: 2, CommentID: 3, Author: "mo", AuthorID: 4, Body: "@bot review"}
	d.Projects = projectsAt{path: "acme/secret", found: true}
	if outcome, reason, err := d.receive(context.Background(), c); err != nil || outcome != outcomeIgnore || reason != "project_id_mismatch" {
		t.Fatalf("got %q, %q, %v", outcome, reason, err)
	}
	if spy.stored != 0 {
		t.Fatal("a command was stored")
	}
	d.Projects = projectsAt{path: "acme/widgets", found: true}
	if _, _, err := d.receive(context.Background(), c); err != nil || spy.stored != 1 {
		t.Fatalf("stored = %d, %v", spy.stored, err)
	}
}

// deliveries is a DeliveryStore in memory.
type deliveries struct {
	seen map[string]bool
	err  error
}

func (d *deliveries) DeliverySeen(_ context.Context, instance string, sum []byte) (bool, error) {
	return d.seen[instance+string(sum)], d.err
}

func (d *deliveries) RecordDelivery(_ context.Context, instance string, sum []byte, _ time.Time) error {
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	d.seen[instance+string(sum)] = true
	return nil
}

// The signature has no time in it: the same bytes count once.
func TestReplayedDeliveryCountsOnce(t *testing.T) {
	d := &deliveries{}
	r := withGitHub(t, func(h *GitHubHandler) { h.Deliveries = d })
	body := fixture(t, "github_opened.json")
	wantStatus(t, r.gh(body), 200)
	wantStatus(t, r.gh(body), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %+v, want 1", r.q.enqueues)
	}
}

// A delivery that failed was not handled, so it can be sent again.
func TestFailedDeliveryCanBeSentAgain(t *testing.T) {
	d := &deliveries{}
	r := withGitHub(t, func(h *GitHubHandler) { h.Deliveries = d })
	body := fixture(t, "github_opened.json")
	r.q.err = errors.New("database is down")
	wantStatus(t, r.gh(body), http.StatusInternalServerError)
	r.q.err = nil
	r.q.enqueues = nil
	wantStatus(t, r.gh(body), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatal("the delivery sent again was ignored")
	}
}

func TestDeliveryLookupFails(t *testing.T) {
	r := withGitHub(t, func(h *GitHubHandler) { h.Deliveries = &deliveries{err: errors.New("down")} })
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), http.StatusInternalServerError)
	noQueueCalls(t, r)
}

// updated_at is inside the signed body, so a delivery too old to be remembered
// can't pass as a new one.
func TestOldDeliveryIsRefused(t *testing.T) {
	body := func(age time.Duration) []byte {
		at := time.Now().Add(-age).UTC().Format(time.RFC3339)
		return bytes.Replace(fixture(t, "github_opened.json"), []byte(`"state": "open",`), []byte(`"state": "open", "updated_at": "`+at+`",`), 1)
	}
	r := newGitHubRig(t)
	wantStatus(t, r.gh(body(deliveryMaxAge+time.Hour)), 200)
	noQueueCalls(t, r)
	wantStatus(t, r.gh(body(time.Hour)), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %+v, want 1", r.q.enqueues)
	}
}
