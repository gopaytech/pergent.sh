package engine

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A repository's own skill named impact, with the feature off: the run was
// dispatched with it (its sources name it), so it is an ordinary skill
// everywhere. These need no database.

func ownImpactRun() store.Run {
	return store.Run{SkillSources: map[string]string{"security": "library", "impact": "library"}}
}

func ownImpactResult(t *testing.T, run store.Run, verdict string) *result.Result {
	t.Helper()
	own := namedSkill(t, "impact", verdict, "", "")
	r, err := parsedFor(run, resultFor(t, sha('a'), namedSkill(t, "security", "no_blocking_issues", "", ""), own))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOwnImpactIsNotTheCheck(t *testing.T) {
	r := ownImpactResult(t, ownImpactRun(), "no_blocking_issues")
	if r.IsCheck("impact") || !impactIsCheck(store.Run{}) {
		t.Errorf("IsCheck = %v, impactIsCheck(no sources) = %v; want an own skill, and the check when nothing is recorded", r.IsCheck("impact"), impactIsCheck(store.Run{}))
	}
	// The same result of a run that was dispatched without such a skill is the check.
	run := store.Run{SkillSources: map[string]string{"security": "library"}}
	if r, err := parsedFor(run, resultFor(t, sha('a'), namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "impact", "no_blocking_issues", "", ""))); err != nil || !r.IsCheck("impact") {
		t.Errorf("a run without the skill: %v, %v; want the check", r, err)
	}
}

// Findings: an own skill is judged by the files it reviewed, like any skill; the
// check covers the whole change.
func TestOwnImpactThreadsAreCoveredByItsFiles(t *testing.T) {
	r := ownImpactResult(t, ownImpactRun(), "no_blocking_issues")
	if fs := buildFindings(r, nil, redactSecret); fs.skills["impact"].AllFiles || !fs.skills["impact"].Reviewed["impact.go"] {
		t.Errorf("coverage = %+v; want its reviewed files, not the whole change", fs.skills["impact"])
	}
	check, err := parsedFor(store.Run{SkillSources: map[string]string{"security": "library"}}, resultFor(t, sha('a'), namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "impact", "no_blocking_issues", "", "")))
	if err != nil {
		t.Fatal(err)
	}
	if fs := buildFindings(check, nil, redactSecret); !fs.skills["impact"].AllFiles {
		t.Errorf("the check's coverage = %+v; want the whole change", fs.skills["impact"])
	}
}

func TestOwnImpactFates(t *testing.T) {
	run := ownImpactRun()
	run.HeadSHA, run.Result = sha('a'), resultFor(t, sha('a'), namedSkill(t, "impact", "no_blocking_issues", "", ""))
	if got := fatesInput([]store.Run{run}, nil).Reviews[0].Skills[0]; got.Whole {
		t.Errorf("an own skill named impact is Whole: %+v", got)
	}
}

