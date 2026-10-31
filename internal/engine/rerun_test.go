package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// namedSkill is a skill of the given name and verdict that reviewed one file
// and has one finding of severity with fingerprint fp ("" for none).
func namedSkill(t *testing.T, name, verdict, severity, fp string) result.Skill {
	t.Helper()
	s := result.Skill{Name: name, Verdict: verdict, Files: result.Files{Reviewed: []string{name + ".go"}}}
	if fp != "" {
		b, err := json.Marshal(result.Finding{
			ID: fp[:6], Fingerprint: fp, File: name + ".go", Line: 3, Severity: severity,
			Title: "finding of " + name, Explanation: "because",
		})
		if err != nil {
			t.Fatal(err)
		}
		s.Findings = []json.RawMessage{b}
	}
	return s
}

// rerunOf queues a re-run of head on merge request 1 that reviews subset and
// builds on base, as a command or a button would.
func rerunOf(t *testing.T, env *testEnv, head, trigger string, subset []string, base string) string {
	t.Helper()
	id, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", trigger, subset, &base)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	return id
}

func TestSubsetJobGetsOnlyThoseSkills(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Library["style"] = []byte("---\nname: style\n---\nCheck the style.\n")
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head, namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "style", "failed", "", "")))
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"style", "gone"}, base)
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	// The repository's own style skill replaces the library one.
	env.GitLab.Reply(http.MethodGet, treePath(), http.StatusOK, []gitlab.TreeEntry{
		{Name: "style.md", Path: ".pergent/skills/style.md", Type: "blob"},
	})
	env.GitLab.Reply(http.MethodGet, filePath(".pergent/skills/style.md"), http.StatusOK, "---\nname: style\n---\nOur own style rules.\n")

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running {
		t.Fatalf("state = %s (%s)\n%s", r.State, r.FailureReason, env.Logs())
	}
	var all strings.Builder
	for _, v := range env.ConfigMap(id).Data {
		all.WriteString(v)
	}
	if !strings.Contains(all.String(), "Our own style rules.") {
		t.Errorf("the repository's style skill is not in the inputs:\n%s", all.String())
	}
	for _, not := range []string{"Look for security problems.", "Check the style."} {
		if strings.Contains(all.String(), not) {
			t.Errorf("inputs hold %q", not)
		}
	}
	if len(r.SkillSources) != 1 || r.SkillSources["style"] != "repo" {
		t.Errorf("skill sources = %v, want only style from the repo", r.SkillSources)
	}
	if r.Trigger != store.TriggerRerunFailed || len(r.SkillSubset) != 2 || r.BaseRunID == nil {
		t.Errorf("run = %q %v %v, want the re-run kept as asked", r.Trigger, r.SkillSubset, r.BaseRunID)
	}
	if !strings.Contains(env.Logs(), "gone") {
		t.Errorf("the skill that is gone was not warned about:\n%s", env.Logs())
	}
}

func TestSubsetOfSkillsThatAreGoneIsAFullRerun(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Library["style"] = []byte("---\nname: style\n---\nCheck the style.\n")
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head, namedSkill(t, "gone", "failed", "", "")))
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"gone"}, base)
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running {
		t.Fatalf("state = %s (%s)", r.State, r.FailureReason)
	}
	if r.Trigger != store.TriggerRerun || r.SkillSubset != nil || r.BaseRunID != nil {
		t.Errorf("run = %q %v %v, want a full re-run", r.Trigger, r.SkillSubset, r.BaseRunID)
	}
	if len(r.SkillSources) != 2 {
		t.Errorf("skill sources = %v, want every skill", r.SkillSources)
	}
}

// reportRerun reports result for the re-run id of merge request 1 at head and
// publishes it.
func reportRerun(t *testing.T, env *testEnv, id, head string, res []byte) {
	t.Helper()
	if got := env.Report(1, head, res); got != id {
		t.Fatalf("reported run %s, want %s", got, id)
	}
	servePublish(env, 1, head)
	publish(t, env)
}

func TestMergedResultKeepsOtherSkills(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head,
		namedSkill(t, "security", "has_blocking_issues", "blocking", "aaaaaa000001"),
		namedSkill(t, "style", "failed", "", "")))
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"style"}, base)

	reportRerun(t, env, id, head, resultFor(t, head, namedSkill(t, "style", "no_blocking_issues", "suggestion", "cccccc000001")))

	body := createdComment(t, env, 1)
	for _, want := range []string{"### security", "finding of security", "### style", "finding of style"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	wantStatus(t, env, head, gitlab.StatusFailed, "Blocking issues found")
	wantNoApproval(t, env, 1)
	wantPublished(t, env, id)

	r, err := result.Parse(env.Run(id).Result)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Merged || len(r.Skills) != 2 || r.Skills[0].Name != "security" || r.Skills[0].Verdict != "has_blocking_issues" ||
		r.Skills[1].Name != "style" || r.Skills[1].Verdict != "no_blocking_issues" {
		t.Errorf("stored result = %+v, want the kept security and the re-run style", r)
	}
	if next, _, err := env.Store.PreviousResult(env.ctx, env.MRKey(1), "none"); err != nil || !strings.Contains(string(next), "aaaaaa000001") {
		t.Errorf("the next run's previous result lacks the kept skill: %s, %v", next, err)
	}
}

