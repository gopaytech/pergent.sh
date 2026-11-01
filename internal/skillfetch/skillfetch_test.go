package skillfetch

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var ctx = context.Background()

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("PERGENT_SH_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("PERGENT_SH_TEST_DATABASE_URL is not set")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenInSchema(ctx, dbURL, "test_skillfetch_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		if err := st.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

var t0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// repo is a git repository the fakes serve: refs to commits, and each
// commit's files by path. Only a commit reads files: a read at a branch or
// tag name is "not found", so a reader that skips ResolveRef sees nothing.
type repo struct {
	mu    sync.Mutex
	refs  map[string]string
	files map[string]map[string]string
	// nonCommitReads counts tree and file reads at something other than a
	// commit.
	nonCommitReads int
}

func (r *repo) at(commit string) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files, ok := r.files[commit]
	if !ok {
		r.nonCommitReads++
	}
	return files, ok
}

// tree lists dir's direct entries in files; ok is false when dir holds none.
func tree(files map[string]string, dir string) (entries []forge.TreeEntry, ok bool) {
	seen := map[string]bool{}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	for p := range files {
		rest, found := strings.CutPrefix(p, prefix)
		if !found {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		e := forge.TreeEntry{Name: name, Path: path.Join(dir, name), Type: "blob"}
		if nested {
			e.Type = "tree"
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, len(entries) > 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// gitlabFake serves the repository as the project acme/skills (ID 42).
func gitlabFake(t *testing.T, rp *repo) forge.SourceReader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, q := r.URL.EscapedPath(), r.URL.Query()
		const proj = "/api/v4/projects/42/repository/"
		switch {
		case p == "/api/v4/projects/acme%2Fskills":
			writeJSON(w, 200, map[string]any{"id": 42})
		case strings.HasPrefix(p, proj+"commits/"):
			ref, _ := url.PathUnescape(strings.TrimPrefix(p, proj+"commits/"))
			rp.mu.Lock()
			commit, ok := rp.refs[ref]
			rp.mu.Unlock()
			if !ok {
				writeJSON(w, 404, map[string]any{"message": "404 Commit Not Found"})
				return
			}
			writeJSON(w, 200, map[string]any{"id": commit})
		case p == proj+"tree":
			files, ok := rp.at(q.Get("ref"))
			entries, found := tree(files, q.Get("path"))
			if !ok || !found {
				writeJSON(w, 404, map[string]any{"message": "404 Tree Not Found"})
				return
			}
			writeJSON(w, 200, entries)
		case strings.HasPrefix(p, proj+"files/") && strings.HasSuffix(p, "/raw"):
			fp, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(p, proj+"files/"), "/raw"))
			files, _ := rp.at(q.Get("ref"))
			content, ok := files[fp]
			if !ok {
				writeJSON(w, 404, map[string]any{"message": "404 File Not Found"})
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, content)
		default:
			writeJSON(w, 404, map[string]any{"message": "404 Not Found"})
		}
	}))
	t.Cleanup(srv.Close)
	return gitlabforge.NewForge(gitlab.New(srv.URL, "review-token", srv.Client()), gitlab.User{ID: 1, Username: "pergent"})
}

func appKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// githubFake serves the repository as acme/skills, with the review App
// installed on it.
func githubFake(t *testing.T, rp *repo) forge.SourceReader {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, q := r.URL.EscapedPath(), r.URL.Query()
		const base = "/repos/acme/skills/"
		switch {
		case p == base+"installation":
			writeJSON(w, 200, map[string]any{"id": 5})
		case r.Method == http.MethodPost && p == "/app/installations/5/access_tokens":
			writeJSON(w, 201, map[string]any{"token": "install-token", "expires_at": time.Now().Add(time.Hour)})
		case strings.HasPrefix(p, base+"commits/"):
			ref, _ := url.PathUnescape(strings.TrimPrefix(p, base+"commits/"))
			rp.mu.Lock()
			commit, ok := rp.refs[ref]
			rp.mu.Unlock()
			if !ok {
				writeJSON(w, 404, map[string]any{"message": "Not Found"})
				return
			}
			writeJSON(w, 200, map[string]any{"sha": commit})
		case p == "/repos/acme/skills/contents" || strings.HasPrefix(p, base+"contents/"):
			fp, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(p, "/repos/acme/skills/contents"), "/"))
			files, _ := rp.at(q.Get("ref"))
			if content, ok := files[fp]; ok {
				w.Header().Set("Content-Type", "application/vnd.github.raw")
				_, _ = io.WriteString(w, content)
				return
			}
			entries, found := tree(files, fp)
			if !found {
				writeJSON(w, 404, map[string]any{"message": "Not Found"})
				return
			}
			out := []map[string]any{}
			for _, e := range entries {
				typ := map[string]string{"blob": "file", "tree": "dir"}[e.Type]
				out = append(out, map[string]any{"name": e.Name, "path": e.Path, "type": typ})
			}
			writeJSON(w, 200, out)
		default:
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
		}
	}))
	t.Cleanup(srv.Close)
	app, err := github.NewApp(srv.URL, 123, appKey(t), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return githubforge.NewForge(app, 2, "pergent", githubforge.ReposFunc(func(context.Context, int64) (githubforge.Repo, error) {
		return githubforge.Repo{}, githubforge.ErrUnknownRepo
	}))
}

// fetcher is a Fetcher on st reading the given instances.
func fetcher(st *store.Store, readers map[string]forge.SourceReader, secrets ...string) *Fetcher {
	return &Fetcher{
		Store: st,
		Readers: func(instance string) (forge.SourceReader, bool) {
			r, ok := readers[instance]
			return r, ok
		},
		Secrets: func(context.Context) ([]string, error) { return secrets, nil },
		Now:     func() time.Time { return t0 },
		Log:     slog.New(slog.DiscardHandler),
	}
}

