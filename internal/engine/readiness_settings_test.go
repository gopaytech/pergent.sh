package engine

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The settings reads of the readiness scan: the default branch resolved to a
// commit, then approvals, protection and required checks, read in the pod
// with the review bot (GitLab) or the review App (GitHub).

const (
	glProjectAPI   = "/projects/7"
	glCommitAPI    = glProjectAPI + "/repository/commits/main"
	glRulesAPI     = glProjectAPI + "/approval_rules"
	glApprovalsAPI = glProjectAPI + "/approvals"
	// glProtectAPI is the protection rule named main; glProtectListAPI
	// lists every rule, wildcards too.
	glProtectAPI     = glProjectAPI + "/protected_branches/main"
	glProtectListAPI = glProjectAPI + "/protected_branches"

	ghBranchAPI  = ghRepoAPI + "/branches/main"
	ghRulesAPI   = ghRepoAPI + "/rules/branches/main"
	ghClassicAPI = ghBranchAPI + "/protection/required_pull_request_reviews"
)

// serveGitLabSettings serves acme/widgets with main at sha('c'), a rule
// needing 2 approvals, main protected with only Maintainers pushing, and
// pipelines that must succeed.
func serveGitLabSettings(env *testEnv) {
	env.GitLab.Reply(http.MethodGet, glProjectAPI, http.StatusOK, map[string]any{
		"id": testProjectID, "path_with_namespace": testProjectPath, "default_branch": "main",
		"http_url_to_repo": testRepoURL, "only_allow_merge_if_pipeline_succeeds": true,
	})
	env.GitLab.Reply(http.MethodGet, glCommitAPI, http.StatusOK, map[string]any{"id": sha('c')})
	env.GitLab.Reply(http.MethodGet, glRulesAPI, http.StatusOK, []map[string]any{
		{"rule_type": "regular", "approvals_required": 2},
	})
	serveProtection(env, map[string]any{"name": "main", "push_access_levels": []map[string]any{{"access_level": 40}}})
}

// serveProtection makes acme/widgets' protected branch rules rules, served as
// GitLab does: all of them in the list, and the one named main (if any) by
// name, else 404.
func serveProtection(env *testEnv, rules ...map[string]any) {
	env.GitLab.Reply(http.MethodGet, glProtectListAPI, http.StatusOK, append([]map[string]any{}, rules...))
	env.GitLab.Reply(http.MethodGet, glProtectAPI, http.StatusNotFound, `{"message":"404 Not found"}`)
	for _, r := range rules {
		if r["name"] == "main" {
			env.GitLab.Reply(http.MethodGet, glProtectAPI, http.StatusOK, r)
		}
	}
}

// refuseProtection makes every protected branch read answer status with body.
func refuseProtection(env *testEnv, status int, body string) {
	env.GitLab.Reply(http.MethodGet, glProtectListAPI, status, body)
	env.GitLab.Reply(http.MethodGet, glProtectAPI, status, body)
}

// serveGitHubSettings serves acme/gadgets with main at sha('d'), protected,
// a ruleset needing 2 approvals and required status checks.
func serveGitHubSettings(gh *fakeGitHub) {
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{
		"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL, "default_branch": "main",
	})
	gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
		"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": true,
	})
	gh.Reply(http.MethodGet, ghRulesAPI, http.StatusOK, []map[string]any{
		{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 2}},
		{"type": "required_status_checks"},
	})
}

func scanSettings(t *testing.T, env *testEnv, instance, project string) (scanTarget, error) {
	t.Helper()
	return env.Engine.scanSettings(env.ctx, instance, project)
}

func checkItem(t *testing.T, name string, got readiness.PodItem, state, reason string) {
	t.Helper()
	if got.State != state || got.Reason != reason {
		t.Errorf("%s = %q %q; want %q %q", name, got.State, got.Reason, state, reason)
	}
}

