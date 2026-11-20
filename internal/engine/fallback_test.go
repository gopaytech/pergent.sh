package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// withProviderError is body with provider_error of class at its top, as
// pergent writes it.
func withProviderError(t *testing.T, body []byte, class string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m["provider_error"] = map[string]any{"class": class, "status": 429, "provider": "openai", "model": "gpt"}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// failedOn is a completed result of head whose one skill failed, with
// provider_error of class ("" for none, as an older pergent writes it).
func failedOn(t *testing.T, head, class string) []byte {
	t.Helper()
	body := resultFor(t, head, result.Skill{Name: "security", Verdict: "failed",
		Failures: []result.Failure{{Files: []string{"a.go"}, Message: "the provider refused the request"}}})
	if class == "" {
		return body
	}
	return withProviderError(t, body, class)
}

// twoTeamEntries is a team that owns acme/widgets with a chain of 2 entries.
func twoTeamEntries(t *testing.T, env *testEnv) (team int64, first, second store.EntryRef) {
	t.Helper()
	team = teamOwning(t, env, "Payments")
	first = addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	second = addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, second}})
	return team, first, second
}

// markOf returns ref's mark; ok is false when it has none.
func markOf(t *testing.T, env *testEnv, ref store.EntryRef) (store.Mark, bool) {
	t.Helper()
	marks, err := env.Store.Marks(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range marks {
		if m.Ref == ref {
			return m, true
		}
	}
	return store.Mark{}, false
}

// wantNoFallback checks run id was published as today: no attempt, no mark.
func wantNoFallback(t *testing.T, env *testEnv, id string, refs ...store.EntryRef) {
	t.Helper()
	wantPublished(t, env, id)
	if as, err := env.Store.Attempts(env.ctx, id); err != nil || len(as) != 1 {
		t.Errorf("attempts = %d, %v; want the run alone", len(as), err)
	}
	for _, ref := range refs {
		if _, ok := markOf(t, env, ref); ok {
			t.Errorf("entry %s was marked", ref.ID)
		}
	}
}

// A quota error on entry 1 of 2: the entry is marked for the cooldown, the
// run closes as fell_back with nothing posted but the shared status, and the
// next attempt is queued.
func TestFallBackOnQuota(t *testing.T) {
	env := newTestEnv(t)
	_, first, _ := twoTeamEntries(t, env)
	head := sha('a')
	servePublish(env, 3, head)
	from := reportedWith(t, env, 3, head, first, "team-a", 1, 2, failedOn(t, head, "quota"))

	publish(t, env)

	r := env.Run(from.ID)
	if r.State != store.Superseded || r.Outcome != store.OutcomeFellBack || r.OutcomeReason != store.ClassQuota {
		t.Fatalf("run = %s %q %q; want superseded fell_back quota", r.State, r.Outcome, r.OutcomeReason)
	}
	as, err := env.Store.Attempts(env.ctx, from.ID)
	if err != nil || len(as) != 2 || as[1].State != store.Queued || as[1].AttemptOf == nil || *as[1].AttemptOf != from.ID {
		t.Fatalf("attempts = %+v, %v; want a queued second attempt", as, err)
	}
	m, ok := markOf(t, env, first)
	if !ok || m.Class != store.ClassQuota || m.Until == nil || !m.Until.Equal(env.Now().Add(time.Hour)) {
		t.Fatalf("mark = %+v, %v; want quota until now+1h", m, ok)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(3))); n != 0 {
		t.Errorf("%d comments posted by the dropped attempt", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(3))); n != 0 {
		t.Errorf("%d approvals by the dropped attempt", n)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: retrying on backup model (2 of 2)" {
		t.Errorf("statuses = %v", got)
	}
	entries, err := env.Store.AuditLog(env.ctx, 0, 10)
	if err != nil || len(entries) == 0 || entries[0].Action != "model_marked" || entries[0].Actor != "pergent" ||
		entries[0].Detail != "model=team-a class=quota" {
		t.Errorf("audit = %+v, %v", entries, err)
	}
}

