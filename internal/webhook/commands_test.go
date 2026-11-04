package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

func TestParseCommand(t *testing.T) {
	const m = "pergent-review"
	tests := []struct {
		name, body        string
		kind, arg, reason string
		ok                bool
		mention           string
	}{
		{name: "review", body: "@pergent-review review", kind: "review", ok: true},
		{name: "review failed", body: "@pergent-review review failed", kind: "review_failed", ok: true},
		{name: "help", body: "@pergent-review help", kind: "help", ok: true},
		{name: "dismiss", body: "@pergent-review dismiss abc123", kind: "dismiss", arg: "abc123", ok: true},
		{name: "dismiss with reason", body: "@pergent-review dismiss ABC123DEF456 not a bug: it's fine.", kind: "dismiss",
			arg: "abc123def456", reason: "not a bug: it's fine.", ok: true},
		{name: "dismiss without id", body: "@pergent-review dismiss", kind: "unknown", ok: true},
		{name: "dismiss with a short id", body: "@pergent-review dismiss abc12", kind: "unknown", ok: true},
		{name: "dismiss with a long id", body: "@pergent-review dismiss abc123def4567", kind: "unknown", ok: true},
		{name: "dismiss with a non-hex id", body: "@pergent-review dismiss xyz123", kind: "unknown", ok: true},
		{name: "no mention", body: "please review this", ok: false},
		{name: "empty", body: "", ok: false},
		{name: "mention mid-line", body: "hey @pergent-review review please", ok: false},
		{name: "mid-line command", body: "ping @pergent-review review", ok: false},
		{name: "sentence containing a command", body: "I'd say: @pergent-review review", ok: false},
		{name: "three spaces of indent", body: "   @pergent-review review", kind: "review", ok: true},
		{name: "four spaces of indent is code", body: "    @pergent-review review", ok: false},
		{name: "tab indent is code", body: "\t@pergent-review review", ok: false},
		{name: "backtick fence", body: "```\n@pergent-review review\n```", ok: false},
		{name: "tilde fence", body: "~~~\n@pergent-review review\n~~~", ok: false},
		{name: "fence with an info string", body: "```text\n@pergent-review review\n```", ok: false},
		{name: "long fence needs a long close", body: "````\n@pergent-review review\n```\n@pergent-review review\n````", ok: false},
		{name: "a tilde fence is not closed by backticks", body: "~~~\n```\n@pergent-review review\n```", ok: false},
		{name: "unclosed fence runs to the end", body: "```\n@pergent-review review", ok: false},
		{name: "command after a closed fence", body: "```\nx\n```\n@pergent-review help", kind: "help", ok: true},
		{name: "indented fence marker is code, not a fence", body: "    ```\n@pergent-review help", kind: "help", ok: true},
		{name: "inline code at line start is not a fence", body: "```x``` hi\n@pergent-review review", kind: "review", ok: true},
		{name: "html comment on one line", body: "<!-- @pergent-review review -->", ok: false},
		{name: "html comment over lines", body: "<!--\n@pergent-review review\n-->", ok: false},
		{name: "command after an html comment", body: "<!-- x\n@pergent-review review\n-->\n@pergent-review help", kind: "help", ok: true},
		{name: "unclosed html comment", body: "<!--\n@pergent-review review", ok: false},
		{name: "mention-only line falls through", body: "@pergent-review\n@pergent-review review", kind: "review", ok: true},
		{name: "mention-only with punctuation falls through", body: "@pergent-review:\nthanks\n@pergent-review help", kind: "help", ok: true},
		{name: "mention-only and nothing else", body: "@pergent-review", ok: false},
		{name: "cc line is not a command", body: "cc @pergent-review", ok: false},
		{name: "a dismiss reason is on the id's line", body: "@pergent-review dismiss abc123\nbecause it is fine", kind: "dismiss", arg: "abc123", ok: true},
		{name: "uppercase", body: "@Pergent-Review REVIEW FAILED", kind: "review_failed", ok: true},
		{name: "two commands, the first counts", body: "@pergent-review help\n@pergent-review review", kind: "help", ok: true},
		{name: "second line", body: "Thanks!\n\n@pergent-review review", kind: "review", ok: true},
		{name: "windows line ends", body: "@pergent-review review\r\nmore", kind: "review", ok: true},
		{name: "unknown word", body: "@pergent-review frobnicate", kind: "unknown", ok: true},
		{name: "mention alone", body: "thanks @pergent-review", ok: false},
		{name: "review with more words is not review", body: "@pergent-review review this please", kind: "unknown", ok: true},
		{name: "trailing punctuation", body: "@pergent-review review.", kind: "review", ok: true},
		{name: "colon after mention", body: "@pergent-review: help", kind: "help", ok: true},
		{name: "longer name is another bot", body: "@pergent-reviewer review", ok: false},
		{name: "name with a suffix dot word", body: "@pergent-review.bot review", ok: false},
		{name: "email is not a mention", body: "mail me@pergent-review review", ok: false},
		{name: "the longer name first, then ours", body: "@pergent-reviewer review\n@pergent-review help", kind: "help", ok: true},
		{name: "quoted reply is not a command", body: "> @pergent-review review\nlooks good", ok: false},
		{name: "other mention", body: "@someone review", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mention := m
			if tt.mention != "" {
				mention = tt.mention
			}
			kind, arg, reason, ok := ParseCommand(tt.body, mention)
			if ok != tt.ok || kind != tt.kind || arg != tt.arg || reason != tt.reason {
				t.Fatalf("ParseCommand(%q) = %q %q %q %v; want %q %q %q %v",
					tt.body, kind, arg, reason, ok, tt.kind, tt.arg, tt.reason, tt.ok)
			}
		})
	}
	if _, _, _, ok := ParseCommand("@ review", ""); ok {
		t.Error("an empty mention matched")
	}
	long := "@pergent-review dismiss abc123 " + strings.Repeat("é", 2000)
	_, _, reason, ok := ParseCommand(long, m)
	if !ok || len([]rune(reason)) != maxReasonRunes {
		t.Errorf("reason = %d runes (ok %v); want it cut to %d", len([]rune(reason)), ok, maxReasonRunes)
	}
}