func TestSettingsGitLab(t *testing.T) {
	env := newTestEnv(t)
	serveGitLabSettings(env)

	target, err := scanSettings(t, env, testInstance, testProjectPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	if target.Branch != "main" || target.Commit != sha('c') || target.RepoURL != testRepoURL ||
		target.GitUser != testDeployUser || string(target.GitToken) != testDeployToken {
		t.Fatalf("target = %+v; want main at sha('c') with the deploy token", target)
	}
	p := target.Pod
	if p.Version != 1 {
		t.Errorf("pod version = %d; want 1", p.Version)
	}
	if p.Approvals.State != readiness.Passed || p.Approvals.Count != 2 {
		t.Errorf("approvals = %+v; want passed, 2", p.Approvals)
	}
	checkItem(t, "protected", p.Protected, readiness.Passed, "main is protected")
	checkItem(t, "required checks", p.RequiredChecks, readiness.Passed, "pipelines must succeed before merging")
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method != http.MethodGet {
			t.Errorf("the scan wrote to GitLab: %s %s", r.Method, r.Path)
		}
		if r.Token != testReviewToken {
			t.Errorf("%s %s read with %q; want the review bot", r.Method, r.Path, r.Token)
		}
	}

	// Developers can push, pipelines needn't succeed, one approval.
	serveProtection(env, map[string]any{
		"name": "main", "push_access_levels": []map[string]any{{"access_level": 40}, {"access_level": 30}},
	})
	env.GitLab.Reply(http.MethodGet, glProjectAPI, http.StatusOK, map[string]any{
		"id": testProjectID, "default_branch": "main", "only_allow_merge_if_pipeline_succeeds": false,
	})
	env.GitLab.Reply(http.MethodGet, glRulesAPI, http.StatusOK, []map[string]any{
		{"rule_type": "regular", "approvals_required": 1},
	})
	target, err = scanSettings(t, env, testInstance, testProjectPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	p = target.Pod
	if p.Approvals.State != readiness.Warning || p.Approvals.Count != 1 {
		t.Errorf("approvals = %+v; want warning, 1", p.Approvals)
	}
	checkItem(t, "protected", p.Protected, readiness.Warning, "Developers can push to main directly")
	checkItem(t, "required checks", p.RequiredChecks, readiness.Warning, "pipelines don't have to succeed before merging")

	// Not protected; the pipeline setting not shown.
	serveProtection(env, map[string]any{"name": "release/*", "push_access_levels": []map[string]any{{"access_level": 40}}})
	env.GitLab.Reply(http.MethodGet, glProjectAPI, http.StatusOK, map[string]any{"id": testProjectID, "default_branch": "main"})
	target, err = scanSettings(t, env, testInstance, testProjectPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	checkItem(t, "protected", target.Pod.Protected, readiness.Missing, "main is not protected")
	checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Unknown, "GitLab didn't show whether pipelines must succeed")
}

// Every rule that matches the default branch counts, its own and wildcards
// alike, as GitLab applies the most permissive of them.
func TestSettingsGitLabWildcardProtection(t *testing.T) {
	env := newTestEnv(t)
	serveGitLabSettings(env)
	for _, tc := range []struct {
		name          string
		rules         []map[string]any
		state, reason string
	}{
		{"wildcard, maintainers push", []map[string]any{
			{"name": "release/*", "push_access_levels": []map[string]any{{"access_level": 30}}},
			{"name": "ma*", "push_access_levels": []map[string]any{{"access_level": 40}}},
		}, readiness.Passed, "main is protected"},
		{"every branch, developers push", []map[string]any{
			{"name": "*", "push_access_levels": []map[string]any{{"access_level": 30}}},
			{"name": "main*", "push_access_levels": []map[string]any{{"access_level": 0}}},
		}, readiness.Warning, "Developers can push to main directly"},
		{"own rule maintainers, wildcard developers", []map[string]any{
			{"name": "main", "push_access_levels": []map[string]any{{"access_level": 40}}},
			{"name": "*", "push_access_levels": []map[string]any{{"access_level": 30}}},
		}, readiness.Warning, "Developers can push to main directly"},
		{"own rule and wildcard, maintainers", []map[string]any{
			{"name": "main", "push_access_levels": []map[string]any{{"access_level": 40}}},
			{"name": "m*n", "push_access_levels": []map[string]any{{"access_level": 0}}},
		}, readiness.Passed, "main is protected"},
		{"no rule matches", []map[string]any{
			{"name": "release/*", "push_access_levels": []map[string]any{{"access_level": 40}}},
			{"name": "main-old", "push_access_levels": []map[string]any{{"access_level": 30}}},
		}, readiness.Missing, "main is not protected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveProtection(env, tc.rules...)
			target, err := scanSettings(t, env, testInstance, testProjectPath)
			if err != nil {
				t.Fatal(err)
			}
			checkItem(t, "protected", target.Pod.Protected, tc.state, tc.reason)
		})
	}

	// The list refused: unknown, never missing.
	refuseProtection(env, http.StatusForbidden, `{"message":"403 Forbidden"}`)
	target, err := scanSettings(t, env, testInstance, testProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	checkItem(t, "protected", target.Pod.Protected, readiness.Unknown, wantGitLabProtection)
}

func TestSettingsGitHub(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	serveGitHubSettings(gh)

	target, err := scanSettings(t, env, ghInstance, ghRepoPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	if target.Branch != "main" || target.Commit != sha('d') || target.RepoURL != ghCloneURL ||
		target.GitUser != "x-access-token" || string(target.GitToken) != ghCloneToken {
		t.Fatalf("target = %+v; want main at sha('d') with a clone token", target)
	}
	p := target.Pod
	if p.Approvals.State != readiness.Passed || p.Approvals.Count != 2 {
		t.Errorf("approvals = %+v; want passed, 2", p.Approvals)
	}
	checkItem(t, "protected", p.Protected, readiness.Passed, "main is protected")
	checkItem(t, "required checks", p.RequiredChecks, readiness.Passed, "a ruleset requires status checks")
	for _, r := range gh.Writes() {
		t.Errorf("the scan wrote to GitHub: %s %s", r.Method, r.Path)
	}
	for _, r := range gh.Requests(http.MethodGet, "") {
		if r.Token != ghReviewToken {
			t.Errorf("%s read with %q; want the review installation's token", r.Path, r.Token)
		}
	}

	// Only a pull_request rule needing one approval, and no classic
	// protection: protected by the rule, no checks required.
	gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
		"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": false,
		"protection": map[string]any{"enabled": false, "required_status_checks": map[string]any{
			"enforcement_level": "off", "contexts": []string{}, "checks": []any{}}},
	})
	gh.Reply(http.MethodGet, ghRulesAPI, http.StatusOK, []map[string]any{
		{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 1}},
	})
	target, err = scanSettings(t, env, ghInstance, ghRepoPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	p = target.Pod
	if p.Approvals.State != readiness.Warning || p.Approvals.Count != 1 {
		t.Errorf("approvals = %+v; want warning, 1", p.Approvals)
	}
	checkItem(t, "protected", p.Protected, readiness.Passed, "main is protected")
	checkItem(t, "required checks", p.RequiredChecks, readiness.Warning, "no status checks are required")

	// Classic protection with checks, no rules: protected, 2 checks.
	gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
		"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": true,
		"protection": map[string]any{"enabled": true, "required_status_checks": map[string]any{
			"enforcement_level": "everyone", "contexts": []string{"ci", "lint"}}},
	})
	gh.Reply(http.MethodGet, ghRulesAPI, http.StatusOK, []any{})
	gh.Reply(http.MethodGet, ghClassicAPI, http.StatusOK, map[string]any{"required_approving_review_count": 2})
	target, err = scanSettings(t, env, ghInstance, ghRepoPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	p = target.Pod
	if p.Approvals.State != readiness.Passed || p.Approvals.Count != 2 {
		t.Errorf("approvals = %+v; want passed, 2 from the classic rule", p.Approvals)
	}
	checkItem(t, "protected", p.Protected, readiness.Passed, "main is protected")
	checkItem(t, "required checks", p.RequiredChecks, readiness.Passed, "2 status checks are required")

	// Nothing protects it: no rule, no classic protection (GitHub answers
	// 404 for it), so no approval is needed, which is known.
	gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
		"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": false,
	})
	gh.Reply(http.MethodGet, ghRulesAPI, http.StatusOK, []map[string]any{{"type": "deletion"}})
	gh.Reply(http.MethodGet, ghClassicAPI, http.StatusNotFound, map[string]string{"message": "Branch not protected"})
	target, err = scanSettings(t, env, ghInstance, ghRepoPath)
	if err != nil {
		t.Fatalf("scanSettings: %v", err)
	}
	checkItem(t, "approvals", target.Pod.Approvals, readiness.Warning, "")
	if target.Pod.Approvals.Count != 0 {
		t.Errorf("approvals count = %d; want 0", target.Pod.Approvals.Count)
	}
	checkItem(t, "protected", target.Pod.Protected, readiness.Missing, "main is not protected")
	checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Warning, "no status checks are required")

	// Classic protection on, shown, but no required checks in it: the App
	// may not be shown the real setting, so it is never a warning.
	for _, protection := range []map[string]any{
		{"enabled": true},
		{"enabled": true, "required_status_checks": map[string]any{"contexts": []string{}, "checks": []any{}}},
	} {
		gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
			"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": true, "protection": protection,
		})
		target, err = scanSettings(t, env, ghInstance, ghRepoPath)
		if err != nil {
			t.Fatalf("scanSettings: %v", err)
		}
		checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Unknown, wantGitHubChecksDark)
		// Protected, and its classic protection can't be read: unknown.
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitHubClassic)
	}
}