// An auth error marks the entry with no end: it lasts until its key is
// edited, which clears it.
func TestFallBackOnAuthMarksUntilEdit(t *testing.T) {
	env := newTestEnv(t)
	team, first, _ := twoTeamEntries(t, env)
	head := sha('a')
	servePublish(env, 3, head)
	from := reportedWith(t, env, 3, head, first, "team-a", 1, 2, failedOn(t, head, "auth"))

	publish(t, env)

	if r := env.Run(from.ID); r.Outcome != store.OutcomeFellBack || r.OutcomeReason != store.ClassAuth {
		t.Fatalf("run = %q %q", r.Outcome, r.OutcomeReason)
	}
	m, ok := markOf(t, env, first)
	if !ok || m.Class != store.ClassAuth || m.Until != nil {
		t.Fatalf("mark = %+v, %v; want auth with no end", m, ok)
	}
	if found, err := env.Store.UpdateTeamModel(env.ctx, team, first.ID, nil, nil, env.Box.Seal([]byte("key-team-a-2")), env.Now()); err != nil || !found {
		t.Fatalf("UpdateTeamModel = %v, %v", found, err)
	}
	if _, ok := markOf(t, env, first); ok {
		t.Error("the auth mark stayed after the key was edited")
	}
}

// A plain rate limit is no reason to fall back: the review is posted as
// today.
func TestNoFallBackOnRateLimit(t *testing.T) {
	env := newTestEnv(t)
	_, first, _ := twoTeamEntries(t, env)
	head := sha('a')
	servePublish(env, 3, head)
	from := reportedWith(t, env, 3, head, first, "team-a", 1, 2, failedOn(t, head, "rate_limit"))

	publish(t, env)
	wantNoFallback(t, env, from.ID, first)
	createdComment(t, env, 3)
}

// A 1-entry chain never falls back.
func TestNoFallBackOnOneEntryChain(t *testing.T) {
	env := newTestEnv(t)
	team := teamOwning(t, env, "Payments")
	only := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	head := sha('a')
	servePublish(env, 3, head)
	from := reportedWith(t, env, 3, head, only, "team-a", 1, 1, failedOn(t, head, "quota"))

	publish(t, env)
	wantNoFallback(t, env, from.ID, only)
	if st := onlyStatus(t, env, head); st["state"] == "success" {
		t.Errorf("a review that hit the quota passed: %v", st)
	}
}

// A pergent image that writes no provider_error gives today's behaviour.
func TestNoFallBackOldImage(t *testing.T) {
	env := newTestEnv(t)
	_, first, _ := twoTeamEntries(t, env)
	head := sha('a')
	servePublish(env, 3, head)
	from := reportedWith(t, env, 3, head, first, "team-a", 1, 2, failedOn(t, head, ""))

	publish(t, env)
	wantNoFallback(t, env, from.ID, first)
}

// An API review fails as today: there is no second attempt, since its inputs
// are gone. The entry is still marked, so the next review skips it.
func TestNoFallBackForAPIReview(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.Engine.Config.LLM = config.LLM{}
	first := globalRef(t, env, "gpt-first", "key-first")
	second := globalRef(t, env, "gpt-second", "key-second")
	saveChain(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{first, second}})
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	wantJobKey(t, env, id, "key-first")
	run := env.Run(id)
	body := withProviderError(t, apiResultFor(t, run.BaseSHA, run.HeadSHA, reviewed("failed")), "quota")
	if code := a.postResult(t, id, string(env.Secret(id).Data["result-token"]), body); code != http.StatusOK {
		t.Fatalf("result: status %d", code)
	}

	publish(t, env)

	wantPublished(t, env, id)
	if as, err := env.Store.Attempts(env.ctx, id); err != nil || len(as) != 1 {
		t.Errorf("attempts = %d, %v; want none after the review", len(as), err)
	}
	if m, ok := markOf(t, env, first); !ok || m.Class != store.ClassQuota {
		t.Errorf("mark = %+v, %v; want the entry marked", m, ok)
	}
	if strings.Contains(env.Logs(), "key-first") {
		t.Error("the key reached the log")
	}
}