type fakeCommands struct {
	stored []store.Command
	seen   map[string]bool
	err    error
}

func (f *fakeCommands) InsertCommand(_ context.Context, c store.Command) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	key := fmt.Sprint(c.MR, c.CommentID)
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	f.stored = append(f.stored, c)
	return true, nil
}

func testBots(instance string) (string, []string, bool) {
	switch instance {
	case "main":
		return "pergent-review", []string{"pergent-review", "pergent-approve"}, true
	case "gh":
		return "pergent-app", []string{"pergent-app[bot]", "approver-app[bot]"}, true
	}
	return "", nil, false
}

type cmdRig struct {
	cmds  *fakeCommands
	q     *fakeQueue
	wakes int
	mux   *http.ServeMux
	logs  *bytes.Buffer
}

func newCmdRig(t *testing.T) *cmdRig {
	t.Helper()
	r := &cmdRig{cmds: &fakeCommands{}, q: &fakeQueue{created: true}, logs: &bytes.Buffer{}}
	cfg := &config.Config{
		GitLab:       []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
		GitHub:       []config.GitHubInstance{{Name: "gh"}},
		Repositories: []config.Repository{{GitLab: "main", Project: "Acme/Widgets"}, {GitHub: "gh", Project: "Acme/Widgets"}},
	}
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	reg := instances.New(cfg, nil, nil)
	wake := func() { r.wakes++ }
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/gitlab/{instance}", &Handler{
		Config: cfg, Instances: reg, Secrets: syncmap.New(map[string]string{"main": testSecret}),
		Queue: r.q, Commands: r.cmds, Bots: testBots, Wake: wake, Log: log,
	})
	r.mux.Handle("/webhooks/github/{instance}", &GitHubHandler{
		Config: cfg, Instances: reg, Secrets: syncmap.New(map[string]string{"gh": ghSecret}),
		Queue: r.q, Commands: r.cmds, Bots: testBots, Wake: wake, Log: log,
	})
	return r
}

type obj = map[string]any

// glNote is a GitLab Note Hook for a comment on merge request 7 of Acme/Widgets.
func glNote(note, user string) obj {
	return obj{
		"object_kind": "note",
		"user":        obj{"id": 5, "username": user},
		"project":     obj{"id": 42, "path_with_namespace": "acme/widgets"},
		"object_attributes": obj{
			"id": 900, "note": note, "noteable_type": "MergeRequest", "type": nil,
			"action": "create", "system": false,
		},
		"merge_request": obj{"iid": 7},
	}
}