// The couldn't-check reasons, exact.
const (
	wantGitLabApprovals  = "the review bot can't read the approval rules here"
	wantGitLabProtection = "the review bot can't read branch protection here"
	wantGitHubClassic    = "no ruleset requires reviews, and the review App can't read the classic branch protection (it needs the optional Administration: Read permission)"
	wantGitHubRules      = "the review App can't read this branch's rules"
	wantGitHubChecks     = "the review App can't see this branch's required checks"
	wantGitHubChecksDark = "the review App can't see this branch's required checks (it needs the optional Administration: Read permission)"
)

func unknownHasReason(t *testing.T, pod readiness.PodResult) {
	t.Helper()
	for name, it := range map[string]readiness.PodItem{
		"approvals": pod.Approvals, "protected": pod.Protected, "required checks": pod.RequiredChecks,
	} {
		if it.State == readiness.Unknown && it.Reason == "" {
			t.Errorf("%s is unknown with no reason", name)
		}
	}
}

func TestSettingsRefusedIsUnknown(t *testing.T) {
	const refused = `{"message":"403 Forbidden"}`
	t.Run("gitlab approval rules", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		env.GitLab.Reply(http.MethodGet, glRulesAPI, http.StatusForbidden, refused)
		target, err := scanSettings(t, env, testInstance, testProjectPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitLabApprovals)
		checkItem(t, "protected", target.Pod.Protected, readiness.Passed, "main is protected")
		checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Passed, "pipelines must succeed before merging")
	})
	t.Run("gitlab project approvals", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		env.GitLab.Reply(http.MethodGet, glRulesAPI, http.StatusOK, `[]`)
		env.GitLab.Reply(http.MethodGet, glApprovalsAPI, http.StatusForbidden, refused)
		target, err := scanSettings(t, env, testInstance, testProjectPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitLabApprovals)
	})
	t.Run("gitlab protection", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		refuseProtection(env, http.StatusForbidden, refused)
		target, err := scanSettings(t, env, testInstance, testProjectPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "protected", target.Pod.Protected, readiness.Unknown, wantGitLabProtection)
		if target.Pod.Approvals.State != readiness.Passed {
			t.Errorf("approvals = %+v; want passed", target.Pod.Approvals)
		}
	})
	t.Run("github rules", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Reply(http.MethodGet, ghRulesAPI, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
		gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
			"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": false,
		})
		target, err := scanSettings(t, env, ghInstance, ghRepoPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitHubRules)
		checkItem(t, "protected", target.Pod.Protected, readiness.Unknown, wantGitHubRules)
		checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Unknown, wantGitHubChecks)
	})
	t.Run("github rules, classic protection shown", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Reply(http.MethodGet, ghRulesAPI, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
		gh.Reply(http.MethodGet, ghBranchAPI, http.StatusOK, map[string]any{
			"name": "main", "commit": map[string]any{"sha": sha('d')}, "protected": true,
			"protection": map[string]any{"enabled": true, "required_status_checks": map[string]any{
				"contexts": []string{"ci"}}},
		})
		target, err := scanSettings(t, env, ghInstance, ghRepoPath)
		if err != nil {
			t.Fatal(err)
		}
		// What was read stands; only the approvals can't be known.
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitHubRules)
		checkItem(t, "protected", target.Pod.Protected, readiness.Passed, "main is protected")
		checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Passed, "1 status check is required")
	})
	t.Run("github classic protection", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Reply(http.MethodGet, ghRulesAPI, http.StatusOK, []map[string]any{{"type": "update"}})
		gh.Reply(http.MethodGet, ghClassicAPI, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
		target, err := scanSettings(t, env, ghInstance, ghRepoPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitHubClassic)
		checkItem(t, "protected", target.Pod.Protected, readiness.Passed, "main is protected")
		// Protected, no rule requires checks, the classic ones not shown.
		checkItem(t, "required checks", target.Pod.RequiredChecks, readiness.Unknown, wantGitHubChecksDark)
	})
	t.Run("github approve side unreachable", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, false)
		serveGitHubSettings(gh)
		target, err := scanSettings(t, env, ghInstance, ghRepoPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, env.Engine.approverUnreachableNote(ghInstance))
		if target.Pod.Approvals.Count != 0 {
			t.Errorf("approvals count = %d; want 0", target.Pod.Approvals.Count)
		}
		checkItem(t, "protected", target.Pod.Protected, readiness.Passed, "main is protected")
	})
}