// fetch starts r and waits for it to end.
func fetch(t *testing.T, f *Fetcher, r Request) store.SkillFetch {
	t.Helper()
	id, err := f.Start(ctx, r)
	if err != nil {
		t.Fatalf("Start(%+v): %v", r, err)
	}
	f.wait()
	got, err := f.Store.SkillFetch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

const commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// A connected GitLab or GitHub instance is read at the commit a ref with a
// "/" resolves to: the commit, each skill's name, path and text, and the
// problems of those that can't be installed are recorded. An unknown ref
// fails the fetch with the reader's not-found message.
func TestFetchInstanceReadsRef(t *testing.T) {
	st := newStore(t)
	rp := &repo{refs: map[string]string{"release/v1": commitA}, files: map[string]map[string]string{commitA: {
		"README.md":              "not a skill: outside the folder",
		"skills/style.md":        "---\nname: style\n---\nBe kind.\n",
		"skills/review/SKILL.md": "Review carefully.\n",
		"skills/review/notes.md": "ignored: not the skill file",
		"skills/broken.md":       "---\npaths: 7\n---\nBroken.\n",
		"skills/empty/other.md":  "a folder with no SKILL.md",
		"skills/notes.txt":       "not markdown",
		"skills/.hidden.md":      "hidden",
	}}}
	f := fetcher(st, map[string]forge.SourceReader{"lab": gitlabFake(t, rp), "hub": githubFake(t, rp)})

	for _, instance := range []string{"lab", "hub"} {
		t.Run(instance, func(t *testing.T) {
			got := fetch(t, f, Request{Instance: instance, Repo: "acme/skills", Ref: "release/v1", Path: "skills/",
				Purpose: store.FetchInstall, Actor: "alice", ActorID: 9})
			if got.Status != store.FetchDone || got.Commit != commitA || got.Error != "" {
				t.Fatalf("fetch = %+v; want done at %s", got, commitA)
			}
			if got.Kind != store.SourceKindInstance || got.Instance != instance || got.Repo != "acme/skills" ||
				got.Ref != "release/v1" || got.Path != "skills" || got.RequestedBy != "alice" || got.RequestedByID != 9 ||
				!got.ExpiresAt.Equal(t0.Add(FetchKeep)) {
				t.Errorf("fetch = %+v", got)
			}
			byPath := map[string]store.FetchedFile{}
			for _, file := range got.Files {
				byPath[file.Path] = file
			}
			if len(got.Files) != 4 {
				t.Errorf("files = %+v; want broken, empty, review, style", got.Files)
			}
			if s := byPath["skills/style.md"]; s.Name != "style" || s.Content != "---\nname: style\n---\nBe kind.\n" || s.Problem != "" {
				t.Errorf("style = %+v", s)
			}
			if s := byPath["skills/review/SKILL.md"]; s.Name != "review" || s.Content != "Review carefully.\n" || s.Problem != "" {
				t.Errorf("review = %+v", s)
			}
			if s := byPath["skills/broken.md"]; s.Name != "broken" || s.Problem == "" {
				t.Errorf("broken = %+v; want a problem", s)
			}
			if s := byPath["skills/empty/SKILL.md"]; s.Name != "empty" || s.Problem != "a skill folder needs a SKILL.md file" {
				t.Errorf("empty = %+v", s)
			}
		})
	}
	if rp.nonCommitReads != 0 {
		t.Errorf("%d reads were made at a branch or tag name; want only at the commit", rp.nonCommitReads)
	}

	got := fetch(t, f, Request{Instance: "lab", Repo: "acme/skills", Ref: "no-such-tag", Purpose: store.FetchInstall})
	if got.Status != store.FetchFailed || got.Error != forge.ErrSourceNotFound.Error() || got.Files != nil {
		t.Errorf("fetch of a bad ref = %+v; want failed: %q", got, forge.ErrSourceNotFound.Error())
	}
	got = fetch(t, f, Request{Instance: "hub", Repo: "acme/other", Ref: "release/v1", Purpose: store.FetchInstall})
	if got.Status != store.FetchFailed || got.Error != forge.ErrSourceNotFound.Error() {
		t.Errorf("fetch of a repository the App can't read = %+v", got)
	}
}

// memReader is an in-memory SourceReader of one repository, any project.
type memReader struct {
	repo
	tooLarge map[string]bool // paths GetFileAt refuses as too large
	// gate, when set, holds every ResolveRef until it is closed.
	gate             chan struct{}
	running, maxSeen int
}

func (m *memReader) ResolveRef(ctx context.Context, _, ref string) (string, error) {
	m.mu.Lock()
	m.running++
	m.maxSeen = max(m.maxSeen, m.running)
	gate := m.gate
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running--
		m.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	commit, ok := m.refs[ref]
	if !ok {
		return "", forge.ErrSourceNotFound
	}
	return commit, nil
}

func (m *memReader) ListTreeAt(_ context.Context, _, dir, commit string) ([]forge.TreeEntry, error) {
	files, _ := m.at(commit)
	entries, _ := tree(files, dir)
	return entries, nil
}

func (m *memReader) GetFileAt(_ context.Context, _, p, commit string) ([]byte, bool, error) {
	if m.tooLarge[p] {
		return nil, false, forge.ErrFileTooLarge
	}
	files, _ := m.at(commit)
	content, ok := files[p]
	if !ok {
		return nil, false, nil
	}
	return []byte(content), true, nil
}

func newMem(files map[string]string) *memReader {
	return &memReader{repo: repo{refs: map[string]string{"main": commitA}, files: map[string]map[string]string{commitA: files}}}
}

// A skill holding a secret pergent.sh holds, or over the size limit (read or
// refused as too large by the platform), gets a problem and its text is not
// kept.
func TestFetchMarksSecretAndOversize(t *testing.T) {
	st := newStore(t)
	const secret = "glpat-held-SECRET"
	mem := newMem(map[string]string{
		"leaky.md": "Use the token " + secret + " when asked.",
		"big.md":   strings.Repeat("x", 128<<10+1),
		"huge.md":  "the platform refuses it",
		"blank.md": "  \n",
		"fine.md":  "Fine.",
	})
	mem.tooLarge = map[string]bool{"huge.md": true}
	f := fetcher(st, map[string]forge.SourceReader{"lab": mem}, secret)

	got := fetch(t, f, Request{Instance: "lab", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall})
	if got.Status != store.FetchDone {
		t.Fatalf("fetch = %+v", got)
	}
	byName := map[string]store.FetchedFile{}
	for _, file := range got.Files {
		byName[file.Name] = file
	}
	if s := byName["leaky"]; s.Problem != "the skill contains a secret pergent.sh holds" || s.Content != "" {
		t.Errorf("leaky = %+v; want the secret problem and no text", s)
	}
	if s := byName["big"]; !strings.Contains(s.Problem, "over the limit") || s.Content != "" {
		t.Errorf("big = %+v; want the size problem and no text", s)
	}
	if s := byName["huge"]; s.Problem != "skill is over the limit of 128 KiB" || s.Content != "" {
		t.Errorf("huge = %+v", s)
	}
	if s := byName["blank"]; s.Problem != "the skill has no text" {
		t.Errorf("blank = %+v", s)
	}
	if s := byName["fine"]; s.Problem != "" || s.Content != "Fine." {
		t.Errorf("fine = %+v", s)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), secret) {
		t.Error("the stored fetch holds the secret")
	}
}

