package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/codegraph"
)

const testGraph = `{"format":"pergent-graph/v1","provider":"graphify","provider_version":"0.9.112",` +
	`"commit":"0123456789abcdef0123456789abcdef01234567","built_at":"2026-11-20T10:00:00Z","nodes":[],"edges":[]}` + "\n"

// graphReceiver keeps what a post-graph posted.
type graphReceiver struct {
	mu     sync.Mutex
	auth   string
	bodies [][]byte
	status int
}

func (rc *graphReceiver) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/graphs/b1/result" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		rc.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		rc.bodies = append(rc.bodies, raw)
		status := rc.status
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// graphDirs makes a build Job's /work and /out, with the files named.
func graphDirs(t *testing.T, work, out map[string]string) (string, string) {
	t.Helper()
	w, o := t.TempDir(), t.TempDir()
	for dir, files := range map[string]map[string]string{w: work, o: out} {
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return w, o
}

func postedError(t *testing.T, body []byte) string {
	t.Helper()
	var e codegraph.ErrorBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("posted %q: %v", body, err)
	}
	return e.Error
}

// post-graph posts the graph file as it is, or the first error: the clone's,
// a clone that wrote nothing, graphify's, then no graph at all.
func TestPostGraph(t *testing.T) {
	for _, tc := range []struct {
		name      string
		work, out map[string]string
		graph     bool
		wantErr   string
	}{
		{name: "graph", work: map[string]string{"commit": "abc\n"}, out: map[string]string{"graph.json": testGraph}, graph: true},
		{name: "clone failed", work: map[string]string{"error": codegraph.MsgCloneFetch + "\n"}, out: map[string]string{"graph.json": testGraph},
			wantErr: codegraph.MsgCloneFetch},
		{name: "clone wrote nothing", out: map[string]string{"graph.json": testGraph}, wantErr: codegraph.MsgCloneUnknown},
		{name: "graphify failed", work: map[string]string{"commit": "abc"}, out: map[string]string{"error": codegraph.MsgGraphify + "\n"},
			wantErr: codegraph.MsgGraphify},
		{name: "no graph", work: map[string]string{"commit": "abc"}, wantErr: codegraph.MsgNoGraph},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &graphReceiver{}
			srv := rc.server(t)
			w, o := graphDirs(t, tc.work, tc.out)
			var stderr bytes.Buffer
			opts := postGraphOptions{work: w, out: o, resultURL: srv.URL + "/graphs/b1/result"}
			if err := postGraph(context.Background(), opts, "tok", newResultClient(), &stderr); err != nil {
				t.Fatalf("postGraph: %v (%s)", err, stderr.String())
			}
			if rc.auth != "Bearer tok" || len(rc.bodies) != 1 {
				t.Fatalf("auth %q, %d posts", rc.auth, len(rc.bodies))
			}
			if tc.graph {
				if string(rc.bodies[0]) != testGraph {
					t.Errorf("posted %q; want the file as it is", rc.bodies[0])
				}
				return
			}
			if got := postedError(t, rc.bodies[0]); got != tc.wantErr {
				t.Errorf("error = %q; want %q", got, tc.wantErr)
			}
		})
	}
}

// A graph over the limit is never sent: its error is.
func TestPostGraphTooLarge(t *testing.T) {
	old := maxGraphPost
	maxGraphPost = 10
	t.Cleanup(func() { maxGraphPost = old })
	rc := &graphReceiver{}
	srv := rc.server(t)
	w, o := graphDirs(t, map[string]string{"commit": "abc"}, map[string]string{"graph.json": testGraph})
	opts := postGraphOptions{work: w, out: o, resultURL: srv.URL + "/graphs/b1/result"}
	if err := postGraph(context.Background(), opts, "tok", newResultClient(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rc.bodies) != 1 || postedError(t, rc.bodies[0]) != codegraph.MsgTooLarge {
		t.Fatalf("posted %q", rc.bodies)
	}
}

// A refused post is the command's error, so the Job fails.
func TestPostGraphRefused(t *testing.T) {
	rc := &graphReceiver{status: http.StatusConflict}
	srv := rc.server(t)
	w, o := graphDirs(t, map[string]string{"commit": "abc"}, map[string]string{"graph.json": testGraph})
	opts := postGraphOptions{work: w, out: o, resultURL: srv.URL + "/graphs/b1/result"}
	if err := postGraph(context.Background(), opts, "tok", newResultClient(), io.Discard); err == nil {
		t.Fatal("no error for a 409")
	}
}

func TestParsePostGraphArgs(t *testing.T) {
	if _, err := parsePostGraphArgs([]string{"--work=/work", "--out=/out"}, io.Discard); err == nil {
		t.Error("no error without --result-url")
	}
	if _, err := parsePostGraphArgs([]string{"--work=/work", "--out=/out", "--result-url=file:///x"}, io.Discard); err == nil {
		t.Error("no error for a file URL")
	}
	o, err := parsePostGraphArgs([]string{"--work=/w", "--out=/o", "--result-url=http://h/graphs/b/result"}, io.Discard)
	if err != nil || o.work != "/w" || o.out != "/o" {
		t.Errorf("parse = %+v, %v", o, err)
	}
}

// get-graph writes the graph it got, with the run token as bearer, and only a
// whole one; anything else leaves no file. It never fails the Job.
func TestGetGraph(t *testing.T) {
	var auth string
	status, body := http.StatusOK, testGraph
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/runs/r1/graph" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	out := filepath.Join(t.TempDir(), "graph.json")
	opts := getGraphOptions{url: srv.URL + "/runs/r1/graph", out: out}

	var stderr bytes.Buffer
	getGraph(context.Background(), opts, "run-token", newResultClient(), &stderr)
	got, err := os.ReadFile(out)
	if err != nil || string(got) != testGraph || auth != "Bearer run-token" {
		t.Fatalf("file = %q, %v; auth %q", got, err, auth)
	}
	if strings.Contains(stderr.String(), "run-token") {
		t.Error("the token is in the log")
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		limit  int64
	}{
		{"not found", http.StatusNotFound, "not found", 0},
		{"server error", http.StatusInternalServerError, "", 0},
		{"too large", http.StatusOK, testGraph, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(out)
			status, body = tc.status, tc.body
			if tc.limit > 0 {
				old := maxGraphGet
				maxGraphGet = tc.limit
				t.Cleanup(func() { maxGraphGet = old })
			}
			getGraph(context.Background(), opts, "run-token", newResultClient(), io.Discard)
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("a file is there: %v", err)
			}
			if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
				t.Errorf("the temp file is there: %v", err)
			}
		})
	}
}