func TestSettingsNoRawErrors(t *testing.T) {
	// A refusal whose body holds a URL and a token. A 500 is retried with
	// real sleeps (7 s), so only one read per platform answers 500.
	const leakURL = "https://internal.example.com/admin"
	const leakToken = "leaked-token-example"
	body := `{"message":"see ` + leakURL + ` with ` + leakToken + `"}`
	check := func(t *testing.T, target scanTarget, err error) {
		t.Helper()
		texts := []string{target.Pod.Approvals.Reason, target.Pod.Protected.Reason, target.Pod.RequiredChecks.Reason}
		if err != nil {
			texts = append(texts, err.Error())
		}
		for _, s := range texts {
			if strings.Contains(s, leakURL) || strings.Contains(s, leakToken) ||
				strings.Contains(s, "gitlab:") || strings.Contains(s, "github:") {
				t.Errorf("raw platform error reached %q", s)
			}
		}
		unknownHasReason(t, target.Pod)
	}

	t.Run("gitlab", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		env.GitLab.Reply(http.MethodGet, glRulesAPI, http.StatusInternalServerError, body)
		refuseProtection(env, http.StatusForbidden, body)
		target, err := scanSettings(t, env, testInstance, testProjectPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitLabApprovals)
		checkItem(t, "protected", target.Pod.Protected, readiness.Unknown, wantGitLabProtection)
		check(t, target, err)

		// The default branch read refused: the error is the reason alone.
		env.GitLab.Reply(http.MethodGet, glProjectAPI, http.StatusForbidden, body)
		target, err = scanSettings(t, env, testInstance, testProjectPath)
		if err == nil || err.Error() != "couldn't read the default branch" {
			t.Fatalf("err = %v; want couldn't read the default branch", err)
		}
		check(t, target, err)
	})
	t.Run("github", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Reply(http.MethodGet, ghRulesAPI, http.StatusOK, []any{})
		gh.Reply(http.MethodGet, ghClassicAPI, http.StatusInternalServerError, map[string]string{"message": body})
		target, err := scanSettings(t, env, ghInstance, ghRepoPath)
		if err != nil {
			t.Fatal(err)
		}
		checkItem(t, "approvals", target.Pod.Approvals, readiness.Unknown, wantGitHubClassic)
		check(t, target, err)

		gh.Reply(http.MethodGet, ghRepoAPI, http.StatusForbidden, map[string]string{"message": body})
		target, err = scanSettings(t, env, ghInstance, ghRepoPath)
		if err == nil || err.Error() != "couldn't read the default branch" {
			t.Fatalf("err = %v; want couldn't read the default branch", err)
		}
		check(t, target, err)
	})
}