func (r *cmdRig) glPost(t *testing.T, payload obj) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// ghComment is a GitHub issue_comment event for a comment on pull request 7.
func ghComment(body, login, userType string) obj {
	return obj{
		"action":     "created",
		"issue":      obj{"number": 7, "pull_request": obj{"url": "https://api.github.com/repos/acme/widgets/pulls/7"}},
		"comment":    obj{"id": 900, "body": body, "user": obj{"login": login, "id": 5, "type": userType}},
		"repository": obj{"id": 42, "full_name": "acme/widgets"},
	}
}

func (r *cmdRig) ghPost(t *testing.T, payload obj) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(ghSecret, body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func (r *cmdRig) noStored(t *testing.T) {
	t.Helper()
	if len(r.cmds.stored) != 0 || r.wakes != 0 {
		t.Fatalf("stored = %+v, wakes = %d; want nothing", r.cmds.stored, r.wakes)
	}
	if len(r.q.enqueues)+len(r.q.supersedes)+len(r.q.republishes) != 0 {
		t.Fatal("a comment touched the run queue")
	}
}

func TestGitLabNoteStored(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.glPost(t, glNote("@pergent-review dismiss abc123 false positive", "alice")), 200)
	want := store.Command{
		MR:          store.MRKey{Instance: "main", ProjectID: 42, IID: 7},
		ProjectPath: "Acme/Widgets", Platform: config.PlatformGitLab, Kind: "dismiss", Arg: "abc123",
		Reason: "false positive", CommentID: 900, Author: "alice", AuthorID: 5,
	}
	if len(r.cmds.stored) != 1 || r.cmds.stored[0] != want {
		t.Fatalf("stored = %+v; want [%+v]", r.cmds.stored, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d; want 1", r.wakes)
	}
}

func TestGitLabNoteWithoutActionStored(t *testing.T) {
	r := newCmdRig(t)
	p := glNote("@pergent-review review", "alice")
	delete(p["object_attributes"].(obj), "action") // older GitLab
	wantStatus(t, r.glPost(t, p), 200)
	if len(r.cmds.stored) != 1 || r.cmds.stored[0].Kind != "review" {
		t.Fatalf("stored = %+v; want one review", r.cmds.stored)
	}
}

func TestGitHubIssueCommentStored(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.ghPost(t, ghComment("@pergent-app review failed", "alice", "User")), 200)
	want := store.Command{
		MR:          store.MRKey{Instance: "gh", ProjectID: 42, IID: 7},
		ProjectPath: "Acme/Widgets", Platform: config.PlatformGitHub, Kind: "review_failed",
		CommentID: 900, Author: "alice", AuthorID: 5,
	}
	if len(r.cmds.stored) != 1 || r.cmds.stored[0] != want {
		t.Fatalf("stored = %+v; want [%+v]", r.cmds.stored, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d; want 1", r.wakes)
	}
}

func TestOwnAndBotCommentsIgnored(t *testing.T) {
	// A pergent review comment lists commands as examples; it must never run.
	review := "<!-- pergent -->\nTo re-run: `@pergent-review review`\n@pergent-review review"
	for _, user := range []string{"pergent-review", "Pergent-Review", "pergent-approve", "renovate[bot]"} {
		r := newCmdRig(t)
		wantStatus(t, r.glPost(t, glNote(review, user)), 200)
		r.noStored(t)
	}
	for _, c := range []struct{ login, typ string }{
		{"pergent-app[bot]", "Bot"}, {"approver-app[bot]", "Bot"}, {"x[bot]", "User"}, {"someone", "Bot"},
	} {
		r := newCmdRig(t)
		wantStatus(t, r.ghPost(t, ghComment("@pergent-app review", c.login, c.typ)), 200)
		r.noStored(t)
	}
}

// GitLab project, group and service-account bots are automation, whatever
// they write; a person whose name merely looks alike is not.
func TestGitLabTokenBotsIgnored(t *testing.T) {
	for _, bot := range []string{"project_42_bot", "project_42_bot_3f9a0c", "group_7_bot", "group_7_bot_a1b2", "service_account_a1b2c3"} {
		r := newCmdRig(t)
		wantStatus(t, r.glPost(t, glNote("@pergent-review review", bot)), 200)
		r.noStored(t)
	}
	for _, person := range []string{"project_x_bot", "myproject_42_bot", "project_42_bot_zz", "service_accountant", "group_7_botany"} {
		r := newCmdRig(t)
		wantStatus(t, r.glPost(t, glNote("@pergent-review review", person)), 200)
		if len(r.cmds.stored) != 1 {
			t.Errorf("%s: stored = %+v; want the command", person, r.cmds.stored)
		}
	}
	// The rule is GitLab's: a GitHub login of that shape is not matched.
	r := newCmdRig(t)
	wantStatus(t, r.ghPost(t, ghComment("@pergent-app review", "project_42_bot", "User")), 200)
	if len(r.cmds.stored) != 1 {
		t.Errorf("GitHub login: stored = %+v; want the command", r.cmds.stored)
	}
}

func TestLineAndThreadNotesIgnored(t *testing.T) {
	for _, typ := range []string{"DiffNote", "DiscussionNote"} {
		r := newCmdRig(t)
		p := glNote("@pergent-review review", "alice")
		p["object_attributes"].(obj)["type"] = typ
		wantStatus(t, r.glPost(t, p), 200)
		r.noStored(t)
	}
	r := newCmdRig(t)
	p := glNote("@pergent-review review", "alice")
	p["object_attributes"].(obj)["system"] = true
	wantStatus(t, r.glPost(t, p), 200)
	r.noStored(t)

	for _, typ := range []string{"Issue", "Commit", "Snippet"} {
		r = newCmdRig(t)
		p = glNote("@pergent-review review", "alice")
		p["object_attributes"].(obj)["noteable_type"] = typ
		wantStatus(t, r.glPost(t, p), 200)
		r.noStored(t)
	}
}

func TestEditedCommentIgnored(t *testing.T) {
	r := newCmdRig(t)
	p := glNote("@pergent-review review", "alice")
	p["object_attributes"].(obj)["action"] = "update"
	wantStatus(t, r.glPost(t, p), 200)
	r.noStored(t)

	for _, action := range []string{"edited", "deleted"} {
		r = newCmdRig(t)
		p = ghComment("@pergent-app review", "alice", "User")
		p["action"] = action
		wantStatus(t, r.ghPost(t, p), 200)
		r.noStored(t)
	}
}

func TestIssueCommentOnAnIssueIgnored(t *testing.T) {
	r := newCmdRig(t)
	p := ghComment("@pergent-app review", "alice", "User")
	p["issue"] = obj{"number": 7}
	wantStatus(t, r.ghPost(t, p), 200)
	r.noStored(t)
}

func TestCommentsWithoutACommandIgnored(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.glPost(t, glNote("looks good to me", "alice")), 200)
	wantStatus(t, r.glPost(t, glNote("@pergent-reviewer review", "alice")), 200)
	wantStatus(t, r.ghPost(t, ghComment("@someone-else review", "alice", "User")), 200)
	r.noStored(t)
}

