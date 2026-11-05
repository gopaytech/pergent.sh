package engine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	styleSkill = "---\nname: style\n---\nCheck the style.\n"
	// What the two skills say, to find them in a Job's inputs.
	securityText, styleText = "Look for security problems.", "Check the style."
)

// jobInputs is everything in the Job inputs of run id, joined. It fails the
// test when the run has no inputs, naming why.
func jobInputs(t *testing.T, env *testEnv, id string) string {
	t.Helper()
	cm := env.ConfigMap(id)
	if cm == nil {
		r := env.Run(id)
		t.Fatalf("no job inputs; run state %s, reason %q\n%s", r.State, r.FailureReason, env.Logs())
	}
	var all strings.Builder
	for _, v := range cm.Data {
		all.WriteString(v)
	}
	return all.String()
}

// attentionItem is the "Needs attention" item of kind for the test repository,
// or nil.
func attentionItem(t *testing.T, env *testEnv, kind string) *store.Attention {
	t.Helper()
	items, err := env.Store.Attention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range items {
		if a.Instance == testInstance && a.Project == testProjectPath && a.Kind == kind {
			return &items[i]
		}
	}
	return nil
}

// repoKinds runs f for a repository from the config and one from the
// dashboard, both naming skills (nil: the default list).
func repoKinds(t *testing.T, skills []string, f func(t *testing.T, env *testEnv)) {
	t.Helper()
	t.Run("config repository", func(t *testing.T) {
		env := newTestEnv(t)
		env.Engine.Config.Repositories[0].Skills = skills
		f(t, env)
	})
	t.Run("dashboard repository", func(t *testing.T) {
		env := newTestEnv(t)
		env.Engine.Config.Repositories = nil
		dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: testProjectPath, CreatedBy: "alice"})
		if skills != nil {
			// Raw SQL: the store refuses names the library doesn't have yet.
			setDashboardSkills(t, env, testInstance, testProjectPath, skills)
		}
		f(t, env)
	})
}

// queue queues a run of merge request 3 at a new head and serves what its
// dispatch reads.
func queue(env *testEnv) (id, head string) {
	head = sha('a')
	id = env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	return id, head
}

// The library lives in the database, not in a directory or the dashboard's
// settings: with no dashboard.url (and no skills_dir) a review still reads its
// skills from the store.
func TestLibraryWithoutDashboard(t *testing.T) {
	env := newTestEnv(t)
	if env.Engine.Config.DashboardEnabled() {
		t.Fatal("the test config has the dashboard on")
	}
	env.AddSkill("style", styleSkill)
	env.SetDefaultSkills("style")
	id, _ := queue(env)

	dispatch(t, env)

	if got := jobInputs(t, env, id); !strings.Contains(got, styleText) {
		t.Errorf("inputs = %q, want the library's style skill", got)
	}
}

func TestNullSkillsUsesDefaults(t *testing.T) {
	repoKinds(t, nil, func(t *testing.T, env *testEnv) {
		env.AddSkill("style", styleSkill)
		env.SetDefaultSkills("style")
		id, _ := queue(env)

		dispatch(t, env)

		got := jobInputs(t, env, id)
		if !strings.Contains(got, styleText) || strings.Contains(got, securityText) {
			t.Errorf("inputs = %q, want only the default list's style skill", got)
		}
	})
}

func TestNamedSkillsExactly(t *testing.T) {
	repoKinds(t, []string{"style"}, func(t *testing.T, env *testEnv) {
		env.AddSkill("style", styleSkill) // default list: security and style
		id, _ := queue(env)

		dispatch(t, env)

		got := jobInputs(t, env, id)
		if !strings.Contains(got, styleText) || strings.Contains(got, securityText) {
			t.Errorf("inputs = %q, want exactly the named style skill", got)
		}
	})
}

// A skill named by a repository that the library lacks fails the run with
// that skill's name, with no fallback to the other skills; the failed review
// is never approved.
func TestMissingLibrarySkillFailsRun(t *testing.T) {
	repoKinds(t, []string{"security", "gone"}, func(t *testing.T, env *testEnv) {
		id, head := queue(env)

		dispatch(t, env)

		r := env.Run(id)
		if r.State != store.Reported || r.FailureReason != `skill "gone" is not in the skill library` {
			t.Fatalf("run = %s %q, want reported, the missing skill named", r.State, r.FailureReason)
		}
		if len(env.Jobs()) != 0 {
			t.Errorf("a job was launched")
		}
		a := attentionItem(t, env, attSkillMissing)
		if a == nil || !strings.Contains(a.Message, `skill "gone" is not in the skill library`) {
			t.Errorf("attention skill_missing = %+v, want it recorded with the reason", a)
		}

		servePublish(env, 3, head)
		publish(t, env)
		wantStatus(t, env, head, gitlab.StatusFailed, "pergent couldn't review this MR")
		if n := len(env.GitLab.Requests(http.MethodPost, approvePath(3))); n != 0 {
			t.Errorf("a review that failed was approved %d times", n)
		}
	})
}