// The merge is done once: publishing the run again (a dismissal, a retry)
// works on the merged result.
func TestMergedResultIsRenderedAgainAfterARedecide(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head,
		namedSkill(t, "security", "has_blocking_issues", "blocking", "aaaaaa000001"),
		namedSkill(t, "style", "failed", "", "")))
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"style"}, base)
	reportRerun(t, env, id, head, resultFor(t, head, namedSkill(t, "style", "no_blocking_issues", "", "")))
	before := string(env.Run(id).Result)

	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
		MR: env.MRKey(1), Fingerprint: "aaaaaa000001", Severity: "blocking", Reason: "false positive", Actor: "maya",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := env.Store.Republish(env.ctx, env.MRKey(1), head, "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	env.GitLab.Reset()
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	servePublish(env, 1, head)
	publish(t, env)

	if after := string(env.Run(id).Result); after != before {
		t.Errorf("the merged result changed on the second publish:\n%s\n%s", before, after)
	}
	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
	puts := env.GitLab.Requests(http.MethodPut, notePath(1, createdNote))
	if len(puts) != 1 {
		t.Fatalf("got %d comment updates, want 1", len(puts))
	}
	updated, _ := puts[0].JSON(t)["body"].(string)
	for _, want := range []string{"### security", "### style", "Dismissed (1)"} {
		if !strings.Contains(updated, want) {
			t.Errorf("updated comment lacks %q:\n%s", want, updated)
		}
	}
}

// A kept and a new finding whose fingerprints start alike get IDs that tell
// them apart, in the comment and in the stored result.
func TestMergedIDsStayUnique(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head,
		namedSkill(t, "security", "has_blocking_issues", "blocking", "abcdef111111"),
		namedSkill(t, "style", "failed", "", "")))
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"style"}, base)

	reportRerun(t, env, id, head, resultFor(t, head, namedSkill(t, "style", "no_blocking_issues", "suggestion", "abcdef222222")))

	body := createdComment(t, env, 1)
	for _, want := range []string{"`abcdef11`", "`abcdef22`"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "`abcdef`") {
		t.Errorf("comment has an ambiguous ID:\n%s", body)
	}
	r, err := result.Parse(env.Run(id).Result)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range r.Skills {
		for _, raw := range s.Findings {
			var f result.Finding
			_ = json.Unmarshal(raw, &f)
			ids = append(ids, f.ID)
		}
	}
	if len(ids) != 2 || ids[0] != "abcdef11" || ids[1] != "abcdef22" {
		t.Errorf("stored ids = %v, want abcdef11 and abcdef22", ids)
	}
}

func TestMergeBaseUnreadable(t *testing.T) {
	for name, baseResult := range map[string]string{
		"does not parse": `{"status":"completed"}`,
		"another commit": string(resultFor(t, sha('c'), namedSkill(t, "security", "no_blocking_issues", "", ""))),
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			base := env.Publish(1, head, []byte(baseResult))
			id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"style"}, base)

			reportRerun(t, env, id, head, resultFor(t, head, namedSkill(t, "style", "no_blocking_issues", "", "")))

			body := createdComment(t, env, 1)
			if !strings.Contains(body, result.NoteBaseUnreadable) {
				t.Errorf("comment lacks the note:\n%s", body)
			}
			if !strings.Contains(body, "No blocking issues.") {
				t.Errorf("comment should be the re-run's own:\n%s", body)
			}
			wantPublished(t, env, id)
			r, err := result.Parse(env.Run(id).Result)
			if err != nil {
				t.Fatal(err)
			}
			if r.Merged || r.MergeNote != result.NoteBaseUnreadable || len(r.Skills) != 1 {
				t.Errorf("stored result = %+v, want the re-run alone, with the note", r)
			}
		})
	}
}

// A run that is not a re-run (no base run) is not touched.
func TestMergeLeavesOtherRunsAlone(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, namedSkill(t, "security", "no_blocking_issues", "", "")))
	servePublish(env, 1, head)

	publish(t, env)

	if r, err := result.Parse(env.Run(id).Result); err != nil || r.Merged || r.MergeNote != "" {
		t.Errorf("a run without a base run had its result rewritten: %+v, %v", r, err)
	}
	if body := createdComment(t, env, 1); !strings.Contains(body, policy.Marker) || strings.Contains(body, result.NoteBaseUnreadable) {
		t.Errorf("comment = %q", body)
	}
}