// With no token, or a server that can't be reached, get-graph still exits 0.
func TestRunGetGraphNeverFails(t *testing.T) {
	t.Setenv("PERGENT_RESULT_TOKEN", "")
	out := filepath.Join(t.TempDir(), "graph.json")
	if code := runGetGraph(context.Background(), getGraphOptions{url: "http://127.0.0.1:1/runs/r/graph", out: out}, io.Discard); code != 0 {
		t.Errorf("exit = %d with no token", code)
	}
	t.Setenv("PERGENT_RESULT_TOKEN", "tok")
	if code := runGetGraph(context.Background(), getGraphOptions{url: "http://127.0.0.1:1/runs/r/graph", out: out}, io.Discard); code != 0 {
		t.Errorf("exit = %d with no server", code)
	}
}

// The two Job commands parse from the command line as the Jobs call them.
func TestParseGraphCommands(t *testing.T) {
	cmd, err := parseArgs([]string{"post-graph", "--work=/work", "--out=/out", "--result-url=http://h:8080/graphs/b/result"}, io.Discard)
	if err != nil || cmd.name != "post-graph" || cmd.postGraph.out != "/out" {
		t.Errorf("post-graph = %+v, %v", cmd, err)
	}
	cmd, err = parseArgs([]string{"get-graph", "--url=http://h:8080/runs/r/graph", "--out=/graph/graph.json"}, io.Discard)
	if err != nil || cmd.name != "get-graph" || cmd.getGraph.out != "/graph/graph.json" {
		t.Errorf("get-graph = %+v, %v", cmd, err)
	}
	if _, err := parseArgs([]string{"get-graph", "--url=ftp://h/x", "--out=/g"}, io.Discard); err == nil {
		t.Error("get-graph took an ftp URL")
	}
}

const testMemberResult = `{"skills":{}}` + "\n"

// panelServer serves /runs/r1/panel/a and /b with the statuses given per
// role, one per try (the last one repeats), and counts the tries.
type panelServer struct {
	mu       sync.Mutex
	statuses map[string][]int
	tries    map[string]int
	auth     string
}

