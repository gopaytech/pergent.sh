package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const cloneCommit = "0123456789abcdef0123456789abcdef01234567"

// receiver is the result callback of a fetch: it keeps what was posted.
type receiver struct {
	mu     sync.Mutex
	auth   string
	ctype  string
	posted []skillfetch.JobResult
	status int
}

func (rc *receiver) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/skill-fetches/f1/result" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		rc.auth, rc.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		var res skillfetch.JobResult
		dec := json.NewDecoder(r.Body)
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// work makes the /work of a fetch Job: the clone in repo, and commit.
func work(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "commit"), cloneCommit+"\n")
	writeFile(t, filepath.Join(dir, "repo", "README.md"), "not a skill")
	writeFile(t, filepath.Join(dir, "repo", "skills", "style.md"), "---\nname: style\n---\nBe kind.\n")
	writeFile(t, filepath.Join(dir, "repo", "skills", "review", "SKILL.md"), "Review carefully.\n")
	writeFile(t, filepath.Join(dir, "repo", "skills", "broken.md"), "---\npaths: 7\n---\n")
	return dir
}

func fetchOpts(dir, resultURL, folder string) fetchOptions {
	return fetchOptions{
		dir: filepath.Join(dir, "repo"), path: folder,
		commitFile: filepath.Join(dir, "commit"), errorFile: filepath.Join(dir, "error"),
		resultURL: resultURL,
	}
}

func byPath(files []store.FetchedFile) map[string]store.FetchedFile {
	out := map[string]store.FetchedFile{}
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

// The skills of the folder in the clone are posted with the commit, with the
// fetch token as bearer.
func TestFetchSkillsPostsFiles(t *testing.T) {
	dir := work(t)
	rc := &receiver{}
	srv := rc.server(t)

	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "skills"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rc.posted) != 1 {
		t.Fatalf("posted %d times", len(rc.posted))
	}
	if rc.auth != "Bearer tok-1" || rc.ctype != "application/json" {
		t.Errorf("headers = %q, %q", rc.auth, rc.ctype)
	}
	got := rc.posted[0]
	if got.Commit != cloneCommit || got.Error != "" {
		t.Errorf("posted = %+v", got)
	}
	files := byPath(got.Files)
	if len(files) != 3 {
		t.Errorf("files = %+v; want broken, review and style", got.Files)
	}
	if s := files["skills/style.md"]; s.Name != "style" || s.Content != "---\nname: style\n---\nBe kind.\n" || s.Problem != "" {
		t.Errorf("style = %+v", s)
	}
	if s := files["skills/review/SKILL.md"]; s.Name != "review" || s.Content != "Review carefully.\n" {
		t.Errorf("review = %+v", s)
	}
	if s := files["skills/broken.md"]; s.Problem == "" {
		t.Errorf("broken = %+v; want a problem", s)
	}

	// The top of the repository, when no folder is given.
	rc.posted = nil
	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", ""), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if files := byPath(rc.posted[0].Files); len(files) != 2 || files["README.md"].Name != "README" ||
		files["skills/SKILL.md"].Problem != "a skill folder needs a SKILL.md file" {
		t.Errorf("top files = %+v", rc.posted[0].Files)
	}

	// A refused result is an error, so the container fails.
	rc.status = http.StatusConflict
	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "skills"), "tok-1", srv.Client(), io.Discard); err == nil {
		t.Error("fetchSkills = nil after a 409; want an error")
	}
}

// When the clone failed, its error is posted instead of files.
func TestFetchSkillsPostsError(t *testing.T) {
	dir := t.TempDir()
	msg := "could not fetch https://example.com/skills.git at main; the URL, branch or tag may be wrong, or outside jobEgress.cidrs"
	writeFile(t, filepath.Join(dir, "error"), msg+"\n")
	rc := &receiver{}
	srv := rc.server(t)

	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "skills"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rc.posted) != 1 || rc.posted[0].Error != msg || rc.posted[0].Commit != "" || len(rc.posted[0].Files) != 0 {
		t.Errorf("posted = %+v", rc.posted)
	}

	// No commit and no error: the clone never finished.
	dir = t.TempDir()
	rc.posted = nil
	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "skills"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(rc.posted) != 1 || rc.posted[0].Error == "" {
		t.Errorf("posted = %+v; want an error", rc.posted)
	}
}

