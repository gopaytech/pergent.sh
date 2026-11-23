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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/callback"
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

func noRetryWait(t *testing.T) {
	t.Helper()
	old := resultRetryWait
	resultRetryWait = 0
	t.Cleanup(func() { resultRetryWait = old })
}

// writeStatus writes status. A 404 carries the reviewers route's no-result
// mark, as the real route answers.
func writeStatus(w http.ResponseWriter, status int) {
	if status == http.StatusNotFound {
		w.Header().Set(callback.NoResultHeader, callback.NoResultValue)
	}
	w.WriteHeader(status)
}

// seqServer serves /runs/r1/reviewers/<n> with the statuses given per
// number, one per try (the last one repeats), and counts the tries.
type seqServer struct {
	mu       sync.Mutex
	statuses map[string][]int
	tries    map[string]int
}

func (ss *seqServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	ss.tries = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		n := strings.TrimPrefix(r.URL.Path, "/runs/r1/reviewers/")
		list := ss.statuses[n]
		if r.Method != http.MethodGet || len(list) == 0 {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		status := list[min(ss.tries[n], len(list)-1)]
		ss.tries[n]++
		writeStatus(w, status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, testMemberResult)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// reviewersOpts asks srv for reviewers nums into a fresh folder.
func reviewersOpts(t *testing.T, srv *httptest.Server, nums ...int) getReviewersOptions {
	t.Helper()
	return getReviewersOptions{baseURL: srv.URL + "/runs/r1/reviewers", reviewers: nums, dir: t.TempDir()}
}

// A failure is tried 3 times; a success on the third try writes the file,
// and 3 failures write none.
func TestGetReviewersRetries(t *testing.T) {
	noRetryWait(t)
	ss := &seqServer{statuses: map[string][]int{
		"1": {http.StatusBadGateway, http.StatusInternalServerError, http.StatusOK},
		"2": {http.StatusServiceUnavailable},
	}}
	o := reviewersOpts(t, ss.start(t), 1, 2)
	if getReviewers(context.Background(), o, "run-token", newResultClient(), io.Discard) {
		t.Error("getReviewers = true with a reviewer that failed 3 times")
	}
	if got, err := os.ReadFile(filepath.Join(o.dir, "reviewer-1.json")); err != nil || string(got) != testMemberResult {
		t.Errorf("reviewer-1.json = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(o.dir, "reviewer-2.json")); !os.IsNotExist(err) {
		t.Errorf("reviewer-2.json is there after 3 failures: %v", err)
	}
	if ss.tries["1"] != resultTries || ss.tries["2"] != resultTries {
		t.Errorf("tries = %v; want %d each", ss.tries, resultTries)
	}
}

// A result over 10 MiB is not written, and not tried again.
func TestGetReviewersTooLargeNotRetried(t *testing.T) {
	noRetryWait(t)
	if maxResultGet != 10<<20 {
		t.Errorf("maxResultGet = %d, want 10 MiB", maxResultGet)
	}
	old := maxResultGet
	maxResultGet = 5
	t.Cleanup(func() { maxResultGet = old })
	ss := &seqServer{statuses: map[string][]int{"1": {http.StatusOK}}}
	o := reviewersOpts(t, ss.start(t), 1)
	getReviewers(context.Background(), o, "run-token", newResultClient(), io.Discard)
	for _, f := range []string{"reviewer-1.json", "reviewer-1.json.tmp"} {
		if _, err := os.Stat(filepath.Join(o.dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", f, err)
		}
	}
	if ss.tries["1"] != 1 {
		t.Errorf("a result over the limit was tried %d times", ss.tries["1"])
	}
}

// A download cut off in the body (Content-Length larger than what is sent)
// leaves no file and no temp file, after every try.
func TestGetReviewersCutOff(t *testing.T) {
	noRetryWait(t)
	var tries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"skills":`)
	}))
	t.Cleanup(srv.Close)
	o := reviewersOpts(t, srv, 1, 2)
	if getReviewers(context.Background(), o, "run-token", newResultClient(), io.Discard) {
		t.Error("getReviewers = true for cut off results")
	}
	if entries, _ := os.ReadDir(o.dir); len(entries) != 0 {
		t.Errorf("files left: %v", entries)
	}
	if n := tries.Load(); n != 2*resultTries {
		t.Errorf("tries = %d; want %d per reviewer", n, resultTries)
	}
}

// A redirect to another host is not followed, is not tried again and writes
// no file.
func TestGetReviewersNoRedirect(t *testing.T) {
	noRetryWait(t)
	var other atomic.Bool
	var tries atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Store(true)
		_, _ = io.WriteString(w, testMemberResult)
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	o := reviewersOpts(t, srv, 1, 2)
	if getReviewers(context.Background(), o, "run-token", newResultClient(), io.Discard) {
		t.Error("getReviewers = true for a redirect")
	}
	if other.Load() {
		t.Error("the redirect was followed")
	}
	if n := tries.Load(); n != 2 {
		t.Errorf("tries = %d; want 1 per reviewer (a redirect is final)", n)
	}
	if entries, _ := os.ReadDir(o.dir); len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
}

// A 401 or a 403 is final, as a 404 is: tried once, no file.
func TestGetReviewersUnauthorizedNotRetried(t *testing.T) {
	noRetryWait(t)
	ss := &seqServer{statuses: map[string][]int{"1": {http.StatusUnauthorized, http.StatusOK}, "2": {http.StatusForbidden, http.StatusOK}}}
	o := reviewersOpts(t, ss.start(t), 1, 2)
	if getReviewers(context.Background(), o, "run-token", newResultClient(), io.Discard) {
		t.Error("getReviewers = true for a 401 and a 403")
	}
	if ss.tries["1"] != 1 || ss.tries["2"] != 1 {
		t.Errorf("tries = %v; want 1 each", ss.tries)
	}
	if entries, _ := os.ReadDir(o.dir); len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
}

// A hanging reviewer does not use up another reviewer's time: the other gets
// its tries and its file while the first waits for the budget to end.
func TestGetReviewersSlowOneDoesNotStopOthers(t *testing.T) {
	noRetryWait(t)
	var tries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/1") {
			<-r.Context().Done()
			return
		}
		if tries.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, testMemberResult)
	}))
	t.Cleanup(srv.Close)
	o := reviewersOpts(t, srv, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var stderr bytes.Buffer
	if getReviewers(ctx, o, "run-token", newResultClient(), &stderr) {
		t.Error("getReviewers = true with a reviewer that timed out")
	}
	if got, err := os.ReadFile(filepath.Join(o.dir, "reviewer-2.json")); err != nil || string(got) != testMemberResult {
		t.Errorf("reviewer-2.json = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(o.dir, "reviewer-1.json")); !os.IsNotExist(err) {
		t.Errorf("reviewer-1.json is there: %v", err)
	}
	if !strings.Contains(stderr.String(), "reviewer 1: not downloaded") || !strings.Contains(stderr.String(), "reviewer 2: downloaded") {
		t.Errorf("log = %q", stderr.String())
	}
}

// reviewerServer serves /runs/r1/reviewers/<n> with the status given per
// number (200 when none), and counts the tries.
type reviewerServer struct {
	mu       sync.Mutex
	statuses map[string]int
	tries    map[string]int
	auth     string
}

func (rs *reviewerServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	rs.tries = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		n := strings.TrimPrefix(r.URL.Path, "/runs/r1/reviewers/")
		if r.Method != http.MethodGet || n == r.URL.Path {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		rs.auth = r.Header.Get("Authorization")
		rs.tries[n]++
		status := http.StatusOK
		if s, ok := rs.statuses[n]; ok {
			status = s
		}
		writeStatus(w, status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"reviewer":`+n+`}`+"\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The results land as reviewer-<n>.json by number; a 404 writes no file; the
// command exits 0.
func TestGetReviewersWritesByNumber(t *testing.T) {
	noRetryWait(t)
	rs := &reviewerServer{statuses: map[string]int{"2": http.StatusNotFound}}
	srv := rs.start(t)
	dir := t.TempDir()
	o, err := parseGetReviewersArgs([]string{"--base-url=" + srv.URL + "/runs/r1/reviewers/", "--reviewers=1,2,3", "--dir=" + dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PERGENT_RESULT_TOKEN", "run-token")
	var stderr bytes.Buffer
	if code := runGetReviewers(context.Background(), o, &stderr); code != 0 {
		t.Errorf("exit = %d", code)
	}
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"reviewer-1.json", "reviewer-3.json"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	for n, want := range map[string]string{"1": `{"reviewer":1}` + "\n", "3": `{"reviewer":3}` + "\n"} {
		b, err := os.ReadFile(filepath.Join(dir, "reviewer-"+n+".json"))
		if err != nil || string(b) != want {
			t.Errorf("reviewer-%s.json = %q, %v", n, b, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, "reviewer-"+n+".json")); fi.Mode().Perm() != 0o644 {
			t.Errorf("reviewer-%s.json mode = %v", n, fi.Mode().Perm())
		}
	}
	if rs.tries["2"] != 1 {
		t.Errorf("a 404 was tried %d times", rs.tries["2"])
	}
	if rs.auth != "Bearer run-token" {
		t.Errorf("Authorization = %q", rs.auth)
	}
	if strings.Contains(stderr.String(), "run-token") {
		t.Error("the token is in the log")
	}
}

// Only numbers 1 to 5 are accepted, and the other flags are needed.
func TestGetReviewersArgs(t *testing.T) {
	cmd, err := parseArgs([]string{"get-reviewers", "--base-url=http://h:8080/runs/r/reviewers", "--reviewers=1,2,5", "--dir=/panel"}, io.Discard)
	if err != nil || cmd.name != "get-reviewers" || cmd.getReviewers.dir != "/panel" || !slices.Equal(cmd.getReviewers.reviewers, []int{1, 2, 5}) {
		t.Errorf("get-reviewers = %+v, %v", cmd, err)
	}
	for _, args := range [][]string{
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=6", "--dir=/panel"},
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=0", "--dir=/panel"},
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=1,1", "--dir=/panel"},
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=a", "--dir=/panel"},
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=", "--dir=/panel"},
		{"get-reviewers", "--base-url=ftp://h/r", "--reviewers=1", "--dir=/panel"},
		{"get-reviewers", "--reviewers=1", "--dir=/panel"},
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=1"},
		{"get-reviewers", "--base-url=http://h/r", "--reviewers=1", "--dir=/panel", "extra"},
	} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("%q parsed", args)
		}
	}
}

// run is get-reviewers against one server answer per reviewer number.
func runReviewers(t *testing.T, handler http.HandlerFunc, nums []int, token string) (code int, dir, stderr string) {
	t.Helper()
	noRetryWait(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	o := getReviewersOptions{baseURL: srv.URL + "/runs/r1/reviewers", reviewers: nums, dir: dir}
	t.Setenv("PERGENT_RESULT_TOKEN", token)
	var buf bytes.Buffer
	code = runGetReviewers(context.Background(), o, &buf)
	return code, dir, buf.String()
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	return got
}

// Only a marked 404 is "no result". Every other failure, a 404 with no mark
// too, exits 1, writes no file for that reviewer, and never prints the token.
func TestGetReviewersFailures(t *testing.T) {
	const token = "secret-run-token"
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { writeStatus(w, code) }
	}
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		token    string
		wantCode int
	}{
		{"404", status(http.StatusNotFound), token, 0},
		// An old pergent.sh with no route, or a proxy: Go's own 404.
		{"404 with no mark", http.NotFound, token, 1},
		{"404 with another mark", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(callback.NoResultHeader, "maybe")
			w.WriteHeader(http.StatusNotFound)
		}, token, 1},
		{"500", status(http.StatusInternalServerError), token, 1},
		{"401", status(http.StatusUnauthorized), token, 1},
		{"403", status(http.StatusForbidden), token, 1},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		}, token, 1},
		{"too large", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		}, token, 1},
		{"empty", status(http.StatusOK), token, 1},
		{"no token", status(http.StatusOK), "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := maxResultGet
			maxResultGet = 10
			t.Cleanup(func() { maxResultGet = old })
			code, dir, stderr := runReviewers(t, tc.handler, []int{1}, tc.token)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d; stderr: %s", code, tc.wantCode, stderr)
			}
			if got := filesIn(t, dir); len(got) != 0 {
				t.Errorf("files written: %v", got)
			}
			if strings.Contains(stderr, token) {
				t.Error("the token is in stderr")
			}
			if tc.wantCode != 0 && stderr == "" {
				t.Error("a failure printed nothing")
			}
		})
	}
}