func (ps *panelServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ps.tries = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		role := strings.TrimPrefix(r.URL.Path, "/runs/r1/panel/")
		if r.Method != http.MethodGet || (role != "a" && role != "b") {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		ps.auth = r.Header.Get("Authorization")
		list := ps.statuses[role]
		status := list[min(ps.tries[role], len(list)-1)]
		ps.tries[role]++
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, testMemberResult)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func panelOpts(t *testing.T, srv *httptest.Server) getPanelOptions {
	t.Helper()
	return getPanelOptions{aURL: srv.URL + "/runs/r1/panel/a", bURL: srv.URL + "/runs/r1/panel/b", dir: t.TempDir()}
}

func noRetryWait(t *testing.T) {
	t.Helper()
	old := panelRetryWait
	panelRetryWait = 0
	t.Cleanup(func() { panelRetryWait = old })
}

// A member with no result (404) gets no file, at once, and get-panel still
// exits 0; the other member's file is written.
func TestGetPanelNotFoundWritesNothing(t *testing.T) {
	noRetryWait(t)
	ps := &panelServer{statuses: map[string][]int{"a": {http.StatusOK}, "b": {http.StatusNotFound}}}
	o := panelOpts(t, ps.start(t))
	t.Setenv("PERGENT_RESULT_TOKEN", "run-token")
	var stderr bytes.Buffer
	if code := runGetPanel(context.Background(), o, &stderr); code != 0 {
		t.Errorf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(o.dir, "b.json")); !os.IsNotExist(err) {
		t.Errorf("b.json is there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.dir, "b.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("the temp file is there: %v", err)
	}
	if ps.tries["b"] != 1 {
		t.Errorf("a 404 was tried %d times", ps.tries["b"])
	}
	got, err := os.ReadFile(filepath.Join(o.dir, "a.json"))
	if err != nil || string(got) != testMemberResult {
		t.Errorf("a.json = %q, %v", got, err)
	}
	if strings.Contains(stderr.String(), "run-token") {
		t.Error("the token is in the log")
	}
}

// get-panel writes each result whole, mode 0600, with the run token as bearer.
func TestGetPanel(t *testing.T) {
	noRetryWait(t)
	ps := &panelServer{statuses: map[string][]int{"a": {http.StatusOK}, "b": {http.StatusOK}}}
	o := panelOpts(t, ps.start(t))
	getPanel(context.Background(), o, "run-token", newResultClient(), io.Discard)
	for _, f := range []string{"a.json", "b.json"} {
		path := filepath.Join(o.dir, f)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != testMemberResult {
			t.Errorf("%s = %q, %v", f, got, err)
			continue
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 0600", f, st.Mode().Perm(), err)
		}
	}
	if ps.auth != "Bearer run-token" {
		t.Errorf("auth = %q", ps.auth)
	}
}

// A failure is tried 3 times; a success on the third try writes the file,
// and 3 failures write none.
func TestGetPanelRetries(t *testing.T) {
	noRetryWait(t)
	ps := &panelServer{statuses: map[string][]int{
		"a": {http.StatusBadGateway, http.StatusInternalServerError, http.StatusOK},
		"b": {http.StatusServiceUnavailable},
	}}
	o := panelOpts(t, ps.start(t))
	getPanel(context.Background(), o, "run-token", newResultClient(), io.Discard)
	if got, err := os.ReadFile(filepath.Join(o.dir, "a.json")); err != nil || string(got) != testMemberResult {
		t.Errorf("a.json = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(o.dir, "b.json")); !os.IsNotExist(err) {
		t.Errorf("b.json is there after 3 failures: %v", err)
	}
	if ps.tries["a"] != 3 || ps.tries["b"] != 3 {
		t.Errorf("tries = %v; want 3 each", ps.tries)
	}
}

// A result over 10 MiB is not written, and not tried again.
func TestGetPanelTooLarge(t *testing.T) {
	noRetryWait(t)
	if maxPanelGet != 10<<20 {
		t.Errorf("maxPanelGet = %d, want 10 MiB", maxPanelGet)
	}
	old := maxPanelGet
	maxPanelGet = 5
	t.Cleanup(func() { maxPanelGet = old })
	ps := &panelServer{statuses: map[string][]int{"a": {http.StatusOK}, "b": {http.StatusOK}}}
	o := panelOpts(t, ps.start(t))
	getPanel(context.Background(), o, "run-token", newResultClient(), io.Discard)
	for _, f := range []string{"a.json", "b.json", "a.json.tmp"} {
		if _, err := os.Stat(filepath.Join(o.dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", f, err)
		}
	}
	if ps.tries["a"] != 1 {
		t.Errorf("a result over the limit was tried %d times", ps.tries["a"])
	}
}

// With no token, or no server, get-panel still exits 0.
func TestRunGetPanelNeverFails(t *testing.T) {
	noRetryWait(t)
	o := getPanelOptions{aURL: "http://127.0.0.1:1/runs/r/panel/a", bURL: "http://127.0.0.1:1/runs/r/panel/b", dir: t.TempDir()}
	t.Setenv("PERGENT_RESULT_TOKEN", "")
	if code := runGetPanel(context.Background(), o, io.Discard); code != 0 {
		t.Errorf("exit = %d with no token", code)
	}
	t.Setenv("PERGENT_RESULT_TOKEN", "tok")
	if code := runGetPanel(context.Background(), o, io.Discard); code != 0 {
		t.Errorf("exit = %d with no server", code)
	}
	if entries, _ := os.ReadDir(o.dir); len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
}

// get-panel parses as Job P's panel-inputs container calls it.
func TestParseGetPanel(t *testing.T) {
	cmd, err := parseArgs([]string{"get-panel", "--a-url=http://h:8080/runs/r/panel/a", "--b-url=http://h:8080/runs/r/panel/b", "--dir=/panel"}, io.Discard)
	if err != nil || cmd.name != "get-panel" || cmd.getPanel.dir != "/panel" || cmd.getPanel.bURL != "http://h:8080/runs/r/panel/b" {
		t.Errorf("get-panel = %+v, %v", cmd, err)
	}
	for _, args := range [][]string{
		{"get-panel", "--a-url=ftp://h/a", "--b-url=http://h/b", "--dir=/panel"},
		{"get-panel", "--a-url=http://h/a", "--dir=/panel"},
		{"get-panel", "--a-url=http://h/a", "--b-url=http://h/b"},
		{"get-panel", "--a-url=http://h/a", "--b-url=http://h/b", "--dir=/panel", "extra"},
	} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("%q parsed", args)
		}
	}
}
