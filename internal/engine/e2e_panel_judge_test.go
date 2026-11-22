package engine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// marksAudited is the number of model_marked entries in the audit log.
func marksAudited(t *testing.T, env *testEnv) int {
	t.Helper()
	entries, err := env.Store.AuditLog(env.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "model_marked" {
			n++
		}
	}
	return n
}

// An API review's Job P reports that the provider refused the judge of
// role b: B's judge entry is marked and audited once, A's entry (the run's
// own) is not, no attempt is queued, and the review is published.
func TestE2EAPIPanelPJudgeRefused(t *testing.T) {
	a, refs := newAPIPanelE2E(t, "b", "c")
	env := a.env
	env.Engine.Config.LLM = config.LLM{}
	first := globalRef(t, env, "gpt-first", "key-first")
	second := globalRef(t, env, "gpt-second", "key-second")
	saveChain(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{first, second}})
	id, im := a.splitAPI(t, "route:1")
	dispatch(t, env)
	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml", "routes/new.yaml"))
	for _, role := range []string{"a", "b"} {
		if code := a.postMember(t, id, memberOfRole(t, env, id, role), clean); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", role, code, env.Logs())
		}
	}
	publish(t, env)
	dispatch(t, env)
	if js := panelJudges(t, env, id); js["a"].ID != first.ID || js["b"].ID != refs[0].ID {
		t.Fatalf("judges = %+v; want A on %s and B on %s\n%s", js, first.ID, refs[0].ID, env.Logs())
	}
	if r := env.Run(id); r.ModelEntryID != first.ID || r.ChainLength != 2 {
		t.Fatalf("run entry = %s of %d; want A's, of 2", r.ModelEntryID, r.ChainLength)
	}

	body := withRoleError(t, clean, "quota", "b")
	if code := a.postResult(t, id, string(env.Secret(id).Data["result-token"]), body); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	publish(t, env)

	wantPublished(t, env, id)
	if m, ok := markOf(t, env, refs[0]); !ok || m.Class != store.ClassQuota {
		t.Errorf("B's judge mark = %+v, %v; want quota", m, ok)
	}
	if m, ok := markOf(t, env, first); ok {
		t.Errorf("A's entry is marked (%+v); want only B's judge", m)
	}
	if n := marksAudited(t, env); n != 1 {
		t.Errorf("%d model_marked entries; want 1", n)
	}
	if as, err := env.Store.Attempts(env.ctx, id); err != nil || len(as) != 1 {
		t.Errorf("attempts = %d, %v; want none after Job P", len(as), err)
	}
}

// An API member's entry is refused and the settle that fails the member
// errors once: the next settle marks it again but the audit entry is
// written once, by the settle that failed the member.
func TestE2EAPIPanelMemberMarkAuditedOnce(t *testing.T) {
	a, refs := newAPIPanelE2E(t, "b", "c")
	env := a.env
	id, im := a.splitAPI(t, "route:1")
	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")
	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml"))
	quota := withProviderError(t, apiResultFor(t, im.Base, im.Head, result.Skill{Name: "security", Verdict: "failed",
		Failures: []result.Failure{{Files: []string{"routes/api.yaml"}, Message: "the provider refused the request"}}}), "quota")
	if code := a.postMember(t, id, ma, clean); code != http.StatusOK {
		t.Fatalf("member A's result: status %d", code)
	}
	if code := a.postMember(t, id, mb, quota); code != http.StatusOK {
		t.Fatalf("member B's result: status %d", code)
	}

	// The member's failure can't be written: the settle errors after the mark.
	execSQL(t, env, `CREATE FUNCTION fail_replace() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.result IS NOT NULL AND NEW.result IS NULL THEN RAISE EXCEPTION 'replace failed'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_replace BEFORE UPDATE ON panel_members FOR EACH ROW EXECUTE FUNCTION fail_replace()`)
	publish(t, env)
	if got := memberOfRole(t, env, id, "b"); got.State != store.MemberReported {
		t.Fatalf("member B = %s; want still reported after the failed settle\n%s", got.State, env.Logs())
	}
	if m, ok := markOf(t, env, refs[0]); !ok || m.Class != store.ClassQuota {
		t.Errorf("B's mark = %+v, %v; want quota", m, ok)
	}
	execSQL(t, env, `DROP TRIGGER fail_replace ON panel_members; DROP FUNCTION fail_replace()`)
	publish(t, env)

	if got := memberOfRole(t, env, id, "b"); got.ID != mb.ID || got.State != store.MemberFailed {
		t.Errorf("member B = %s; want failed", got.State)
	}
	if n := marksAudited(t, env); n != 1 {
		t.Errorf("%d model_marked entries; want 1", n)
	}
}

// An API client past its monthly budget at Job P: no judge runs, and the
// log names the client's budget, not a repository's quota.
func TestE2EAPIPanelPBudgetNoJudge(t *testing.T) {
	a, _ := newAPIPanelE2E(t, "b")
	env := a.env
	id, im := a.splitAPI(t, "route:1")
	dispatch(t, env)
	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml", "routes/new.yaml"))
	for _, role := range []string{"a", "b"} {
		if code := a.postMember(t, id, memberOfRole(t, env, id, role), clean); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", role, code, env.Logs())
		}
	}
	publish(t, env)
	if err := env.Store.RecordUsage(env.ctx, id, []store.SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 100, Out: 100}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.Limits.MonthlyTokenBudget = 100 }); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	job := env.Job(id)
	if job == nil || !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--panel-no-judge") {
		t.Fatalf("no Job P with --panel-no-judge\n%s", env.Logs())
	}
	logs := env.Logs()
	if !strings.Contains(logs, `no_judge="the client's monthly token budget is used up"`) || strings.Contains(logs, "repository's token quota") {
		t.Errorf("the no-judge log does not name the client's budget:\n%s", logs)
	}
}

// Member B failed, so B does not judge: Job P still names B's model as its
// entry is now, base URL included, not as the member recorded it.
func TestE2EPanelPNamesBEntry(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	env.Engine.ScanImage = testScanImage
	refs := savePanelChain(t, env, "team-b")
	url := "https://llm-b.example.com/v1"
	if found, err := env.Store.UpdateTeamModel(env.ctx, ownerTeam(t, env), refs[0].ID, nil, &url, nil, env.Now()); err != nil || !found {
		t.Fatalf("UpdateTeamModel = %v, %v", found, err)
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), errorResult(t, head, "B broke"))

	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	vars := jobEnv(env, id)
	if vars["PERGENT_PANEL_B_MODEL"] != "gpt-team-b" || vars["PERGENT_PANEL_B_BASE_URL"] != url {
		t.Errorf("P's B = %q at %q; want gpt-team-b at %q", vars["PERGENT_PANEL_B_MODEL"], vars["PERGENT_PANEL_B_BASE_URL"], url)
	}
	if js := panelJudges(t, env, id); len(js) != 1 || js["a"].Name != "team-a" {
		t.Errorf("judges = %+v; want A's alone", js)
	}
}
