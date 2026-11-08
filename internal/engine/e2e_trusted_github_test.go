package engine

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ghTrustEnv is a GitHub instance trusting only authors with write access,
// with pull request 3 at head by login (id), and that login's permission on
// acme/gadgets answered with permission ("": not a collaborator, 404).
func ghTrustEnv(t *testing.T, head, login string, id int64, permission string) (*testEnv, *fakeGitHub) {
	t.Helper()
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	env.Engine.Config.GitHub[0].TrustedAuthorsOnly = config.TrustOn
	pr := ghPR(3, head)
	pr["user"] = map[string]any{"login": login, "id": id, "type": "User"}
	if strings.HasSuffix(login, "[bot]") {
		pr["user"].(map[string]any)["type"] = "Bot"
	}
	gh.ServePR(3, pr)
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	gh.Reply(http.MethodPost, ghRepoAPI+"/issues/3/comments", http.StatusCreated, map[string]any{"id": 77})
	perm := ghRepoAPI + "/collaborators/" + url.PathEscape(login) + "/permission"
	if permission == "" {
		gh.Reply(http.MethodGet, perm, http.StatusNotFound, map[string]any{"message": "Not Found"})
	} else {
		gh.Reply(http.MethodGet, perm, http.StatusOK, map[string]any{"permission": permission, "role_name": permission})
	}
	return env, gh
}

// checkRuns are the check runs posted, as status/title.
func ghCheckRuns(t *testing.T, gh *fakeGitHub) []string {
	t.Helper()
	var out []string
	for _, r := range gh.Requests(http.MethodPost, ghRepoAPI+"/check-runs") {
		var m struct {
			Status string `json:"status"`
			Output struct {
				Title string `json:"title"`
			} `json:"output"`
		}
		if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.Status+"/"+m.Output.Title)
	}
	return out
}

// A stranger's pull request waits: a queued check run and one note.
func TestGitHubUntrustedWaits(t *testing.T) {
	head := sha('a')
	env, gh := ghTrustEnv(t, head, "stranger", 4242, "")
	id := ghEnqueue(t, env, 3, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting {
		t.Fatalf("run = %s/%q, want waiting; logs:\n%s", r.State, r.SkipReason, env.Logs())
	}
	if got := ghCheckRuns(t, gh); len(got) != 1 || got[0] != "queued/"+waitingDescription {
		t.Errorf("check runs = %v", got)
	}
	if n := len(gh.Requests(http.MethodPost, ghRepoAPI+"/issues/3/comments")); n != 1 {
		t.Errorf("%d notes, want 1", n)
	}
}

// A collaborator with write is trusted.
func TestGitHubWriterReviewed(t *testing.T) {
	head := sha('a')
	env, _ := ghTrustEnv(t, head, "dev", 51, "write")
	id := ghEnqueue(t, env, 3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
}

// A bot without write access waits, unless it is on the allowlist.
func TestGitHubBotNeedsTheAllowlist(t *testing.T) {
	head := sha('a')
	env, _ := ghTrustEnv(t, head, "dependabot[bot]", 49699333, "")
	id := ghEnqueue(t, env, 3, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitUntrusted {
		t.Fatalf("bot's run = %s/%q, want waiting/untrusted_author", r.State, r.SkipReason)
	}

	env, _ = ghTrustEnv(t, head, "dependabot[bot]", 49699333, "")
	env.Engine.Config.Repositories[1].TrustedAuthors = []store.TrustedAuthor{{ID: 49699333, Username: "dependabot[bot]"}}
	id = ghEnqueue(t, env, 3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("allowlisted bot: no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
}
