package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The people of the command tests, with their GitLab user IDs and the
// access levels they have in the project.
const (
	userMaintainer = int64(50) // mo
	userDeveloper  = int64(51) // dev
	userReporter   = int64(52) // rep
	userAuthor     = int64(53) // maya: a maintainer, and the author of the change
	userStranger   = int64(99) // not a member
	userNamesake   = int64(77) // a maintainer who has maya's username on another account

	botMention = "pergent-review"
)

var commandUsers = map[string]int64{
	"mo": userMaintainer, "dev": userDeveloper, "rep": userReporter, "maya": userAuthor, "eve": userStranger,
}

// commandFixture is an environment whose GitLab serves what the command step
// calls: the merge requests (authored by maya), the commenters' memberships,
// reactions and replies.
type commandFixture struct {
	*testEnv
	head string
	// authorID is the user ID the platform reports for the author of the
	// merge requests (userAuthor).
	authorID int64
	// comments counts the comment IDs handed out.
	comments int64
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	f := &commandFixture{testEnv: newTestEnv(t), head: sha('a'), authorID: userAuthor}
	// Most command tests have people with write access, who are not the
	// author, use commands: the switch is off here, and the tests of the
	// gate turn it on (gate).
	f.gate(false)
	servePublish(f.testEnv, 1, f.head)
	f.serveMR(1, false)
	f.GitLab.Reply(http.MethodGet, membersPath(userMaintainer), http.StatusOK, member(40))
	f.GitLab.Reply(http.MethodGet, membersPath(userDeveloper), http.StatusOK, member(30))
	f.GitLab.Reply(http.MethodGet, membersPath(userReporter), http.StatusOK, member(20))
	f.GitLab.Reply(http.MethodGet, membersPath(userAuthor), http.StatusOK, member(40))
	f.GitLab.Reply(http.MethodGet, membersPath(userNamesake), http.StatusOK, member(40))
	// userStranger: 404, not a member.
	for _, id := range []int64{userMaintainer, userDeveloper, userReporter, userAuthor, userNamesake, userStranger, userStranger + 1} {
		f.GitLab.Reply(http.MethodGet, userPath(id), http.StatusOK, map[string]any{"id": id, "bot": false})
	}
	f.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusCreated, map[string]any{"id": createdNote})
	f.GitLab.Reply(http.MethodPost, notesPath(2), http.StatusCreated, map[string]any{"id": createdNote})
	return f
}

// gate sets the repository's "only the author and maintainers use commands"
// switch.
func (f *commandFixture) gate(on bool) {
	f.Engine.Config.Repositories[0].CommandsAuthorAndMaintainersOnly = &on
}

func membersPath(userID int64) string {
	return "/projects/" + strconv.FormatInt(testProjectID, 10) + "/members/all/" + strconv.FormatInt(userID, 10)
}

func userPath(userID int64) string { return "/users/" + strconv.FormatInt(userID, 10) }

func member(level int) map[string]any {
	return map[string]any{"access_level": level, "state": "active"}
}

func emojiPath(iid, comment int64) string {
	return notesPath(iid) + "/" + strconv.FormatInt(comment, 10) + "/award_emoji"
}

// serveMR serves merge request iid at the fixture's head, authored by maya.
func (f *commandFixture) serveMR(iid int64, draft bool) {
	mr := openMR(iid, f.head)
	mr.Draft = draft
	mr.Author = gitlab.User{ID: f.authorID, Username: "maya"}
	f.GitLab.SetMR(mr)
	f.GitLab.Reply(http.MethodPost, emojiPath(iid, 0), http.StatusCreated, `{}`)
}

