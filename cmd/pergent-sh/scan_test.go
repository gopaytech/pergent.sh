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

	"github.com/gopaytech/pergent.sh/internal/readiness"
)

// scanReceiver is the result callback of a scan: it keeps what was posted.
type scanReceiver struct {
	mu     sync.Mutex
	auth   string
	ctype  string
	sizes  []int
	posted []readiness.JobResult
	status int
}

func (rc *scanReceiver) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/readiness-scans/s1/result" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		rc.auth, rc.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		rc.sizes = append(rc.sizes, len(raw))
		var res readiness.JobResult
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&res); err != nil {
			t.Errorf("body: %v", err)
		}
		rc.posted = append(rc.posted, res)
		status := rc.status
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scanWork makes the /work and /in of a scan Job: a small Go repository in
// repo, the commit, and the input.
func scanWork(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "commit"), cloneCommit+"\n")
	writeFile(t, filepath.Join(dir, "input"), `{"version":1,"library":[{"name":"go","paths":["**/*.go"],"assigned":true}]}`)
	writeFile(t, filepath.Join(dir, "repo", "README.md"), "# widgets\n")
	writeFile(t, filepath.Join(dir, "repo", "AGENTS.md"), "Run make test.\n")
	writeFile(t, filepath.Join(dir, "repo", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "repo", "util", "util.go"), "package util\n")
	writeFile(t, filepath.Join(dir, "repo", "api", "api.pb.go"), "package api\n")
	return dir
}

func scanOpts(dir, resultURL string) scanOptions {
	return scanOptions{
		dir: filepath.Join(dir, "repo"), commitFile: filepath.Join(dir, "commit"),
		errorFile: filepath.Join(dir, "error"), inputFile: filepath.Join(dir, "input"),
		resultURL: resultURL,
	}
}

