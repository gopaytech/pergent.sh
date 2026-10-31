package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// cmdE2E is the dashboard end-to-end environment with comment commands on:
// the webhook handler stores what people write in notes, the engine's
// command step answers it, and the dashboard's buttons act on the same
// store.
type cmdE2E struct {
	*dashE2E
	// next is the last comment ID handed out.
	next int64
}

func newCommandsE2E(t *testing.T) *cmdE2E {
	t.Helper()
	// The dashboard's buttons wake the engine, as the binary wires them; the
	// engine exists only once the environment is built.
	var eng *Engine
	e := newDashboardE2E(t, func(o *dashboard.Options) { o.Wake = func() { eng.Wake() } })
	env := e.env
	eng = env.Engine
	// What main passes as Bots: the review bot's mention, and the bots whose
	// comments are never commands.
	bots := func(instance string) (string, []string, bool) {
		p, ok := env.Engine.Platforms.Get(instance)
		if !ok || p.Forge == nil {
			return "", nil, false
		}
		return p.Forge.Mention(), []string{p.Forge.Bot().Username, p.Approver.Bot().Username}, true
	}
	mux := server.Routes(server.Deps{
		Config: env.Engine.Config, Instances: env.Engine.Instances, WebhookSecrets: env.Engine.WebhookSecrets,
		Store: env.Store, Bots: bots, Wake: env.Engine.Wake, Now: env.Now, Dashboard: e.dash.Handler(), Log: env.Engine.Log,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.e2e.srv = srv
	return &cmdE2E{dashE2E: e, next: 2000}
}

// say posts the Note Hook GitLab sends when userID (username) writes a new
// comment with body on merge request 1, and returns the comment's ID. The
// reaction route for that comment is served first.
func (e *cmdE2E) say(t *testing.T, username string, userID int64, body string) int64 {
	t.Helper()
	e.next++
	id := e.next
	e.env.GitLab.Reply(http.MethodPost, emojiPath(1, id), http.StatusCreated, `{}`)
	hook, _ := json.Marshal(map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"id": userID, "username": username},
		"project":     map[string]any{"id": testProjectID, "path_with_namespace": testProjectPath},
		"object_attributes": map[string]any{
			"id": id, "note": body, "noteable_type": "MergeRequest", "type": nil, "action": "create", "system": false,
		},
		"merge_request": map[string]any{"iid": 1},
	})
	status, resp, _, _ := e.do(t, http.MethodPost, "/webhooks/gitlab/"+testInstance, json.RawMessage(hook), nil, map[string]string{
		"X-Gitlab-Token": testWebhookSecret, "X-Gitlab-Event": "Note Hook",
	})
	if status != http.StatusOK {
		t.Fatalf("note webhook = %d: %s", status, resp)
	}
	return id
}

// answer runs the engine's command step once.
func (e *cmdE2E) answer(t *testing.T) {
	t.Helper()
	if err := e.env.Engine.CommandsOnce(e.env.ctx); err != nil {
		t.Fatalf("CommandsOnce: %v", err)
	}
}

// reactions are the emoji the review bot put on comment id, in order.
func (e *cmdE2E) reactions(t *testing.T, id int64) []string {
	t.Helper()
	var out []string
	for _, r := range e.env.GitLab.Requests(http.MethodPost, emojiPath(1, id)) {
		if r.Token != testReviewToken {
			t.Errorf("reaction on %d made with token %q, want the review bot's", id, r.Token)
		}
		out = append(out, r.JSON(t)["name"].(string))
	}
	return out
}

func (e *cmdE2E) wantReactions(t *testing.T, id int64, want ...string) {
	t.Helper()
	if got := e.reactions(t, id); !slices.Equal(got, want) {
		t.Errorf("reactions on comment %d = %v, want %v\n%s", id, got, want, e.env.Logs())
	}
}

// replies are the plain comments the review bot posted on merge request 1:
// every note it created that is not the review comment.
func (e *cmdE2E) replies() []string {
	var out []string
	for _, r := range e.env.GitLab.Requests(http.MethodPost, notesPath(1)) {
		var m map[string]any
		_ = json.Unmarshal(r.Body, &m)
		if body, _ := m["body"].(string); !strings.Contains(body, policy.Marker) {
			out = append(out, body)
		}
	}
	return out
}

func (e *cmdE2E) wantReplies(t *testing.T, want ...string) {
	t.Helper()
	if got := e.replies(); !slices.Equal(got, want) {
		t.Errorf("replies = %q, want %q", got, want)
	}
}