// A server that does not answer in time fails the command.
func TestGetReviewersTimeout(t *testing.T) {
	old := getReviewersTimeout
	getReviewersTimeout = 100 * time.Millisecond
	t.Cleanup(func() { getReviewersTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	code, dir, stderr := runReviewers(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, []int{1}, "secret-run-token")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if got := filesIn(t, dir); len(got) != 0 {
		t.Errorf("files written: %v", got)
	}
	if strings.Contains(stderr, "secret-run-token") {
		t.Error("the token is in stderr")
	}
}

// A 500 retries 3 times, then fails.
func TestGetReviewersServerErrorIsTried(t *testing.T) {
	rs := &reviewerServer{statuses: map[string]int{"1": http.StatusInternalServerError}}
	srv := rs.start(t)
	noRetryWait(t)
	t.Setenv("PERGENT_RESULT_TOKEN", "tok")
	o := getReviewersOptions{baseURL: srv.URL + "/runs/r1/reviewers", reviewers: []int{1}, dir: t.TempDir()}
	if code := runGetReviewers(context.Background(), o, io.Discard); code != 1 {
		t.Errorf("exit = %d", code)
	}
	if rs.tries["1"] != resultTries {
		t.Errorf("tries = %d, want %d", rs.tries["1"], resultTries)
	}
}

// The mixed-version case: reviewer 2 reaches a pergent.sh with no reviewers
// route and gets Go's plain 404 on every try. That is not "no result", so
// the command exits 1 and the judge never runs on reviewers 1 and 3 alone.
// The 404 is tried again, since another try may reach a new pod.
func TestGetReviewersUnmarkedNotFoundFails(t *testing.T) {
	noRetryWait(t)
	var tries atomic.Int32
	code, dir, stderr := runReviewers(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/2") {
			tries.Add(1)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, testMemberResult)
	}, []int{1, 2, 3}, "tok")
	if code != 1 {
		t.Errorf("exit = %d, want 1; stderr: %s", code, stderr)
	}
	if n := tries.Load(); n != resultTries {
		t.Errorf("tries = %d, want %d", n, resultTries)
	}
	if !strings.Contains(stderr, "reviewer 2: not downloaded: pergent.sh answered 404 with no no-result mark") {
		t.Errorf("stderr = %q", stderr)
	}
	if got := filesIn(t, dir); slices.Contains(got, "reviewer-2.json") {
		t.Errorf("files = %v", got)
	}
}

// 200 and 404 exits 0 with one file. 200, 404 and 500 exits 1, and the files
// of the reviewers that did answer stay (the Job fails anyway).
func TestGetReviewersMixed(t *testing.T) {
	handler := func(statuses map[string]int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			n := strings.TrimPrefix(r.URL.Path, "/runs/r1/reviewers/")
			code := http.StatusOK
			if c, ok := statuses[n]; ok {
				code = c
			}
			writeStatus(w, code)
			if code == http.StatusOK {
				_, _ = io.WriteString(w, `{"reviewer":`+n+`}`)
			}
		}
	}
	code, dir, _ := runReviewers(t, handler(map[string]int{"2": 404}), []int{1, 2}, "tok")
	if code != 0 || !slices.Equal(filesIn(t, dir), []string{"reviewer-1.json"}) {
		t.Errorf("200+404: exit = %d, files = %v", code, filesIn(t, dir))
	}
	code, _, _ = runReviewers(t, handler(map[string]int{"2": 404, "3": 500}), []int{1, 2, 3}, "tok")
	if code != 1 {
		t.Errorf("200+404+500: exit = %d, want 1", code)
	}
}
