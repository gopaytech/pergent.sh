package webhook

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// draftRig serves both webhooks for acme/widgets, with review_drafts set to
// reviewDrafts in the config.
func draftRig(t *testing.T, reviewDrafts bool) *rig {
	t.Helper()
	r := &rig{q: &fakeQueue{created: true}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
		GitHub: []config.GitHubInstance{{Name: "gh"}},
		Repositories: []config.Repository{
			{GitLab: "main", Project: "Acme/Widgets", ReviewDrafts: &reviewDrafts},
			{GitHub: "gh", Project: "Acme/Widgets", ReviewDrafts: &reviewDrafts},
		},
	}
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/gitlab/{instance}", &Handler{
		Config: cfg, Secrets: map[string]string{"main": testSecret}, Queue: r.q,
		Wake: func() { r.wakes++ }, Log: log,
	})
	r.mux.Handle("/webhooks/github/{instance}", &GitHubHandler{
		Config: cfg, Secrets: map[string]string{"gh": ghSecret}, Queue: r.q,
		Wake: func() { r.wakes++ }, Log: log,
	})
	return r
}

func replaceOnce(t *testing.T, body []byte, old, new string) []byte {
	t.Helper()
	if !bytes.Contains(body, []byte(old)) {
		t.Fatalf("fixture lacks %q", old)
	}
	return bytes.Replace(body, []byte(old), []byte(new), 1)
}

// gitlabDraft is a GitLab event body with the draft flag set.
func gitlabDraft(t *testing.T, fixtureName, flag string) []byte {
	return replaceOnce(t, fixture(t, fixtureName), `"iid": 7,`, `"iid": 7, "`+flag+`": true,`)
}

// githubDraft is a GitHub event body with the draft flag set.
func githubDraft(t *testing.T, fixtureName string) []byte {
	return replaceOnce(t, fixture(t, fixtureName), `"number": 7,
    "state": "open",`, `"number": 7, "draft": true,
    "state": "open",`)
}

func wantDraftSkipped(t *testing.T, r *rig) {
	t.Helper()
	noQueueCalls(t, r)
	if !strings.Contains(r.logs.String(), "outcome=draft_skipped") {
		t.Errorf("draft_skipped not logged: %s", r.logs.String())
	}
}

func TestGitLabDraftSkippedWhenDraftsAreOff(t *testing.T) {
	for _, flag := range []string{"draft", "work_in_progress"} {
		for _, fx := range []string{"mr_open.json", "mr_push.json", "mr_retarget.json"} {
			t.Run(flag+"/"+fx, func(t *testing.T) {
				r := draftRig(t, false)
				wantStatus(t, r.hook(gitlabDraft(t, fx, flag)), 200)
				wantDraftSkipped(t, r)
			})
		}
	}
}

func TestGitLabDraftReviewedWhenDraftsAreOn(t *testing.T) {
	r := draftRig(t, true)
	wantStatus(t, r.hook(gitlabDraft(t, "mr_open.json", "draft")), 200)
	if len(r.q.enqueues) != 1 || r.wakes != 1 {
		t.Fatalf("enqueues = %+v, wakes = %d; want one", r.q.enqueues, r.wakes)
	}
}

func TestGitLabReadyMergeRequestReviewedWhenDraftsAreOff(t *testing.T) {
	r := draftRig(t, false)
	wantStatus(t, r.hook(fixture(t, "mr_open.json")), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %+v, want one", r.q.enqueues)
	}
}

// Closing or merging a draft supersedes its runs whatever the setting.
func TestGitLabDraftCloseAndMergeStillHandled(t *testing.T) {
	for _, fx := range []string{"mr_close.json", "mr_merge.json"} {
		r := draftRig(t, false)
		wantStatus(t, r.hook(gitlabDraft(t, fx, "draft")), 200)
		if len(r.q.supersedes) != 1 || len(r.q.enqueues) != 0 {
			t.Fatalf("%s: supersedes = %+v, enqueues = %+v", fx, r.q.supersedes, r.q.enqueues)
		}
	}
}