// review publishes a review of the fixture's head on merge request 1, so
// there is something to refer to.
func (f *commandFixture) review(skills ...result.Skill) string {
	f.t.Helper()
	id := f.Report(1, f.head, resultFor(f.t, f.head, skills...))
	if err := f.Store.MarkPublished(f.ctx, id, store.RunDecision{Outcome: store.OutcomeNoBlocking}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// say stores a command by user (a key of commandUsers) in a new comment on
// merge request iid, written at `at`, and returns its comment ID.
func (f *commandFixture) say(iid int64, user, kind, arg, reason string, at time.Time) int64 {
	f.t.Helper()
	return f.sayAs(iid, user, commandUsers[user], kind, arg, reason, at)
}

// sayAs is say with the commenter's user ID spelled out.
func (f *commandFixture) sayAs(iid int64, user string, userID int64, kind, arg, reason string, at time.Time) int64 {
	f.t.Helper()
	f.comments++
	id := 1000 + f.comments
	f.GitLab.Reply(http.MethodPost, emojiPath(iid, id), http.StatusCreated, `{}`)
	_, err := f.Store.InsertCommand(f.ctx, store.Command{
		MR:          store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: iid},
		ProjectPath: testProjectPath, Platform: "gitlab", Kind: kind, Arg: arg, Reason: reason,
		CommentID: id, Author: user, AuthorID: userID, CreatedAt: at,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// run answers the stored commands once.
func (f *commandFixture) run() {
	f.t.Helper()
	if err := f.Engine.CommandsOnce(f.ctx); err != nil {
		f.t.Fatalf("CommandsOnce: %v", err)
	}
}

// reactions are the emoji put on comment id, in order.
func (f *commandFixture) reactions(iid, id int64) []string {
	var out []string
	for _, r := range f.GitLab.Requests(http.MethodPost, emojiPath(iid, id)) {
		out = append(out, fmt.Sprint(r.JSON(f.t)["name"]))
	}
	return out
}

func (f *commandFixture) wantReactions(iid, id int64, want ...string) {
	f.t.Helper()
	if got := f.reactions(iid, id); !slices.Equal(got, want) {
		f.t.Errorf("reactions on comment %d = %v, want %v\n%s", id, got, want, f.Logs())
	}
}

// replies are the plain comments the bot posted on merge request iid.
func (f *commandFixture) replies(iid int64) []string {
	var out []string
	for _, r := range f.GitLab.Requests(http.MethodPost, notesPath(iid)) {
		out = append(out, fmt.Sprint(r.JSON(f.t)["body"]))
	}
	return out
}

func (f *commandFixture) wantReplies(iid int64, want ...string) {
	f.t.Helper()
	if got := f.replies(iid); !slices.Equal(got, want) {
		f.t.Errorf("replies on !%d = %q, want %q", iid, got, want)
	}
}

// queued claims the runs waiting to be dispatched, which the test then owns.
func (f *commandFixture) queued() []store.Run {
	f.t.Helper()
	runs, err := f.Store.ClaimQueued(f.ctx, 100, f.Now().Add(24*time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	return runs
}

func (f *commandFixture) audit() []store.AuditEntry {
	f.t.Helper()
	es, err := f.Store.AuditLog(f.ctx, 0, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return es
}

func (f *commandFixture) dismissals(iid int64) []store.Dismissal {
	f.t.Helper()
	ds, err := f.Store.Dismissals(f.ctx, store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: iid})
	if err != nil {
		f.t.Fatal(err)
	}
	return ds
}

// The default: both reactions of a carried-out command, no reply.
const (
	eyes = "eyes"
	up   = "thumbsup"
	down = "thumbsdown"
)

var help = "Commands: `@" + botMention + " review` · `@" + botMention + " review failed` · `@" + botMention + " dismiss <id> <reason>`"

func TestRerunCommand(t *testing.T) {
	f := newCommandFixture(t)
	old := f.review(reviewed("no_blocking_issues", "a.go"))
	at := time.Now()

	id := f.say(1, "dev", "review", "", "", at)
	f.run()

	f.wantReactions(1, id, eyes, up)
	f.wantReplies(1)
	runs := f.queued()
	if len(runs) != 1 {
		t.Fatalf("queued %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.HeadSHA != f.head || r.TargetBranch != "main" || r.Trigger != store.TriggerRerun || r.SkillSubset != nil || r.BaseRunID != nil {
		t.Errorf("run = head %s target %s %q %v %v, want a full re-run of the head", r.HeadSHA, r.TargetBranch, r.Trigger, r.SkillSubset, r.BaseRunID)
	}
	if got := f.Run(old).State; got != store.Superseded {
		t.Errorf("the earlier review is %s, want superseded", got)
	}
}

func TestRerunCommandNeedsWriteAccess(t *testing.T) {
	for _, user := range []string{"rep", "eve"} {
		t.Run(user, func(t *testing.T) {
			f := newCommandFixture(t)
			f.review(reviewed("no_blocking_issues", "a.go"))

			id := f.say(1, user, "review", "", "", time.Now())
			f.run()

			f.wantReactions(1, id, eyes, down)
			f.wantReplies(1, "you don't have permission to do that here")
			if n := len(f.queued()); n != 0 {
				t.Errorf("%d runs queued, want none", n)
			}
		})
	}
}

// A head already queued is not queued twice, and the request is still
// acknowledged as done.
func TestRerunCommandForQueuedHeadIsAcknowledged(t *testing.T) {
	f := newCommandFixture(t)
	f.review(reviewed("no_blocking_issues", "a.go"))
	at := time.Now()
	first := f.say(1, "dev", "review", "", "", at)
	second := f.say(1, "mo", "review", "", "", at.Add(time.Minute))

	f.run()

	f.wantReactions(1, first, eyes, up)
	f.wantReactions(1, second, eyes, up)
	if n := len(f.queued()); n != 1 {
		t.Errorf("%d runs queued, want 1", n)
	}
	if n := countAction(f.audit(), "review_rerun"); n != 1 {
		t.Errorf("%d re-runs audited, want 1 (the second started nothing)", n)
	}
}

func skillOf(t *testing.T, name, verdict string) result.Skill {
	return namedSkill(t, name, verdict, "", "")
}

func TestRerunFailedCommand(t *testing.T) {
	t.Run("only the failed skills", func(t *testing.T) {
		f := newCommandFixture(t)
		base := f.review(skillOf(t, "security", "no_blocking_issues"), skillOf(t, "style", "failed"))

		id := f.say(1, "dev", "review_failed", "", "", time.Now())
		f.run()

		f.wantReactions(1, id, eyes, up)
		f.wantReplies(1)
		runs := f.queued()
		if len(runs) != 1 {
			t.Fatalf("queued %d runs, want 1", len(runs))
		}
		r := runs[0]
		if r.Trigger != store.TriggerRerunFailed || !slices.Equal(r.SkillSubset, []string{"style"}) || r.BaseRunID == nil || *r.BaseRunID != base {
			t.Errorf("run = %q %v %v, want rerun_failed of style on %s", r.Trigger, r.SkillSubset, r.BaseRunID, base)
		}
	})

	t.Run("a dismissed blocking finding does not count as failed", func(t *testing.T) {
		f := newCommandFixture(t)
		blocking := namedSkill(t, "security", "has_blocking_issues", "blocking", "aaaaaaaaaaaa")
		f.review(blocking, skillOf(t, "style", "no_blocking_issues"))
		if _, err := f.Store.Dismiss(f.ctx, store.Dismissal{
			MR: f.MRKey(1), Fingerprint: "aaaaaaaaaaaa", Severity: "blocking", Reason: "x", Actor: "mo",
		}); err != nil {
			t.Fatal(err)
		}

		id := f.say(1, "dev", "review_failed", "", "", time.Now())
		f.run()

		f.wantReactions(1, id, eyes, up)
		f.wantReplies(1, "nothing failed")
		if n := len(f.queued()); n != 0 {
			t.Errorf("%d runs queued, want none", n)
		}
	})

	t.Run("nothing failed", func(t *testing.T) {
		f := newCommandFixture(t)
		f.review(skillOf(t, "security", "no_blocking_issues"), skillOf(t, "style", "no_blocking_issues"))

		id := f.say(1, "dev", "review_failed", "", "", time.Now())
		f.run()

		f.wantReactions(1, id, eyes, up)
		f.wantReplies(1, "nothing failed")
		if n := len(f.queued()); n != 0 {
			t.Errorf("%d runs queued, want none", n)
		}
		if n := countAction(f.audit(), "review_rerun_failed"); n != 0 {
			t.Errorf("%d re-runs audited, want none", n)
		}
	})

	errorResult := []byte(`{"schema_version":1,"status":"error","base":"` + sha('b') + `","head":"` + sha('a') +
		`","error":{"code":"internal","message":"x"},"comment":{"markdown":"x"}}`)
	for name, seed := range map[string]func(f *commandFixture){
		"the whole review failed": func(f *commandFixture) { f.Publish(1, f.head, errorResult) },
		"no review of the current head": func(f *commandFixture) {
			f.Publish(1, sha('c'), resultFor(f.t, sha('c'), skillOf(f.t, "security", "no_blocking_issues")))
		},
		"no review at all": func(f *commandFixture) {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCommandFixture(t)
			seed(f)

			id := f.say(1, "dev", "review_failed", "", "", time.Now())
			f.run()

			f.wantReactions(1, id, eyes, up)
			f.wantReplies(1, "the whole review failed, so all of it is re-run")
			runs := f.queued()
			if len(runs) != 1 || runs[0].Trigger != store.TriggerRerun || runs[0].SkillSubset != nil || runs[0].BaseRunID != nil {
				t.Fatalf("queued %+v, want one full re-run", runs)
			}
		})
	}
}

func TestDismissCommand(t *testing.T) {
	f := newCommandFixture(t)
	id := f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "mo", "dismiss", "abc123", "false positive, the id is an integer", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	ds := f.dismissals(1)
	if len(ds) != 1 || ds[0].Fingerprint != sqlFingerprint || ds[0].Severity != "blocking" ||
		ds[0].Reason != "false positive, the id is an integer" || ds[0].Actor != "mo (gitlab main)" || ds[0].ActorID != nil {
		t.Fatalf("dismissals = %+v", ds)
	}
	if got := f.Run(id).State; got != store.Reported {
		t.Fatalf("run is %s, want reported to be decided again", got)
	}
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued; a dismissal starts none", n)
	}

	publish(t, f.testEnv)

	body := createdComment(t, f.testEnv, 1)
	for _, want := range []string{"Dismissed (1)", "dismissed by mo (gitlab main): false positive, the id is an integer", "### ✅ No blocking issues (1 dismissed)"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	wantLayout(t, body)
	wantStatus(t, f.testEnv, f.head, gitlab.StatusSuccess, "No blocking issues")
	if n := len(f.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Errorf("approve called %d times, want 1", n)
	}
}

// The ID may also be the start of the finding's fingerprint, when it is
// unambiguous.
func TestDismissByFingerprintPrefix(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "mo", "dismiss", "abc123def4", "constant", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Fingerprint != sqlFingerprint {
		t.Errorf("dismissals = %+v", ds)
	}
}

// Images are stripped from a reason before it is stored, so the review
// comment can't pull one in; a reason of nothing else is no reason.
func TestDismissReasonHasNoImages(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	at := time.Now()

	onlyImage := f.say(1, "mo", "dismiss", "abc123", "![x](https://evil.example/p.png)", at)
	f.run()
	f.wantReactions(1, onlyImage, eyes, down)
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Fatalf("dismissals = %+v; want none (no reason)", ds)
	}

	f.SetNow(f.Now().Add(commandThrottleWindow + time.Second))
	f.say(1, "mo", "dismiss", "abc123", "a constant ![x](https://evil.example/p.png)", at.Add(time.Minute))
	f.run()
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Reason != "a constant" {
		t.Errorf("dismissals = %+v; want the reason without its image", ds)
	}
}

func TestDismissAlreadyDismissed(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	at := time.Now()
	f.say(1, "mo", "dismiss", "abc123", "constant", at)
	f.run()

	// mo's next command is answered once the throttle window has passed.
	f.SetNow(f.Now().Add(commandThrottleWindow + time.Second))
	c := f.say(1, "mo", "dismiss", "abc123def4", "again", at.Add(time.Minute))
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1, "finding abc123def4 is already dismissed")
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Reason != "constant" {
		t.Errorf("dismissals = %+v, want the first one kept", ds)
	}
	if n := countAction(f.audit(), "finding_dismissed"); n != 1 {
		t.Errorf("%d dismissals audited, want 1", n)
	}
}

// A dismissal this very command stored on an earlier attempt, which then
// failed to re-decide the review, is carried on, not answered as a repeat.
func TestDismissRetryAfterPartialAttempt(t *testing.T) {
	f := newCommandFixture(t)
	id := f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	at := time.Now()
	if _, err := f.Store.Dismiss(f.ctx, store.Dismissal{
		MR: f.MRKey(1), Fingerprint: sqlFingerprint, Severity: "blocking", Reason: "constant",
		Actor: "mo (gitlab main)", CreatedAt: at.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	c := f.say(1, "mo", "dismiss", "abc123", "constant", at)
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	if got := f.Run(id).State; got != store.Reported {
		t.Errorf("run is %s, want reported to be decided again", got)
	}
	// Audited when it was stored, which this test didn't do: not again.
	if n := countAction(f.audit(), "finding_dismissed"); n != 0 {
		t.Errorf("%d dismissals audited, want none on the retry", n)
	}
}

func TestAuthorCannotDismissBlocking(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "maya", "dismiss", "abc123", "I know better", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, "the author of a change can't dismiss its blocking findings")
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
	if got := f.Run(f.latestRun()).State; got != store.Published {
		t.Errorf("run is %s; nothing was decided again", got)
	}
}

// The author may dismiss a suggestion of their own change.
func TestAuthorDismissesSuggestion(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))

	c := f.say(1, "maya", "dismiss", "abc123", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Severity != "suggestion" {
		t.Errorf("dismissals = %+v", ds)
	}
}

func TestDismissNeedsReason(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "mo", "dismiss", "abc123", "   ", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, "dismissing a blocking finding needs a reason: `@pergent-review dismiss <id> <reason>`")
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

func TestDeveloperDismissesSuggestionOnly(t *testing.T) {
	t.Run("blocking", func(t *testing.T) {
		f := newCommandFixture(t)
		f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

		c := f.say(1, "dev", "dismiss", "abc123", "constant", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, down)
		f.wantReplies(1, "you don't have permission to do that here")
		if ds := f.dismissals(1); len(ds) != 0 {
			t.Errorf("dismissals = %+v, want none", ds)
		}
	})
	t.Run("suggestion, no reason", func(t *testing.T) {
		f := newCommandFixture(t)
		id := f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))

		c := f.say(1, "dev", "dismiss", "abc123", "", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, up)
		f.wantReplies(1)
		if ds := f.dismissals(1); len(ds) != 1 || ds[0].Severity != "suggestion" || ds[0].Actor != "dev (gitlab main)" {
			t.Errorf("dismissals = %+v", ds)
		}
		if got := f.Run(id).State; got != store.Reported {
			t.Errorf("run is %s, want reported to be decided again", got)
		}
	})
	t.Run("not a member", func(t *testing.T) {
		f := newCommandFixture(t)
		f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))

		c := f.say(1, "eve", "dismiss", "abc123", "", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, down)
		f.wantReplies(1, "you don't have permission to do that here")
	})
}

func TestUnknownIDAndHelp(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	at := time.Now()

	unknownID := f.say(1, "mo", "dismiss", "ffffff", "reason", at)
	askHelp := f.say(1, "eve", "help", "", "", at)
	unknown := f.say(1, "dev", "unknown", "", "", at)
	// Not a member: the permission is refused before the review is looked at.
	// (Another one than eve: a second command of hers would be throttled.)
	stranger := f.sayAs(1, "zed", userStranger+1, "dismiss", "ffffff", "reason", at)
	f.run()

	f.wantReactions(1, unknownID, eyes, down)
	f.wantReactions(1, askHelp, eyes, up)
	f.wantReactions(1, unknown, eyes, up)
	f.wantReactions(1, stranger, eyes, down)
	f.wantReplies(1, "no finding ffffff in the latest review", help, help, "you don't have permission to do that here")
}

