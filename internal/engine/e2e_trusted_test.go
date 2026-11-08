package engine

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	strangerID   = int64(42)
	strangerName = "stranger"
)

// trustEnv is a test environment whose instance trusts only authors with
// write access, with merge request iid by the stranger at head, and the
// stranger's membership answered with level (0: not a member).
func trustEnv(t *testing.T, iid int64, head string, level int) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.Engine.Config.GitLab[0].TrustedAuthorsOnly = config.TrustOn
	mr := openMR(iid, head)
	mr.Author = gitlab.User{ID: strangerID, Username: strangerName}
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	env.GitLab.Reply(http.MethodPost, notesPath(iid), http.StatusCreated, `{"id": 900}`)
	strangerMember(env, level)
	return env
}

// strangerMember answers the stranger's membership of acme/widgets with level.
func strangerMember(env *testEnv, level int) {
	path := "/projects/7/members/all/42"
	if level == 0 {
		env.GitLab.Reply(http.MethodGet, path, http.StatusNotFound, `{"message":"404 Not found"}`)
		return
	}
	env.GitLab.Reply(http.MethodGet, path, http.StatusOK, map[string]any{"id": strangerID, "access_level": level, "state": "active"})
}

// statuses returns the states and descriptions set on head, in order.
func statusesOf(t *testing.T, env *testEnv, head string) []string {
	t.Helper()
	var out []string
	for _, r := range env.GitLab.Requests(http.MethodPost, statusPath(head)) {
		m := r.JSON(t)
		out = append(out, m["state"].(string)+": "+m["description"].(string))
	}
	return out
}

func notes(env *testEnv, iid int64) []string {
	var out []string
	for _, r := range env.GitLab.Requests(http.MethodPost, notesPath(iid)) {
		out = append(out, string(r.Body))
	}
	return out
}

// A Reporter's change waits: no Job, one note, a pending status; a second
// push waits too, with no second note.
func TestUntrustedRunWaits(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 20)
	id := env.Enqueue(3, head)
	dispatch(t, env)

	run := env.Run(id)
	if run.State != store.Waiting || run.SkipReason != store.WaitUntrusted {
		t.Fatalf("run = %s/%q, want waiting/untrusted_author", run.State, run.SkipReason)
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: "+waitingDescription {
		t.Errorf("statuses = %v", got)
	}
	ns := notes(env, 3)
	if len(ns) != 1 || !strings.Contains(ns[0], "Not reviewed automatically: the author isn't a trusted author of this repository.") ||
		!strings.Contains(ns[0], "`@pergent-review review`") {
		t.Fatalf("notes = %v", ns)
	}

	head2 := sha('c')
	mr := openMR(3, head2)
	mr.Author = gitlab.User{ID: strangerID, Username: strangerName}
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, statusPath(head2), http.StatusCreated, `{}`)
	id2 := env.Enqueue(3, head2)
	dispatch(t, env)
	if r := env.Run(id2); r.State != store.Waiting {
		t.Errorf("second head = %s, want waiting", r.State)
	}
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("first head = %s, want superseded", r.State)
	}
	if got := statusesOf(t, env, head2); len(got) != 1 || got[0] != "pending: "+waitingDescription {
		t.Errorf("statuses of the second head = %v", got)
	}
	if n := len(notes(env, 3)); n != 1 {
		t.Errorf("%d notes, want 1", n)
	}
}

// A Developer's change is reviewed as before.
func TestTrustedRunGoesAhead(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 30)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v", env.Run(id))
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}
}

// An access check that keeps failing is tried again until trustPatience,
// then the run waits with no note; one that recovers lets the run go ahead.
func TestTrustCheckRetriesThenGivesUp(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 0)
	env.GitLab.Reply(http.MethodGet, "/projects/7/members/all/42", http.StatusForbidden, `{"message":"403 Forbidden"}`)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued || r.NotBefore.IsZero() {
		t.Fatalf("after a failed check: run = %s not_before=%v, want queued and postponed", r.State, r.NotBefore)
	}

	// Past the window it waits, says the check failed and posts no note.
	env.SetNow(env.Now().Add(trustPatience + time.Minute))
	dispatch(t, env)
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitCheckFailed {
		t.Fatalf("run = %s/%q, want waiting/trust_check_failed", r.State, r.SkipReason)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: "+checkFailedDescription("pergent-review") {
		t.Errorf("statuses = %v", got)
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}
}

func TestTrustCheckRecovers(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 0)
	env.GitLab.Reply(http.MethodGet, "/projects/7/members/all/42", http.StatusForbidden, `{"message":"403 Forbidden"}`)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	strangerMember(env, 30)
	env.SetNow(env.Now().Add(trustRetryDelay))
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job after the check recovered; run = %+v", env.Run(id))
	}
}

// A draft the repository doesn't review is a draft, whoever wrote it.
func TestUntrustedDraftIsADraft(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 20)
	no := false
	env.Engine.Config.Repositories[0].ReviewDrafts = &no
	mr := openMR(3, head)
	mr.Author = gitlab.User{ID: strangerID, Username: strangerName}
	mr.Draft = true
	env.GitLab.SetMR(mr)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("run = %s, want superseded as a draft", r.State)
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}
}

// A started run is not checked again.
func TestStartRunSkipsCheck(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 20)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if _, _, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil {
		t.Fatal(err)
	}
	env.GitLab.Reset()
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job for the started run; run = %+v", env.Run(id))
	}
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/members/all/42")); n != 0 {
		t.Errorf("the author's access was read %d times", n)
	}
}

// The retry window starts at the first failed check, not when the run was
// queued: a run that waited in a backlog still gets its retries.
func TestTrustCheckPatienceAfterBacklog(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 0)
	env.GitLab.Reply(http.MethodGet, "/projects/7/members/all/42", http.StatusForbidden, `{"message":"403 Forbidden"}`)
	id := env.Enqueue(3, head)
	env.SetNow(env.Now().Add(time.Hour))
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("after a first failed check an hour after queueing: run = %s/%q, want queued for a retry", r.State, r.SkipReason)
	}
	env.SetNow(env.Now().Add(trustPatience + time.Minute))
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitCheckFailed {
		t.Errorf("past the window: run = %s/%q, want waiting/trust_check_failed", r.State, r.SkipReason)
	}
}