// lastComment is the body of the review comment as last written: the newest
// create or update of it.
func (e *cmdE2E) lastComment(t *testing.T) string {
	t.Helper()
	var last string
	for _, r := range e.env.GitLab.Requests("", "") {
		if (r.Method == http.MethodPost && r.Path == notesPath(1)) || (r.Method == http.MethodPut && r.Path == notePath(1, createdNote)) {
			if body, _ := r.JSON(t)["body"].(string); strings.Contains(body, policy.Marker) {
				last = body
			}
		}
	}
	if last == "" {
		t.Fatalf("no review comment was posted")
	}
	return last
}

// statuses are the states of the commit statuses set on head, in order.
func (e *cmdE2E) statuses(t *testing.T, head string) []string {
	t.Helper()
	var out []string
	for _, r := range e.env.GitLab.Requests(http.MethodPost, statusPath(head)) {
		out = append(out, r.JSON(t)["state"].(string))
	}
	return out
}

func (e *cmdE2E) wantStatuses(t *testing.T, head string, want ...string) {
	t.Helper()
	if got := e.statuses(t, head); !slices.Equal(got, want) {
		t.Errorf("commit statuses of %s = %v, want %v", head[:7], got, want)
	}
}

func (e *cmdE2E) calls(method, path string) int {
	return len(e.env.GitLab.Requests(method, path))
}