// With the gate off, help reads no access: a platform that can't say who is a
// member doesn't keep it from being answered.
func TestHelpNeedsNoAccessWithTheGateOff(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Handle(http.MethodGet, membersPath(userDeveloper), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	c := f.say(1, "dev", "help", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1, help)
}

// An ambiguous fingerprint prefix is no finding.
func TestDismissAmbiguousPrefix(t *testing.T) {
	f := newCommandFixture(t)
	a := namedSkill(t, "security", "has_blocking_issues", "blocking", "abc123000001")
	b := namedSkill(t, "style", "has_blocking_issues", "blocking", "abc123000002")
	f.review(a, b)

	c := f.say(1, "mo", "dismiss", "abc12300000", "reason", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, "no finding abc12300000 in the latest review")
}

func TestReplyIDKeepsNothingButHex(t *testing.T) {
	if got := replyUnknownID("AB12<>\n@zz"); got != "no finding ab12 in the latest review" {
		t.Errorf("reply = %q", got)
	}
}

func TestDraftRerun(t *testing.T) {
	for _, cmd := range []string{"review", "review_failed"} {
		t.Run(cmd, func(t *testing.T) {
			f := newCommandFixture(t)
			no := false
			f.Engine.Config.Repositories[0].ReviewDrafts = &no
			f.serveMR(1, true)
			f.review(skillOf(t, "security", "failed"))

			c := f.say(1, "dev", cmd, "", "", time.Now())
			f.run()

			f.wantReactions(1, c, eyes, down)
			f.wantReplies(1, "drafts aren't reviewed in this repository")
			if n := len(f.queued()); n != 0 {
				t.Errorf("%d runs queued, want none", n)
			}
		})
	}
}

func TestDuplicateWithin30s(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	at := time.Now()
	first := f.say(1, "dev", "review", "", "", at)
	// (Four people: a second command of one of them would be throttled.)
	again := f.say(1, "mo", "review", "", "", at.Add(10*time.Second))
	other := f.say(1, "maya", "review_failed", "", "", at.Add(11*time.Second))
	later := f.sayAs(1, "nia", userNamesake, "review", "", "", at.Add(40*time.Second))

	f.run()

	f.wantReactions(1, first, eyes, up)
	f.wantReactions(1, again, eyes, down)
	f.wantReactions(1, other, eyes, up)
	f.wantReactions(1, later, eyes, up)
	// The dropped one is not answered with a reply, and the other command
	// finds its head queued already.
	f.wantReplies(1)
}

// A comment of a GitLab project, group or service-account bot is no command
// whatever its username looks like: GitLab says so (the user's bot flag), and
// the bot gets a 👎 and no reply, nor does the command run.
func TestCommandOfABotIsDropped(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Reply(http.MethodGet, userPath(userDeveloper), http.StatusOK, map[string]any{"id": userDeveloper, "bot": true})

	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued by a bot", n)
	}
	if n := len(f.GitLab.Requests(http.MethodGet, membersPath(userDeveloper))); n != 0 {
		t.Errorf("a bot's access was looked up %d times", n)
	}
}

// When GitLab can't say whether the author is a bot, nothing is done and the
// command is tried again, then given up on (never run).
func TestCommandWhenTheAuthorCannotBeReadIsRetried(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Reply(http.MethodGet, userPath(userDeveloper), http.StatusInternalServerError, `{"message":"boom"}`)

	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()
	f.wantReactions(1, c, eyes)
	f.wantReplies(1)
	if n := len(f.queued()); n != 0 {
		t.Fatalf("%d runs queued while the author is unknown", n)
	}

	// Once GitLab answers, the retry runs the command.
	f.GitLab.Reply(http.MethodGet, userPath(userDeveloper), http.StatusOK, map[string]any{"id": userDeveloper, "bot": false})
	f.SetNow(f.Now().Add(10 * time.Minute))
	f.run()
	f.wantReactions(1, c, eyes, up)
	if n := len(f.queued()); n != 1 {
		t.Errorf("%d runs queued after the retry, want 1", n)
	}
}

// Every answer costs platform calls and a comment of the bot, so a person's
// next command on a change is left alone for two minutes after one of theirs
// was answered: no reaction, no reply, no platform call.
func TestCommandsAreThrottledPerAuthorAndChange(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	at := time.Now()

	first := f.say(1, "eve", "help", "", "", at)
	f.run()
	f.wantReactions(1, first, eyes, up)
	f.wantReplies(1, help)

	// The same person again, a help or a refusal alike: nothing at all.
	reads := len(f.GitLab.Requests(http.MethodGet, mrPath(1)))
	again := f.say(1, "eve", "help", "", "", at.Add(time.Second))
	refusal := f.say(1, "eve", "dismiss", "ffffff", "reason", at.Add(2*time.Second))
	f.run()
	f.wantReactions(1, again)
	f.wantReactions(1, refusal)
	f.wantReplies(1, help)
	if n := len(f.GitLab.Requests(http.MethodGet, mrPath(1))); n != reads {
		t.Errorf("a throttled command made %d platform reads", n-reads)
	}

	// A real command inside the window is throttled too, and left undone.
	real := f.sayAs(1, "eve", userStranger, "review", "", "", at.Add(3*time.Second))
	f.run()
	f.wantReactions(1, real)
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued by a throttled command", n)
	}

	// Throttled commands do not extend the window: it is counted from the
	// last answer.
	f.SetNow(f.Now().Add(commandThrottleWindow - time.Second))
	soon := f.say(1, "eve", "help", "", "", at.Add(4*time.Second))
	f.run()
	f.wantReactions(1, soon)
	f.SetNow(f.Now().Add(2 * time.Second))
	// (Written long after the first: the same command inside 30 seconds of
	// an answered one is dropped, which is another rule.)
	later := f.say(1, "eve", "help", "", "", at.Add(3*time.Minute))
	f.run()
	f.wantReactions(1, later, eyes, up)
	f.wantReplies(1, help, help)

	// Another person, and another change, are not throttled.
	other := f.say(1, "rep", "help", "", "", at.Add(5*time.Minute))
	f.serveMR(2, false)
	elsewhere := f.say(2, "eve", "help", "", "", at.Add(5*time.Minute))
	f.run()
	f.wantReactions(1, other, eyes, up)
	f.wantReactions(2, elsewhere, eyes, up)
}

// twoFindings is a skill with two blocking findings, abc123def456 and
// 654321fedcba.
func twoFindings(t *testing.T) result.Skill {
	t.Helper()
	s := skillWithFinding(t, "has_blocking_issues", "blocking")
	b, err := json.Marshal(result.Finding{
		ID: "654321", Fingerprint: "654321fedcba", File: "db/other.go", Line: 3, Severity: "blocking",
		Title: "Another problem", Explanation: "Explained.",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Findings = append(s.Findings, b)
	return s
}

// The throttle holds off the commands that did nothing: a command that was
// carried out does not start the window, so a maintainer working through a
// review's findings is not ignored.
func TestCarriedOutCommandsDoNotThrottle(t *testing.T) {
	f := newCommandFixture(t)
	f.review(twoFindings(t))
	at := time.Now()

	first := f.say(1, "mo", "dismiss", "abc123", "constant", at)
	second := f.say(1, "mo", "dismiss", "654321", "also constant", at.Add(time.Second))
	f.run()

	f.wantReactions(1, first, eyes, up)
	f.wantReactions(1, second, eyes, up)
	if ds := f.dismissals(1); len(ds) != 2 {
		t.Errorf("dismissals = %+v, want both", ds)
	}
}

// A maintainer asks for a re-run and, when it is out, dismisses one of its
// findings: both within two minutes, both carried out.
func TestReviewThenDismissAreBothCarriedOut(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	at := time.Now()

	rerun := f.say(1, "mo", "review", "", "", at)
	f.run()
	f.wantReactions(1, rerun, eyes, up)
	// The re-run's review is out a moment later.
	id := f.Report(1, f.head, resultFor(t, f.head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	if err := f.Store.MarkPublished(f.ctx, id, store.RunDecision{Outcome: store.OutcomeNoBlocking}); err != nil {
		t.Fatal(err)
	}

	dismiss := f.say(1, "mo", "dismiss", "abc123", "constant", at.Add(time.Second))
	f.run()

	f.wantReactions(1, dismiss, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 {
		t.Errorf("dismissals = %+v, want 1", ds)
	}
}

// A command answered without doing anything starts the window, whatever the
// reason: help, a refusal, a command with nothing to do. The next command,
// a valid one too, is left alone until the window has passed.
func TestCommandsThatDidNothingThrottle(t *testing.T) {
	cases := map[string]func(f *commandFixture, at time.Time) (first int64){
		"help":    func(f *commandFixture, at time.Time) int64 { return f.say(1, "mo", "help", "", "", at) },
		"unknown": func(f *commandFixture, at time.Time) int64 { return f.say(1, "mo", "unknown", "", "", at) },
		"refusal": func(f *commandFixture, at time.Time) int64 {
			return f.say(1, "mo", "dismiss", "ffffff", "constant", at)
		},
		"nothing failed": func(f *commandFixture, at time.Time) int64 {
			return f.say(1, "mo", "review_failed", "", "", at)
		},
		"already dismissed": func(f *commandFixture, at time.Time) int64 {
			dismiss(f.t, f.testEnv, 1, "blocking", "earlier")
			return f.say(1, "mo", "dismiss", "abc123", "again", at)
		},
	}
	for name, first := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
			at := time.Now()
			one := first(f, at)
			f.run()
			if name == "refusal" {
				f.wantReactions(1, one, eyes, down)
			} else {
				f.wantReactions(1, one, eyes, up)
			}

			// A valid command inside the window: left undone.
			real := f.say(1, "mo", "review", "", "", at.Add(time.Minute))
			f.run()
			f.wantReactions(1, real)
			if n := len(f.queued()); n != 0 {
				t.Errorf("%d runs queued by a throttled command", n)
			}

			// After the window it is answered.
			f.SetNow(f.Now().Add(commandThrottleWindow + time.Second))
			later := f.say(1, "mo", "review", "", "", at.Add(3*time.Minute))
			f.run()
			f.wantReactions(1, later, eyes, up)
			if n := len(f.queued()); n != 1 {
				t.Errorf("%d runs queued after the window, want 1", n)
			}
		})
	}
}

func TestCommandOnSwitchedOffRepo(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	c := f.say(1, "dev", "review", "", "", time.Now())
	f.Engine.Config.Repositories = nil

	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued, want none", n)
	}
}

func TestCommandOnClosedChange(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	mr := openMR(1, f.head)
	mr.State = "merged"
	f.GitLab.SetMR(mr)

	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)
}

func ptr[T any](v T) *T { return &v }

