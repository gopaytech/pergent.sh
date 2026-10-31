package engine

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// ghWebhookSecret is the GitHub instance's webhook secret in the end-to-end
// test. It is not a real secret.
const ghWebhookSecret = "github-webhook-secret-example"

// ghE2E is e2e for the GitHub instance gh: the routes main serves, with the
// GitHub webhook secret, in front of a testEnv with a fake GitHub.
type ghE2E struct {
	*e2e
	gh *fakeGitHub
}

func newGitHubE2E(t *testing.T) *ghE2E {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	mux := server.Routes(server.Deps{
		Config:               env.Engine.Config,
		Instances:            env.Engine.Instances,
		WebhookSecrets:       env.Engine.WebhookSecrets,
		GitHubWebhookSecrets: syncmap.New(map[string]string{ghInstance: ghWebhookSecret}),
		Store:                env.Store,
		Wake:                 env.Engine.Wake,
		Now:                  env.Now,
		Log:                  env.Engine.Log,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &ghE2E{e2e: &e2e{env: env, srv: srv}, gh: gh}
}

// prHook posts a pull_request event of action for pull request n at head
// against main, signed with secret; extra fields are merged into the
// pull_request object. It returns the status code.
func (e *ghE2E) prHook(t *testing.T, secret, action string, n int64, head string, extra map[string]any) int {
	t.Helper()
	pr := map[string]any{
		"number": n, "state": "open", "merged": false, "draft": false,
		"head": map[string]any{"ref": "feature", "sha": head},
		"base": map[string]any{"ref": "main", "sha": sha('5')},
	}
	for k, v := range extra {
		pr[k] = v
	}
	body, _ := json.Marshal(map[string]any{
		"action":       action,
		"number":       n,
		"pull_request": pr,
		"repository":   map[string]any{"id": ghRepoID, "full_name": ghRepoPath},
	})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/webhooks/github/"+ghInstance, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("Content-Type", "application/json")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// signedHook is prHook with the instance's secret; anything but 200 fails.
func (e *ghE2E) signedHook(t *testing.T, action string, n int64, head string, extra map[string]any) {
	t.Helper()
	if code := e.prHook(t, ghWebhookSecret, action, n, head, extra); code != http.StatusOK {
		t.Fatalf("webhook %s %s: status %d", action, head, code)
	}
}

// checkRuns returns the bodies of the Check Runs created on head.
func (e *ghE2E) checkRuns(t *testing.T, head string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range e.gh.Requests(http.MethodPost, ghRepoAPI+"/check-runs") {
		if r.Token != ghReviewToken {
			t.Errorf("check run created with token %q, want the review App's", r.Token)
		}
		if body := r.JSON(t); body["head_sha"] == head {
			out = append(out, body)
		}
	}
	return out
}

// The 2a end-to-end flow on GitHub, through the routes main serves: a signed
// pull_request webhook, the Job with its scoped clone token, a push that
// supersedes the running review, the late result discarded, the clean result
// published and approved by the approve App, a push that dismisses the
// approval, and a merge that forgets it without writing to GitHub.
func TestEndToEndGitHub(t *testing.T) {
	e := newGitHubE2E(t)
	env, gh := e.env, e.gh
	headA, headB, headC := sha('a'), sha('c'), sha('d')
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	comments := ghRepoAPI + "/issues/1/comments"
	gh.Reply(http.MethodGet, comments, http.StatusOK, []any{})
	gh.Reply(http.MethodPost, comments, http.StatusCreated, map[string]any{"id": 77})
	gh.Reply(http.MethodPost, ghPRPath(1)+"/reviews", http.StatusOK, map[string]any{"id": 9})

	// A webhook signed with another secret is refused and queues nothing.
	if code := e.prHook(t, "not-the-secret", "opened", 1, headA, nil); code != http.StatusUnauthorized {
		t.Fatalf("badly signed webhook: status %d, want 401", code)
	}
	dispatch(t, env)
	if n := len(env.Jobs()); n != 0 || len(gh.Requests("", "")) != 0 {
		t.Fatalf("a refused webhook started %d Jobs and made %d GitHub requests", n, len(gh.Requests("", "")))
	}

	// 1. A signed "opened" event queues a run of A.
	gh.ServePR(1, ghPR(1, headA))
	e.signedHook(t, "opened", 1, headA, nil)

	// 2. Dispatch: A's Job, with a clone token minted for this repository
	// alone, and an in_progress Check Run.
	dispatch(t, env)
	runA := e.newJob(t)
	if r := env.Run(runA); r.HeadSHA != headA || r.State != store.Running || r.MR != ghKey(1) {
		t.Fatalf("run A = %+v", r)
	}
	var scoped []githubRequest
	for _, r := range gh.Requests(http.MethodPost, "/app/installations/55/access_tokens") {
		if bytes.Contains(r.Body, []byte("repository_ids")) {
			scoped = append(scoped, r)
		}
	}
	if len(scoped) != 1 || scoped[0].App != ghReviewApp {
		t.Fatalf("scoped token requests = %+v; want one by the review App", scoped)
	}
	if got, _ := json.Marshal(scoped[0].JSON(t)); string(got) != `{"permissions":{"contents":"read"},"repository_ids":[900]}` {
		t.Errorf("clone token request = %s", got)
	}
	if got := string(env.Secret(runA).Data["git-token"]); got != ghCloneToken {
		t.Errorf("A's git-token = %q, want the minted clone token", got)
	}
	if spec, _ := json.Marshal(env.Job(runA)); bytes.Contains(spec, []byte(ghCloneToken)) {
		t.Error("the clone token is in the Job spec")
	}
	if runs := e.checkRuns(t, headA); len(runs) != 1 || runs[0]["status"] != "in_progress" {
		t.Errorf("A's check runs = %v, want one in_progress", runs)
	}
	tokenA := string(env.Secret(runA).Data["result-token"])

	// 3. synchronize to B: Job A is deleted and A's Check Run cancelled.
	gh.ServePR(1, ghPR(1, headB))
	e.signedHook(t, "synchronize", 1, headB, nil)
	cleanup(t, env)
	if env.Job(runA) != nil {
		t.Error("Job A still exists after the push of B")
	}
	if got := env.Run(runA).State; got != store.Superseded {
		t.Errorf("run A = %s, want superseded", got)
	}
	runsA := e.checkRuns(t, headA)
	if len(runsA) != 2 || runsA[1]["status"] != "completed" || runsA[1]["conclusion"] != "cancelled" {
		t.Errorf("A's check runs = %v, want the last one cancelled", runsA)
	}
	dispatch(t, env)
	runB := e.newJob(t, runA)
	if r := env.Run(runB); r.HeadSHA != headB || r.State != store.Running {
		t.Fatalf("run B = %+v", r)
	}
	tokenB := string(env.Secret(runB).Data["result-token"])

	// 4. A's late clean result is accepted and discarded; nothing is posted.
	gh.Reset()
	if code := e.postResult(t, runA, tokenA, resultFor(t, headA, reviewed("no_blocking_issues", "a.go"))); code != http.StatusOK {
		t.Errorf("A's result: status %d, want 200", code)
	}
	if r := env.Run(runA); r.State != store.Superseded || r.Result != nil {
		t.Errorf("run A = %s with result %v; want superseded without one", r.State, r.Result != nil)
	}
	publish(t, env)
	cleanup(t, env)
	if reqs := gh.Requests("", ""); len(reqs) != 0 {
		t.Errorf("A's late result made %d GitHub requests, want none", len(reqs))
	}

	// 5. B's clean result: one comment, a success Check Run and an APPROVE
	// review of B by the approve App.
	if code := e.postResult(t, runB, tokenB, resultFor(t, headB, reviewed("no_blocking_issues", "a.go"))); code != http.StatusOK {
		t.Fatalf("B's result: status %d, want 200", code)
	}
	publish(t, env)
	wantPublished(t, env, runB)
	posted := gh.Requests(http.MethodPost, comments)
	if len(posted) != 1 || posted[0].Token != ghReviewToken {
		t.Fatalf("comments = %+v; want one by the review App", posted)
	}
	if body, _ := posted[0].JSON(t)["body"].(string); !strings.Contains(body, policy.Marker) {
		t.Errorf("comment lacks the marker: %q", body)
	}
	runsB := e.checkRuns(t, headB)
	if len(runsB) != 1 || runsB[0]["status"] != "completed" || runsB[0]["conclusion"] != "success" {
		t.Errorf("B's check runs since its dispatch = %v, want one success", runsB)
	}
	if n := len(e.checkRuns(t, headA)); n != 0 {
		t.Errorf("%d check runs on A after its result, want none", n)
	}
	reviews := gh.Requests(http.MethodPost, ghPRPath(1)+"/reviews")
	if len(reviews) != 1 || reviews[0].Token != ghApproveToken {
		t.Fatalf("reviews = %+v; want one by the approve App", reviews)
	}
	if body := reviews[0].JSON(t); body["event"] != "APPROVE" || body["commit_id"] != headB {
		t.Errorf("review = %v, want APPROVE of B", body)
	}
	if a := approvals(t, env); len(a) != 1 || a[0].ApprovedSHA != headB {
		t.Fatalf("approvals = %+v, want B", a)
	}

	// 6. synchronize to C: the approve App dismisses its approval of B.
	gh.Reply(http.MethodGet, ghPRPath(1)+"/reviews", http.StatusOK, []map[string]any{
		{"id": 9, "user": map[string]any{"id": ghApproveBot}, "state": "APPROVED", "commit_id": headB},
	})
	gh.Reply(http.MethodPut, ghPRPath(1)+"/reviews/9/dismissals", http.StatusOK, map[string]any{"id": 9})
	gh.ServePR(1, ghPR(1, headC))
	e.signedHook(t, "synchronize", 1, headC, nil)
	cleanup(t, env)
	dismissals := gh.Requests(http.MethodPut, ghPRPath(1)+"/reviews/9/dismissals")
	if len(dismissals) != 1 || dismissals[0].Token != ghApproveToken {
		t.Fatalf("dismissals = %+v; want one by the approve App", dismissals)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Fatalf("approvals = %+v, want none after the push", a)
	}

	// C is reviewed clean and approved, so the merge has an approval to
	// forget.
	dispatch(t, env)
	runC := e.newJob(t, runA, runB)
	if code := e.postResult(t, runC, string(env.Secret(runC).Data["result-token"]), resultFor(t, headC, reviewed("no_blocking_issues", "a.go"))); code != http.StatusOK {
		t.Fatalf("C's result: status %d, want 200", code)
	}
	publish(t, env)
	wantPublished(t, env, runC)
	if a := approvals(t, env); len(a) != 1 || a[0].ApprovedSHA != headC {
		t.Fatalf("approvals = %+v, want C", a)
	}

	// 7. closed with merged: true: the approval record is cleared and
	// nothing is written to GitHub.
	merged := ghPR(1, headC)
	merged["state"], merged["merged"] = "closed", true
	gh.Reply(http.MethodGet, ghPRPath(1), http.StatusOK, merged)
	gh.Reset()
	e.signedHook(t, "closed", 1, headC, map[string]any{"state": "closed", "merged": true})
	publish(t, env)
	cleanup(t, env)
	sweep(t, env)
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %+v, want none after the merge", a)
	}
	if w := gh.Writes(); len(w) != 0 {
		t.Errorf("GitHub writes after the merge: %+v", w)
	}

	for _, tok := range []string{ghReviewToken, ghApproveToken, ghCloneToken, ghWebhookSecret} {
		if strings.Contains(env.Logs(), tok) {
			t.Errorf("secret %q logged", tok)
		}
	}
}