// Comment commands and the dashboard's buttons, from the webhook to the
// platform, on one merge request: a blocking finding is dismissed by the
// people who may and refused to those who may not, restored on the
// dashboard (which withdraws the approval), a failed skill is re-run alone
// and merged with the rest, and the re-run cap and the 30-second rule hold.
func TestEndToEndCommands(t *testing.T) {
	e := newCommandsE2E(t)
	env := e.env
	gl := env.GitLab
	// Two skills, so a re-run of the failed one leaves the other alone, and a
	// cap of one re-run per merge request.
	env.Engine.Library["style"] = []byte("---\nname: style\n---\nCheck the style.\n")
	one := 1
	env.Engine.Config.Repositories[0].RerunLimitPerMR = &one

	// People: maya wrote the change and is a maintainer; mo is a maintainer;
	// dev a developer.
	const (
		maya, mo, dev = "maya", "mo", "dev"
		fp            = "aaaaaaaaaaaa"
		id            = "aaaaaa"
	)
	gl.Reply(http.MethodGet, membersPath(userAuthor), http.StatusOK, member(40))
	gl.Reply(http.MethodGet, membersPath(userMaintainer), http.StatusOK, member(40))
	gl.Reply(http.MethodGet, membersPath(userDeveloper), http.StatusOK, member(30))
	// serve routes everything a review of head needs.
	serve := func(head string) {
		servePublish(env, 1, head)
		mr := openMR(1, head)
		mr.Author = gitlab.User{ID: userAuthor, Username: maya}
		gl.SetMR(mr)
		gl.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
		gl.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	}
	// review opens or moves the merge request to head, dispatches its Job and
	// delivers res as its result, then publishes it.
	review := func(head, oldrev string, res []byte, known ...string) string {
		t.Helper()
		serve(head)
		action := "open"
		if oldrev != "" {
			action = "update"
		}
		e.hook(t, action, 1, head, oldrev)
		dispatch(t, env)
		run := e.newJob(t, known...)
		token := string(env.Secret(run).Data["result-token"])
		if code := e.postResult(t, run, token, res); code != http.StatusOK {
			t.Fatalf("result of %s: status %d", head[:7], code)
		}
		publish(t, env)
		wantPublished(t, env, run)
		return run
	}

	// 1. The merge request is opened at A: security has a blocking finding.
	// The review is published: failed, not approved.
	headA, headB := sha('a'), sha('c')
	runA := review(headA, "", resultFor(t, headA,
		namedSkill(t, "security", "has_blocking_issues", "blocking", fp), namedSkill(t, "style", "no_blocking_issues", "", "")))
	e.wantStatuses(t, headA, "running", "failed")
	wantNoApproval(t, env, 1)

	// 2. A developer may not dismiss a blocking finding: 👎 and a reply.
	c := e.say(t, dev, userDeveloper, "@pergent-review dismiss "+id+" not a real problem")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, "you don't have permission to do that here")
	if ds := mustDismissals(t, env); len(ds) != 0 {
		t.Fatalf("dismissals after a developer's command = %+v, want none", ds)
	}

	// 3. The author, a maintainer, may not dismiss one on their own change.
	c = e.say(t, maya, userAuthor, "@pergent-review dismiss "+id+" it is fine")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, "you don't have permission to do that here", "the author of a change can't dismiss its blocking findings")

	// A word that is not a command is told what the bot understands, and
	// restoring is not one of them: it is a dashboard button.
	c = e.say(t, dev, userDeveloper, "@pergent-review restore "+id)
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	if got := e.replies(); len(got) != 3 || !strings.HasPrefix(got[2], "Commands: `@pergent-review review`") {
		t.Fatalf("replies = %q, want the list of commands as the third", got)
	}

	// 4. A maintainer dismisses it with a reason: 👍, the comment is written
	// again with a Dismissed section, and the review is approved.
	c = e.say(t, mo, userMaintainer, "@pergent-review dismiss "+id+" the input is a constant")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	if ds := mustDismissals(t, env); len(ds) != 1 || ds[0].Fingerprint != fp || ds[0].Severity != "blocking" ||
		ds[0].Actor != "mo (gitlab main)" || ds[0].Reason != "the input is a constant" {
		t.Fatalf("dismissals = %+v, want the finding dismissed by mo", ds)
	}
	if n := len(env.Jobs()); n != 1 {
		t.Fatalf("a dismissal started a run: %d Jobs", n)
	}
	publish(t, env)
	body := e.lastComment(t)
	for _, want := range []string{
		"Dismissed (1)", "finding of security", "`" + id + "`",
		"dismissed by mo (gitlab main): the input is a constant", "✅ **No blocking issues** (1 dismissed)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment after the dismissal lacks %q:\n%s", want, body)
		}
	}
	e.wantStatuses(t, headA, "running", "failed", "success")
	if reqs := gl.Requests(http.MethodPost, approvePath(1)); len(reqs) != 1 || reqs[0].Token != testApproveToken || reqs[0].JSON(t)["sha"] != headA {
		t.Fatalf("approvals after the dismissal = %d, want one of A by the approve bot", len(reqs))
	}
	if got := recordedApproval(t, env, 1); got != headA {
		t.Fatalf("recorded approval = %q, want %s", got, headA)
	}

	// 5. alice, an admin of the repository, presses Restore on the review
	// page: the finding blocks again and the approval is withdrawn.
	alice := e.adminAlice(t)
	path := "/api/reviews/" + testInstance + "/" + testProjectPath + "/1/restore"
	if status, resp := e.write(t, alice, http.MethodPost, path, map[string]string{"fingerprint": fp}); status != http.StatusOK ||
		!strings.Contains(string(resp), `"restored"`) {
		t.Fatalf("restore = %d: %s", status, resp)
	}
	publish(t, env)
	body = e.lastComment(t)
	if strings.Contains(body, "Dismissed") || !strings.Contains(body, testMarkdown) {
		t.Errorf("comment after the restore = %q, want the review as delivered", body)
	}
	e.wantStatuses(t, headA, "running", "failed", "success", "failed")
	if reqs := gl.Requests(http.MethodPost, unapprovePath(1)); len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("unapprovals after the restore = %d, want one by the approve bot", len(reqs))
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval after the restore = %q, want none", got)
	}
	if ds := mustDismissals(t, env); len(ds) != 0 {
		t.Errorf("dismissals after the restore = %+v, want none", ds)
	}

	// 6. A push of B: security is clean, style failed.
	runB := review(headB, headA, resultFor(t, headB,
		namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "style", "failed", "", "")), runA)
	e.wantStatuses(t, headB, "running", "failed")
	if n := len(gl.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Fatalf("approvals = %d, want only the one of A", n)
	}

	// 7. "review failed" re-runs style alone: a Job with only that skill,
	// whose result is merged with security's into one review.
	known := []string{runA, runB}
	c = e.say(t, dev, userDeveloper, "@pergent-review review failed")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	dispatch(t, env)
	runC := e.newJob(t, known...)
	var inputs strings.Builder
	for _, v := range env.ConfigMap(runC).Data {
		inputs.WriteString(v)
	}
	if !strings.Contains(inputs.String(), "Check the style.") || strings.Contains(inputs.String(), "Look for security problems.") {
		t.Errorf("the re-run's inputs are not style alone:\n%s", inputs.String())
	}
	if r := env.Run(runC); r.Trigger != store.TriggerRerunFailed || !slices.Equal(r.SkillSubset, []string{"style"}) || r.BaseRunID == nil || *r.BaseRunID != runB {
		t.Errorf("run = %q %v %v, want a re-run of style on %s", r.Trigger, r.SkillSubset, r.BaseRunID, runB)
	}
	token := string(env.Secret(runC).Data["result-token"])
	if code := e.postResult(t, runC, token, resultFor(t, headB, namedSkill(t, "style", "no_blocking_issues", "", ""))); code != http.StatusOK {
		t.Fatalf("result of the re-run: status %d", code)
	}
	publish(t, env)
	wantPublished(t, env, runC)
	body = e.lastComment(t)
	for _, want := range []string{"### security", "### style"} {
		if !strings.Contains(body, want) {
			t.Errorf("merged comment lacks %q:\n%s", want, body)
		}
	}
	merged, err := result.Parse(env.Run(runC).Result)
	if err != nil || !merged.Merged || len(merged.Skills) != 2 || merged.Skills[0].Name != "security" || merged.Skills[1].Name != "style" {
		t.Errorf("stored result = %+v, %v; want security and style, merged", merged, err)
	}
	e.wantStatuses(t, headB, "running", "failed", "running", "success")
	if reqs := gl.Requests(http.MethodPost, approvePath(1)); len(reqs) != 2 || reqs[1].JSON(t)["sha"] != headB {
		t.Fatalf("approvals = %d, want a second one, of B", len(reqs))
	}

	// 8. The cap of one re-run per merge request is reached: a reply, 👎 and
	// nothing queued.
	jobs := len(env.Jobs())
	c = e.say(t, mo, userMaintainer, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	capReply := "re-run limit reached for this MR; it resets at 00:00 UTC"
	if got := e.replies(); got[len(got)-1] != capReply {
		t.Errorf("last reply = %q, want %q", got[len(got)-1], capReply)
	}
	dispatch(t, env)
	if n := len(env.Jobs()); n != jobs {
		t.Errorf("a command at the cap started a Job (%d, was %d)", n, jobs)
	}

	// 9. The same command again within 30 seconds is dropped: 👎, no reply.
	replies := len(e.replies())
	c = e.say(t, dev, userDeveloper, "@pergent-review review failed")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	if n := len(e.replies()); n != replies {
		t.Errorf("a repeated command got a reply: %q", e.replies()[replies:])
	}

	// 10. What is not a command from a person does nothing: the bot's own
	// comment, an account ending in [bot], and talk without the mention.
	for _, w := range []struct {
		user string
		body string
	}{
		{"pergent-review", "@pergent-review review"},
		{"release[bot]", "@pergent-review review"},
		{dev, "please review this, @mo"},
	} {
		c = e.say(t, w.user, userDeveloper, w.body)
		e.answer(t)
		e.wantReactions(t, c)
	}

	// The audit log tells who did what; the log holds no token.
	entries, err := env.Store.AuditLog(env.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	type audited struct{ action, actor string }
	var got []audited
	for _, a := range entries {
		switch a.Action {
		case "finding_dismissed", "finding_restored", "review_rerun", "review_rerun_failed":
			got = append(got, audited{a.Action, a.Actor})
			if !strings.Contains(a.Detail, "mr=1 head=") {
				t.Errorf("audit entry %s has detail %q, want the change and head", a.Action, a.Detail)
			}
		}
	}
	want := []audited{
		{"review_rerun_failed", "dev (gitlab main)"},
		{"finding_restored", "alice (gitlab main)"},
		{"finding_dismissed", "mo (gitlab main)"},
	}
	if !slices.Equal(got, want) { // newest first
		t.Errorf("audited actions = %v, want %v", got, want)
	}
	for _, secret := range []string{testReviewToken, testApproveToken, testWebhookSecret, aliceToken, testLLMKey} {
		if strings.Contains(env.Logs(), secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

func mustDismissals(t *testing.T, env *testEnv) []store.Dismissal {
	t.Helper()
	ds, err := env.Store.Dismissals(env.ctx, env.MRKey(1))
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// adminAlice signs alice in with GitLab and has the operator put her on an
// admin team, as the dashboard end-to-end test does.
func (e *cmdE2E) adminAlice(t *testing.T) dashUser {
	t.Helper()
	env := e.env
	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	operator := e.passwordSignIn(t, "admin", bootstrapPassword)
	if status, body := e.write(t, operator, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}
	team := e.newTeam(t, operator, "platform", store.RoleAdmin)
	alice := e.signIn(t, aliceCode)
	e.setTeam(t, operator, team, "platform", store.RoleAdmin, []int64{e.accountID(t, operator, "alice")}, nil)
	var me struct {
		Admin bool `json:"admin"`
	}
	e.getJSON(t, alice, "/api/me", &me)
	if !me.Admin {
		t.Fatalf("alice is not an admin after joining the team")
	}
	return alice
}
