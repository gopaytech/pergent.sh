package engine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The publisher records what it decided on the run, for the dashboard.
func TestPublishRecordsDecision(t *testing.T) {
	head := sha('a')
	clean := func(t *testing.T) []byte { return resultFor(t, head, reviewed("no_blocking_issues", "a.go")) }
	for name, tc := range map[string]struct {
		result   func(t *testing.T) []byte
		failure  string
		setup    func(env *testEnv)
		want     store.RunDecision
		inReason string
	}{
		"approved": {result: clean, want: store.RunDecision{Outcome: store.OutcomeApproved, Status: "success", Approved: true}},
		"blocking": {
			result: func(t *testing.T) []byte { return resultFor(t, head, reviewed("has_blocking_issues", "a.go")) },
			want:   store.RunDecision{Outcome: store.OutcomeBlocking, Status: "failed"}, inReason: "Blocking issues found",
		},
		"failed": {failure: "the review Job ran out of memory (OOMKilled)",
			want: store.RunDecision{Outcome: store.OutcomeFailed, Status: "failed"}, inReason: "OOMKilled"},
		"draft": {result: clean, setup: func(env *testEnv) {
			mr := openMR(1, head)
			mr.Draft = true
			env.GitLab.SetMR(mr)
		}, want: store.RunDecision{Outcome: store.OutcomeDraft, Status: "success"}, inReason: "draft"},
		"approval off": {result: clean, setup: func(env *testEnv) {
			off := false
			env.Engine.Config.Repositories[0].Approve = &off
		}, want: store.RunDecision{Outcome: store.OutcomeNoBlocking, Status: "success"}, inReason: "approval is turned off"},
		// Merged between the decision and the approval: the approval is
		// held back and the run says so.
		"closed before approval": {result: clean, setup: func(env *testEnv) {
			n := 0
			env.GitLab.Handle(http.MethodGet, mrPath(1), func(w http.ResponseWriter, _ *http.Request) {
				n++
				mr := openMR(1, head)
				if n > 1 {
					mr.State = "merged"
				}
				writeJSON(w, http.StatusOK, mr)
			})
		}, want: store.RunDecision{Outcome: store.OutcomeNoBlocking, Status: "success"}, inReason: "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			var id string
			if tc.failure != "" {
				id = env.Dispatched(1, head)
				if _, err := env.Store.ReportFailure(env.ctx, id, tc.failure, env.Now()); err != nil {
					t.Fatal(err)
				}
			} else {
				id = env.Report(1, head, tc.result(t))
			}
			servePublish(env, 1, head)
			if tc.setup != nil {
				tc.setup(env)
			}

			publish(t, env)

			r := env.Run(id)
			if r.State != store.Published || r.Outcome != tc.want.Outcome || r.DecisionStatus != tc.want.Status ||
				r.Approved != tc.want.Approved {
				t.Errorf("run = state %s outcome %q status %q approved %v; want %+v",
					r.State, r.Outcome, r.DecisionStatus, r.Approved, tc.want)
			}
			if tc.inReason == "" && r.OutcomeReason != "" || !strings.Contains(r.OutcomeReason, tc.inReason) {
				t.Errorf("reason = %q, want one containing %q", r.OutcomeReason, tc.inReason)
			}
		})
	}
}

// A draft marked ready is decided again: the run is then approved.
func TestPublishRecordsDecisionAgainWhenReady(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	draft := openMR(1, head)
	draft.Draft = true
	env.GitLab.SetMR(draft)
	publish(t, env)
	if r := env.Run(id); r.Outcome != store.OutcomeDraft {
		t.Fatalf("outcome = %q, want draft", r.Outcome)
	}

	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	if ok, err := env.Store.Republish(env.ctx, env.MRKey(1), head, "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	publish(t, env)
	if r := env.Run(id); r.Outcome != store.OutcomeApproved || !r.Approved {
		t.Errorf("after ready: outcome %q approved %v", r.Outcome, r.Approved)
	}
}

// The model and where each skill came from are recorded at dispatch: a
// repository skill replacing a library one of the same name is "repo".
func TestDispatchRecordsModelAndSkillSources(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Library["style"] = []byte("---\nname: style\n---\nCheck the style.\n")
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	env.GitLab.Reply(http.MethodGet, treePath(), http.StatusOK, []gitlab.TreeEntry{
		{Name: "security.md", Path: ".pergent/skills/security.md", Type: "blob"},
		{Name: "team.md", Path: ".pergent/skills/team.md", Type: "blob"},
	})
	env.GitLab.Reply(http.MethodGet, filePath(".pergent/skills/security.md"), http.StatusOK, "---\nname: security\n---\nOur security rules.\n")
	env.GitLab.Reply(http.MethodGet, filePath(".pergent/skills/team.md"), http.StatusOK, "---\nname: team\n---\nOur rules.\n")

	dispatch(t, env)
	env.Engine.Config.LLM.Model = "another-model"

	r := env.Run(id)
	if r.State != store.Running {
		t.Fatalf("state = %s (%s)", r.State, r.FailureReason)
	}
	if r.Model == nil || *r.Model != "claude-sonnet-4" {
		t.Errorf("model = %v", r.Model)
	}
	want := map[string]string{"security": "repo", "team": "repo", "style": "library"}
	if len(r.SkillSources) != len(want) {
		t.Errorf("skill sources = %v, want %v", r.SkillSources, want)
	}
	for k, v := range want {
		if r.SkillSources[k] != v {
			t.Errorf("skill sources = %v, want %v", r.SkillSources, want)
		}
	}
}
