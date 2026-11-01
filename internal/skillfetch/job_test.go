package skillfetch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fakeLauncher records the fetch Jobs it is asked to start.
type fakeLauncher struct {
	mu     sync.Mutex
	params []kube.FetchJobParams
	tokens []string
	err    error
}

func (l *fakeLauncher) LaunchFetch(_ context.Context, p kube.FetchJobParams, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.params = append(l.params, p)
	l.tokens = append(l.tokens, token)
	return l.err
}

func jobFetcher(st *store.Store, l *fakeLauncher, secrets ...string) *Fetcher {
	f := fetcher(st, nil, secrets...)
	f.Launcher, f.Namespace, f.GitImage, f.Image = l, "reviews", "ghcr.io/gopaytech/pergent:v0.1.1", "ghcr.io/gopaytech/pergent.sh:v0.3.0"
	f.CallbackBase = "http://pergent-sh.pergent-sh.svc:8080/"
	return f
}

const urlSource = "https://example.com/acme/skills.git"

// A public URL is never read by the pod: Start records the fetch and starts a
// Job with the URL, the ref and the folder, and a fresh one-time token, the
// only secret the Job gets. The token completes the fetch once.
func TestStartURLLaunchesJob(t *testing.T) {
	st := newStore(t)
	const held = "glpat-held-SECRET"
	l := &fakeLauncher{}
	f := jobFetcher(st, l, held)

	id, err := f.Start(ctx, Request{URL: urlSource, Ref: "v1.0", Path: "/skills/", Purpose: store.FetchInstall, Actor: "alice", ActorID: 9})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.SkillFetch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != store.SourceKindURL || got.URL != urlSource || got.Ref != "v1.0" || got.Path != "skills" ||
		got.Status != store.FetchRunning || got.RequestedBy != "alice" || got.RequestedByID != 9 ||
		!got.ExpiresAt.Equal(t0.Add(FetchKeep)) {
		t.Errorf("fetch = %+v", got)
	}

	if len(l.params) != 1 {
		t.Fatalf("launched %d Jobs; want 1", len(l.params))
	}
	p, token := l.params[0], l.tokens[0]
	want := kube.FetchJobParams{
		Name: kube.FetchJobName(id), Namespace: "reviews", FetchID: id,
		GitImage: "ghcr.io/gopaytech/pergent:v0.1.1", Image: "ghcr.io/gopaytech/pergent.sh:v0.3.0",
		URL: urlSource, Ref: "v1.0", Path: "skills",
		ResultURL: "http://pergent-sh.pergent-sh.svc:8080/skill-fetches/" + id + "/result",
		Timeout:   FetchTimeout,
	}
	if p != want {
		t.Errorf("params = %+v; want %+v", p, want)
	}
	if len(token) < 40 {
		t.Errorf("token %q is too short", token)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), held) || strings.Contains(token, held) {
		t.Error("a held secret reached the Job")
	}

	// Another token is refused; the Job's token works once.
	body := `{"commit":"` + commitA + `","files":[{"name":"style","path":"skills/style.md","content":"Be kind."}]}`
	if out, err := f.CompleteJob(ctx, id, "wrong", []byte(body)); err != nil || out != JobUnauthorized {
		t.Errorf("CompleteJob with another token = %v, %v; want unauthorized", out, err)
	}
	if out, err := f.CompleteJob(ctx, id, token, []byte(body)); err != nil || out != JobStored {
		t.Fatalf("CompleteJob = %v, %v; want stored", out, err)
	}
	if out, err := f.CompleteJob(ctx, id, token, []byte(body)); err != nil || out != JobConflict {
		t.Errorf("CompleteJob again = %v, %v; want conflict", out, err)
	}
	got, _ = st.SkillFetch(ctx, id)
	if got.Status != store.FetchDone || got.Commit != commitA || len(got.Files) != 1 || got.Files[0].Content != "Be kind." {
		t.Errorf("fetch = %+v", got)
	}

	// Each fetch has its own token.
	id2, err := f.Start(ctx, Request{URL: urlSource, Ref: "main", Purpose: store.FetchInstall})
	if err != nil {
		t.Fatal(err)
	}
	if l.tokens[1] == token || l.params[1].FetchID != id2 || l.params[1].Path != "" {
		t.Errorf("second Job = %+v", l.params[1])
	}
}