// The caps count the re-runs of commands and of buttons alike, per change and
// per repository, over the UTC day.
func TestRerunCapsAcrossCommandsAndButtons(t *testing.T) {
	// A button's re-run, as the dashboard queues it.
	button := func(f *commandFixture, iid int64) {
		t.Helper()
		id, created, err := f.Store.EnqueueRerun(f.ctx, store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: iid},
			testProjectPath, f.head, "main", store.TriggerRerun, nil, nil)
		if err != nil || !created {
			t.Fatalf("EnqueueRerun = %v, %v", created, err)
		}
		if err := f.Store.Supersede(f.ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	atCap := "re-run limit reached for this MR; it resets at 00:00 UTC"

	t.Run("per change", func(t *testing.T) {
		f := newCommandFixture(t)
		f.Engine.Config.Repositories[0].RerunLimitPerMR = ptr(2)
		button(f, 1)
		button(f, 1)

		c := f.say(1, "dev", "review", "", "", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, down)
		f.wantReplies(1, atCap)
		if n := len(f.queued()); n != 0 {
			t.Errorf("%d runs queued at the cap, want none", n)
		}

		// One short of the cap is allowed, and counts.
		f2 := newCommandFixture(t)
		f2.Engine.Config.Repositories[0].RerunLimitPerMR = ptr(2)
		button(f2, 1)
		c1 := f2.say(1, "dev", "review", "", "", time.Now())
		f2.run()
		f2.wantReactions(1, c1, eyes, up)
		if n := len(f2.queued()); n != 1 {
			t.Fatalf("%d runs queued under the cap, want 1", n)
		}
		// That run is queued no more: the next command finds the cap used up.
		c2 := f2.say(1, "mo", "review_failed", "", "", time.Now().Add(time.Minute))
		f2.run()
		f2.wantReactions(1, c2, eyes, down)
		f2.wantReplies(1, atCap)
	})

	t.Run("per repository", func(t *testing.T) {
		f := newCommandFixture(t)
		f.serveMR(2, false)
		f.Engine.Config.Repositories[0].RerunLimitPerMR = ptr(5)
		f.Engine.Config.Repositories[0].RerunLimitPerRepo = ptr(2)
		button(f, 1)
		button(f, 1)

		c := f.say(2, "dev", "review", "", "", time.Now())
		f.run()

		f.wantReactions(2, c, eyes, down)
		f.wantReplies(2, atCap)
		if n := len(f.queued()); n != 0 {
			t.Errorf("%d runs queued at the repository's cap, want none", n)
		}
	})

	t.Run("resets at 00:00 UTC", func(t *testing.T) {
		f := newCommandFixture(t)
		f.Engine.Config.Repositories[0].RerunLimitPerMR = ptr(1)
		button(f, 1)
		now := time.Now().UTC()
		midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

		f.SetNow(midnight.Add(-time.Second))
		before := f.say(1, "dev", "review", "", "", time.Now())
		f.run()
		f.wantReactions(1, before, eyes, down)

		f.SetNow(midnight)
		after := f.say(1, "mo", "review_failed", "", "", time.Now().Add(time.Minute))
		f.run()
		f.wantReactions(1, after, eyes, up)
		if n := len(f.queued()); n != 1 {
			t.Errorf("%d runs queued after the reset, want 1", n)
		}
	})

	t.Run("a push is never capped and never counts", func(t *testing.T) {
		f := newCommandFixture(t)
		f.Engine.Config.Repositories[0].RerunLimitPerMR = ptr(2)
		button(f, 1)
		// Pushes at the boundary always run, and are not re-runs.
		for _, head := range []string{sha('d'), sha('e')} {
			if _, created, err := f.Store.EnqueueRun(f.ctx, f.MRKey(1), testProjectPath, head, "main"); err != nil || !created {
				t.Fatalf("EnqueueRun = %v, %v", created, err)
			}
		}
		one := f.say(1, "dev", "review", "", "", time.Now())
		f.run()
		f.wantReactions(1, one, eyes, up)
		// That one was the second re-run of the day; the pushes were not.
		two := f.say(1, "mo", "review_failed", "", "", time.Now().Add(time.Minute))
		f.run()
		f.wantReactions(1, two, eyes, down)
		f.wantReplies(1, atCap)
	})
}

func TestCommandPlatformChurnIsRetriedThenGivenUp(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Reply(http.MethodGet, mrPath(1), http.StatusBadRequest, `{"message":"boom"}`)
	c := f.say(1, "dev", "review", "", "", time.Now())

	f.run()
	// Not due again yet.
	f.run()
	if n := len(f.GitLab.Requests(http.MethodGet, mrPath(1))); n != 1 {
		t.Fatalf("the merge request was read %d times in the first pass and the one after, want 1", n)
	}

	// Three retries, each later than the backoff; the platform comes back for
	// none of them.
	for i := 0; i < maxCommandRetries; i++ {
		f.SetNow(f.Now().Add(10 * time.Minute))
		f.run()
	}
	if n := len(f.GitLab.Requests(http.MethodGet, mrPath(1))); n != 1+maxCommandRetries {
		t.Errorf("the merge request was read %d times, want %d", n, 1+maxCommandRetries)
	}
	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)

	// Given up on: not tried again.
	f.SetNow(f.Now().Add(time.Hour))
	f.run()
	if n := len(f.GitLab.Requests(http.MethodGet, mrPath(1))); n != 1+maxCommandRetries {
		t.Errorf("a command given up on was tried again (%d reads)", n)
	}
}

func TestCommandRetrySucceedsWhenPlatformRecovers(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Reply(http.MethodGet, mrPath(1), http.StatusBadRequest, `{"message":"boom"}`)
	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()
	f.wantReactions(1, c, eyes)

	f.serveMR(1, false)
	f.SetNow(f.Now().Add(time.Minute))
	f.run()

	// 👀 once, however many passes.
	f.wantReactions(1, c, eyes, up)
	if n := len(f.queued()); n != 1 {
		t.Errorf("%d runs queued, want 1", n)
	}
}

// A refusal whose reply can't be posted is tried again, so the commenter is
// never left without an answer.
func TestRefusalWithoutReplyIsRetried(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusBadRequest, `{"message":"boom"}`)
	c := f.say(1, "rep", "review", "", "", time.Now())
	f.run()
	f.wantReactions(1, c, eyes)

	f.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusCreated, map[string]any{"id": createdNote})
	f.SetNow(f.Now().Add(time.Minute))
	f.run()

	f.wantReactions(1, c, eyes, down)
}

func TestCommandsAudited(t *testing.T) {
	f := newCommandFixture(t)
	f.review(namedSkill(t, "security", "has_blocking_issues", "blocking", sqlFingerprint), skillOf(t, "style", "failed"))
	at := time.Now()
	f.say(1, "mo", "dismiss", "abc123", "false positive", at)
	f.run()
	publish(t, f.testEnv)
	f.say(1, "dev", "review_failed", "", "", at.Add(time.Minute))
	f.run()

	entries := f.audit()
	find := func(action string) store.AuditEntry {
		t.Helper()
		for _, e := range entries {
			if e.Action == action {
				return e
			}
		}
		t.Fatalf("no %s in the audit log: %+v", action, entries)
		return store.AuditEntry{}
	}
	d := find("finding_dismissed")
	if d.Actor != "mo (gitlab main)" || d.ActorID != nil || d.Instance != testInstance || d.Project != testProjectPath {
		t.Errorf("dismissal audited as %+v", d)
	}
	for _, want := range []string{"mr=1", "head=" + f.head, "fingerprint=" + sqlFingerprint, `reason="false positive"`} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q lacks %q", d.Detail, want)
		}
	}
	rf := find("review_rerun_failed")
	if rf.Actor != "dev (gitlab main)" || !strings.Contains(rf.Detail, "head="+f.head) || !strings.Contains(rf.Detail, "skills=style") {
		t.Errorf("re-run audited as %+v", rf)
	}
}

func TestCommandsAuditFullRerun(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.say(1, "dev", "review", "", "", time.Now())
	f.run()

	var found bool
	for _, e := range f.audit() {
		if e.Action == "review_rerun" {
			found = true
			if e.Actor != "dev (gitlab main)" || !strings.Contains(e.Detail, "head="+f.head) {
				t.Errorf("re-run audited as %+v", e)
			}
		}
	}
	if !found {
		t.Error("the re-run was not audited")
	}
}

// Two dismissals in one pass: the first re-decides the run, which is then
// waiting to be published; the second still finds the review.
func TestTwoDismissalsInOnePass(t *testing.T) {
	f := newCommandFixture(t)
	a := namedSkill(t, "security", "has_blocking_issues", "blocking", "aaaaaaaaaaaa")
	b := namedSkill(t, "style", "has_blocking_issues", "blocking", "bbbbbbbbbbbb")
	f.review(a, b)
	at := time.Now()
	one := f.say(1, "mo", "dismiss", "aaaaaa", "constant", at)
	two := f.sayAs(1, "nia", userNamesake, "dismiss", "bbbbbb", "constant", at)

	f.run()

	f.wantReactions(1, one, eyes, up)
	f.wantReactions(1, two, eyes, up)
	if ds := f.dismissals(1); len(ds) != 2 {
		t.Errorf("dismissals = %+v, want 2", ds)
	}
}