func TestCommentsOnOtherRepositoriesIgnored(t *testing.T) {
	r := newCmdRig(t)
	p := glNote("@pergent-review review", "alice")
	p["project"] = obj{"id": 43, "path_with_namespace": "acme/other"}
	wantStatus(t, r.glPost(t, p), 200)
	p = ghComment("@pergent-app review", "alice", "User")
	p["repository"] = obj{"id": 43, "full_name": "acme/other"}
	wantStatus(t, r.ghPost(t, p), 200)
	r.noStored(t)
}

func TestRedeliveryStoredOnce(t *testing.T) {
	r := newCmdRig(t)
	p := glNote("@pergent-review review", "alice")
	wantStatus(t, r.glPost(t, p), 200)
	wantStatus(t, r.glPost(t, p), 200)
	if len(r.cmds.stored) != 1 || r.wakes != 1 {
		t.Fatalf("gitlab: stored %d, wakes %d; want 1 and 1", len(r.cmds.stored), r.wakes)
	}

	r = newCmdRig(t)
	g := ghComment("@pergent-app review", "alice", "User")
	wantStatus(t, r.ghPost(t, g), 200)
	wantStatus(t, r.ghPost(t, g), 200)
	if len(r.cmds.stored) != 1 || r.wakes != 1 {
		t.Fatalf("github: stored %d, wakes %d; want 1 and 1", len(r.cmds.stored), r.wakes)
	}
}

func TestCommandStorageErrorIs500(t *testing.T) {
	r := newCmdRig(t)
	r.cmds.err = errors.New("db down")
	wantStatus(t, r.glPost(t, glNote("@pergent-review review", "alice")), 500)
	wantStatus(t, r.ghPost(t, ghComment("@pergent-app review", "alice", "User")), 500)
	if r.wakes != 0 {
		t.Errorf("wakes = %d; want 0", r.wakes)
	}
}