// With the held secrets unknown, nothing is offered: the fetch fails, and
// its message names no secret.
func TestFetchSecretsErrorFailsClosed(t *testing.T) {
	st := newStore(t)
	f := fetcher(st, map[string]forge.SourceReader{"lab": newMem(map[string]string{"a.md": "A."})})
	f.Secrets = func(context.Context) ([]string, error) {
		return nil, errors.New("opening review_token of lab-SECRET failed")
	}
	got := fetch(t, f, Request{Instance: "lab", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall})
	if got.Status != store.FetchFailed || got.Files != nil || strings.Contains(got.Error, "SECRET") || got.Error == "" {
		t.Errorf("fetch = %+v; want failed with a plain message", got)
	}
}

// A folder of more skills than the library holds, or of more text than a
// fetch keeps, fails the fetch.
func TestFetchLimits(t *testing.T) {
	st := newStore(t)
	many := map[string]string{}
	for i := range MaxFetchFiles + 1 {
		many[fmt.Sprintf("s%03d.md", i)] = "A."
	}
	heavy := map[string]string{}
	for i := range 40 {
		heavy[fmt.Sprintf("s%02d.md", i)] = strings.Repeat("y", 120<<10)
	}
	f := fetcher(st, map[string]forge.SourceReader{"many": newMem(many), "heavy": newMem(heavy)})

	got := fetch(t, f, Request{Instance: "many", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall})
	if got.Status != store.FetchFailed || !strings.Contains(got.Error, "more than 200 skills") {
		t.Errorf("fetch of %d skills = %+v", len(many), got)
	}
	got = fetch(t, f, Request{Instance: "heavy", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall})
	if got.Status != store.FetchFailed || !strings.Contains(got.Error, "more than 4 MiB") {
		t.Errorf("fetch of 4.7 MiB = %+v", got)
	}
}