// The scan of the clone is posted with the commit from the file, version 1 and
// the scan token as bearer, in a body under MaxResultBytes.
func TestScanRepoPostsResult(t *testing.T) {
	dir := scanWork(t)
	rc := &scanReceiver{}
	srv := rc.server(t)

	if err := scanRepo(context.Background(), scanOpts(dir, srv.URL+"/readiness-scans/s1/result"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rc.posted) != 1 {
		t.Fatalf("posted %d times", len(rc.posted))
	}
	if rc.auth != "Bearer tok-1" || rc.ctype != "application/json" {
		t.Errorf("headers = %q, %q", rc.auth, rc.ctype)
	}
	if rc.sizes[0] > readiness.MaxResultBytes {
		t.Errorf("body is %d bytes, over %d", rc.sizes[0], readiness.MaxResultBytes)
	}
	got := rc.posted[0]
	if got.Version != 1 || got.Commit != cloneCommit || got.Error != "" {
		t.Errorf("posted = %+v", got)
	}
	if got.Files == 0 || got.SourceFiles == 0 || got.Context.AgentFile != "AGENTS.md" || got.Context.Readme != "README.md" {
		t.Errorf("the tree was not scanned: %+v", got)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "go" || got.Skills[0].Files != 3 {
		t.Errorf("skills = %+v; want go over 3 files", got.Skills)
	}

	// A refused result is an error, so the container fails.
	rc.status = http.StatusConflict
	if err := scanRepo(context.Background(), scanOpts(dir, srv.URL+"/readiness-scans/s1/result"), "tok-1", srv.Client(), io.Discard); err == nil {
		t.Error("scanRepo = nil after a 409; want an error")
	}
}

// When the clone failed, its error is posted and the tree is not read: here
// there is no tree at all, and no input.
func TestScanRepoPostsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "error"), "the default branch moved during the scan; scan again\n")
	rc := &scanReceiver{}
	srv := rc.server(t)

	if err := scanRepo(context.Background(), scanOpts(dir, srv.URL+"/readiness-scans/s1/result"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	want := readiness.JobResult{Version: 1, Error: "the default branch moved during the scan; scan again"}
	if len(rc.posted) != 1 || rc.posted[0].Version != want.Version || rc.posted[0].Error != want.Error ||
		rc.posted[0].Commit != "" || rc.posted[0].Files != 0 {
		t.Errorf("posted = %+v; want %+v", rc.posted, want)
	}

	// No error and no commit: the clone never finished.
	dir = t.TempDir()
	rc.posted = nil
	if err := scanRepo(context.Background(), scanOpts(dir, srv.URL+"/readiness-scans/s1/result"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rc.posted) != 1 || rc.posted[0].Error != msgScanCloneUnknown || rc.posted[0].Version != 1 {
		t.Errorf("posted = %+v; want the unfinished-clone error", rc.posted)
	}
}

// An input that is not JSON, or is missing, posts an error and reads no tree.
func TestScanRepoBadInput(t *testing.T) {
	for name, input := range map[string]*string{
		"not JSON":      ptrTo("not json"),
		"trailing data": ptrTo(`{"version":1} {}`),
		"unknown field": ptrTo(`{"version":1,"surprise":true}`),
		"missing":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			dir := scanWork(t)
			if input == nil {
				if err := os.Remove(filepath.Join(dir, "input")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, filepath.Join(dir, "input"), *input)
			}
			rc := &scanReceiver{}
			srv := rc.server(t)
			if err := scanRepo(context.Background(), scanOpts(dir, srv.URL+"/readiness-scans/s1/result"), "tok-1", srv.Client(), io.Discard); err != nil {
				t.Fatal(err)
			}
			if len(rc.posted) != 1 || rc.posted[0].Error != "the scan input couldn't be read" || rc.posted[0].Version != 1 ||
				rc.posted[0].Files != 0 {
				t.Errorf("posted = %+v", rc.posted)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

// A tree that can't be scanned posts an error that says where to look.
func TestScanRepoUnreadableTree(t *testing.T) {
	dir := scanWork(t)
	if err := os.RemoveAll(filepath.Join(dir, "repo")); err != nil {
		t.Fatal(err)
	}
	rc := &scanReceiver{}
	srv := rc.server(t)
	var stderr strings.Builder
	if err := scanRepo(context.Background(), scanOpts(dir, srv.URL+"/readiness-scans/s1/result"), "tok-1", srv.Client(), &stderr); err != nil {
		t.Fatal(err)
	}
	if len(rc.posted) != 1 || rc.posted[0].Error != msgScanRead || rc.posted[0].Commit != "" {
		t.Errorf("posted = %+v", rc.posted)
	}
	if !strings.Contains(stderr.String(), "scanning the clone") {
		t.Errorf("stderr = %q; the cause belongs in the log", stderr.String())
	}
}

// A result over MaxResultBytes is posted as an error instead.
func TestScanResultBodyTooLarge(t *testing.T) {
	small, err := scanResultBody(readiness.JobResult{Version: 1, Commit: cloneCommit, Files: 3})
	if err != nil || len(small) > readiness.MaxResultBytes {
		t.Fatalf("small = %d bytes, %v", len(small), err)
	}
	huge := readiness.JobResult{Version: 1, Commit: cloneCommit}
	for range 20000 {
		huge.Skills = append(huge.Skills, readiness.SkillFit{Name: strings.Repeat("n", 64), Problem: strings.Repeat("p", 64)})
	}
	body, err := scanResultBody(huge)
	if err != nil {
		t.Fatal(err)
	}
	var got readiness.JobResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != msgScanTooLarge || got.Version != 1 || len(got.Skills) != 0 || len(body) > readiness.MaxResultBytes {
		t.Errorf("body = %d bytes, %+v", len(body), got)
	}
}

// The command needs its token; the input is a file, never the environment.
func TestRunScanRepoNeedsToken(t *testing.T) {
	t.Setenv("PERGENT_SH_SCAN_TOKEN", "")
	var stderr strings.Builder
	if code := runScanRepo(context.Background(), scanOptions{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "PERGENT_SH_SCAN_TOKEN") {
		t.Errorf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestParseScanRepoArgs(t *testing.T) {
	cmd, err := parseArgs([]string{"scan-repo", "--dir=/work/repo", "--commit-file=/work/commit", "--error-file=/work/error",
		"--input-file=/in/scan-input", "--result-url=http://pergent-sh:8080/readiness-scans/s1/result"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := scanOptions{dir: "/work/repo", commitFile: "/work/commit", errorFile: "/work/error", inputFile: "/in/scan-input",
		resultURL: "http://pergent-sh:8080/readiness-scans/s1/result"}
	if cmd.name != "scan-repo" || cmd.scan != want {
		t.Errorf("cmd = %+v", cmd)
	}
	for _, bad := range [][]string{
		{"scan-repo", "--dir=/work/repo"},
		{"scan-repo", "--dir=d", "--commit-file=c", "--error-file=e", "--input-file=i"},
		{"scan-repo", "--dir=d", "--commit-file=c", "--error-file=e", "--result-url=http://x/y"},
		{"scan-repo", "--dir=d", "--commit-file=c", "--error-file=e", "--input-file=i", "--result-url=ftp://x/y"},
		{"scan-repo", "--dir=d", "--commit-file=c", "--error-file=e", "--input-file=i", "--result-url=http://x/y", "extra"},
	} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) = nil; want an error", bad)
		}
	}
}