func TestCommentsIgnoredWithoutBotsOrStore(t *testing.T) {
	// An instance whose bots aren't known yet (not connected) has no mention.
	r := newCmdRig(t)
	r.mux = http.NewServeMux()
	cfg := &config.Config{
		GitLab:       []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
		Repositories: []config.Repository{{GitLab: "main", Project: "Acme/Widgets"}},
	}
	for _, bots := range []Bots{
		nil,
		func(string) (string, []string, bool) { return "", nil, false },
		func(string) (string, []string, bool) { return "", nil, true }, // an empty mention matches nothing
	} {
		r.mux = http.NewServeMux()
		r.mux.Handle("/webhooks/gitlab/{instance}", &Handler{
			Config: cfg, Instances: instances.New(cfg, nil, nil), Secrets: syncmap.New(map[string]string{"main": testSecret}),
			Queue: r.q, Commands: r.cmds, Bots: bots, Wake: func() { r.wakes++ },
		})
		wantStatus(t, r.glPost(t, glNote("@ review", "alice")), 200)
		wantStatus(t, r.glPost(t, glNote("@pergent-review review", "alice")), 200)
	}
	r.noStored(t)

	// Without a command store the hook still answers 200.
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/gitlab/{instance}", &Handler{
		Config: cfg, Instances: instances.New(cfg, nil, nil), Secrets: syncmap.New(map[string]string{"main": testSecret}),
		Queue: r.q, Bots: testBots,
	})
	wantStatus(t, r.glPost(t, glNote("@pergent-review review", "alice")), 200)
	r.noStored(t)
}

func TestNoteHookBadTokenAndBadJSON(t *testing.T) {
	r := newCmdRig(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", strings.NewReader("{"))
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 200)

	req = httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", strings.NewReader("{}"))
	req.Header.Set("X-Gitlab-Token", "wrong")
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	rec = httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 401)
	r.noStored(t)
}

// A comment by a person can hold text that is not theirs: a pasted finding, a
// quoted review. None of it is a command.
func TestHumanCommentsHoldingReviewTextIgnored(t *testing.T) {
	const cmd = "@pergent-review review"
	bodies := map[string]string{
		"review marker":        "<!-- pergent -->\n" + cmd,
		"review marker, later": cmd + "\n<!-- pergent:findings [] -->",
		"marker with a suffix": "<!-- pergent:skill=x -->\n" + cmd,
		"backtick fence":       "Here is the text:\n```\n" + cmd + "\n```",
		"tilde fence":          "~~~\n" + cmd + "\n~~~",
		"html comment":         "<!--\n" + cmd + "\n-->",
		"indented code":        "Run this:\n\n    " + cmd,
		"tab-indented code":    "\t" + cmd,
		"mid-sentence":         "I think you could say " + cmd + " here",
		"cc line":              "cc " + cmd,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			r := newCmdRig(t)
			wantStatus(t, r.glPost(t, glNote(body, "alice")), 200)
			r.noStored(t)

			r = newCmdRig(t)
			wantStatus(t, r.ghPost(t, ghComment(strings.ReplaceAll(body, "@pergent-review", "@pergent-app"), "alice", "User")), 200)
			r.noStored(t)
		})
	}
}

func TestMentionOnlyLineThenCommandRuns(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.glPost(t, glNote("@pergent-review\n@pergent-review review", "alice")), 200)
	if len(r.cmds.stored) != 1 || r.cmds.stored[0].Kind != "review" {
		t.Fatalf("stored = %+v; want one review", r.cmds.stored)
	}
	r = newCmdRig(t)
	wantStatus(t, r.ghPost(t, ghComment("@pergent-app\n@pergent-app review", "alice", "User")), 200)
	if len(r.cmds.stored) != 1 || r.cmds.stored[0].Kind != "review" {
		t.Fatalf("stored = %+v; want one review", r.cmds.stored)
	}
}