// At most two reads run at once; the others wait their turn.
func TestFetchAtMostTwoAtOnce(t *testing.T) {
	st := newStore(t)
	mem := newMem(map[string]string{"a.md": "A."})
	mem.gate = make(chan struct{})
	f := fetcher(st, map[string]forge.SourceReader{"lab": mem})
	var ids []string
	for range 4 {
		id, err := f.Start(ctx, Request{Instance: "lab", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	time.Sleep(100 * time.Millisecond)
	close(mem.gate)
	f.wait()
	if mem.maxSeen != 2 {
		t.Errorf("at most %d reads ran at once; want 2", mem.maxSeen)
	}
	for _, id := range ids {
		if got, _ := st.SkillFetch(ctx, id); got.Status != store.FetchDone {
			t.Errorf("fetch %s = %+v", id, got)
		}
	}
}

func TestStartRefusals(t *testing.T) {
	st := newStore(t)
	f := fetcher(st, map[string]forge.SourceReader{"lab": newMem(nil)})
	ok := Request{Instance: "lab", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall}
	for name, edit := range map[string]func(r *Request){
		"purpose":         func(r *Request) { r.Purpose = "delete" },
		"no ref":          func(r *Request) { r.Ref = "" },
		"ref with space":  func(r *Request) { r.Ref = "a b" },
		"ref with dots":   func(r *Request) { r.Ref = "a..b" },
		"ref option":      func(r *Request) { r.Ref = "--upload-pack=x" },
		"path up":         func(r *Request) { r.Path = "skills/../.." },
		"path backslash":  func(r *Request) { r.Path = `skills\x` },
		"no repo":         func(r *Request) { r.Repo = "" },
		"no instance":     func(r *Request) { r.Instance = "" },
		"both":            func(r *Request) { r.URL = "https://example.com/skills.git" },
		"bad from ref":    func(r *Request) { r.Purpose, r.FromRef = store.FetchUpdate, "a b" },
		"repo with space": func(r *Request) { r.Repo = "acme/my skills" },
	} {
		r := ok
		edit(&r)
		_, err := f.Start(ctx, r)
		var bad RequestError
		if !errors.As(err, &bad) {
			t.Errorf("%s: Start = %v; want a RequestError", name, err)
		}
	}
	if _, err := f.Start(ctx, Request{Instance: "nope", Repo: "acme/skills", Ref: "main", Purpose: store.FetchInstall}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Start on an instance not connected = %v; want ErrNotConnected", err)
	}
	// A public URL is read in a Job, which comes later: nothing is fetched.
	_, err := f.Start(ctx, Request{URL: "https://example.com/skills.git", Ref: "main", Purpose: store.FetchInstall})
	var bad RequestError
	if !errors.As(err, &bad) {
		t.Errorf("Start with a URL = %v; want a RequestError", err)
	}
	f.wait()

	// An update reads the ref the skills came from unless told another.
	id, err := f.Start(ctx, Request{Instance: "lab", Repo: "acme/skills", Ref: "main", Purpose: store.FetchUpdate})
	if err != nil {
		t.Fatal(err)
	}
	f.wait()
	if got, _ := st.SkillFetch(ctx, id); got.FromRef != "main" || got.Purpose != store.FetchUpdate {
		t.Errorf("update fetch = %+v; want from ref main", got)
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{
		"https://example.com/acme/skills.git",
		"https://example.com:8443/skills",
		"HTTPS://example.com/skills",
	} {
		if err := ValidateURL(ok); err != nil {
			t.Errorf("ValidateURL(%q) = %v; want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"https://user:pass@example.com/skills.git",
		"https://token@example.com/skills.git",
		"http://example.com/skills.git",
		"ssh://git@example.com/skills.git",
		"git@example.com:acme/skills.git",
		"https://example.com/skills.git?ref=main",
		"https://example.com/skills.git?",
		"https://example.com/skills.git#main",
		"https:///skills.git",
		"https:example.com/skills",
		"file:///etc/passwd",
		"https://example.com/skills .git",
		"https://example.com/\nskills",
	} {
		err := ValidateURL(bad)
		var re RequestError
		if !errors.As(err, &re) {
			t.Errorf("ValidateURL(%q) = %v; want a RequestError", bad, err)
		}
	}
}

// The sweep fails a fetch still running past FetchTimeout and a minute, and
// deletes those past their expiry.
func TestSweepFailsStaleAndPrunes(t *testing.T) {
	st := newStore(t)
	add := func(created time.Time) string {
		id, err := st.CreateSkillFetch(ctx, store.SkillFetch{Kind: store.SourceKindInstance, Instance: "lab", Repo: "acme/skills",
			Ref: "main", Purpose: store.FetchInstall, CreatedAt: created, ExpiresAt: created.Add(FetchKeep)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	now := t0.Add(10 * time.Minute)
	stale := add(now.Add(-FetchTimeout - time.Minute - time.Second))
	young := add(now.Add(-FetchTimeout))
	expired := add(now.Add(-FetchKeep))
	f := fetcher(st, nil)
	f.Now = func() time.Time { return now }

	if err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SkillFetch(ctx, stale); err != nil || got.Status != store.FetchFailed || got.Error == "" {
		t.Errorf("stale fetch = %+v, %v; want failed", got, err)
	}
	if got, err := st.SkillFetch(ctx, young); err != nil || got.Status != store.FetchRunning {
		t.Errorf("young fetch = %+v, %v; want running", got, err)
	}
	if _, err := st.SkillFetch(ctx, expired); !errors.Is(err, store.ErrSkillFetchNotFound) {
		t.Errorf("expired fetch = %v; want pruned", err)
	}
}