// A draft marked ready with no review of its head to decide again is queued.
func TestGitLabDraftReadyEnqueuesWhenNothingToRepublish(t *testing.T) {
	r := draftRig(t, false)
	r.q.republished = false
	wantStatus(t, r.hook(fixture(t, "mr_ready.json")), 200)
	want := enqueueCall{store.MRKey{Instance: "main", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
	if len(r.q.republishes) != 1 || len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("republishes = %+v, enqueues = %+v; want one each, enqueue %+v", r.q.republishes, r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
}

func TestGitLabDraftReadyRepublishedIsNotQueued(t *testing.T) {
	r := draftRig(t, false)
	r.q.republished = true
	wantStatus(t, r.hook(fixture(t, "mr_ready.json")), 200)
	if len(r.q.republishes) != 1 || len(r.q.enqueues) != 0 || r.wakes != 1 {
		t.Fatalf("republishes = %+v, enqueues = %+v, wakes = %d", r.q.republishes, r.q.enqueues, r.wakes)
	}
}

func TestGitHubDraftSkippedWhenDraftsAreOff(t *testing.T) {
	for _, fx := range []string{"github_opened.json", "github_reopened.json", "github_synchronize.json", "github_edited_base.json"} {
		t.Run(fx, func(t *testing.T) {
			r := draftRig(t, false)
			wantStatus(t, r.gh(githubDraft(t, fx)), 200)
			wantDraftSkipped(t, r)
		})
	}
}

func TestGitHubDraftReviewedWhenDraftsAreOn(t *testing.T) {
	r := draftRig(t, true)
	wantStatus(t, r.gh(githubDraft(t, "github_opened.json")), 200)
	if len(r.q.enqueues) != 1 || r.wakes != 1 {
		t.Fatalf("enqueues = %+v, wakes = %d; want one", r.q.enqueues, r.wakes)
	}
}

func TestGitHubReadyPullRequestReviewedWhenDraftsAreOff(t *testing.T) {
	r := draftRig(t, false)
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %+v, want one", r.q.enqueues)
	}
}

func TestGitHubDraftClosedStillHandled(t *testing.T) {
	r := draftRig(t, false)
	wantStatus(t, r.gh(githubDraft(t, "github_closed.json")), 200)
	if len(r.q.supersedes) != 1 || len(r.q.enqueues) != 0 {
		t.Fatalf("supersedes = %+v, enqueues = %+v", r.q.supersedes, r.q.enqueues)
	}
}

func TestGitHubReadyForReviewEnqueuesWhenNothingToRepublish(t *testing.T) {
	r := draftRig(t, false)
	r.q.republished = false
	wantStatus(t, r.gh(fixture(t, "github_ready.json")), 200)
	want := enqueueCall{store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
	if len(r.q.republishes) != 1 || len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("republishes = %+v, enqueues = %+v; want one each, enqueue %+v", r.q.republishes, r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
}

func TestGitHubReadyForReviewRepublishedIsNotQueued(t *testing.T) {
	r := draftRig(t, false)
	r.q.republished = true
	wantStatus(t, r.gh(fixture(t, "github_ready.json")), 200)
	if len(r.q.republishes) != 1 || len(r.q.enqueues) != 0 || r.wakes != 1 {
		t.Fatalf("republishes = %+v, enqueues = %+v, wakes = %d", r.q.republishes, r.q.enqueues, r.wakes)
	}
}

// With drafts off, a retarget of a draft is skipped, so the head's published
// review (if any) is still the old target's. Marking the draft ready then
// asks for that target no more: the head is queued against the new one.
func TestGitLabDraftRetargetedThenReady(t *testing.T) {
	r := draftRig(t, false)
	r.q.republished, r.q.publishedTarget = true, "main"

	wantStatus(t, r.hook(gitlabDraft(t, "mr_retarget.json", "draft")), 200)
	wantDraftSkipped(t, r)

	ready := replaceOnce(t, fixture(t, "mr_ready.json"), `"target_branch": "main"`, `"target_branch": "develop"`)
	wantStatus(t, r.hook(ready), 200)
	mr := store.MRKey{Instance: "main", ProjectID: 42, IID: 7}
	if want := (republishCall{mr, openHead, "develop"}); len(r.q.republishes) != 1 || r.q.republishes[0] != want {
		t.Fatalf("republishes = %+v, want [%+v]", r.q.republishes, want)
	}
	if want := (enqueueCall{mr, "Acme/Widgets", openHead, "develop"}); len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("enqueues = %+v, want [%+v]", r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
	if !strings.Contains(r.logs.String(), "result=enqueued") {
		t.Errorf("log doesn't say the ready event enqueued: %s", r.logs.String())
	}
}

func TestGitHubDraftRetargetedThenReady(t *testing.T) {
	r := draftRig(t, false)
	r.q.republished, r.q.publishedTarget = true, "main"

	wantStatus(t, r.gh(githubDraft(t, "github_edited_base.json")), 200)
	wantDraftSkipped(t, r)

	ready := replaceOnce(t, fixture(t, "github_ready.json"), `"base": {"ref": "main"`, `"base": {"ref": "develop"`)
	wantStatus(t, r.gh(ready), 200)
	mr := store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}
	if want := (republishCall{mr, openHead, "develop"}); len(r.q.republishes) != 1 || r.q.republishes[0] != want {
		t.Fatalf("republishes = %+v, want [%+v]", r.q.republishes, want)
	}
	if want := (enqueueCall{mr, "Acme/Widgets", openHead, "develop"}); len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("enqueues = %+v, want [%+v]", r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
	if !strings.Contains(r.logs.String(), "result=enqueued") {
		t.Errorf("log doesn't say the ready event enqueued: %s", r.logs.String())
	}
}
