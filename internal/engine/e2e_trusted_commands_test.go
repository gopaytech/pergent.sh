package engine

import (
	"net/http"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// trustedCommands is the commands harness with the instance trusting only
// authors with write access and the command gate on: merge request 1, at
// head, is by author (rep, a Reporter, or dev, a Developer), and its run of
// head waits when the author is the reporter.
func trustedCommands(t *testing.T, authorID int64, author string) (*cmdE2E, string) {
	t.Helper()
	e := newCommandsE2E(t)
	env := e.env
	gl := env.GitLab
	env.Engine.Config.GitLab[0].TrustedAuthorsOnly = config.TrustOn
	on := true
	env.Engine.Config.Repositories[0].CommandsAuthorAndMaintainersOnly = &on
	gl.Reply(http.MethodGet, membersPath(userDeveloper), http.StatusOK, member(30))
	gl.Reply(http.MethodGet, membersPath(userReporter), http.StatusOK, member(20))
	for _, id := range []int64{userDeveloper, userReporter} {
		gl.Reply(http.MethodGet, userPath(id), http.StatusOK, map[string]any{"id": id, "bot": false})
	}
	gl.Reply(http.MethodPost, notesPath(1), http.StatusCreated, `{"id": 900}`)
	head := sha('a')
	setAuthor(env, head, authorID, author)
	return e, head
}

// setAuthor serves merge request 1 at head as opened by author.
func setAuthor(env *testEnv, head string, authorID int64, author string) {
	mr := openMR(1, head)
	mr.Author = gitlab.User{ID: authorID, Username: author}
	env.GitLab.SetMR(mr)
}

// waitingRun queues the push run of head and holds it, as the dispatcher
// does for an untrusted author.
func waitingRun(t *testing.T, env *testEnv, head string) string {
	t.Helper()
	id := env.Enqueue(1, head)
	if ok, err := env.Store.HoldRun(env.ctx, id, store.WaitUntrusted); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	return id
}

// A Developer who isn't the author starts the untrusted change's review even
// with the command gate on.
func TestStartByWriterPassesGate(t *testing.T) {
	e, head := trustedCommands(t, userReporter, "rep")
	env := e.env
	id := waitingRun(t, env, head)

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	e.wantReplies(t)
	r := env.Run(id)
	if r.State != store.Queued || r.Trigger != store.TriggerStart {
		t.Fatalf("run = %s/%s, want queued/start", r.State, r.Trigger)
	}
	entries, err := env.Store.AuditLog(env.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range entries {
		found = found || a.Action == "review_start"
	}
	if !found {
		t.Errorf("no review_start in the audit log: %+v", entries)
	}
}

// The untrusted author can't start their own review, and a second try within
// two minutes gets no answer at all.
func TestAuthorCannotStartOwnUntrusted(t *testing.T) {
	e, head := trustedCommands(t, userReporter, "rep")
	env := e.env
	id := waitingRun(t, env, head)

	c := e.say(t, "rep", userReporter, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, "only someone with write access can start the review of this change")
	if r := env.Run(id); r.State != store.Waiting {
		t.Errorf("run = %s, want still waiting", r.State)
	}

	again := e.say(t, "rep", userReporter, "@pergent-review review failed")
	e.answer(t)
	e.wantReactions(t, again)
	if r := env.Run(id); r.State != store.Waiting {
		t.Errorf("run after a second try = %s, want still waiting", r.State)
	}
}

// Write access gets past the gate only to start a review: a Developer's
// dismiss on someone else's change is refused by the gate as before.
func TestDismissStillGatedForWriter(t *testing.T) {
	e, head := trustedCommands(t, userReporter, "rep")
	waitingRun(t, e.env, head)

	c := e.say(t, "dev", userDeveloper, "@pergent-review dismiss abc123 not a problem")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, "Only the author of this MR and the repository's maintainers can use pergent commands.")
}

// A trusted author's change is re-run as before, against both caps.
func TestTrustedChangeRerunUnchanged(t *testing.T) {
	e, head := trustedCommands(t, userDeveloper, "dev")
	env := e.env
	servePublish(env, 1, head)
	run := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "no_blocking_issues", "suggestion")))
	if err := env.Store.MarkPublished(env.ctx, run, store.RunDecision{Outcome: store.OutcomeNoBlocking}); err != nil {
		t.Fatal(err)
	}
	setAuthor(env, head, userDeveloper, "dev")

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	runs, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(1<<40))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Trigger != store.TriggerRerun {
		t.Fatalf("queued = %+v, want one rerun", runs)
	}
}

// A start counts against the change's cap only: with the repository's cap
// used up it still goes ahead, and the change's cap stops it.
func TestStartCaps(t *testing.T) {
	e, head := trustedCommands(t, userReporter, "rep")
	env := e.env
	one := 1
	env.Engine.Config.Repositories[0].RerunLimitPerRepo = &one
	env.Engine.Config.Repositories[0].RerunLimitPerMR = &one
	// Another change of the repository used the repository's cap.
	if _, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(9), testProjectPath, sha('9'), "main", store.TriggerRerun, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	id := waitingRun(t, env, head)
	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	if r := env.Run(id); r.Trigger != store.TriggerStart {
		t.Fatalf("run = %s/%s, want a start", r.State, r.Trigger)
	}

	// The change's cap (1) is now used: the next start is refused.
	head2 := sha('c')
	setAuthor(env, head2, userReporter, "rep")
	waitingRun(t, env, head2)
	env.SetNow(env.Now().Add(3 * 60e9))
	c2 := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c2, eyes, down)
}

// A start reviews only a head that existed when it was asked for: a push
// that lands after the comment, before it is handled, waits again.
func TestStartRefusesHeadPushedAfterComment(t *testing.T) {
	e, head := trustedCommands(t, userReporter, "rep")
	env := e.env
	first := waitingRun(t, env, head)

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	// The stranger pushes before the command is handled.
	head2 := sha('c')
	setAuthor(env, head2, userReporter, "rep")
	second := waitingRun(t, env, head2)
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, "this change was pushed after your comment; start the review again")
	if r := env.Run(second); r.State != store.Waiting {
		t.Errorf("the newer head = %s/%s, want still waiting", r.State, r.Trigger)
	}
	if r := env.Run(first); r.State != store.Superseded {
		t.Errorf("the older head = %s, want superseded", r.State)
	}

	// Asked again, it starts.
	env.SetNow(env.Now().Add(3 * 60e9))
	c2 := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c2, eyes, up)
	if r := env.Run(second); r.Trigger != store.TriggerStart {
		t.Errorf("after a new comment: %s/%s, want a start", r.State, r.Trigger)
	}
}