// A symlink in the clone is never followed: one pointing outside it, as a
// skill file, a skill folder, a SKILL.md or the folder itself, is reported
// as a problem and its target is never read.
func TestFetchSkillsRefusesSymlinkOutside(t *testing.T) {
	dir := work(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "secret.md"), "OUTSIDE-SECRET")
	writeFile(t, filepath.Join(outside, "folder", "SKILL.md"), "OUTSIDE-SECRET")
	repo := filepath.Join(dir, "repo")
	for link, target := range map[string]string{
		"skills/evil.md":           filepath.Join(outside, "secret.md"),
		"skills/evil-folder":       filepath.Join(outside, "folder"),
		"skills/up.md":             "../../../outside/secret.md",
		"skills/linked/SKILL.md":   filepath.Join(outside, "secret.md"),
		"linked-skills":            outside,
		"skills/review/other.link": filepath.Join(outside, "secret.md"),
	} {
		p := filepath.Join(repo, link)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	rc := &receiver{}
	srv := rc.server(t)

	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "skills"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rc.posted)
	if strings.Contains(string(raw), "OUTSIDE-SECRET") {
		t.Fatal("a symlink's target was read")
	}
	files := byPath(rc.posted[0].Files)
	for _, p := range []string{"skills/evil.md", "skills/evil-folder", "skills/up.md", "skills/linked/SKILL.md"} {
		if s := files[p]; s.Problem != "the skill is a symbolic link; it was not read" || s.Content != "" {
			t.Errorf("%s = %+v; want the symlink problem", p, s)
		}
	}
	if s := files["skills/style.md"]; s.Problem != "" || s.Content == "" {
		t.Errorf("style = %+v; the other skills are still read", s)
	}

	// The folder itself a symlink: nothing is read, and the error says why.
	rc.posted = nil
	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "linked-skills"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(rc.posted)
	if strings.Contains(string(raw), "OUTSIDE-SECRET") || rc.posted[0].Error != `the folder "linked-skills" is a symbolic link; it was not read` {
		t.Errorf("posted = %s", raw)
	}
}

// A folder whose text would make the result too large posts an error instead.
func TestFetchSkillsTooLarge(t *testing.T) {
	dir := work(t)
	for i := range 40 {
		writeFile(t, filepath.Join(dir, "repo", "heavy", fmt.Sprintf("s%02d.md", i)), strings.Repeat("y", 120<<10))
	}
	rc := &receiver{}
	srv := rc.server(t)
	if err := fetchSkills(context.Background(), fetchOpts(dir, srv.URL+"/skill-fetches/f1/result", "heavy"), "tok-1", srv.Client(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := rc.posted[0]; !strings.Contains(got.Error, "more than 4 MiB") || len(got.Files) != 0 {
		t.Errorf("posted error = %q, %d files", got.Error, len(got.Files))
	}
}

func TestParseFetchSkillsArgs(t *testing.T) {
	cmd, err := parseArgs([]string{"fetch-skills", "--dir=/work/repo", "--path=skills", "--commit-file=/work/commit",
		"--error-file=/work/error", "--result-url=http://pergent-sh:8080/skill-fetches/f1/result"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := fetchOptions{dir: "/work/repo", path: "skills", commitFile: "/work/commit", errorFile: "/work/error",
		resultURL: "http://pergent-sh:8080/skill-fetches/f1/result"}
	if cmd.name != "fetch-skills" || cmd.fetch != want {
		t.Errorf("cmd = %+v", cmd)
	}
	for _, bad := range [][]string{
		{"fetch-skills", "--dir=/work/repo"},
		{"fetch-skills", "--dir=/work/repo", "--commit-file=c", "--error-file=e", "--result-url=ftp://x/y"},
		{"fetch-skills", "--dir=/work/repo", "--commit-file=c", "--error-file=e", "--result-url=http://x/y", "--path=../x"},
		{"fetch-skills", "--dir=/work/repo", "--commit-file=c", "--error-file=e", "--result-url=http://x/y", "extra"},
	} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) = nil; want an error", bad)
		}
	}
}
