package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// [CI] These need the test database, like every test that builds a testEnv.

const (
	linkedTargetID = int64(901)
	linkedTarget   = "acme/api"
)

// linkedGitHub is a GitHub environment whose repository has the setting on
// and one link to acme/api (public; the repository itself is private). The
// fake GitHub knows both repositories; compare routes are the test's.
func linkedGitHub(t *testing.T) (*testEnv, *fakeGitHub) {
	t.Helper()
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	on := true
	repos := env.Engine.Config.Repositories
	r := &repos[len(repos)-1]
	r.LinkedReposEnabled = &on
	r.LinkedRepos = []store.Link{{Target: linkedTarget, Watched: []string{"render/**"}, CanBlock: false}}

	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "private": true, "visibility": "private"})
	gh.Install(ghReviewApp, linkedTarget, ghInstallation)
	gh.Reply(http.MethodGet, "/repos/"+linkedTarget, http.StatusOK, map[string]any{"id": linkedTargetID, "private": false, "visibility": "public"})
	return env, gh
}

// serveCompare makes the comparison from..to answer as an ancestor with the
// given changed files.
func serveCompare(gh *fakeGitHub, from, to string, files ...string) {
	out := []map[string]any{}
	for _, f := range files {
		out = append(out, map[string]any{"filename": f, "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b\n"})
	}
	gh.Reply(http.MethodGet, ghRepoAPI+"/compare/"+from+"..."+to, http.StatusOK, map[string]any{
		"status": "ahead", "merge_base_commit": map[string]any{"sha": from}, "files": out,
	})
}