// Carried over, and compared with the skills of the repository, like any other.
func TestOwnImpactCarriesAndMatchesTheSkills(t *testing.T) {
	r := ownImpactResult(t, ownImpactRun(), "no_blocking_issues")
	hashes := map[string]string{"security": "h1", "impact": "h2"}
	got := carriedFiles(r, hashes, hashes, []string{"other.go"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(got["impact"]) != 1 || got["impact"][0] != "impact.go" {
		t.Errorf("carried = %v; want impact's file carried like any skill's", got)
	}
	in := &inputs.Inputs{Skills: map[string][]byte{"security": nil, "impact": nil}}
	if reason := skillsChanged(r, in); reason != "" {
		t.Errorf("skillsChanged = %q; want the same skills", reason)
	}
	// A result whose impact is the check, against inputs that now hold a skill
	// of that name, is a change of skills.
	check, _ := parsedFor(store.Run{SkillSources: map[string]string{"security": "library"}}, resultFor(t, sha('a'), namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "impact", "no_blocking_issues", "", "")))
	if reason := skillsChanged(check, in); reason == "" {
		t.Error("a check in the earlier result and a skill now read as the same skills")
	}
}

// holdImpact leaves a result with an own skill named impact as it is, whatever
// it says, and writes no failed check for it.
func TestHoldImpactLeavesAnOwnSkillAlone(t *testing.T) {
	run := ownImpactRun()
	run.Result = resultFor(t, sha('a'), namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "impact", "has_blocking_issues", "", ""))
	// With links recorded too (an image that has no reservation could do this).
	run.LinkedRepos = []store.Link{{Target: "acme/api"}}
	e := &Engine{}
	got, err := e.holdImpact(t.Context(), run, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || string(got.Result) != string(run.Result) {
		t.Fatalf("holdImpact changed an own skill's result: %v\n%s", err, got.Result)
	}
}

// The messages pergent.sh writes name the links the run was given; withheld
// findings leave nothing of pergent's comment behind.
func TestImpactFailureNamesTheLinks(t *testing.T) {
	msg := result.ImpactMessage(result.ImpactNotRunMessage, linkTargets(store.Run{LinkedRepos: []store.Link{{Target: "acme/api"}, {Target: "acme/web"}}}))
	if !strings.HasSuffix(msg, " Linked repositories: acme/api, acme/web.") || !strings.HasPrefix(msg, result.ImpactNotRunMessage) {
		t.Errorf("message = %q", msg)
	}
	if got := result.ImpactMessage(result.ImpactNotRunMessage, nil); got != result.ImpactNotRunMessage {
		t.Errorf("no links: %q", got)
	}
}

// --- [CI] these need the test database, like every test that builds a testEnv ---

// A review of the skill named impact and another, with the feature off: asked
// again, "impact" is a skill to re-run, not the linked repositories' check.
func TestOwnImpactSkillIsReRunLikeAnyOther(t *testing.T) {
	env := newTestEnv(t)
	env.AddSkill("a", "---\nname: a\n---\nCheck a.\n")
	env.AddSkill("impact", "---\nname: impact\n---\nCheck impact.\n")
	env.SetDefaultSkills("a", "impact")
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head,
		namedSkill(t, "a", "no_blocking_issues", "", ""), namedSkill(t, "impact", "failed", "", "")))
	recordVersions(t, env, base, map[string]int{"a": 1, "impact": 1})
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"impact"}, base)
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running || r.Trigger != store.TriggerRerunFailed || len(r.SkillSubset) != 1 || r.SkillSubset[0] != "impact" {
		t.Fatalf("run = %s %q %v (%s); want the re-run of the skill impact\n%s", r.State, r.Trigger, r.SkillSubset, r.FailureReason, env.Logs())
	}
	if n := skillsInJob(env, id); n != 1 {
		t.Errorf("%d skills in the Job, want impact alone", n)
	}
	if r.SkillSources["impact"] == "" || r.SkillSources["a"] == "" {
		t.Errorf("sources = %v; want both skills recorded", r.SkillSources)
	}
	if _, ok := pergentEnv(env, id)["PERGENT_LINKED_REPOS"]; ok || r.LinkedRepos != nil {
		t.Error("a re-run of a skill is given linked repositories")
	}
}

// While linked repositories are on, a skill named impact is refused: it never
// runs where its result could read as the check's.
func TestSkillNamedImpactIsRefusedWhileLinked(t *testing.T) {
	env, gh := linkedGitHub(t)
	env.AddSkill("impact", "---\nname: impact\n---\nCheck impact.\n")
	env.SetDefaultSkills("impact")
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	serveCompare(gh, sha('b'), head, "render/comment.go")

	dispatch(t, env)

	r := env.Run(id)
	if r.FailureReason != reasonImpactReserved {
		t.Fatalf("run = %s, reason %q; want the reservation named\n%s", r.State, r.FailureReason, env.Logs())
	}
	if env.Job(id) != nil {
		t.Error("a Job was started")
	}
	if r.LinkedRepos != nil {
		t.Errorf("links recorded: %+v", r.LinkedRepos)
	}
}

// With the feature off the same skill runs as it always did.
func TestSkillNamedImpactRunsWithTheFeatureOff(t *testing.T) {
	env, gh := linkedGitHub(t)
	off := false
	repos := env.Engine.Config.Repositories
	repos[len(repos)-1].LinkedReposEnabled = &off
	env.AddSkill("impact", "---\nname: impact\n---\nCheck impact.\n")
	env.SetDefaultSkills("impact")
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Running || env.Job(id) == nil || r.SkillSources["impact"] == "" {
		t.Fatalf("run = %s (%s), sources %v\n%s", r.State, r.FailureReason, r.SkillSources, env.Logs())
	}
}