func TestCommentPostedByAnAppIgnored(t *testing.T) {
	r := newCmdRig(t)
	p := ghComment("@pergent-app review", "alice", "User") // a person's login, posted through an App
	p["comment"].(obj)["performed_via_github_app"] = obj{"id": 9, "slug": "some-ci"}
	wantStatus(t, r.ghPost(t, p), 200)
	r.noStored(t)

	// null is how GitHub says a comment was written by hand.
	p = ghComment("@pergent-app review", "alice", "User")
	p["comment"].(obj)["performed_via_github_app"] = nil
	wantStatus(t, r.ghPost(t, p), 200)
	if len(r.cmds.stored) != 1 {
		t.Fatalf("stored = %+v; want one", r.cmds.stored)
	}
}

func TestIssueCommentWithBadSignatureStoresNothing(t *testing.T) {
	r := newCmdRig(t)
	body, err := json.Marshal(ghComment("@pergent-app review", "alice", "User"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []string{"", "sha256=00", sign("another-secret", body)} {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "issue_comment")
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, req)
		wantStatus(t, rec, 401)
	}
	r.noStored(t)
}

func TestParseThreadCommand(t *testing.T) {
	const m = "pergent-review"
	tests := []struct {
		name, body        string
		kind, arg, reason string
		ok                bool
	}{
		{name: "id and reason", body: "@pergent-review dismiss abc123 because", kind: "dismiss", arg: "abc123", reason: "because", ok: true},
		{name: "id alone", body: "@pergent-review dismiss abc123", kind: "dismiss", arg: "abc123", ok: true},
		{name: "upper case id", body: "@pergent-review dismiss ABC123DEF456 fine", kind: "dismiss", arg: "abc123def456", reason: "fine", ok: true},
		{name: "reason without an id", body: "@pergent-review dismiss because it is fine", kind: "dismiss", reason: "because it is fine", ok: true},
		{name: "reason starting with a short hex word", body: "@pergent-review dismiss abc12 is fine", kind: "dismiss", reason: "abc12 is fine", ok: true},
		{name: "nothing after dismiss", body: "@pergent-review dismiss", kind: "dismiss", ok: true},
		{name: "dismiss with a full stop", body: "@pergent-review dismiss.", kind: "dismiss", ok: true},
		{name: "dismiss, colon, reason", body: "@pergent-review: dismiss: not a bug", kind: "dismiss", reason: "not a bug", ok: true},
		{name: "upper case command", body: "@Pergent-Review DISMISS false positive", kind: "dismiss", reason: "false positive", ok: true},
		{name: "review in a thread is still review", body: "@pergent-review review", kind: "review", ok: true},
		{name: "review failed", body: "@pergent-review review failed", kind: "review_failed", ok: true},
		{name: "help", body: "@pergent-review help", kind: "help", ok: true},
		{name: "unknown", body: "@pergent-review frobnicate", kind: "unknown", ok: true},
		{name: "dismissed is not dismiss", body: "@pergent-review dismissed abc123", kind: "unknown", ok: true},
		{name: "not addressed", body: "dismiss this please", ok: false},
		{name: "in a fence", body: "```\n@pergent-review dismiss fine\n```", ok: false},
		{name: "quoted", body: "> @pergent-review dismiss fine", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, arg, reason, ok := ParseThreadCommand(tt.body, m)
			if ok != tt.ok || kind != tt.kind || arg != tt.arg || reason != tt.reason {
				t.Fatalf("ParseThreadCommand(%q) = %q %q %q %v; want %q %q %q %v",
					tt.body, kind, arg, reason, ok, tt.kind, tt.arg, tt.reason, tt.ok)
			}
		})
	}
	if _, _, _, ok := ParseThreadCommand("@ dismiss x", ""); ok {
		t.Error("an empty mention matched")
	}
	long := "@pergent-review dismiss " + strings.Repeat("é", 2000)
	_, _, reason, ok := ParseThreadCommand(long, m)
	if !ok || len([]rune(reason)) != maxReasonRunes {
		t.Errorf("reason = %d runes (ok %v); want it cut to %d", len([]rune(reason)), ok, maxReasonRunes)
	}
	// A plain comment still needs the ID.
	if kind, _, _, _ := ParseCommand("@pergent-review dismiss because it is fine", m); kind != kindUnknown {
		t.Errorf("ParseCommand without an id = %q; want unknown", kind)
	}
}

// ghReviewReply is a GitHub pull_request_review_comment event: a reply (to
// comment 500) on a line of pull request 7.
func ghReviewReply(body, login, userType string) obj {
	return obj{
		"action":       "created",
		"pull_request": obj{"number": 7},
		"comment": obj{"id": 901, "body": body, "in_reply_to_id": 500,
			"user": obj{"login": login, "id": 5, "type": userType}},
		"repository": obj{"id": 42, "full_name": "acme/widgets"},
	}
}