// scopedTokenRepos are the repository IDs of the clone token requests.
func scopedTokenRepos(t *testing.T, gh *fakeGitHub) [][]int64 {
	t.Helper()
	var out [][]int64
	for _, r := range gh.Requests(http.MethodPost, "/app/installations/55/access_tokens") {
		if len(r.Body) == 0 {
			continue
		}
		var body struct {
			IDs   []int64           `json:"repository_ids"`
			Perms map[string]string `json:"permissions"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Perms) != 1 || body.Perms["contents"] != "read" {
			t.Errorf("clone token permissions = %v; want contents:read alone", body.Perms)
		}
		out = append(out, body.IDs)
	}
	return out
}

func checkoutEnv(t *testing.T, env *testEnv, id string) map[string]string {
	t.Helper()
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	out := map[string]string{}
	for _, v := range job.Spec.Template.Spec.InitContainers[0].Env {
		out[v.Name] = v.Value
	}
	return out
}

func pergentEnv(env *testEnv, id string) map[string]string {
	out := map[string]string{}
	for _, v := range env.Job(id).Spec.Template.Spec.Containers[0].Env {
		out[v.Name] = v.Value
	}
	return out
}

func linkedEntries(t *testing.T, env *testEnv, id string) []linkedEntry {
	t.Helper()
	raw, ok := env.ConfigMap(id).Data["linked-repos.json"]
	if !ok {
		return nil
	}
	var out []linkedEntry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("linked-repos.json = %q: %v", raw, err)
	}
	return out
}

func TestDispatchRecordsTheLinks(t *testing.T) {
	env, gh := linkedGitHub(t)
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	serveCompare(gh, sha('b'), head, "render/comment.go", "cmd/main.go")

	dispatch(t, env)

	co := checkoutEnv(t, env, id)
	if co["LINKED_CLONES"] != "linked-1 https://github.com/acme/api.git" {
		t.Errorf("LINKED_CLONES = %q", co["LINKED_CLONES"])
	}
	if got := pergentEnv(env, id)["PERGENT_LINKED_REPOS"]; got != "/in/linked-repos.json" {
		t.Errorf("PERGENT_LINKED_REPOS = %q", got)
	}
	entries := linkedEntries(t, env, id)
	if len(entries) != 1 || entries[0].Name != linkedTarget || entries[0].Path != "/work/linked-1" ||
		entries[0].Error != "" || entries[0].CanBlock || len(entries[0].Watched) != 1 || entries[0].Watched[0] != "render/**" {
		t.Errorf("entries = %+v", entries)
	}
	if r := env.Run(id); len(r.LinkedRepos) != 1 || r.LinkedRepos[0].Target != linkedTarget {
		t.Errorf("run links = %+v", r.LinkedRepos)
	}
	// One call covers the checkout and the target.
	if got := scopedTokenRepos(t, gh); len(got) != 1 || len(got[0]) != 2 || got[0][0] != ghRepoID || got[0][1] != linkedTargetID {
		t.Errorf("clone token repositories = %v; want [[%d %d]]", got, ghRepoID, linkedTargetID)
	}
	// The clone gets time: the Job's deadline counts the target's bound.
	want := int64(kube.Deadline(10*time.Minute, kube.CloneAllowance(0, 1)) / time.Second)
	if got := *env.Job(id).Spec.ActiveDeadlineSeconds; got != want {
		t.Errorf("activeDeadlineSeconds = %d; want %d", got, want)
	}
	// The token is in the Secret, nowhere a clone's URL or the model can see.
	if got := string(env.Secret(id).Data["git-token"]); got != ghCloneToken {
		t.Errorf("git-token = %q", got)
	}
	spec, _ := json.Marshal([]any{env.Job(id), env.ConfigMap(id)})
	if bytes.Contains(spec, []byte(ghCloneToken)) || strings.Contains(env.Logs(), ghCloneToken) {
		t.Error("the clone token is in the Job, the ConfigMap or the log")
	}
	if strings.Contains(co["LINKED_CLONES"], "@") {
		t.Error("a clone URL carries credentials")
	}
}

// With the setting on and nothing matched, the review is the one it always
// was: no env, no file, no extra repository in the token, nothing recorded.
func TestDispatchLinksNothingMatched(t *testing.T) {
	env, gh := linkedGitHub(t)
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	serveCompare(gh, sha('b'), head, "cmd/main.go")

	dispatch(t, env)

	if _, ok := checkoutEnv(t, env, id)["LINKED_CLONES"]; ok {
		t.Error("LINKED_CLONES set for a change that matched nothing")
	}
	if _, ok := pergentEnv(env, id)["PERGENT_LINKED_REPOS"]; ok {
		t.Error("PERGENT_LINKED_REPOS set for a change that matched nothing")
	}
	if _, ok := env.ConfigMap(id).Data["linked-repos.json"]; ok {
		t.Error("linked-repos.json written for a change that matched nothing")
	}
	if got := scopedTokenRepos(t, gh); len(got) != 1 || len(got[0]) != 1 || got[0][0] != ghRepoID {
		t.Errorf("clone token repositories = %v; want only the repository", got)
	}
	if r := env.Run(id); r.LinkedRepos != nil {
		t.Errorf("run links = %+v; want none", r.LinkedRepos)
	}
	// Nothing about the targets was asked.
	if n := len(gh.Requests(http.MethodGet, "/repos/"+linkedTarget)); n != 0 {
		t.Errorf("%d reads of the target for a change that matched nothing", n)
	}
	limit := env.Job(id).Spec.Template.Spec.Volumes[0].EmptyDir.SizeLimit
	if want := env.Engine.Config.Jobs.WorkSize; (limit == nil) != want.IsZero() || (limit != nil && limit.Cmp(want) != 0) {
		t.Errorf("work volume limit %v; want %v", limit, want.String())
	}
}

// The setting off: the repository's links are not even looked at.
func TestDispatchLinksSettingOff(t *testing.T) {
	env, gh := linkedGitHub(t)
	off := false
	repos := env.Engine.Config.Repositories
	repos[len(repos)-1].LinkedReposEnabled = &off
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	serveCompare(gh, sha('b'), head, "render/comment.go")

	dispatch(t, env)

	if _, ok := checkoutEnv(t, env, id)["LINKED_CLONES"]; ok {
		t.Error("LINKED_CLONES set with the setting off")
	}
	if n := len(gh.Requests(http.MethodGet, ghRepoAPI+"/compare/"+sha('b')+"..."+head)); n != 0 {
		t.Errorf("%d comparisons with the setting off", n)
	}
	if got := scopedTokenRepos(t, gh); len(got) != 1 || len(got[0]) != 1 {
		t.Errorf("clone token repositories = %v", got)
	}
}

// A link that the rules refuse, or whose visibility can't be read, is an
// error entry and is never cloned. The review itself goes on.
func TestDispatchRefusedLinkIsAnError(t *testing.T) {
	env, gh := linkedGitHub(t)
	// A public repository can't link a private one.
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "private": false, "visibility": "public"})
	gh.Reply(http.MethodGet, "/repos/"+linkedTarget, http.StatusOK, map[string]any{"id": linkedTargetID, "private": true, "visibility": "private"})
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	serveCompare(gh, sha('b'), head, "render/comment.go")

	dispatch(t, env)

	entries := linkedEntries(t, env, id)
	if len(entries) != 1 || entries[0].Error != linkErrRefused {
		t.Fatalf("entries = %+v; want one with %q", entries, linkErrRefused)
	}
	if _, ok := checkoutEnv(t, env, id)["LINKED_CLONES"]; ok {
		t.Error("a refused link is cloned")
	}
	if got := scopedTokenRepos(t, gh); len(got) != 1 || len(got[0]) != 1 {
		t.Errorf("the token covers a refused target: %v", got)
	}
	if got := pergentEnv(env, id)["PERGENT_LINKED_REPOS"]; got == "" {
		t.Error("pergent is not told about the failed link: impact would be missing, not failed")
	}
	if r := env.Run(id); r.State != store.Running {
		t.Errorf("run state = %s; want running", r.State)
	}
}

func TestDispatchVisibilityErrorIsAnError(t *testing.T) {
	for name, reply := range map[string]map[string]any{
		"GitHub does not say":   {"id": linkedTargetID, "private": false},
		"GitHub contradicts":    {"id": linkedTargetID, "private": true, "visibility": "public"},
		"an unknown visibility": {"id": linkedTargetID, "private": false, "visibility": "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			env, gh := linkedGitHub(t)
			gh.Reply(http.MethodGet, "/repos/"+linkedTarget, http.StatusOK, reply)
			head := sha('a')
			id := ghEnqueue(t, env, 3, head)
			gh.ServePR(3, ghPR(3, head))
			gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
			serveCompare(gh, sha('b'), head, "render/comment.go")

			dispatch(t, env)

			entries := linkedEntries(t, env, id)
			if len(entries) != 1 || entries[0].Error != linkErrVisibility {
				t.Fatalf("entries = %+v; want %q", entries, linkErrVisibility)
			}
			if _, ok := checkoutEnv(t, env, id)["LINKED_CLONES"]; ok {
				t.Error("a link with no visibility is cloned")
			}
		})
	}
	t.Run("the source's visibility can't be read", func(t *testing.T) {
		env, gh := linkedGitHub(t)
		gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "private": true})
		head := sha('a')
		id := ghEnqueue(t, env, 3, head)
		gh.ServePR(3, ghPR(3, head))
		gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
		serveCompare(gh, sha('b'), head, "render/comment.go")

		dispatch(t, env)

		if entries := linkedEntries(t, env, id); len(entries) != 1 || entries[0].Error != linkErrVisibility {
			t.Fatalf("entries = %+v", entries)
		}
	})
	t.Run("the App is not installed on the target", func(t *testing.T) {
		env, gh := linkedGitHub(t)
		gh.Uninstall(ghReviewApp, linkedTarget)
		head := sha('a')
		id := ghEnqueue(t, env, 3, head)
		gh.ServePR(3, ghPR(3, head))
		gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
		serveCompare(gh, sha('b'), head, "render/comment.go")

		dispatch(t, env)

		if entries := linkedEntries(t, env, id); len(entries) != 1 || entries[0].Error != linkErrNotReadable {
			t.Fatalf("entries = %+v", entries)
		}
	})
	t.Run("the target is in another installation", func(t *testing.T) {
		env, gh := linkedGitHub(t)
		gh.Install(ghReviewApp, linkedTarget, 777)
		head := sha('a')
		id := ghEnqueue(t, env, 3, head)
		gh.ServePR(3, ghPR(3, head))
		gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
		serveCompare(gh, sha('b'), head, "render/comment.go")
		// Reading its visibility works through its own installation.
		gh.Reply(http.MethodPost, "/app/installations/777/access_tokens", http.StatusCreated, map[string]any{"token": "ghs_other", "expires_at": time.Now().Add(time.Hour).UTC()})

		dispatch(t, env)

		if entries := linkedEntries(t, env, id); len(entries) != 1 || entries[0].Error != linkErrInstall {
			t.Fatalf("entries = %+v", entries)
		}
		if got := scopedTokenRepos(t, gh); len(got) != 1 || len(got[0]) != 1 {
			t.Errorf("the token covers a target of another installation: %v", got)
		}
	})
}

// ghPublished publishes a completed review of pull request 3 at head, made by
// a pergent that takes --carried, with skills as given (each a map of the
// result). The run is dispatched first, as every review is, so its skills'
// hashes are recorded.
func ghPublished(t *testing.T, env *testEnv, gh *fakeGitHub, head string, reviewed []string, skills ...map[string]any) string {
	t.Helper()
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	serveCompare(gh, sha('b'), head, "cmd/main.go")
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("the earlier review was not dispatched; run = %+v\n%s", env.Run(id), env.Logs())
	}
	all := []any{map[string]any{
		"name": "security", "verdict": "no_blocking_issues", "findings": []any{},
		"files": map[string]any{"reviewed": reviewed, "skipped": []any{}, "not_reviewed": []any{}},
	}}
	for _, s := range skills {
		all = append(all, s)
	}
	body, err := json.Marshal(map[string]any{
		"schema_version": 1, "status": "completed", "pergent_version": "0.5.0", "base": sha('b'), "head": head,
		"skills":  all,
		"comment": map[string]any{"markdown": testMarkdown, "findings_block": "<!-- pergent:findings [] -->"},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := string(env.Secret(id).Data["result-token"])
	if out, err := env.Store.StoreResult(env.ctx, id, token, body, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := env.Store.MarkPublished(env.ctx, id, store.RunDecision{}); err != nil {
		t.Fatal(err)
	}
	return id
}

func impactSkillJSON(verdict string, findings ...map[string]any) map[string]any {
	fs := []any{}
	for _, f := range findings {
		fs = append(fs, f)
	}
	return map[string]any{
		"name": "impact", "verdict": verdict, "findings": fs,
		"files": map[string]any{"reviewed": []any{"render/comment.go"}, "skipped": []any{}, "not_reviewed": []any{}},
	}
}

func blockingImpact() map[string]any {
	return map[string]any{
		"id": "f1", "fingerprint": "fp-impact-1", "file": "render/comment.go", "line": 3, "severity": "blocking",
		"title": "breaks acme/api", "explanation": "a caller there breaks",
	}
}

// Review Focus 2: a push that touches no watched path still runs the check
// while the last impact result is failed or has an open blocking finding, and
// stops when it comes back clean.
func TestDispatchOpenImpactMatchesEverything(t *testing.T) {
	for _, tc := range []struct {
		name   string
		impact map[string]any
		want   bool
	}{
		{"failed", impactSkillJSON("failed"), true},
		{"an open blocking finding", impactSkillJSON("has_blocking_issues", blockingImpact()), true},
		{"clean", impactSkillJSON("no_blocking_issues"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, gh := linkedGitHub(t)
			ghPublished(t, env, gh, sha('1'), []string{"a.go"}, tc.impact)

			head := sha('2')
			id := ghEnqueue(t, env, 3, head)
			gh.ServePR(3, ghPR(3, head))
			serveCompare(gh, sha('b'), head, "cmd/main.go") // nothing watched
			dispatch(t, env)

			_, cloned := checkoutEnv(t, env, id)["LINKED_CLONES"]
			if cloned != tc.want {
				t.Errorf("cloned = %v; want %v\n%s", cloned, tc.want, env.Logs())
			}
			if got := len(linkedEntries(t, env, id)) == 1; got != tc.want {
				t.Errorf("entry = %v; want %v", got, tc.want)
			}
		})
	}
}

// A blocking finding nobody dismissed keeps the check running; a dismissed
// one doesn't.
func TestDispatchDismissedImpactDoesNotMatch(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghPublished(t, env, gh, sha('1'), []string{"a.go"}, impactSkillJSON("has_blocking_issues", blockingImpact()))
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
		MR: ghKey(3), Skill: "impact", Fingerprint: "fp-impact-1", Severity: "blocking",
		Actor: "bob", Reason: "known", Title: "breaks acme/api", File: "render/comment.go", Teaches: true,
	}); err != nil {
		t.Fatal(err)
	}

	head := sha('2')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	serveCompare(gh, sha('b'), head, "cmd/main.go")
	dispatch(t, env)

	if _, cloned := checkoutEnv(t, env, id)["LINKED_CLONES"]; cloned {
		t.Errorf("a dismissed finding kept the check running\n%s", env.Logs())
	}
}

// An incremental review matches the new commits' files only: the pull
// request touched a watched path before, and the push did not.
func TestIncrementalMatchesNewCommitsOnly(t *testing.T) {
	env, gh := linkedGitHub(t)
	on := true
	repos := env.Engine.Config.Repositories
	repos[len(repos)-1].IncrementalReviews = &on
	headA, headB := sha('1'), sha('2')
	ghPublished(t, env, gh, headA, []string{"a.go", "b.go"})

	id := ghEnqueue(t, env, 3, headB)
	gh.ServePR(3, ghPR(3, headB))
	// The pull request as a whole touches a watched path, the new commits don't.
	serveCompare(gh, sha('b'), headB, "render/comment.go")
	serveCompare(gh, headA, headB, "cmd/main.go")
	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job\n%s", env.Logs())
	}
	if _, ok := env.ConfigMap(id).Data["carried.json"]; !ok {
		t.Fatalf("not an incremental review\n%s", env.Logs())
	}
	if _, cloned := checkoutEnv(t, env, id)["LINKED_CLONES"]; cloned {
		t.Error("the whole pull request's files were matched, not the new commits'")
	}

	// And a push that does touch a watched path matches.
	headC := sha('3')
	id2 := ghEnqueue(t, env, 3, headC)
	gh.ServePR(3, ghPR(3, headC))
	serveCompare(gh, sha('b'), headC, "render/comment.go")
	serveCompare(gh, headB, headC, "render/other.go")
	// The last review is now the first one still (the second is not published).
	serveCompare(gh, headA, headC, "render/other.go")
	dispatch(t, env)
	if _, cloned := checkoutEnv(t, env, id2)["LINKED_CLONES"]; !cloned {
		t.Errorf("a push that touches a watched path did not match\n%s", env.Logs())
	}
}