// A dashboard repository's skills can name a skill deleted from the library
// behind its back (raw SQL here: the store refuses it). The run fails; no
// skill is dropped and nothing falls back to the rest of the library.
func TestDispatchDashboardRepoMissingSkillsFailsRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		skills []string
		want   string
	}{
		{"some left", []string{"security", "gone"}, `skill "gone" is not in the skill library`},
		{"none left", []string{"gone", "also-gone"}, `skill "gone" is not in the skill library`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.Engine.Config.Repositories = nil
			env.AddSkill("style", styleSkill)
			dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: testProjectPath, CreatedBy: "alice"})
			setDashboardSkills(t, env, testInstance, testProjectPath, tc.skills)
			id, _ := queue(env)

			dispatch(t, env)

			r := env.Run(id)
			if r.State != store.Reported || r.FailureReason != tc.want {
				t.Fatalf("run = %s %q, want reported %q", r.State, r.FailureReason, tc.want)
			}
			if len(env.Jobs()) != 0 {
				t.Errorf("a job was launched: nothing falls back to the library")
			}
		})
	}
}

func TestNoFallbackWhenAllNamedGone(t *testing.T) {
	repoKinds(t, []string{"gone", "also-gone"}, func(t *testing.T, env *testEnv) {
		env.AddSkill("style", styleSkill)
		id, _ := queue(env)

		dispatch(t, env)

		if r := env.Run(id); r.State != store.Reported || r.FailureReason != `skill "gone" is not in the skill library` {
			t.Errorf("run = %s %q, want reported with the first missing skill named", r.State, r.FailureReason)
		}
		if len(env.Jobs()) != 0 {
			t.Errorf("a job was launched with the library's skills instead")
		}
	})
}

// A detected repository skill that is turned on but gone from the target
// branch fails the run.
func TestTurnedOnRepoSkillGoneFailsRun(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].RepoSkills = []string{"team"}
	id, _ := queue(env)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Reported || r.FailureReason != `skill "team" is turned on but not found in the repository` {
		t.Fatalf("run = %s %q, want reported, the skill named", r.State, r.FailureReason)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
	if attentionItem(t, env, attSkillMissing) == nil {
		t.Errorf("attention skill_missing is not recorded")
	}
}

// A repository skill in .pergent/skills counts: a repository with an empty
// library still reviews.
func TestRepoSkillAloneIsAReview(t *testing.T) {
	env := newTestEnv(t)
	env.SetDefaultSkills()
	id, _ := queue(env)
	env.GitLab.Reply(http.MethodGet, treePath(), http.StatusOK, []gitlab.TreeEntry{
		{Name: "team.md", Path: ".pergent/skills/team.md", Type: "blob"},
	})
	env.GitLab.Reply(http.MethodGet, filePath(".pergent/skills/team.md"), http.StatusOK, "---\nname: team\n---\nOur rules.\n")

	dispatch(t, env)

	if got := jobInputs(t, env, id); !strings.Contains(got, "Our rules.") {
		t.Errorf("inputs = %q, want the repository's own skill", got)
	}
	if env.Run(id).State == store.Skipped {
		t.Errorf("a repository with its own skill was skipped")
	}
}

// With no skills at all there is no review: no Job, no comment and no status,
// and "Needs attention" says why.
func TestNoSkillsSkips(t *testing.T) {
	repoKinds(t, nil, func(t *testing.T, env *testEnv) {
		env.SetDefaultSkills()
		id, head := queue(env)

		dispatch(t, env)

		r := env.Run(id)
		if r.State != store.Skipped || r.SkipReason != "this repository has no skills" {
			t.Fatalf("run = %s %q, want skipped, no skills", r.State, r.SkipReason)
		}
		if len(env.Jobs()) != 0 {
			t.Errorf("a job was launched")
		}
		for _, req := range env.GitLab.Requests("", "") {
			if req.Method != http.MethodGet {
				t.Errorf("a skipped run made the write %s %s", req.Method, req.Path)
			}
		}
		if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))) + len(env.GitLab.Requests(http.MethodPost, notesPath(3))); n != 0 {
			t.Errorf("a skipped run set a status or comment")
		}
		a := attentionItem(t, env, attNoSkills)
		if a == nil || a.Message != "this repository has no skills" {
			t.Errorf("attention no_skills = %+v", a)
		}
	})
}

