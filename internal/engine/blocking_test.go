package engine

import (
	"net/http"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

// setBlockingNeutral sets blocking_status neutral on every config repository.
func setBlockingNeutral(env *testEnv) {
	for i := range env.Engine.Config.Repositories {
		env.Engine.Config.Repositories[i].BlockingStatus = config.BlockingStatusNeutral
	}
}

// With blocking_status neutral, a review with blocking findings is neutral on
// both platforms (success on GitLab), says so, and is never approved.
func TestPublishBlockingNeutral(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	setBlockingNeutral(env)
	head := sha('a')
	lab := env.Report(1, head, resultFor(t, head, reviewed("has_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	hub := ghReport(t, env, 3, head, resultFor(t, head, reviewed("has_blocking_issues", "a.go")))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	const desc = "Blocking issues found · not blocking merge"
	wantStatus(t, env, head, gitlab.StatusSuccess, desc)
	wantPublished(t, env, lab)
	wantNoApproval(t, env, 1)
	wantPublished(t, env, hub)
	run := onlyCheckRun(t, gh)
	out, _ := run["output"].(map[string]any)
	if run["status"] != "completed" || run["conclusion"] != "neutral" || out["title"] != desc {
		t.Errorf("check run = %v", run)
	}
	if reviews := gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews"); len(reviews) != 0 {
		t.Errorf("approved on GitHub %d times", len(reviews))
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
	for _, id := range []string{lab, hub} {
		if r := env.Run(id); r.DecisionStatus != "neutral" {
			t.Errorf("run %s recorded status %q, want neutral", id, r.DecisionStatus)
		}
	}
}

// A review with blocking findings that can't be posted is neutral too with
// blocking_status neutral, also with failure_status failed.
func TestPublishGiveUpBlockingNeutral(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	setBlockingNeutral(env)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)

	publishUntilGivenUp(t, env, id)

	wantStatus(t, env, head, gitlab.StatusSuccess, "pergent couldn't post its review · not blocking merge")
	wantNoApproval(t, env, 1)
	if r := env.Run(id); r.DecisionStatus != "neutral" {
		t.Errorf("recorded status = %q, want neutral", r.DecisionStatus)
	}
}
