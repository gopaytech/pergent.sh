package engine

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// teamOwning makes a team that owns acme/widgets and returns its ID.
func teamOwning(t *testing.T, env *testEnv, name string) int64 {
	t.Helper()
	tm, err := env.Store.CreateTeam(env.ctx, name, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	ok, err := env.Store.UpdateTeam(env.ctx, tm.ID, func(team *store.Team) error {
		team.Repos = append(team.Repos, store.TeamRepo{Instance: testInstance, Project: testProjectPath, Owner: true})
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	return tm.ID
}

// addTeamModel adds an entry with its own key to team and loads the list.
func addTeamModel(t *testing.T, env *testEnv, team int64, name, model, key string) store.EntryRef {
	t.Helper()
	m, err := env.Store.CreateTeamModel(env.ctx, store.TeamModel{TeamID: team, Name: name, Provider: "openai", Model: model,
		APIKeySealed: env.Box.Seal([]byte(key))}, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	return store.EntryRef{Source: store.SourceTeam, ID: m.ID}
}

// globalRef adds a global entry with its own key and returns its ref.
func globalRef(t *testing.T, env *testEnv, model, key string) store.EntryRef {
	t.Helper()
	return store.EntryRef{Source: store.SourceGlobal, ID: addModel(t, env, "openai", model, "", key)}
}

func saveChain(t *testing.T, env *testEnv, c store.Chain) {
	t.Helper()
	if err := env.Store.SaveChain(env.ctx, c, env.Now()); err != nil {
		t.Fatalf("SaveChain: %v", err)
	}
}

// setMark marks ref with class, until until (nil: an auth mark).
func setMark(t *testing.T, env *testEnv, ref store.EntryRef, class string, until *time.Time) {
	t.Helper()
	u := "NULL"
	if until != nil {
		u = "'" + until.UTC().Format(time.RFC3339) + "'"
	}
	execSQL(t, env, fmt.Sprintf(`INSERT INTO model_entry_marks (source, model_id, class, until, set_at)
		VALUES ('%s', '%s', '%s', %s, now())`, ref.Source, ref.ID, class, u))
}

// dispatchMR queues a run of head on merge request iid, serves it and
// dispatches; it returns the run's ID.
func dispatchMR(t *testing.T, env *testEnv, iid int64, head string) string {
	t.Helper()
	id := env.Enqueue(iid, head)
	env.GitLab.SetMR(openMR(iid, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	dispatch(t, env)
	return id
}

// wantJobKey checks run id has a Job whose Secret holds key.
func wantJobKey(t *testing.T, env *testEnv, id, key string) {
	t.Helper()
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	if got := string(env.Secret(id).Data["llm-api-key"]); got != key {
		t.Fatalf("the Job's key = %q, want %q", got, key)
	}
}

// reportedOn moves a run of head on merge request iid to reported, as if it
// had run on entry ref at position pos of a chain of length n.
func reportedOn(t *testing.T, env *testEnv, iid int64, head string, ref store.EntryRef, name string, pos, n int) store.Run {
	t.Helper()
	return reportedWith(t, env, iid, head, ref, name, pos, n, []byte(`{"schema_version":1,"status":"failed","skills":[]}`))
}

// reportedWith is reportedOn with body as the stored result.
func reportedWith(t *testing.T, env *testEnv, iid int64, head string, ref store.EntryRef, name string, pos, n int, body []byte) store.Run {
	t.Helper()
	id := env.Dispatched(iid, head)
	if err := env.Store.SetRunEntry(env.ctx, id, store.RunEntry{Source: ref.Source, ID: ref.ID, Name: name, Position: pos, Length: n}); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", body, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return env.Run(id)
}

// A repository's own chain wins over its owner team's.
func TestDispatchUsesCustomChain(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	teamA := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	teamB := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{teamA}})
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath, Entries: []store.EntryRef{teamB}})

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-team-b")
}

// A repository with no chain of its own uses its owner team's.
func TestDispatchUsesOwnerTeamChain(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	ref := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{ref}})

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-team-a")
	if vars := jobEnv(env, id); vars["OPENCODE_MODEL"] != "gpt-a" {
		t.Errorf("the Job's model = %q, want the team entry's", vars["OPENCODE_MODEL"])
	}
}

// An owner team with no chain: its repositories use the global chain, here
// the config's entry, as today.
func TestDispatchGlobalWhenOwnerHasNoChain(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, testLLMKey)
}

// The entry is on the run before the Job starts: source, ID, name, team,
// position and why position 1 was passed over.
func TestDispatchRecordsEntry(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	first := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	second := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, second}})
	until := env.Now().Add(time.Hour)
	setMark(t, env, first, store.ClassQuota, &until)

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-team-b")
	r := env.Run(id)
	if r.ModelSource != store.SourceTeam || r.ModelEntryID != second.ID || r.ModelEntryName != "team-b" ||
		r.ModelTeamID == nil || *r.ModelTeamID != team || r.ChainPosition != 2 || r.ChainLength != 2 || r.FallbackClass != store.ClassQuota {
		t.Fatalf("run entry = source %q id %q name %q team %v pos %d/%d class %q", r.ModelSource, r.ModelEntryID, r.ModelEntryName,
			r.ModelTeamID, r.ChainPosition, r.ChainLength, r.FallbackClass)
	}
}

