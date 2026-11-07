package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// [CI] These need the test database, like every test that builds a testEnv.
// They run on GitHub, the only platform with linked repositories.

const impactTitle = "Caller of the old signature breaks"

// impactSkillOf is the impact skill as pergent reports it: no reviewed file,
// the changed code as the finding's place and the caller in Linked.
func impactSkillOf(t *testing.T, verdict, severity string, notes ...string) result.Skill {
	t.Helper()
	s := result.Skill{Name: result.ImpactSkill, Verdict: verdict, Notes: notes}
	if severity != "" {
		b, err := json.Marshal(result.Finding{
			ID: "abc123", Fingerprint: sqlFingerprint, File: "render/comment.go", Line: 12, Severity: severity,
			Title: impactTitle, Explanation: "The caller in client.go still passes the old arguments.",
			Linked: &result.Linked{Repo: linkedTarget, File: "client.go", Line: 40},
		})
		if err != nil {
			t.Fatal(err)
		}
		s.Findings = []json.RawMessage{b}
	}
	return s
}

// ghReportLinked is ghReport for a run the Job was given linked repositories
// for: the run records the link before its result arrives.
func ghReportLinked(t *testing.T, env *testEnv, n int64, head string, res []byte) string {
	t.Helper()
	id := ghEnqueue(t, env, n, head)
	if _, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(3600e9)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.SetRunLinks(env.ctx, id, []store.Link{{Target: linkedTarget, CanBlock: true}}); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.MarkDispatched(env.ctx, id, "job-"+id[:8], sha('b'), sha('5'), hashToken("seed-token"), env.Now().Add(3600e9), env.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", res, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return id
}

// ghVisibility sets what GitHub says about the repository and its link.
func ghVisibility(gh *fakeGitHub, source, target string) {
	repo := func(id int64, vis string) map[string]any {
		return map[string]any{"id": id, "private": vis == "private", "visibility": vis}
	}
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, repo(ghRepoID, source))
	gh.Reply(http.MethodGet, "/repos/"+linkedTarget, http.StatusOK, repo(linkedTargetID, target))
}

// ghLastComment is the body of the newest comment written for pull request n.
func ghLastComment(t *testing.T, gh *fakeGitHub, n int64) string {
	t.Helper()
	var body string
	for _, r := range gh.Requests("", "") {
		isNew := r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/issues/3/comments")
		isEdit := r.Method == http.MethodPatch && strings.Contains(r.Path, "/issues/comments/")
		if isNew || isEdit {
			body, _ = r.JSON(t)["body"].(string)
		}
	}
	if body == "" {
		t.Fatal("no comment was written")
	}
	return body
}

// impactOfRun is the impact skill of the result stored on run id.
func impactOfRun(t *testing.T, env *testEnv, id string) result.Skill {
	t.Helper()
	r, err := result.Parse(env.Run(id).Result)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Skills {
		if s.Name == result.ImpactSkill {
			return s
		}
	}
	t.Fatalf("run %s has no impact skill", id)
	return result.Skill{}
}

func wantImpactWithheld(t *testing.T, env *testEnv, gh *fakeGitHub, id string) {
	t.Helper()
	comment := ghLastComment(t, gh, 3)
	for _, bad := range []string{impactTitle, "client.go", "old arguments"} {
		if strings.Contains(comment, bad) {
			t.Errorf("the comment shows %q of the impact check:\n%s", bad, comment)
		}
	}
	if !strings.Contains(comment, "linked repository could not be shown to be as open as this one") {
		t.Errorf("the comment doesn't say the check was left out:\n%s", comment)
	}
	s := impactOfRun(t, env, id)
	if s.Verdict != "failed" || len(s.Findings) != 0 || len(s.Failures) != 1 || s.Failures[0].Message != result.ImpactWithheldMessage {
		t.Errorf("stored impact = %+v; want failed with the fixed message and no finding", s)
	}
	run := onlyCheckRun(t, gh)
	if run["conclusion"] == "success" {
		t.Errorf("check run = %v; want it not a success", run)
	}
	if n := len(gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews")); n != 0 {
		t.Errorf("%d approvals of a review whose impact check failed", n)
	}
	if got := recordedApprovalGH(t, env, 3); got != "" {
		t.Errorf("approval recorded for %s", got)
	}
}