// Problems clear when a run of the repository dispatches a Job.
func TestAttentionClearedWhenAJobStarts(t *testing.T) {
	env := newTestEnv(t)
	for _, kind := range []string{attNoSkills, attSkillMissing} {
		if err := env.Store.SetAttention(env.ctx, testInstance, testProjectPath, kind, "old"); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := queue(env)

	dispatch(t, env)

	if env.Job(id) == nil {
		t.Fatalf("no job started\n%s", env.Logs())
	}
	if attentionItem(t, env, attNoSkills) != nil || attentionItem(t, env, attSkillMissing) != nil {
		t.Errorf("the skills problems are still listed after a job started")
	}
}

// A skill_missing problem is stale once the repository's skills all resolve,
// also when they turn out to be none: the list then shows no_skills alone.
func TestNoSkillsClearsSkillMissing(t *testing.T) {
	env := newTestEnv(t)
	env.SetDefaultSkills()
	if err := env.Store.SetAttention(env.ctx, testInstance, testProjectPath, attSkillMissing, "old"); err != nil {
		t.Fatal(err)
	}
	id, _ := queue(env)

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Skipped {
		t.Fatalf("run = %s, want skipped", r.State)
	}
	if attentionItem(t, env, attNoSkills) == nil {
		t.Errorf("attention no_skills is not recorded")
	}
	if attentionItem(t, env, attSkillMissing) != nil {
		t.Errorf("a stale skill_missing is still listed beside no_skills")
	}
}

// An approval of the head a skipped run was to review is withdrawn: nothing
// reviewed it, so nothing stands behind it.
func TestSkippedRunWithdrawsApprovalOfSameHead(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	env.Publish(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	env.SetDefaultSkills()
	id, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Skipped {
		t.Fatalf("state = %s, want skipped", r.State)
	}
	if reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1)); len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Errorf("unapprovals = %d, want 1 by the approve bot", len(reqs))
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 0 {
		t.Errorf("a skipped run set a commit status")
	}
}

// When the approve bot can't withdraw right now the withdrawal is owed, and
// the approval sweep makes it later.
func TestSkippedRunOwesWithdrawalWhenItFails(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	env.Publish(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	env.SetDefaultSkills()
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Skipped {
		t.Fatalf("state = %s, want skipped", r.State)
	}
	if got := pending(t, env); len(got) != 1 || got[0].MR != env.MRKey(1) {
		t.Errorf("pending withdrawals = %v, want MR 1's recorded as owed", got)
	}
}

// A push that is skipped leaves the earlier head's approval stale: the
// cleanup lists it and takes it off.
func TestSkippedRunOnNewHeadIsStaleApproval(t *testing.T) {
	env := newTestEnv(t)
	h1, h2 := sha('a'), sha('c')
	env.Publish(1, h1, resultFor(t, h1, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), h1, "main"); err != nil {
		t.Fatal(err)
	}
	env.SetDefaultSkills()
	id := env.Enqueue(1, h2)
	env.GitLab.SetMR(openMR(1, h2))

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Skipped {
		t.Fatalf("state = %s, want skipped", r.State)
	}
	stale, err := env.Store.StaleApprovals(env.ctx, env.Now())
	if err != nil || len(stale) != 1 || stale[0].MR != env.MRKey(1) || stale[0].ApprovedSHA != h1 {
		t.Fatalf("StaleApprovals = %v, %v; want MR 1's approval of the old head", stale, err)
	}
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	cleanup(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("unapprovals = %d, want 1", n)
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
}

// A repeated webhook for a skipped head queues nothing; a re-run command
// reviews it again, now that the repository has skills.
func TestRerunOfSkippedHeadDispatches(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	env.SetDefaultSkills()
	id := env.Enqueue(1, head)
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Skipped {
		t.Fatalf("state = %s, want skipped", r.State)
	}

	again, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, head, "main")
	if err != nil || created || again != id {
		t.Fatalf("EnqueueRun of the same head = %s, %v, %v; want the skipped run, no new one", again, created, err)
	}

	env.SetDefaultSkills("security")
	rerun, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false)
	if err != nil || !created || rerun == id {
		t.Fatalf("EnqueueRerun = %s, %v, %v; want a new run", rerun, created, err)
	}
	dispatch(t, env)
	if env.Job(rerun) == nil {
		t.Errorf("the re-run of a skipped head started no job\n%s", env.Logs())
	}
}

// The skills are read when the run is dispatched, not when it was queued.
func TestEditBetweenEnqueueAndDispatch(t *testing.T) {
	env := newTestEnv(t)
	id, _ := queue(env)
	const edited = "Look for injection problems."
	if _, err := env.Store.UpdateLibrarySkill(env.ctx, "security", "---\nname: security\n---\n"+edited+"\n", "alice", 1, env.Now()); err != nil {
		t.Fatal(err)
	}

	dispatch(t, env)

	if got := jobInputs(t, env, id); !strings.Contains(got, edited) || strings.Contains(got, securityText) {
		t.Errorf("inputs = %q, want the content as edited", got)
	}
}