// Every entry skipped: the review fails with each entry's reason, and no Job
// starts.
func TestDispatchExhaustedFails(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	first := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	second := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, second}})
	until := env.Now().Add(time.Hour)
	setMark(t, env, first, store.ClassQuota, &until)
	setMark(t, env, second, store.ClassAuth, nil)

	id := dispatchMR(t, env, 3, sha('a'))
	r := env.Run(id)
	want := "all models exhausted: team-a (out of quota), team-b (key rejected)"
	if r.State != store.Reported || r.FailureReason != want {
		t.Fatalf("run = %s %q, want reported %q", r.State, r.FailureReason, want)
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d jobs started", n)
	}
	for _, key := range []string{"key-team-a", "key-team-b"} {
		if strings.Contains(r.FailureReason+env.Logs(), key) {
			t.Errorf("a key reached the reason or the log")
		}
	}
}

// A chain with nothing in it waits, as an empty model list does today.
func TestDispatchEmptyChainWaits(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.LLM = config.LLM{}
	team := teamOwning(t, env, "Payments")
	// A team entry exists, so the dispatcher claims; the team has no chain
	// and the global chain has no entry.
	addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")

	id := dispatchMR(t, env, 3, sha('a'))
	r := env.Run(id)
	if r.State != store.Queued || r.Attempts != 0 || r.FailureReason != "" {
		t.Errorf("run = %s attempts %d %q; want queued, no attempt", r.State, r.Attempts, r.FailureReason)
	}
	// Claimed and put back: a dispatcher that saw no model at all would have
	// left it alone.
	if !r.NotBefore.After(env.Now()) {
		t.Errorf("not_before = %v; the run was not claimed and postponed", r.NotBefore)
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d jobs started", n)
	}
}

// The instance quota counts global keys only: a used-up instance skips the
// global entry and runs the team's.
func TestDispatchInstanceQuotaRunsTeamEntry(t *testing.T) {
	t.Parallel()
	env := quotaPushEnv(t, 0, 0)
	instQuota := int64(500)
	env.Engine.Config.GitLab[0].MonthlyTokenQuota = &instQuota
	seedQuotaUsage(t, env, 500)
	team := teamOwning(t, env, "Payments")
	global := globalRef(t, env, "gpt-global", "key-global")
	if found, err := env.Store.SetLLMModelMeta(env.ctx, global.ID, nil, boolPtr(true), env.Now()); err != nil || !found {
		t.Fatalf("SetLLMModelMeta = %v, %v", found, err)
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	own := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{global, own}})

	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantJobKey(t, env, id, "key-team-a")
	if r := env.Run(id); r.FallbackClass != "instance_quota" {
		t.Errorf("fallback class = %q", r.FallbackClass)
	}
}

// The repository quota counts every token: it holds the run whatever the
// chain holds.
func TestDispatchRepoQuotaHoldsAll(t *testing.T) {
	t.Parallel()
	env := quotaPushEnv(t, 100, 100)
	team := teamOwning(t, env, "Payments")
	own := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{own}})

	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantHeld(t, env, id)
}

// The Job gets the key of the entry it runs and no other: not in its Secret,
// not anywhere in its spec.
func TestDispatchOnlyTheEntryKeyInSecret(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	first := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	second := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	global := globalRef(t, env, "gpt-global", "key-global")
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		Entries: []store.EntryRef{first, second, global}})
	setMark(t, env, first, store.ClassAuth, nil)

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-team-b")
	spec := fmt.Sprintf("%+v %+v", env.Job(id).Spec, env.ConfigMap(id).Data)
	for k, v := range env.Secret(id).Data {
		if k != "llm-api-key" {
			spec += string(v)
		}
	}
	for _, other := range []string{"key-team-a", "key-global", testLLMKey, "key-team-b"} {
		if strings.Contains(spec, other) {
			t.Errorf("the Job holds %q outside its key", other)
		}
	}
}

// An attempt is the same review as its first run: the author was checked
// then, so the trust check does not run again.
func TestAttemptSkipsTrustCheck(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	first := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	second := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, second}})
	head := sha('a')
	from := reportedOn(t, env, 3, head, first, "team-a", 1, 2)
	until := env.Now().Add(time.Hour)
	next, created, err := env.Store.EnqueueAttempt(env.ctx, from, store.Mark{Ref: first, Class: store.ClassQuota, Until: &until, SetAt: env.Now()})
	if err != nil || !created {
		t.Fatalf("EnqueueAttempt = %v, %v", created, err)
	}
	// Now nobody is trusted, and the author's access can't be read at all.
	env.Engine.Config.GitLab[0].TrustedAuthorsOnly = config.TrustOn
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)
	wantJobKey(t, env, next, "key-team-b")
}