// Without the pergent.sh image (set by the chart) no URL can be read.
func TestStartURLNeedsImage(t *testing.T) {
	st := newStore(t)
	l := &fakeLauncher{}
	f := jobFetcher(st, l)
	f.Image = ""
	_, err := f.Start(ctx, Request{URL: urlSource, Ref: "main", Purpose: store.FetchInstall})
	var bad RequestError
	if !errors.As(err, &bad) || bad.Error() != "installing from a URL needs PERGENT_SH_IMAGE (set by the chart)" {
		t.Errorf("Start = %v; want the PERGENT_SH_IMAGE RequestError", err)
	}
	if len(l.params) != 0 {
		t.Error("a Job was started")
	}
}

// A Job that can't be started fails its fetch at once, with a plain message.
func TestStartURLLaunchFails(t *testing.T) {
	st := newStore(t)
	l := &fakeLauncher{err: errors.New("jobs is forbidden: SECRET-ish detail")}
	f := jobFetcher(st, l)
	id, err := f.Start(ctx, Request{URL: urlSource, Ref: "main", Purpose: store.FetchInstall})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.SkillFetch(ctx, id)
	if got.Status != store.FetchFailed || got.Error != "couldn't start the Job that reads the URL; see the pergent.sh log" {
		t.Errorf("fetch = %+v", got)
	}
}

func startJob(t *testing.T, f *Fetcher, l *fakeLauncher) (id, token string) {
	t.Helper()
	id, err := f.Start(ctx, Request{URL: urlSource, Ref: "main", Path: "skills", Purpose: store.FetchInstall})
	if err != nil {
		t.Fatal(err)
	}
	return id, l.tokens[len(l.tokens)-1]
}

// What a Job sends goes through the same checks as a read by the pod: a held
// secret or an oversize text is not kept, a bad skill gets its problem, and
// a body that isn't a result, or a commit that isn't one, fails the fetch.
func TestCompleteJobChecksFiles(t *testing.T) {
	st := newStore(t)
	const held = "glpat-held-SECRET"
	l := &fakeLauncher{}
	f := jobFetcher(st, l, held)

	id, token := startJob(t, f, l)
	res := JobResult{Commit: commitA, Files: []store.FetchedFile{
		{Name: "leaky", Path: "skills/leaky.md", Content: "use " + held},
		{Name: "big", Path: "skills/big.md", Content: strings.Repeat("x", 128<<10+1)},
		{Name: "bad", Path: "skills/bad.md", Content: "---\npaths: 7\n---\nBad.\n"},
		{Name: "link", Path: "skills/link.md", Problem: "the skill is a symbolic link; it was not read"},
		{Name: "fine", Path: "skills/fine.md", Content: "Fine."},
	}}
	body, _ := json.Marshal(res)
	if out, err := f.CompleteJob(ctx, id, token, body); err != nil || out != JobStored {
		t.Fatalf("CompleteJob = %v, %v", out, err)
	}
	got, _ := st.SkillFetch(ctx, id)
	byName := map[string]store.FetchedFile{}
	for _, file := range got.Files {
		byName[file.Name] = file
	}
	if s := byName["leaky"]; s.Problem != "the skill contains a secret pergent.sh holds" || s.Content != "" {
		t.Errorf("leaky = %+v", s)
	}
	if s := byName["big"]; !strings.Contains(s.Problem, "over the limit") || s.Content != "" {
		t.Errorf("big = %+v", s)
	}
	if s := byName["bad"]; s.Problem == "" {
		t.Errorf("bad = %+v; want a problem", s)
	}
	if s := byName["link"]; s.Problem != "the skill is a symbolic link; it was not read" {
		t.Errorf("link = %+v", s)
	}
	if s := byName["fine"]; s.Problem != "" || s.Content != "Fine." {
		t.Errorf("fine = %+v", s)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), held) {
		t.Error("the stored fetch holds the secret")
	}

	for name, body := range map[string]string{
		"not json":   `{"commit":`,
		"bad commit": `{"commit":"main","files":[]}`,
		"unknown":    `{"commit":"` + commitA + `","files":[],"extra":1}`,
		"bad path":   `{"commit":"` + commitA + `","files":[{"name":"x","path":"../x.md","content":"X."}]}`,
	} {
		id, token := startJob(t, f, l)
		if out, err := f.CompleteJob(ctx, id, token, []byte(body)); err != nil || out != JobStored {
			t.Fatalf("%s: CompleteJob = %v, %v", name, out, err)
		}
		if got, _ := st.SkillFetch(ctx, id); got.Status != store.FetchFailed ||
			got.Error != "the Job that read the URL sent a result pergent.sh couldn't use" {
			t.Errorf("%s: fetch = %+v; want failed", name, got)
		}
	}
}