func recordedApprovalGH(t *testing.T, env *testEnv, n int64) string {
	t.Helper()
	all, err := env.Store.Approvals(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.MR == ghKey(n) {
			return a.ApprovedSHA
		}
	}
	return ""
}

// The repository is public and its link went private while the review ran:
// nothing the check found reaches the comment, the check is failed, the
// status is not a success and the bot approves nothing. The rest of the review
// is posted.
func TestPublishRechecksVisibility(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghVisibility(gh, "public", "private")
	head := sha('a')
	id := ghReportLinked(t, env, 3, head, resultFor(t, head,
		reviewed("no_blocking_issues", "render/comment.go"), impactSkillOf(t, "has_blocking_issues", "blocking")))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	wantImpactWithheld(t, env, gh, id)
	if comment := ghLastComment(t, gh, 3); !strings.Contains(comment, "security") {
		t.Errorf("the other skills' results are not posted:\n%s", comment)
	}
}

// A read that fails is a failure too, whichever of the two repositories it is.
func TestPublishVisibilityErrorFailsImpact(t *testing.T) {
	for name, fail := range map[string]string{"the link": "/repos/" + linkedTarget, "the repository": ghRepoAPI} {
		t.Run(name, func(t *testing.T) {
			env, gh := linkedGitHub(t)
			ghVisibility(gh, "private", "public")
			gh.Reply(http.MethodGet, fail, http.StatusInternalServerError, map[string]string{"message": "boom"})
			head := sha('a')
			id := ghReportLinked(t, env, 3, head, resultFor(t, head,
				reviewed("no_blocking_issues", "render/comment.go"), impactSkillOf(t, "has_blocking_issues", "blocking")))
			servePublishGitHub(gh, 3, head)

			publish(t, env)

			wantPublished(t, env, id)
			wantImpactWithheld(t, env, gh, id)
		})
	}
}

// With the rule holding, the check's findings are posted as they were and the
// result is stored as it came.
func TestPublishKeepsAllowedImpact(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghVisibility(gh, "private", "public")
	head := sha('a')
	res := resultFor(t, head, reviewed("no_blocking_issues", "render/comment.go"), impactSkillOf(t, "has_blocking_issues", "blocking"))
	id := ghReportLinked(t, env, 3, head, res)
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	if comment := ghLastComment(t, gh, 3); !strings.Contains(comment, impactTitle) {
		t.Errorf("the allowed check's finding is not in the comment:\n%s", comment)
	}
	if s := impactOfRun(t, env, id); s.Verdict != "has_blocking_issues" || len(s.Findings) != 1 {
		t.Errorf("stored impact = %+v; want it as it came", s)
	}
	if run := onlyCheckRun(t, gh); run["conclusion"] == "success" {
		t.Errorf("check run = %v; a blocking finding is not a success", run)
	}
}