// A slow platform call holds a pass up for the command budget at most, and
// never keeps a queued review from starting.
func TestCommandsDoNotBlockDispatch(t *testing.T) {
	defer func(b, c time.Duration) { commandsBudget, commandTimeout = b, c }(commandsBudget, commandTimeout)
	commandsBudget, commandTimeout = 400*time.Millisecond, 400*time.Millisecond

	f := newCommandFixture(t)
	f.GitLab.Reply(http.MethodGet, treePath(), http.StatusOK, []gitlab.TreeEntry{})
	// Every reaction and read of the command hangs until the caller gives up.
	var hung atomic.Int32
	slow := func(w http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	id := f.say(1, "dev", "review", "", "", time.Now())
	f.GitLab.Handle(http.MethodPost, emojiPath(1, id), slow)
	f.GitLab.Handle(http.MethodGet, membersPath(userDeveloper), slow)
	queued := f.Enqueue(2, f.head)
	f.GitLab.SetMR(openMR(2, f.head))
	f.GitLab.Reply(http.MethodPost, statusPath(f.head), http.StatusCreated, `{}`)

	start := time.Now()
	f.Engine.pass(f.ctx)
	took := time.Since(start)

	if took > 5*time.Second {
		t.Errorf("a pass took %s with a hung platform, want about the %s budget", took, commandsBudget)
	}
	if hung.Load() == 0 {
		t.Error("the command never reached the slow platform")
	}
	if j := f.Job(queued); j == nil {
		t.Errorf("the queued review was not dispatched in the pass\n%s", f.Logs())
	}
	// The command is not lost: it is given back, to be tried again.
	f.GitLab.Reply(http.MethodGet, membersPath(userDeveloper), http.StatusOK, member(30))
	f.GitLab.Reply(http.MethodPost, emojiPath(1, id), http.StatusCreated, `{}`)
	f.SetNow(f.Now().Add(time.Minute))
	f.run()
	if got := f.reactions(1, id); len(got) == 0 || got[len(got)-1] != up {
		t.Errorf("reactions after the platform recovered = %v, want to end with %s", got, up)
	}
}

// A command cut short because the pass ran out of its budget was not given a
// fair try: it is put off, with no attempt counted. One that took longer than
// its own time is counted.
func TestCommandCutShortByThePassIsNotCounted(t *testing.T) {
	defer func(b, c time.Duration) { commandsBudget, commandTimeout = b, c }(commandsBudget, commandTimeout)
	hang := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	for name, tc := range map[string]struct {
		budget, timeout time.Duration
		wantAttempts    int
	}{
		"the pass's budget ran out":      {400 * time.Millisecond, 10 * time.Second, 0},
		"the command's own time ran out": {10 * time.Second, 400 * time.Millisecond, 1},
	} {
		t.Run(name, func(t *testing.T) {
			commandsBudget, commandTimeout = tc.budget, tc.timeout
			f := newCommandFixture(t)
			f.review(skillOf(t, "security", "no_blocking_issues"))
			f.say(1, "dev", "review", "", "", time.Now())
			f.GitLab.Handle(http.MethodGet, membersPath(userDeveloper), hang)

			f.run()

			// Offered again once its wait is over, with the attempts it had.
			f.SetNow(f.Now().Add(time.Minute))
			again, err := f.Store.ClaimCommands(f.ctx, 10, f.Now())
			if err != nil || len(again) != 1 || again[0].Attempts != tc.wantAttempts {
				t.Fatalf("commands afterwards = %+v, %v; want one with %d attempts\n%s", again, err, tc.wantAttempts, f.Logs())
			}
		})
	}
}

func TestCommandsWaitForUnconnectedInstance(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	c := f.say(1, "dev", "review", "", "", time.Now())
	plat, _ := f.Engine.Platforms.Get(testInstance)
	f.Engine.Platforms.Delete(testInstance)

	f.run()
	if n := len(f.GitLab.Requests("", "")); n != 0 {
		t.Errorf("%d platform calls for an instance that isn't connected", n)
	}
	// Still waiting, untouched: not claimed, not tried, not finished.
	waiting, err := f.Store.ClaimCommands(f.ctx, 10, f.Now())
	if err != nil || len(waiting) != 1 || waiting[0].Attempts != 0 {
		t.Fatalf("waiting commands = %+v, %v; want the one, never tried", waiting, err)
	}

	// Connected, and the claim of the probe above lapsed: it is answered.
	f.Engine.Platforms.Set(testInstance, plat)
	f.SetNow(f.Now().Add(10 * time.Minute))
	f.run()
	f.wantReactions(1, c, eyes, up)
	if n := len(f.queued()); n != 1 {
		t.Errorf("%d runs queued once connected, want 1", n)
	}
}

func TestChangeTerm(t *testing.T) {
	for platform, want := range map[string]string{"gitlab": "MR", "github": "PR"} {
		if got := replyCap(changeTerm(platform)); got != "re-run limit reached for this "+want+"; it resets at 00:00 UTC" {
			t.Errorf("%s: %q", platform, got)
		}
	}
}

// latestRun is the ID of the newest decided run of merge request 1.
func (f *commandFixture) latestRun() string {
	f.t.Helper()
	r, found, err := f.Store.LatestDecidedRun(f.ctx, f.MRKey(1))
	if err != nil || !found {
		f.t.Fatalf("LatestDecidedRun = %v, %v", found, err)
	}
	return r.ID
}

func countAction(es []store.AuditEntry, action string) int {
	n := 0
	for _, e := range es {
		if e.Action == action {
			n++
		}
	}
	return n
}

// The author is told by user ID, never by username: a namesake on another
// account isn't the author, and the author under another login still is.
func TestAuthorComparedByID(t *testing.T) {
	t.Run("same login, another account", func(t *testing.T) {
		f := newCommandFixture(t)
		f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

		c := f.sayAs(1, "maya", userNamesake, "dismiss", "abc123", "constant", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, up)
		if ds := f.dismissals(1); len(ds) != 1 || ds[0].Severity != "blocking" {
			t.Errorf("dismissals = %+v, want the blocking one", ds)
		}
	})
	t.Run("same account, another login", func(t *testing.T) {
		f := newCommandFixture(t)
		f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

		c := f.sayAs(1, "maya-renamed", userAuthor, "dismiss", "abc123", "constant", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, down)
		f.wantReplies(1, "the author of a change can't dismiss its blocking findings")
		if ds := f.dismissals(1); len(ds) != 0 {
			t.Errorf("dismissals = %+v, want none", ds)
		}
	})
}

// A change whose author the platform didn't report can't be told from the
// commenter: a blocking finding is not dismissed (a suggestion is).
func TestUnknownAuthorCannotBeTold(t *testing.T) {
	f := newCommandFixture(t)
	f.authorID = 0
	f.serveMR(1, false)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "mo", "dismiss", "abc123", "constant", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, "the author of a change can't dismiss its blocking findings")
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

// A hung reply or reaction takes its own short time out of nothing else: the
// work is done, and the command is finished once, never offered again.
func TestSlowReplyOrReactionStillFinishesOnce(t *testing.T) {
	defer func(a, b, c time.Duration) { commandTimeout, reactTimeout, commandSettleTimeout = a, b, c }(
		commandTimeout, reactTimeout, commandSettleTimeout)
	commandTimeout, reactTimeout, commandSettleTimeout = 4*time.Second, 300*time.Millisecond, 1500*time.Millisecond
	hang := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}

	for name, tc := range map[string]struct {
		kind string
		slow func(f *commandFixture, comment int64)
		// done is what the command did besides being finished.
		done func(t *testing.T, f *commandFixture)
	}{
		"reply": {"help", func(f *commandFixture, _ int64) { f.GitLab.Handle(http.MethodPost, notesPath(1), hang) }, func(*testing.T, *commandFixture) {}},
		"reactions": {"review", func(f *commandFixture, c int64) { f.GitLab.Handle(http.MethodPost, emojiPath(1, c), hang) },
			func(t *testing.T, f *commandFixture) {
				if n := len(f.queued()); n != 1 {
					t.Errorf("%d runs queued, want the re-run despite the hung reaction", n)
				}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.review(skillOf(t, "security", "no_blocking_issues"))
			at := time.Now()
			c := f.say(1, "dev", tc.kind, "", "", at)
			tc.slow(f, c)

			f.run()

			tc.done(t, f)
			reads, reacted := len(f.GitLab.Requests(http.MethodGet, mrPath(1))), len(f.reactions(1, c))
			if reads != 1 {
				t.Fatalf("the merge request was read %d times, want 1\n%s", reads, f.Logs())
			}
			done, err := f.Store.RecentCommand(f.ctx, f.MRKey(1), tc.kind, "", at.Add(-time.Second))
			if err != nil || !done {
				t.Fatalf("the command is not finished as done: %v, %v\n%s", done, err, f.Logs())
			}

			// Long after its claim lapsed it is not offered again.
			f.SetNow(f.Now().Add(time.Hour))
			f.run()
			if n := len(f.GitLab.Requests(http.MethodGet, mrPath(1))); n != reads {
				t.Errorf("the command ran again (%d reads, want %d)", n, reads)
			}
			if n := len(f.reactions(1, c)); n != reacted {
				t.Errorf("the command was reacted to again (%d reactions, want %d)", n, reacted)
			}
		})
	}
}