// The error a Job reports is kept, bounded, on one line and without a held
// secret.
func TestCompleteJobError(t *testing.T) {
	st := newStore(t)
	const held = "glpat-held-SECRET"
	l := &fakeLauncher{}
	f := jobFetcher(st, l, held)

	id, token := startJob(t, f, l)
	msg := "could not fetch " + urlSource + " at main; the URL, branch or tag may be wrong, or outside jobEgress.cidrs"
	body, _ := json.Marshal(JobResult{Error: msg + "\n"})
	if out, err := f.CompleteJob(ctx, id, token, body); err != nil || out != JobStored {
		t.Fatalf("CompleteJob = %v, %v", out, err)
	}
	if got, _ := st.SkillFetch(ctx, id); got.Status != store.FetchFailed || got.Error != msg {
		t.Errorf("fetch = %+v; want failed with %q", got, msg)
	}

	id, token = startJob(t, f, l)
	body, _ = json.Marshal(JobResult{Error: "line one\nline\x00two " + strings.Repeat("y", 2000)})
	if _, err := f.CompleteJob(ctx, id, token, body); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SkillFetch(ctx, id); len(got.Error) > 500 || strings.ContainsAny(got.Error, "\n\x00") ||
		!strings.HasPrefix(got.Error, "line one line two") {
		t.Errorf("error = %q", got.Error)
	}

	id, token = startJob(t, f, l)
	body, _ = json.Marshal(JobResult{Error: "oops " + held})
	if _, err := f.CompleteJob(ctx, id, token, body); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SkillFetch(ctx, id); strings.Contains(got.Error, held) || got.Error == "" {
		t.Errorf("error = %q; want no secret", got.Error)
	}
}

// A body over MaxFetchBytes fails the fetch, with the token only.
func TestJobTooLarge(t *testing.T) {
	st := newStore(t)
	l := &fakeLauncher{}
	f := jobFetcher(st, l)
	id, token := startJob(t, f, l)
	if err := f.JobTooLarge(ctx, id, "wrong"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SkillFetch(ctx, id); got.Status != store.FetchRunning {
		t.Errorf("fetch = %+v; another token changed it", got)
	}
	if err := f.JobTooLarge(ctx, id, token); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SkillFetch(ctx, id); got.Status != store.FetchFailed || !strings.Contains(got.Error, "more than 4 MiB") {
		t.Errorf("fetch = %+v", got)
	}
}

// A fetch read by the pod has no token: no Job result can complete it.
func TestCompleteJobRefusesInstanceFetch(t *testing.T) {
	st := newStore(t)
	id, err := st.CreateSkillFetch(ctx, store.SkillFetch{Kind: store.SourceKindInstance, Instance: "lab", Repo: "acme/skills",
		Ref: "main", Purpose: store.FetchInstall, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := jobFetcher(st, &fakeLauncher{})
	out, err := f.CompleteJob(ctx, id, "anything", []byte(`{"commit":"`+commitA+`","files":[]}`))
	if err != nil || out != JobUnauthorized {
		t.Errorf("CompleteJob = %v, %v; want unauthorized", out, err)
	}
	if out, _ := f.CompleteJob(ctx, "nope", "anything", []byte(`{}`)); out != JobUnauthorized {
		t.Errorf("CompleteJob of an unknown fetch = %v; want unauthorized", out)
	}
}

// A result posted with a wrong token is refused before any work: the held
// secrets are not read and the result is not checked. The right token still
// completes the fetch afterwards.
func TestCompleteJobWrongTokenDoesNoWork(t *testing.T) {
	st := newStore(t)
	l := &fakeLauncher{}
	f := jobFetcher(st, l, "glpat-held-SECRET")
	var reads int
	inner := f.Secrets
	f.Secrets = func(ctx context.Context) ([]string, error) {
		reads++
		return inner(ctx)
	}

	id, token := startJob(t, f, l)
	body, _ := json.Marshal(JobResult{Commit: commitA, Files: []store.FetchedFile{{Name: "a", Path: "skills/a.md", Content: "A."}}})
	if out, err := f.CompleteJob(ctx, id, "wrong", body); err != nil || out != JobUnauthorized {
		t.Fatalf("CompleteJob with a wrong token = %v, %v; want unauthorized", out, err)
	}
	if reads != 0 {
		t.Errorf("the held secrets were read %d times for a wrong token; want 0", reads)
	}
	if got, _ := st.SkillFetch(ctx, id); got.Status != store.FetchRunning {
		t.Errorf("fetch = %+v; a wrong token changed it", got)
	}
	if out, err := f.CompleteJob(ctx, id, token, body); err != nil || out != JobStored {
		t.Fatalf("CompleteJob = %v, %v; want stored", out, err)
	}
	if reads != 1 {
		t.Errorf("the held secrets were read %d times; want 1", reads)
	}
}