// A republish reads the rule again: the finding posted while the link was open
// is left out once the link goes private.
func TestRepublishChecksVisibilityAgain(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghVisibility(gh, "public", "public")
	head := sha('a')
	id := ghReportLinked(t, env, 3, head, resultFor(t, head,
		reviewed("no_blocking_issues", "render/comment.go"), impactSkillOf(t, "has_blocking_issues", "blocking")))
	servePublishGitHub(gh, 3, head)
	gh.Reply(http.MethodPatch, ghRepoAPI+"/issues/comments/77", http.StatusOK, map[string]any{"id": 77})

	publish(t, env)
	if comment := ghLastComment(t, gh, 3); !strings.Contains(comment, impactTitle) {
		t.Fatalf("the first publish lacks the finding:\n%s", comment)
	}

	ghVisibility(gh, "public", "private")
	gh.Reset()
	servePublishGitHub(gh, 3, head)
	gh.Reply(http.MethodPatch, ghRepoAPI+"/issues/comments/77", http.StatusOK, map[string]any{"id": 77})
	if ok, err := env.Store.Republish(env.ctx, ghKey(3), head, "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}

	publish(t, env)

	wantImpactWithheld(t, env, gh, id)
}

// An image that doesn't know the feature ignores the links and sends no impact
// skill: pergent.sh writes a failed one, and the review is not approved.
func TestMissingImpactIsSynthesisedFailed(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghVisibility(gh, "private", "public")
	head := sha('a')
	id := ghReportLinked(t, env, 3, head, resultFor(t, head, reviewed("no_blocking_issues", "render/comment.go")))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	s := impactOfRun(t, env, id)
	if want := result.ImpactMessage(result.ImpactNotRunMessage, []string{linkedTarget}); s.Verdict != "failed" || len(s.Failures) != 1 || s.Failures[0].Message != want {
		t.Errorf("stored impact = %+v; want the synthesised failed skill naming the link", s)
	}
	comment := ghLastComment(t, gh, 3)
	if !strings.Contains(comment, "the review returned no result for the linked repositories") || !strings.Contains(comment, linkedTarget) {
		t.Errorf("the comment doesn't say the check did not happen, for which repositories:\n%s", comment)
	}
	if run := onlyCheckRun(t, gh); run["conclusion"] == "success" {
		t.Errorf("check run = %v; want it not a success", run)
	}
	if n := len(gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews")); n != 0 {
		t.Errorf("%d approvals with the impact check missing", n)
	}
}

// A run with no recorded links gains no impact skill, and its result is left
// byte for byte as it came: nothing is read or written for it.
func TestNoImpactRunIsLeftAlone(t *testing.T) {
	env, gh := linkedGitHub(t)
	head := sha('a')
	res := resultFor(t, head, reviewed("no_blocking_issues", "render/comment.go"))
	id := ghReport(t, env, 3, head, res)
	servePublishGitHub(gh, 3, head)
	before := env.Run(id).Result

	publish(t, env)

	wantPublished(t, env, id)
	if got := env.Run(id).Result; string(got) != string(before) {
		t.Errorf("the stored result changed:\n%s\nwas\n%s", got, before)
	}
	if strings.Contains(strings.ToLower(ghLastComment(t, gh, 3)), "impact") {
		t.Errorf("the comment of a run with no check mentions it:\n%s", ghLastComment(t, gh, 3))
	}
	if n := len(gh.Requests(http.MethodGet, "/repos/"+linkedTarget)); n != 0 {
		t.Errorf("%d reads of the link for a run that has none", n)
	}
	if n := len(gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews")); n != 1 {
		t.Errorf("%d approvals, want 1", n)
	}
}

// A clean run says so, in the words pergent wrote, and is approved.
func TestCleanImpactSaysSoAndApproves(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghVisibility(gh, "private", "public")
	head := sha('a')
	const note = "Impact: checked acme/api, nothing found"
	id := ghReportLinked(t, env, 3, head, resultFor(t, head,
		reviewed("no_blocking_issues", "render/comment.go"), impactSkillOf(t, "no_blocking_issues", "", note)))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	if comment := ghLastComment(t, gh, 3); !strings.Contains(comment, note) {
		t.Errorf("the comment lacks %q:\n%s", note, comment)
	}
	if n := len(gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews")); n != 1 {
		t.Errorf("%d approvals, want 1", n)
	}
}

// A person dismisses an impact finding, a blocking one too, as any finding:
// the review is decided again from the stored result and is approved.
func TestDismissImpactFinding(t *testing.T) {
	env, gh := linkedGitHub(t)
	ghVisibility(gh, "private", "public")
	head := sha('a')
	id := ghReportLinked(t, env, 3, head, resultFor(t, head,
		reviewed("no_blocking_issues", "render/comment.go"), impactSkillOf(t, "has_blocking_issues", "blocking")))
	servePublishGitHub(gh, 3, head)
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
		MR: ghKey(3), Fingerprint: sqlFingerprint, Severity: "blocking", Reason: "the caller is fixed in its own pull request",
		Actor: "maya (gh)", Teaches: true,
	}); err != nil {
		t.Fatal(err)
	}

	publish(t, env)

	wantPublished(t, env, id)
	comment := ghLastComment(t, gh, 3)
	for _, want := range []string{"Dismissed (1)", impactTitle, "the caller is fixed in its own pull request"} {
		if !strings.Contains(comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, comment)
		}
	}
	if n := len(gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews")); n != 1 {
		t.Errorf("%d approvals after the dismissal, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Re-runs

// impactRerunEnv is a GitHub environment whose library has the skills a and b
// and a review of head by both and the impact check: base, with the versions a
// dispatch records.
func impactRerunEnv(t *testing.T, a, b, impact result.Skill) (env *testEnv, gh *fakeGitHub, head, base string) {
	t.Helper()
	env, gh = linkedGitHub(t)
	env.AddSkill("a", "---\nname: a\n---\nCheck a.\n")
	env.AddSkill("b", "---\nname: b\n---\nCheck b.\n")
	env.SetDefaultSkills("a", "b")
	head = sha('a')
	base = ghReportLinked(t, env, 3, head, resultFor(t, head, a, b, impact))
	if err := env.Store.MarkPublished(env.ctx, base, store.RunDecision{}); err != nil {
		t.Fatal(err)
	}
	recordVersions(t, env, base, map[string]int{"a": 1, "b": 1})
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	return env, gh, head, base
}

func skillsInJob(env *testEnv, id string) int {
	n := 0
	for key := range env.ConfigMap(id).Data {
		if strings.HasPrefix(key, "skill-") {
			n++
		}
	}
	return n
}

// A re-run of a failed skill keeps the earlier impact result: the impact
// check in the earlier result does not read as "the skills changed", which
// would turn the re-run into a full review, and the Job is given no links.
func TestRerunKeepsImpact(t *testing.T) {
	env, _, head, base := impactRerunEnv(t,
		namedSkill(t, "a", "failed", "", ""), namedSkill(t, "b", "no_blocking_issues", "", ""),
		impactSkillOf(t, "no_blocking_issues", "", "Impact: checked acme/api, nothing found"))
	id, created, err := env.Store.EnqueueRerun(env.ctx, ghKey(3), ghRepoPath, head, "main", store.TriggerRerunFailed, []string{"a"}, &base, false)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running || r.Trigger != store.TriggerRerunFailed || len(r.SkillSubset) != 1 || r.SkillSubset[0] != "a" {
		t.Fatalf("run = %s %q %v (%s); want the re-run of a\n%s", r.State, r.Trigger, r.SkillSubset, r.FailureReason, env.Logs())
	}
	if _, ok := pergentEnv(env, id)["PERGENT_LINKED_REPOS"]; ok {
		t.Error("a re-run of another skill is given linked repositories")
	}
	if r.LinkedRepos != nil {
		t.Errorf("run links = %+v; want none", r.LinkedRepos)
	}
	if n := skillsInJob(env, id); n != 1 {
		t.Errorf("%d skills in the Job, want a alone", n)
	}
}

// pergent.sh asks again for a failed impact check: a failed impact is the only
// thing to re-run. pergent needs at least one skill, so the first by name runs
// with it and is merged too; every link is checked.
func TestReviewFailedRerunsImpactAlone(t *testing.T) {
	failed := impactSkillOf(t, "failed", "")
	failed.Failures = []result.Failure{{Message: result.ImpactNotRunMessage}}
	env, _, head, base := impactRerunEnv(t,
		namedSkill(t, "a", "no_blocking_issues", "", ""), namedSkill(t, "b", "no_blocking_issues", "", ""), failed)
	id, created, err := env.Store.EnqueueRerun(env.ctx, ghKey(3), ghRepoPath, head, "main", store.TriggerRerunFailed, []string{"impact"}, &base, false)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running || r.Trigger != store.TriggerRerunFailed {
		t.Fatalf("run = %s %q (%s); want a re-run, not a full review\n%s", r.State, r.Trigger, r.FailureReason, env.Logs())
	}
	if len(r.SkillSubset) != 2 || r.SkillSubset[0] != "a" || r.SkillSubset[1] != "impact" {
		t.Errorf("subset = %v; want the carrier skill, then impact", r.SkillSubset)
	}
	if n := skillsInJob(env, id); n != 1 {
		t.Errorf("%d skills in the Job, want one", n)
	}
	entries := linkedEntries(t, env, id)
	if len(entries) != 1 || entries[0].Name != linkedTarget || entries[0].Error != "" {
		t.Errorf("entries = %+v; want the link, to clone", entries)
	}
	if len(r.LinkedRepos) != 1 {
		t.Errorf("run links = %+v; want the link recorded", r.LinkedRepos)
	}
	if got := checkoutEnv(t, env, id)["LINKED_CLONES"]; got != "linked-1 https://github.com/acme/api.git" {
		t.Errorf("LINKED_CLONES = %q", got)
	}
}

// The setting was turned off since the re-run was asked for: the check can't
// be repeated, so the run becomes a full review, which has no impact check.
func TestRerunOfImpactWithTheSettingOffBecomesAFullReview(t *testing.T) {
	failed := impactSkillOf(t, "failed", "")
	env, _, head, base := impactRerunEnv(t,
		namedSkill(t, "a", "no_blocking_issues", "", ""), namedSkill(t, "b", "no_blocking_issues", "", ""), failed)
	off := false
	repos := env.Engine.Config.Repositories
	repos[len(repos)-1].LinkedReposEnabled = &off
	id, _, err := env.Store.EnqueueRerun(env.ctx, ghKey(3), ghRepoPath, head, "main", store.TriggerRerunFailed, []string{"impact"}, &base, false)
	if err != nil {
		t.Fatal(err)
	}

	dispatch(t, env)
	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running || r.Trigger != store.TriggerRerun || len(r.SkillSubset) != 0 || r.BaseRunID != nil {
		t.Fatalf("run = %s %q %v; want a full re-run\n%s", r.State, r.Trigger, r.SkillSubset, env.Logs())
	}
	if _, ok := pergentEnv(env, id)["PERGENT_LINKED_REPOS"]; ok {
		t.Error("the Job is given linked repositories with the setting off")
	}
	if n := skillsInJob(env, id); n != 2 {
		t.Errorf("%d skills in the Job, want both", n)
	}
}

// A push after a review whose impact check failed, with the setting turned
// off since: the incremental review would keep the failed check for good (the
// merge keeps an open check the new run lacks), so the push is a full review.
func TestPushWithAStaleOpenImpactIsAFullReview(t *testing.T) {
	env, gh := linkedGitHub(t)
	on := true
	repos := env.Engine.Config.Repositories
	repos[len(repos)-1].IncrementalReviews = &on
	headA, headB := sha('1'), sha('2')
	ghPublished(t, env, gh, headA, []string{"a.go", "b.go"}, impactSkillJSON("failed"))
	off := false
	repos[len(repos)-1].LinkedReposEnabled = &off

	id := ghEnqueue(t, env, 3, headB)
	gh.ServePR(3, ghPR(3, headB))
	serveCompare(gh, headA, headB, "c.go")
	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running {
		t.Fatalf("state = %s (%s)\n%s", r.State, r.FailureReason, env.Logs())
	}
	if _, ok := env.ConfigMap(id).Data["carried.json"]; ok || r.Incremental || r.BaseRunID != nil {
		t.Errorf("incremental = %t, base = %v, carried file = %t; want a full review", r.Incremental, r.BaseRunID, ok)
	}

	// Without the stale check the same push is incremental (the control).
	env2, gh2 := linkedGitHub(t)
	repos2 := env2.Engine.Config.Repositories
	repos2[len(repos2)-1].IncrementalReviews = &on
	ghPublished(t, env2, gh2, headA, []string{"a.go", "b.go"})
	repos2[len(repos2)-1].LinkedReposEnabled = &off
	id2 := ghEnqueue(t, env2, 3, headB)
	gh2.ServePR(3, ghPR(3, headB))
	serveCompare(gh2, headA, headB, "c.go")
	dispatch(t, env2)
	if _, ok := env2.ConfigMap(id2).Data["carried.json"]; !ok {
		t.Errorf("the control push is not incremental\n%s", env2.Logs())
	}
}