func TestSettingsEmptyRepo(t *testing.T) {
	const want = "the repository has no default branch yet"
	t.Run("gitlab no default branch", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		env.GitLab.Reply(http.MethodGet, glProjectAPI, http.StatusOK, map[string]any{"id": testProjectID, "default_branch": nil})
		if _, err := scanSettings(t, env, testInstance, testProjectPath); err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
		if reqs := env.GitLab.Requests(http.MethodGet, glRulesAPI); len(reqs) != 0 {
			t.Error("settings were read with no branch to read them for")
		}
	})
	t.Run("gitlab branch without commits", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		env.GitLab.Reply(http.MethodGet, glCommitAPI, http.StatusNotFound, `{"message":"404 Commit Not Found"}`)
		if _, err := scanSettings(t, env, testInstance, testProjectPath); err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})
	t.Run("github no default branch", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "clone_url": ghCloneURL})
		if _, err := scanSettings(t, env, ghInstance, ghRepoPath); err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})
	t.Run("github empty repository", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Reply(http.MethodGet, ghBranchAPI, http.StatusNotFound, map[string]string{"message": "Branch not found"})
		if _, err := scanSettings(t, env, ghInstance, ghRepoPath); err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})
}

// The other reasons a scan stops on. No clone token still stores what the
// pod read, and the reason is dispatch's own.
func TestSettingsStopReasons(t *testing.T) {
	t.Run("not set up", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		if _, err := scanSettings(t, env, testInstance, "acme/unknown"); err == nil ||
			err.Error() != "pergent.sh hasn't set this repository up yet" {
			t.Fatalf("err = %v", err)
		}
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		env.SeedGitHub(func(p *store.Project) { p.InstallationID = 0 })
		if _, err := scanSettings(t, env, ghInstance, ghRepoPath); err == nil ||
			err.Error() != "pergent.sh hasn't set this repository up yet" {
			t.Fatalf("GitHub without an installation: err = %v", err)
		}
	})
	t.Run("gitlab no clone token", func(t *testing.T) {
		env := newTestEnv(t)
		serveGitLabSettings(env)
		env.SeedProject(func(p *store.Project) { p.DeployTokenSealed = nil })
		target, err := scanSettings(t, env, testInstance, testProjectPath)
		if err == nil || err.Error() != "pergent.sh has no clone token for this repository yet" || err.Error() != reasonNoCloneToken {
			t.Fatalf("err = %v; want dispatch's no-clone-token reason", err)
		}
		if target.Commit != sha('c') || target.Pod.Approvals.State != readiness.Passed ||
			target.Pod.Protected.State != readiness.Passed || target.Pod.RequiredChecks.State != readiness.Passed {
			t.Errorf("pod = %+v, commit %q; want the settings read", target.Pod, target.Commit)
		}
		if len(target.GitToken) != 0 {
			t.Error("a git token came back")
		}
	})
	t.Run("github clone token refused", func(t *testing.T) {
		env := newTestEnv(t)
		gh := addGitHub(t, env, true)
		serveGitHubSettings(gh)
		gh.Handle(http.MethodPost, "/app/installations/55/access_tokens", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte("repository_ids")) {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"token": ghReviewToken, "expires_at": time.Now().Add(time.Hour).UTC()})
		})
		target, err := scanSettings(t, env, ghInstance, ghRepoPath)
		if err == nil || err.Error() != reasonNoCloneToken {
			t.Fatalf("err = %v; want %q", err, reasonNoCloneToken)
		}
		if len(target.GitToken) != 0 {
			t.Error("a git token came back")
		}
	})
	t.Run("instance not connected", func(t *testing.T) {
		env := newTestEnv(t)
		env.Engine.Platforms.Delete(testInstance)
		if _, err := scanSettings(t, env, testInstance, testProjectPath); err == nil ||
			err.Error() != "couldn't read the default branch" {
			t.Fatalf("err = %v", err)
		}
	})
}