// An attempt's running status says it is a backup: its number in the review
// and the chain's length.
func TestAttemptRunningDescription(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	b := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	c := addTeamModel(t, env, team, "team-c", "gpt-c", "key-team-c")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{a, b, c}})
	head := sha('a')
	from := reportedOn(t, env, 3, head, a, "team-a", 1, 3)
	until := env.Now().Add(time.Hour)
	next, created, err := env.Store.EnqueueAttempt(env.ctx, from, store.Mark{Ref: a, Class: store.ClassQuota, Until: &until, SetAt: env.Now()})
	if err != nil || !created {
		t.Fatalf("EnqueueAttempt = %v, %v", created, err)
	}
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)
	wantJobKey(t, env, next, "key-team-b")
	got := statusesOf(t, env, head)
	if len(got) == 0 || got[len(got)-1] != "running: retrying on backup model (2 of 3)" {
		t.Fatalf("statuses = %v", got)
	}
	if r := env.Run(next); r.ChainPosition != 2 || r.FallbackClass != store.ClassQuota {
		t.Errorf("attempt entry = pos %d class %q", r.ChainPosition, r.FallbackClass)
	}
}

// A client that names a model runs on it, as today, even when it is marked.
func TestAPIReviewPinnedModelUnchanged(t *testing.T) {
	t.Parallel()
	a := newAPIE2E(t)
	env := a.env
	pinned := globalRef(t, env, "gpt-pinned", "key-pinned")
	setMark(t, env, pinned, store.ClassAuth, nil)
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = pinned.ID }); err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)
	wantJobKey(t, env, id, "key-pinned")
}

// A client with no model of its own uses the global chain, and a marked entry
// is skipped when its review starts.
func TestAPIReviewSkipsMarkedGlobal(t *testing.T) {
	t.Parallel()
	a := newAPIE2E(t)
	env := a.env
	env.Engine.Config.LLM = config.LLM{}
	first := globalRef(t, env, "gpt-first", "key-first")
	second := globalRef(t, env, "gpt-second", "key-second")
	saveChain(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{first, second}})
	until := env.Now().Add(time.Hour)
	setMark(t, env, first, store.ClassQuota, &until)
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)
	wantJobKey(t, env, id, "key-second")
	if r := env.Run(id); r.ModelEntryID != second.ID || r.ChainPosition != 2 {
		t.Errorf("run entry = %q at %d", r.ModelEntryID, r.ChainPosition)
	}
}

// A repository chain an admin saved runs a global entry no team may use, even
// once a team owns the repository (owner ruling 2026-11-20): the seeded chain
// of the upgrade is an admin's. The same chain saved by the team's maintainer
// skips it.
func TestDispatchAdminSavedRepoChainKeepsGlobal(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	global := globalRef(t, env, "gpt-global", "key-global")
	own := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		SavedByAdmin: true, Entries: []store.EntryRef{global, own}})
	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-global")

	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		Entries: []store.EntryRef{global, own}})
	id = dispatchMR(t, env, 4, sha('b'))
	wantJobKey(t, env, id, "key-team-a")
	if r := env.Run(id); r.FallbackClass != "not_allowed" {
		t.Errorf("fallback class = %q, want not_allowed", r.FallbackClass)
	}
}

// A run somebody asked for under a used-up instance quota (it started
// because a team entry was left, owner ruling 2026-11-20) runs on the team's
// entry, not on the global one in front of it. With no team entry it goes
// ahead on the global one, as a checked run did before chains.
func TestCheckedRunUnderInstanceQuotaPrefersTeamEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		teamEntry bool
		wantKey   string
	}{
		{"team entry left", true, "key-team-a"},
		{"global only", false, "key-global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := quotaPushEnv(t, 0, 0)
			instQuota := int64(500)
			env.Engine.Config.GitLab[0].MonthlyTokenQuota = &instQuota
			seedQuotaUsage(t, env, 500)
			team := teamOwning(t, env, "Payments")
			global := globalRef(t, env, "gpt-global", "key-global")
			if found, err := env.Store.SetLLMModelMeta(env.ctx, global.ID, nil, boolPtr(true), env.Now()); err != nil || !found {
				t.Fatalf("SetLLMModelMeta = %v, %v", found, err)
			}
			if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
				t.Fatal(err)
			}
			refs := []store.EntryRef{global}
			if tc.teamEntry {
				refs = append(refs, addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a"))
			}
			saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: refs})

			head := sha('a')
			servePush(env, head)
			id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, true)
			if err != nil {
				t.Fatal(err)
			}
			dispatch(t, env)
			wantJobKey(t, env, id, tc.wantKey)
		})
	}
}