func (r *cmdRig) ghReviewPost(t *testing.T, payload obj) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(ghSecret, body))
	req.Header.Set("X-GitHub-Event", "pull_request_review_comment")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// glThreadNote is glNote written as a reply in discussion d.
func glThreadNote(note, user, typ, d string) obj {
	p := glNote(note, user)
	attrs := p["object_attributes"].(obj)
	attrs["type"] = typ
	attrs["discussion_id"] = d
	return p
}

func TestGitHubReviewCommentReplyStored(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.ghReviewPost(t, ghReviewReply("@pergent-app dismiss false positive", "alice", "User")), 200)
	want := store.Command{
		MR:          store.MRKey{Instance: "gh", ProjectID: 42, IID: 7},
		ProjectPath: "Acme/Widgets", Platform: config.PlatformGitHub, Kind: "dismiss", Arg: "", Reason: "false positive",
		CommentID: 901, Author: "alice", AuthorID: 5, ReplyTo: 500,
	}
	if len(r.cmds.stored) != 1 || r.cmds.stored[0] != want {
		t.Fatalf("stored = %+v; want [%+v]", r.cmds.stored, want)
	}
	if !r.cmds.stored[0].InThread() {
		t.Error("the command is not in a thread")
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d; want 1", r.wakes)
	}
}

func TestGitHubReviewCommentWithAnIDStored(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.ghReviewPost(t, ghReviewReply("@pergent-app dismiss abc123 constant", "alice", "User")), 200)
	if len(r.cmds.stored) != 1 || r.cmds.stored[0].Arg != "abc123" || r.cmds.stored[0].Reason != "constant" || r.cmds.stored[0].ReplyTo != 500 {
		t.Fatalf("stored = %+v", r.cmds.stored)
	}
}

func TestGitHubTopLevelReviewCommentIgnored(t *testing.T) {
	for _, reply := range []any{nil, 0, "absent"} {
		r := newCmdRig(t)
		p := ghReviewReply("@pergent-app dismiss fine", "alice", "User")
		if reply == "absent" {
			delete(p["comment"].(obj), "in_reply_to_id")
		} else {
			p["comment"].(obj)["in_reply_to_id"] = reply
		}
		wantStatus(t, r.ghReviewPost(t, p), 200)
		r.noStored(t)
		if !strings.Contains(r.logs.String(), "not_a_reply") {
			t.Errorf("in_reply_to_id %v: logs lack not_a_reply:\n%s", reply, r.logs)
		}
	}
}

func TestGitHubReviewCommentOtherActionsAndAuthorsIgnored(t *testing.T) {
	for _, action := range []string{"edited", "deleted"} {
		r := newCmdRig(t)
		p := ghReviewReply("@pergent-app dismiss fine", "alice", "User")
		p["action"] = action
		wantStatus(t, r.ghReviewPost(t, p), 200)
		r.noStored(t)
	}
	for _, c := range []struct{ login, typ string }{
		{"pergent-app[bot]", "Bot"}, {"x[bot]", "User"}, {"someone", "Bot"},
	} {
		r := newCmdRig(t)
		wantStatus(t, r.ghReviewPost(t, ghReviewReply("@pergent-app dismiss fine", c.login, c.typ)), 200)
		r.noStored(t)
	}
	r := newCmdRig(t)
	p := ghReviewReply("@pergent-app dismiss fine", "alice", "User")
	p["comment"].(obj)["performed_via_github_app"] = obj{"id": 9}
	wantStatus(t, r.ghReviewPost(t, p), 200)
	r.noStored(t)

	// The bot's own text is no command either.
	r = newCmdRig(t)
	wantStatus(t, r.ghReviewPost(t, ghReviewReply("<!-- pergent -->\n@pergent-app dismiss fine", "alice", "User")), 200)
	r.noStored(t)

	// A reply that doesn't address the bot.
	r = newCmdRig(t)
	wantStatus(t, r.ghReviewPost(t, ghReviewReply("thanks, will fix", "alice", "User")), 200)
	r.noStored(t)
}

func TestGitHubReviewCommentOnOtherRepositoryIgnored(t *testing.T) {
	r := newCmdRig(t)
	p := ghReviewReply("@pergent-app dismiss fine", "alice", "User")
	p["repository"] = obj{"id": 43, "full_name": "acme/other"}
	wantStatus(t, r.ghReviewPost(t, p), 200)
	r.noStored(t)
}