// findingRaw is a stored finding.
func findingRaw(t *testing.T, id, fingerprint, severity string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(result.Finding{ID: id, Fingerprint: fingerprint, File: "a.go", Line: 1, Severity: severity,
		Title: "finding " + id, Explanation: "because"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// One fingerprint reported at two severities (by two skills) is dismissed at
// the higher one, whichever ID names it.
func TestDismissByIDUsesTheHighestSeverityOfTheFingerprint(t *testing.T) {
	seed := func(f *commandFixture) {
		a := skillOf(f.t, "security", "no_blocking_issues")
		a.Findings = []json.RawMessage{findingRaw(f.t, "abc123", "ffffffffffff", "suggestion")}
		b := skillOf(f.t, "style", "has_blocking_issues")
		b.Findings = []json.RawMessage{findingRaw(f.t, "def456", "ffffffffffff", "blocking")}
		f.review(a, b)
	}
	t.Run("a developer can't dismiss it", func(t *testing.T) {
		f := newCommandFixture(t)
		seed(f)

		c := f.say(1, "dev", "dismiss", "abc123", "", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, down)
		f.wantReplies(1, "you don't have permission to do that here")
	})
	t.Run("a maintainer dismisses it as blocking", func(t *testing.T) {
		f := newCommandFixture(t)
		seed(f)

		c := f.say(1, "mo", "dismiss", "abc123", "constant", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, up)
		if ds := f.dismissals(1); len(ds) != 1 || ds[0].Severity != "blocking" {
			t.Errorf("dismissals = %+v, want one at blocking", ds)
		}
	})
}

// denyRunUpdates makes every update of run id fail in the database, until
// the returned function is called.
func (f *commandFixture) denyRunUpdates(id string) (restore func()) {
	f.t.Helper()
	conn, err := pgx.Connect(f.ctx, os.Getenv(testDBEnv))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close(f.ctx) })
	rows, err := conn.Query(f.ctx, `SELECT table_schema FROM information_schema.tables
		WHERE table_name = 'runs' AND table_schema LIKE 'test_engine_%'`)
	if err != nil {
		f.t.Fatal(err)
	}
	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		schemas = append(schemas, s)
	}
	rows.Close()
	for _, schema := range schemas {
		var mine bool
		if err := conn.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM "`+schema+`".runs WHERE id = $1::uuid)`, id).Scan(&mine); err != nil {
			f.t.Fatal(err)
		}
		if !mine {
			continue
		}
		for _, q := range []string{
			`CREATE FUNCTION "` + schema + `".deny_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'denied'; END $$`,
			`CREATE TRIGGER deny BEFORE UPDATE ON "` + schema + `".runs FOR EACH ROW EXECUTE FUNCTION "` + schema + `".deny_update()`,
		} {
			if _, err := conn.Exec(f.ctx, q); err != nil {
				f.t.Fatal(err)
			}
		}
		return func() {
			if _, err := conn.Exec(f.ctx, `DROP TRIGGER deny ON "`+schema+`".runs`); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	f.t.Fatalf("run %s is in none of %v", id, schemas)
	return nil
}

// A dismissal that is stored is audited when it is, so a re-decision that
// then fails and is retried leaves one entry, not none and not two.
func TestDismissAuditedEvenWhenRedecidingFails(t *testing.T) {
	f := newCommandFixture(t)
	id := f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	restore := f.denyRunUpdates(id)
	at := f.Now()
	c := f.say(1, "mo", "dismiss", "abc123", "constant", at)

	f.run()

	f.wantReactions(1, c, eyes)
	if ds := f.dismissals(1); len(ds) != 1 {
		t.Fatalf("dismissals = %+v, want the stored one", ds)
	}
	if n := countAction(f.audit(), "finding_dismissed"); n != 1 {
		t.Fatalf("%d dismissals audited after the failed re-decision, want 1\n%s", n, f.Logs())
	}

	restore()
	f.SetNow(f.Now().Add(time.Minute))
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	if got := f.Run(id).State; got != store.Reported {
		t.Errorf("run is %s, want reported to be decided again", got)
	}
	if n := countAction(f.audit(), "finding_dismissed"); n != 1 {
		t.Errorf("%d dismissals audited after the retry, want still 1", n)
	}
}

// The audit names the head of the review the finding was found in, and a
// change that moved on since (nothing to re-decide) doesn't hold the
// command up.
func TestDismissOnReviewOfAnEarlierHead(t *testing.T) {
	f := newCommandFixture(t)
	old := f.head
	id := f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	f.head = sha('c')
	f.serveMR(1, false)

	c := f.say(1, "mo", "dismiss", "abc123", "constant", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if got := f.Run(id).State; got != store.Published {
		t.Errorf("run is %s; the change moved on, so nothing is decided again", got)
	}
	var detail string
	for _, e := range f.audit() {
		if e.Action == "finding_dismissed" {
			detail = e.Detail
		}
	}
	if !strings.Contains(detail, "head="+old) {
		t.Errorf("audit detail %q lacks the reviewed head %s", detail, old)
	}
}

// What a reason says, a secret included, never reaches the audit log.
func TestAuditReasonIsRedacted(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	f.say(1, "mo", "dismiss", "abc123", "see https://x.example/?k="+testLLMKey, time.Now())
	f.run()

	for _, e := range f.audit() {
		if strings.Contains(e.Detail, testLLMKey) {
			t.Errorf("audit detail %q holds a secret", e.Detail)
		}
	}
	if n := countAction(f.audit(), "finding_dismissed"); n != 1 {
		t.Errorf("%d dismissals audited, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Dismissing from a finding's thread

// recForge records the reactions and plain replies made through a Forge.
type recForge struct {
	forge.Forge
	calls *[]string
}

func (f recForge) React(_ context.Context, project, iid, comment int64, r forge.Reaction) error {
	*f.calls = append(*f.calls, fmt.Sprintf("react %d/%d/%d %s", project, iid, comment, r))
	return nil
}

func (f recForge) Reply(_ context.Context, project, iid int64, body string) error {
	*f.calls = append(*f.calls, fmt.Sprintf("reply %d/%d %s", project, iid, body))
	return nil
}

// recThreadForge is a recForge whose platform has threads.
type recThreadForge struct {
	recForge
	forge.Inline
}

func (f recThreadForge) ReactThread(_ context.Context, project, iid, comment int64, r forge.Reaction) error {
	*f.calls = append(*f.calls, fmt.Sprintf("react-thread %d/%d/%d %s", project, iid, comment, r))
	return nil
}

func (f recThreadForge) ReplyThread(_ context.Context, project, iid int64, t forge.ThreadRef, body string) error {
	*f.calls = append(*f.calls, fmt.Sprintf("reply-thread %d/%d %s#%d %s", project, iid, t.ID, t.CommentID, body))
	return nil
}

func TestReactionsAndRepliesOfAThreadCommand(t *testing.T) {
	mr := store.MRKey{Instance: testInstance, ProjectID: 42, IID: 7}
	plain := store.Command{MR: mr, CommentID: 900}
	github := store.Command{MR: mr, CommentID: 901, ReplyTo: 500}
	glReply := store.Command{MR: mr, CommentID: 902, Discussion: "d1"}
	ref := &forge.ThreadRef{ID: "PRRT_x", CommentID: 500}

	tests := []struct {
		name   string
		cmd    store.Command
		inline bool
		thread *forge.ThreadRef
		want   []string
	}{
		{"plain comment", plain, true, nil, []string{"react 42/7/900 seen", "reply 42/7 hi"}},
		{"GitHub reply reacts to its own comment", github, true, ref, []string{"react-thread 42/7/901 seen", "reply-thread 42/7 PRRT_x#500 hi"}},
		{"GitLab reply", glReply, true, ref, []string{"react-thread 42/7/902 seen", "reply-thread 42/7 PRRT_x#500 hi"}},
		{"a thread that isn't known gets a plain comment", github, true, nil, []string{"react-thread 42/7/901 seen", "reply 42/7 hi"}},
		{"a platform without threads", github, false, ref, []string{"react 42/7/901 seen", "reply 42/7 hi"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			var f forge.Forge = recForge{calls: &calls}
			if tt.inline {
				f = recThreadForge{recForge: recForge{calls: &calls}}
			}
			ctx := context.Background()
			if err := reactOn(ctx, f, tt.cmd, forge.ReactSeen); err != nil {
				t.Fatal(err)
			}
			if err := replyOn(ctx, f, tt.cmd, tt.thread, "hi"); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(calls, tt.want) {
				t.Errorf("calls = %q, want %q", calls, tt.want)
			}
		})
	}
}

func TestThreadReason(t *testing.T) {
	long := strings.Repeat("é", 600)
	tests := []struct{ name, arg, id, reason, want string }{
		{"no id written", "", "abc123", "false positive", "false positive"},
		{"the finding's id", "abc123", "abc123", "false positive", "false positive"},
		{"the finding's id, any case", "ABC123", "abc123", "constant", "constant"},
		{"not the finding's id is the start of the reason", "deadbe", "abc123", "looks fine", "deadbe looks fine"},
		{"not the finding's id, nothing else", "deadbe", "abc123", "", "deadbe"},
		{"nothing at all", "", "abc123", "", ""},
		{"the first word keeps its case", "Decade", "abc123", "old path", "Decade old path"},
		{"the first word keeps its case, nothing else", "Decade", "abc123", "", "Decade"},
		{"kept within the longest reason", "deadbe", "abc123", long, string([]rune("deadbe " + long)[:500])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := threadReason(tt.arg, tt.id, tt.reason); got != tt.want {
				t.Errorf("threadReason(%q, %q, %q) = %q, want %q", tt.arg, tt.id, tt.reason, got, tt.want)
			}
		})
	}
}

// The ask for a reason shows the form of the place it was written: a thread
// names no ID.
func TestReplyNeedsReasonText(t *testing.T) {
	if got, want := replyNeedsReason("pergent-review", false), "dismissing a blocking finding needs a reason: `@pergent-review dismiss <id> <reason>`"; got != want {
		t.Errorf("in a comment: %q, want %q", got, want)
	}
	if got, want := replyNeedsReason("pergent-review", true), "dismissing a blocking finding needs a reason: `@pergent-review dismiss <reason>`"; got != want {
		t.Errorf("in a thread: %q, want %q", got, want)
	}
	if got, want := replyThreadFindingGone, "This thread's finding isn't in the latest review, so there is nothing to dismiss."; got != want {
		t.Errorf("replyThreadFindingGone = %q, want %q", got, want)
	}
}

func TestReplyNotFindingThreadText(t *testing.T) {
	want := "This thread isn't one of pergent's findings. To dismiss a finding, reply in its thread, or comment `@pergent-review dismiss <id> <reason>`."
	if got := replyNotFindingThread("pergent-review"); got != want {
		t.Errorf("replyNotFindingThread = %q, want %q", got, want)
	}
}

// threadDiscussion is the discussion of the finding thread of the tests.
const threadDiscussion = "disc1"

// findingThread records the thread pergent.sh posted for the finding with
// fingerprint fp, and serves its replies.
func (f *commandFixture) findingThread(fp string) {
	f.t.Helper()
	err := f.Store.PutInlineThread(f.ctx, store.InlineThread{
		MR: f.MRKey(1), Fingerprint: fp, Skill: "security", File: "db/query.go", Line: 12, EndLine: 12,
		Commit: f.head, ThreadID: threadDiscussion, CommentID: 1500, State: "open",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.GitLab.Reply(http.MethodPost, discussionNotesPath(1, threadDiscussion), http.StatusCreated, `{}`)
}

// sayInThread is say written as a reply in a thread of discussion.
func (f *commandFixture) sayInThread(iid int64, user, discussion, kind, arg, reason string, at time.Time) int64 {
	f.t.Helper()
	f.comments++
	id := 1000 + f.comments
	f.GitLab.Reply(http.MethodPost, emojiPath(iid, id), http.StatusCreated, `{}`)
	_, err := f.Store.InsertCommand(f.ctx, store.Command{
		MR:          store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: iid},
		ProjectPath: testProjectPath, Platform: "gitlab", Kind: kind, Arg: arg, Reason: reason,
		CommentID: id, Author: user, AuthorID: commandUsers[user], CreatedAt: at, Discussion: discussion,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// threadReplies are the replies the bot posted in the discussion.
func (f *commandFixture) threadReplies(discussion string) []string {
	var out []string
	for _, r := range f.GitLab.Requests(http.MethodPost, discussionNotesPath(1, discussion)) {
		out = append(out, fmt.Sprint(r.JSON(f.t)["body"]))
	}
	return out
}

func (f *commandFixture) wantThreadReplies(discussion string, want ...string) {
	f.t.Helper()
	if got := f.threadReplies(discussion); !slices.Equal(got, want) {
		f.t.Errorf("replies in %s = %q, want %q", discussion, got, want)
	}
}

func TestThreadDismissSuggestion(t *testing.T) {
	f := newCommandFixture(t)
	id := f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "dev", threadDiscussion, "dismiss", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	f.wantThreadReplies(threadDiscussion) // "Dismissed by …" is the publisher's, after the re-publish
	ds := f.dismissals(1)
	if len(ds) != 1 || ds[0].Fingerprint != sqlFingerprint || ds[0].Severity != "suggestion" || ds[0].Actor != "dev (gitlab main)" {
		t.Fatalf("dismissals = %+v", ds)
	}
	if got := f.Run(id).State; got != store.Reported {
		t.Errorf("run is %s, want reported to be published again", got)
	}
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued; a dismissal starts none", n)
	}
}

func TestThreadDismissBlockingWithAReason(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "abc123", "the id is an integer", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Severity != "blocking" || ds[0].Reason != "the id is an integer" {
		t.Fatalf("dismissals = %+v", ds)
	}
}

func TestThreadDismissBlockingNeedsAReason(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)
	f.wantThreadReplies(threadDiscussion, "dismissing a blocking finding needs a reason: `@pergent-review dismiss <reason>`")
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

func TestThreadDismissBlockingByAuthorRefused(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "maya", threadDiscussion, "dismiss", "", "I know better", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)
	f.wantThreadReplies(threadDiscussion, replyAuthor)
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

func TestThreadDismissWithoutWriteAccessRefused(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "rep", threadDiscussion, "dismiss", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantThreadReplies(threadDiscussion, replyNoPermission)
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

func TestThreadDismissUnknownThread(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
	f.findingThread(sqlFingerprint)

	// A reply in a thread that is not one of pergent's findings.
	c := f.sayInThread(1, "mo", "someone-elses", "dismiss", "", "not mine", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, "This thread isn't one of pergent's findings. To dismiss a finding, reply in its thread, or comment `@pergent-review dismiss <id> <reason>`.")
	f.wantThreadReplies(threadDiscussion)
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

// A thread whose finding is not in the latest review any more has nothing to
// dismiss.
func TestThreadDismissFindingGone(t *testing.T) {
	f := newCommandFixture(t)
	f.review(reviewed("no_blocking_issues", "db/query.go"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "", "fine", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantThreadReplies(threadDiscussion, "This thread's finding isn't in the latest review, so there is nothing to dismiss.")
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

// A first word that looks like an ID but is not the finding's is the start of
// the reason.
func TestThreadDismissArgNotIDIsReason(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "deadbe", "looks fine", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Reason != "deadbe looks fine" {
		t.Fatalf("dismissals = %+v, want the reason \"deadbe looks fine\"", ds)
	}
}

// The first word of a reason that looks like an ID keeps the case it was
// written in.
func TestThreadDismissReasonKeepsTheCaseOfItsFirstWord(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "Decade", "old path", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Reason != "Decade old path" {
		t.Fatalf("dismissals = %+v, want the reason \"Decade old path\"", ds)
	}
}

// "dismiss <id> <reason>" in a thread that isn't pergent's is the plain
// command: the ID says which finding.
func TestThreadDismissByIDInSomeOtherThread(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.sayInThread(1, "mo", "someone-elses", "dismiss", "abc123", "a constant", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	if ds := f.dismissals(1); len(ds) != 1 || ds[0].Fingerprint != sqlFingerprint || ds[0].Reason != "a constant" {
		t.Fatalf("dismissals = %+v, want the finding dismissed with its reason", ds)
	}
}

func TestThreadDismissByUnknownIDInSomeOtherThread(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.sayInThread(1, "mo", "someone-elses", "dismiss", "deadbe", "nope", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, replyUnknownID("deadbe"))
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
}

// Two dismissals in two threads within the 30 seconds, whose reasons start
// with the same hex-looking word, are two commands: no repeat check applies
// to a dismissal in a thread.
func TestThreadDismissalsWithTheSameFirstWordAreNotRepeats(t *testing.T) {
	f := newCommandFixture(t)
	f.review(twoFindings(t))
	f.findingThread(sqlFingerprint)
	err := f.Store.PutInlineThread(f.ctx, store.InlineThread{
		MR: f.MRKey(1), Fingerprint: "654321fedcba", Skill: "security", File: "db/other.go", Line: 3, EndLine: 3,
		Commit: f.head, ThreadID: "disc2", CommentID: 1501, State: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.GitLab.Reply(http.MethodPost, discussionNotesPath(1, "disc2"), http.StatusCreated, `{}`)
	at := time.Now()

	a := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "deadbeef", "is a constant here", at)
	b := f.sayInThread(1, "mo", "disc2", "dismiss", "deadbeef", "is a constant there", at.Add(time.Second))
	f.run()

	f.wantReactions(1, a, eyes, up)
	f.wantReactions(1, b, eyes, up)
	if ds := f.dismissals(1); len(ds) != 2 {
		t.Errorf("dismissals = %+v, want two", ds)
	}
}

func TestThreadDismissAlreadyDismissedReplyIsInTheThread(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
	f.findingThread(sqlFingerprint)
	at := time.Now()

	f.sayInThread(1, "dev", threadDiscussion, "dismiss", "", "", at)
	f.run()
	f.SetNow(f.Now().Add(commandThrottleWindow + time.Second))
	c := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "", "", at.Add(time.Minute))
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	f.wantThreadReplies(threadDiscussion, replyAlreadyDismissed("abc123"))
}

// Dismissals in two threads within the 30 seconds are two commands, not a
// repeat: neither names an ID.
func TestThreadDismissalsInTwoThreadsAreNotRepeats(t *testing.T) {
	f := newCommandFixture(t)
	f.review(twoFindings(t))
	f.findingThread(sqlFingerprint)
	err := f.Store.PutInlineThread(f.ctx, store.InlineThread{
		MR: f.MRKey(1), Fingerprint: "654321fedcba", Skill: "security", File: "db/query.go", Line: 20, EndLine: 20,
		Commit: f.head, ThreadID: "disc2", CommentID: 1501, State: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()

	a := f.sayInThread(1, "mo", threadDiscussion, "dismiss", "", "first", at)
	b := f.sayInThread(1, "mo", "disc2", "dismiss", "", "second", at.Add(time.Second))
	f.run()

	f.wantReactions(1, a, eyes, up)
	f.wantReactions(1, b, eyes, up)
	if ds := f.dismissals(1); len(ds) != 2 {
		t.Errorf("dismissals = %+v, want two", ds)
	}
}

// Other commands written in a thread work as in a comment; their reply goes
// in the thread when it is one of pergent's, and their reaction on the reply.
func TestHelpInAFindingThread(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "mo", threadDiscussion, "help", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	f.wantThreadReplies(threadDiscussion, help)
}

func TestHelpInSomeOtherThreadIsAPlainComment(t *testing.T) {
	f := newCommandFixture(t)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))

	c := f.sayInThread(1, "mo", "someone-elses", "help", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1, help)
}

// ---------------------------------------------------------------------------
// The command gate: only the author of a change and the repository's
// maintainers use commands.

// gateReply is the refusal of the gate on a GitLab merge request.
const gateReply = "Only the author of this MR and the repository's maintainers can use pergent commands."

func TestReplyOnlyAuthorAndMaintainers(t *testing.T) {
	for platform, want := range map[string]string{
		"gitlab": "Only the author of this MR and the repository's maintainers can use pergent commands.",
		"github": "Only the author of this PR and the repository's maintainers can use pergent commands.",
	} {
		if got := replyOnlyAuthorAndMaintainers(changeTerm(platform)); got != want {
			t.Errorf("%s: reply = %q, want %q", platform, got, want)
		}
	}
}

// Refusals about who may are audited; no other outcome is.
func TestWhoMayOutcome(t *testing.T) {
	for outcome, want := range map[string]bool{
		refused(whyNotAuthorOrMaintainer, "x").outcome: true,
		refused(whyNoPermission, "x").outcome:          true,
		refused(whyAuthorOfChange, "x").outcome:        true,
		"refused: not the author or a maintainer":      true,
		"refused: no permission":                       true,
		"refused: author of the change":                true,
		refused("draft", "x").outcome:                  false,
		refused("unknown finding", "x").outcome:        false,
		refused("reason needed", "x").outcome:          false,
		refused("re-run limit reached", "x").outcome:   false,
		refused("not a finding thread", "x").outcome:   false,
		store.CommandDone:                              false,
		store.CommandNoop("help"):                      false,
		store.CommandThrottled:                         false,
		dropped("the author is a bot").outcome:         false,
		"gave up: boom":                                false,
	} {
		if got := whoMayOutcome(outcome); got != want {
			t.Errorf("whoMayOutcome(%q) = %v, want %v", outcome, got, want)
		}
	}
}

// The author is told by known IDs only: an unknown ID, on either side, is
// nobody.
func TestIsChangeAuthor(t *testing.T) {
	for _, tc := range []struct {
		name            string
		commenter, made int64
		want            bool
	}{
		{"same account", 53, 53, true},
		{"another account", 51, 53, false},
		{"change author unknown", 51, 0, false},
		{"commenter unknown", 0, 53, false},
		{"both unknown", 0, 0, false},
	} {
		c := &commandRun{cmd: store.Command{AuthorID: tc.commenter}, change: forge.Change{AuthorID: tc.made}}
		if got := c.isChangeAuthor(); got != tc.want {
			t.Errorf("%s: isChangeAuthor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// refusals are the command_refused entries of the audit log, newest first.
func (f *commandFixture) refusals() []store.AuditEntry {
	f.t.Helper()
	var out []store.AuditEntry
	for _, e := range f.audit() {
		if e.Action == "command_refused" {
			out = append(out, e)
		}
	}
	return out
}

// wantRefusal checks one audit entry of a refusal: who, which change and
// head, and which command.
func (f *commandFixture) wantRefusal(e store.AuditEntry, actor string, iid int64, command string) {
	f.t.Helper()
	if e.Actor != actor || e.Instance != testInstance || e.Project != testProjectPath {
		f.t.Errorf("refusal audited as %+v, want actor %q on %s/%s", e, actor, testInstance, testProjectPath)
	}
	want := fmt.Sprintf("mr=%d head=%s command=%s", iid, f.head, command)
	if e.Detail != want {
		f.t.Errorf("refusal detail = %q, want %q", e.Detail, want)
	}
}

func TestCommandGateRefusesDeveloper(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillOf(t, "security", "no_blocking_issues"))

	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, gateReply)
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued by a refused command, want none", n)
	}
	rs := f.refusals()
	if len(rs) != 1 {
		t.Fatalf("%d command_refused entries, want 1: %+v", len(rs), f.audit())
	}
	f.wantRefusal(rs[0], "dev (gitlab main)", 1, "review")
	if n := countAction(f.audit(), "review_rerun"); n != 0 {
		t.Errorf("%d re-runs audited, want none", n)
	}
}

// The author may always use commands, whatever their access: a Reporter here.
func TestCommandGateAllowsAuthorWithReadAccess(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.authorID = userReporter
	f.serveMR(1, false)
	f.review(skillOf(t, "security", "no_blocking_issues"))

	c := f.say(1, "rep", "review", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	if n := len(f.queued()); n != 1 {
		t.Errorf("%d runs queued, want the re-run", n)
	}
	if rs := f.refusals(); len(rs) != 0 {
		t.Errorf("the author was audited as refused: %+v", rs)
	}
}

func TestCommandGateAllowsMaintainer(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillOf(t, "security", "no_blocking_issues"))

	c := f.say(1, "mo", "review", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, up)
	f.wantReplies(1)
	if n := len(f.queued()); n != 1 {
		t.Errorf("%d runs queued, want the re-run", n)
	}
	if rs := f.refusals(); len(rs) != 0 {
		t.Errorf("a maintainer was audited as refused: %+v", rs)
	}
}

// When the platform doesn't report who opened the change, nobody is the
// author: a Developer is refused.
func TestCommandGateUnknownAuthorIsNotAuthor(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.authorID = 0
	f.serveMR(1, false)
	f.review(skillOf(t, "security", "no_blocking_issues"))

	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, gateReply)
	if n := len(f.queued()); n != 0 {
		t.Errorf("%d runs queued, want none", n)
	}
	if rs := f.refusals(); len(rs) != 1 {
		t.Errorf("%d command_refused entries, want 1", len(rs))
	}
}

// help and commands the bot doesn't know are refused too.
func TestCommandGateRefusesHelp(t *testing.T) {
	for _, kind := range []string{"help", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			f := newCommandFixture(t)
			f.gate(true)

			c := f.say(1, "dev", kind, "", "", time.Now())
			f.run()

			f.wantReactions(1, c, eyes, down)
			f.wantReplies(1, gateReply)
			rs := f.refusals()
			if len(rs) != 1 {
				t.Fatalf("%d command_refused entries, want 1", len(rs))
			}
			f.wantRefusal(rs[0], "dev (gitlab main)", 1, kind)
		})
	}
}

// A person is told once per change; every refusal is still audited.
func TestRefusalRepliedOncePerPersonPerChange(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	at := time.Now()

	first := f.say(1, "dev", "review", "", "", at)
	f.run()
	f.wantReactions(1, first, eyes, down)
	f.wantReplies(1, gateReply)

	// Once the throttle window has passed, the same person's next command
	// on the change is refused with a 👎 only.
	f.SetNow(f.Now().Add(commandThrottleWindow + time.Second))
	second := f.say(1, "dev", "help", "", "", at.Add(3*time.Minute))
	f.run()
	f.wantReactions(1, second, eyes, down)
	f.wantReplies(1, gateReply)
	if rs := f.refusals(); len(rs) != 2 {
		t.Errorf("%d command_refused entries, want both refusals audited", len(rs))
	}

	// Their first refusal on another change is answered with the reply.
	f.serveMR(2, false)
	elsewhere := f.say(2, "dev", "review", "", "", at.Add(4*time.Minute))
	f.run()
	f.wantReactions(2, elsewhere, eyes, down)
	f.wantReplies(2, gateReply)
	f.wantReplies(1, gateReply)
	if rs := f.refusals(); len(rs) != 3 {
		t.Errorf("%d command_refused entries, want 3", len(rs))
	}
}

// A refusal whose reply failed is tried again; it is audited when it has been
// answered, once.
func TestRefusalAuditedOnceAcrossRetry(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillOf(t, "security", "no_blocking_issues"))
	f.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusBadRequest, `{"message":"boom"}`)

	c := f.say(1, "dev", "review", "", "", time.Now())
	f.run()
	f.wantReactions(1, c, eyes)
	if rs := f.refusals(); len(rs) != 0 {
		t.Fatalf("%d command_refused entries while the reply failed, want none", len(rs))
	}

	f.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusCreated, map[string]any{"id": createdNote})
	f.SetNow(f.Now().Add(time.Minute))
	f.run()

	f.wantReactions(1, c, eyes, down)
	// The reply was posted twice: the failed attempt and the retry, which
	// didn't take its own earlier attempt for a refusal "before" and so
	// didn't leave the reply out.
	f.wantReplies(1, gateReply, gateReply)
	if rs := f.refusals(); len(rs) != 1 {
		t.Errorf("%d command_refused entries after the retry, want exactly 1", len(rs))
	}
}

// With the switch off, the author is still allowed whatever their access,
// and a Reporter who is not the author is refused as before, and audited.
func TestAuthorAllowedWhenSwitchOff(t *testing.T) {
	t.Run("the author re-runs", func(t *testing.T) {
		f := newCommandFixture(t)
		f.gate(false)
		f.authorID = userReporter
		f.serveMR(1, false)
		f.review(skillOf(t, "security", "no_blocking_issues"))

		r := f.say(1, "rep", "review", "", "", time.Now())
		f.run()

		f.wantReactions(1, r, eyes, up)
		if n := len(f.queued()); n != 1 {
			t.Errorf("%d runs queued, want the author's re-run", n)
		}
		f.wantReplies(1)
		if rs := f.refusals(); len(rs) != 0 {
			t.Errorf("the author was audited as refused: %+v", rs)
		}
	})
	// (A separate fixture: a dismissal moves the review to "reported", and a
	// re-run of a head with a reported run is answered as already queued.)
	t.Run("the author dismisses a suggestion", func(t *testing.T) {
		f := newCommandFixture(t)
		f.gate(false)
		f.authorID = userReporter
		f.serveMR(1, false)
		f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))

		d := f.say(1, "rep", "dismiss", "abc123", "", time.Now())
		f.run()

		f.wantReactions(1, d, eyes, up)
		if ds := f.dismissals(1); len(ds) != 1 || ds[0].Severity != "suggestion" || ds[0].Actor != "rep (gitlab main)" {
			t.Fatalf("dismissals = %+v, want the suggestion dismissed by the author", ds)
		}
		f.wantReplies(1)
		if rs := f.refusals(); len(rs) != 0 {
			t.Errorf("the author was audited as refused: %+v", rs)
		}
	})
	t.Run("a reporter who is not the author", func(t *testing.T) {
		f := newCommandFixture(t)
		f.gate(false)
		f.review(skillOf(t, "security", "no_blocking_issues"))

		c := f.say(1, "rep", "review", "", "", time.Now())
		f.run()

		f.wantReactions(1, c, eyes, down)
		f.wantReplies(1, replyNoPermission)
		if n := len(f.queued()); n != 0 {
			t.Errorf("%d runs queued, want none", n)
		}
		rs := f.refusals()
		if len(rs) != 1 {
			t.Fatalf("%d command_refused entries, want 1", len(rs))
		}
		f.wantRefusal(rs[0], "rep (gitlab main)", 1, "review")
	})
}

// The author, a maintainer, still can't dismiss a blocking finding of their
// own change, and the refusal is audited.
func TestAuthorBlockingDismissStillRefusedAndAudited(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "maya", "dismiss", "abc123", "I know better", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, replyAuthor)
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
	rs := f.refusals()
	if len(rs) != 1 {
		t.Fatalf("%d command_refused entries, want 1", len(rs))
	}
	f.wantRefusal(rs[0], "maya (gitlab main)", 1, "dismiss")
}

// A blocking dismissal by someone without maintainer access is refused as
// "no permission" and audited, with the switch off.
func TestBlockingDismissWithoutMaintainAccessAudited(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(false)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	c := f.say(1, "dev", "dismiss", "abc123", "constant", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1, replyNoPermission)
	rs := f.refusals()
	if len(rs) != 1 {
		t.Fatalf("%d command_refused entries, want 1", len(rs))
	}
	f.wantRefusal(rs[0], "dev (gitlab main)", 1, "dismiss")
}

// Ordinary mistakes are not audited.
func TestOrdinaryRefusalsAreNotAudited(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillWithFinding(t, "has_blocking_issues", "blocking"))

	unknown := f.say(1, "mo", "dismiss", "ffffff", "reason", time.Now())
	f.run()

	f.wantReactions(1, unknown, eyes, down)
	if rs := f.refusals(); len(rs) != 0 {
		t.Errorf("an unknown finding ID was audited: %+v", rs)
	}
}

// A dismissal written in a finding's thread is gated too, and the refusal is
// answered in the thread.
func TestThreadCommandGate(t *testing.T) {
	f := newCommandFixture(t)
	f.gate(true)
	f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
	f.findingThread(sqlFingerprint)

	c := f.sayInThread(1, "dev", threadDiscussion, "dismiss", "", "not a problem", time.Now())
	f.run()

	f.wantReactions(1, c, eyes, down)
	f.wantReplies(1)
	f.wantThreadReplies(threadDiscussion, gateReply)
	if ds := f.dismissals(1); len(ds) != 0 {
		t.Errorf("dismissals = %+v, want none", ds)
	}
	rs := f.refusals()
	if len(rs) != 1 {
		t.Fatalf("%d command_refused entries, want 1", len(rs))
	}
	f.wantRefusal(rs[0], "dev (gitlab main)", 1, "dismiss")
}

// The author with only read access dismisses a suggestion in its finding's
// thread, whether the gate is on or off.
func TestThreadDismissByAuthorWithReadAccess(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate %v", on), func(t *testing.T) {
			f := newCommandFixture(t)
			f.gate(on)
			f.authorID = userReporter
			f.serveMR(1, false)
			f.review(skillWithFinding(t, "no_blocking_issues", "suggestion"))
			f.findingThread(sqlFingerprint)

			c := f.sayInThread(1, "rep", threadDiscussion, "dismiss", "", "", time.Now())
			f.run()

			f.wantReactions(1, c, eyes, up)
			f.wantReplies(1)
			f.wantThreadReplies(threadDiscussion)
			ds := f.dismissals(1)
			if len(ds) != 1 || ds[0].Fingerprint != sqlFingerprint || ds[0].Severity != "suggestion" || ds[0].Actor != "rep (gitlab main)" {
				t.Fatalf("dismissals = %+v, want the suggestion dismissed by the author", ds)
			}
			if rs := f.refusals(); len(rs) != 0 {
				t.Errorf("the author was audited as refused: %+v", rs)
			}
		})
	}
}
