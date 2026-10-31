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
		{name: "mention mid-line", body: "hey @pergent-review review please", kind: "unknown", ok: true},
		{name: "mid-line command", body: "ping @pergent-review review", kind: "review", ok: true},
		{name: "uppercase", body: "@Pergent-Review REVIEW FAILED", kind: "review_failed", ok: true},
		{name: "two commands, the first counts", body: "@pergent-review help\n@pergent-review review", kind: "help", ok: true},
		{name: "second line", body: "Thanks!\n\n@pergent-review review", kind: "review", ok: true},
		{name: "windows line ends", body: "@pergent-review review\r\nmore", kind: "review", ok: true},
		{name: "unknown word", body: "@pergent-review frobnicate", kind: "unknown", ok: true},
		{name: "mention alone", body: "thanks @pergent-review", kind: "unknown", ok: true},
		{name: "review with more words is not review", body: "@pergent-review review this please", kind: "unknown", ok: true},
		{name: "trailing punctuation", body: "@pergent-review review.", kind: "review", ok: true},
		{name: "colon after mention", body: "@pergent-review: help", kind: "help", ok: true},
		{name: "longer name is another bot", body: "@pergent-reviewer review", ok: false},
		{name: "name with a suffix dot word", body: "@pergent-review.bot review", ok: false},
		{name: "email is not a mention", body: "mail me@pergent-review review", ok: false},
		{name: "the longer name first, then ours", body: "@pergent-reviewer x @pergent-review review", kind: "review", ok: true},
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