func TestGitHubReviewCommentMalformedAndRedelivered(t *testing.T) {
	r := newCmdRig(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", strings.NewReader("{"))
	req.Header.Set("X-Hub-Signature-256", sign(ghSecret, []byte("{")))
	req.Header.Set("X-GitHub-Event", "pull_request_review_comment")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 200)
	r.noStored(t)

	p := ghReviewReply("@pergent-app dismiss fine", "alice", "User")
	wantStatus(t, r.ghReviewPost(t, p), 200)
	wantStatus(t, r.ghReviewPost(t, p), 200)
	if len(r.cmds.stored) != 1 || r.wakes != 1 {
		t.Fatalf("stored %d, wakes %d; want 1 and 1", len(r.cmds.stored), r.wakes)
	}

	r = newCmdRig(t)
	r.cmds.err = errors.New("db down")
	wantStatus(t, r.ghReviewPost(t, ghReviewReply("@pergent-app dismiss fine", "alice", "User")), 500)
}

func TestGitLabDiffNoteReplyStoresTheDiscussion(t *testing.T) {
	for _, typ := range []string{"DiffNote", "DiscussionNote"} {
		r := newCmdRig(t)
		wantStatus(t, r.glPost(t, glThreadNote("@pergent-review dismiss abc123 not a bug", "alice", typ, "d1f2a3")), 200)
		want := store.Command{
			MR:          store.MRKey{Instance: "main", ProjectID: 42, IID: 7},
			ProjectPath: "Acme/Widgets", Platform: config.PlatformGitLab, Kind: "dismiss", Arg: "abc123",
			Reason: "not a bug", CommentID: 900, Author: "alice", AuthorID: 5, Discussion: "d1f2a3",
		}
		if len(r.cmds.stored) != 1 || r.cmds.stored[0] != want {
			t.Fatalf("%s: stored = %+v; want [%+v]", typ, r.cmds.stored, want)
		}
		if !r.cmds.stored[0].InThread() || r.wakes != 1 {
			t.Errorf("%s: InThread %v, wakes %d", typ, r.cmds.stored[0].InThread(), r.wakes)
		}
	}
}

func TestGitLabThreadDismissWithoutAnID(t *testing.T) {
	r := newCmdRig(t)
	wantStatus(t, r.glPost(t, glThreadNote("@pergent-review dismiss because it is fine", "alice", "DiffNote", "d1")), 200)
	if len(r.cmds.stored) != 1 || r.cmds.stored[0].Kind != "dismiss" || r.cmds.stored[0].Arg != "" ||
		r.cmds.stored[0].Reason != "because it is fine" || r.cmds.stored[0].Discussion != "d1" {
		t.Fatalf("stored = %+v", r.cmds.stored)
	}
}

func TestGitLabThreadNoteWithoutADiscussionStillIgnored(t *testing.T) {
	for _, typ := range []string{"DiffNote", "DiscussionNote"} {
		for _, d := range []any{"", nil} {
			r := newCmdRig(t)
			p := glNote("@pergent-review review", "alice")
			p["object_attributes"].(obj)["type"] = typ
			p["object_attributes"].(obj)["discussion_id"] = d
			wantStatus(t, r.glPost(t, p), 200)
			r.noStored(t)
		}
	}
	// An unknown note type with a discussion is not a thread reply either, and
	// a system note never is.
	r := newCmdRig(t)
	wantStatus(t, r.glPost(t, glThreadNote("@pergent-review review", "alice", "Other", "d1")), 200)
	r.noStored(t)
	p := glThreadNote("@pergent-review review", "alice", "DiscussionNote", "d1")
	p["object_attributes"].(obj)["system"] = true
	wantStatus(t, r.glPost(t, p), 200)
	r.noStored(t)
}

func TestGitLabPlainNoteKeepsTheDismissRule(t *testing.T) {
	// A plain comment still needs the ID to dismiss: without one it is an
	// unknown command, answered with the help.
	r := newCmdRig(t)
	wantStatus(t, r.glPost(t, glNote("@pergent-review dismiss because it is fine", "alice")), 200)
	if len(r.cmds.stored) != 1 || r.cmds.stored[0].Kind != "unknown" || r.cmds.stored[0].InThread() {
		t.Fatalf("stored = %+v", r.cmds.stored)
	}
}
