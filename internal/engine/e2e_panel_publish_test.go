package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// B's entry has text no comment may hold: its provider, model and base URL.
const (
	panelBProvider = "zeta-prov"
	panelBModel    = "omega-mdl"
	panelBURL      = "https://llm-b.example.test/v1"
)

// newHiddenPanelE2E is newPanelE2E with chain A [team-a] (openai/gpt-team-a)
// and chain B [team-b] on B's distinctive provider, model and base URL.
func newHiddenPanelE2E(t *testing.T) *e2e {
	t.Helper()
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	env.Engine.ScanImage = testScanImage
	team := ownerTeam(t, env)
	m, err := env.Store.CreateTeamModel(env.ctx, store.TeamModel{TeamID: team, Name: "team-b", Provider: panelBProvider,
		Model: panelBModel, BaseURL: panelBURL, APIKeySealed: env.Box.Seal([]byte("key-team-b"))}, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{{Source: store.SourceTeam, ID: m.ID}}})
	return e
}

// editResult is body with edit applied to its JSON object.
func editResult(t *testing.T, body []byte, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// firstSkill is the first skill of a result's JSON object.
func firstSkill(m map[string]any) map[string]any { return m["skills"].([]any)[0].(map[string]any) }

// panelMerge is a clean P result of head whose skill tokens are a and b on
// the 2 models, with the panel fields, and, when text is not "", a
// suggestion whose texts name text.
func panelMerge(t *testing.T, head string, a, b int, text string) []byte {
	t.Helper()
	return editResult(t, withTokens(t, clean(t, head), a+b, 0), func(m map[string]any) {
		m["panel"] = map[string]any{"a": map[string]any{"provider": "openai", "model": "gpt-team-a"},
			"b": map[string]any{"provider": panelBProvider, "model": panelBModel}, "mode": "both", "judged": true}
		s := firstSkill(m)
		s["panel"] = map[string]any{"mode": "both"}
		s["tokens_by_model"] = []any{
			map[string]any{"role": "a", "provider": "openai", "model": "gpt-team-a", "tokens": map[string]any{"input": a, "output": 0}},
			map[string]any{"role": "b", "provider": panelBProvider, "model": panelBModel, "tokens": map[string]any{"input": b, "output": 0}},
		}
		if text != "" {
			s["findings"] = []any{map[string]any{"id": "abc123", "fingerprint": "abc123def456", "file": "a.go", "line": 1,
				"severity": "suggestion", "title": "Slow call to " + text, "explanation": "The call to " + text + " is slow.",
				"suggested_fix": "Ask " + text + " less.",
				"panel":         map[string]any{"found_by": "a", "disagreed_by": "b", "reason": text + " thinks it is fine"}}}
		}
	})
}

// runP dispatches Job P of run id, posts body as its result and publishes.
func (e *e2e) runP(t *testing.T, id string, body []byte) {
	t.Helper()
	dispatch(t, e.env)
	if e.env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", e.env.Logs())
	}
	if code := e.postResult(t, id, string(e.env.Secret(id).Data["result-token"]), body); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, e.env.Logs())
	}
	publish(t, e.env)
	wantPublished(t, e.env, id)
}

// wantNoModelText checks text names no provider, model or base URL of the
// panel's 3 Jobs.
func wantNoModelText(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"openai", "gpt-team-a", panelBProvider, panelBModel, "llm-b.example.test"} {
		if strings.Contains(text, bad) {
			t.Errorf("the comment names %q:\n%s", bad, text)
		}
	}
}

// Only Job P publishes, once: one comment that names the 2 models by their
// entry names and no provider or model, the success status of a clean
// merge, and the approval of P's run. The members post nothing.
func TestE2EPanelPublishesOnce(t *testing.T) {
	e := newHiddenPanelE2E(t)
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	env.GitLab.Reset()
	e.runP(t, id, panelMerge(t, head, 10, 20, ""))

	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 1 {
		t.Fatalf("%d comments; want 1", n)
	}
	comment := createdComment(t, env, 1)
	if !strings.Contains(comment, "Panel: team-a (A) + team-b (B)") {
		t.Errorf("comment lacks the panel's entry names:\n%s", comment)
	}
	wantNoModelText(t, comment)
	if strings.Contains(comment, "panel off") {
		t.Errorf("a panel's comment says the panel is off:\n%s", comment)
	}
	if got := onlyStatus(t, env, head)["state"]; got != string(gitlab.StatusSuccess) {
		t.Errorf("status = %v; want success", got)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 || recordedApproval(t, env, 1) != head {
		t.Errorf("%d approvals, recorded %q; want P's one", n, recordedApproval(t, env, 1))
	}
	// A second pass posts nothing more.
	env.GitLab.Reset()
	publish(t, env)
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s after the publish; want nothing", r.Method, r.Path)
		}
	}
}

// B's provider, model and base URL, and A's model, in a finding's text and
// in the reason of the model that disagreed, are hidden in the comment.
func TestPanelCommentHidesBothModels(t *testing.T) {
	e := newHiddenPanelE2E(t)
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	env.GitLab.Reset()
	text := panelBProvider + "/" + panelBModel + " at " + panelBURL + " and gpt-team-a"
	e.runP(t, id, panelMerge(t, head, 10, 20, text))

	comment := createdComment(t, env, 1)
	wantNoModelText(t, comment)
	for _, want := range []string{"The call to the model at the provider and the model is slow.", "team-b disagreed"} {
		if !strings.Contains(comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, comment)
		}
	}
	// The stored result keeps the full text for the dashboard.
	if r := env.Run(id); !strings.Contains(string(r.Result), panelBModel) {
		t.Error("the stored result lost the model's name")
	}
}

// pollUsage waits for run id's recorded tokens (in) to reach want: the
// callbacks write usage after they answer.
func pollUsage(t *testing.T, env *testEnv, id string, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		in, _, _, err := env.Store.RunUsage(env.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if in == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run usage = %d tokens; want %d", in, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The quota counts all 3 Jobs: run_usage of the review holds A + B + P,
// run_usage_models one row per member and one per judge, and quota.Check
// sees the total.
func TestE2EPanelQuotaCountsAll(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, withTokens(t, clean(t, head), 100, 0), withTokens(t, clean(t, head), 200, 0))
	pollUsage(t, env, id, 300)
	e.runP(t, id, panelMerge(t, head, 30, 40, ""))
	pollUsage(t, env, id, 370)

	cfg, err := pgx.ParseConfig(env.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	var members, merges int
	if err := conn.QueryRow(env.ctx, `SELECT count(*) FILTER (WHERE part = 'member'), count(*) FILTER (WHERE part = 'merge')
		FROM run_usage_models WHERE run_id = $1::uuid`, id).Scan(&members, &merges); err != nil {
		t.Fatal(err)
	}
	if members != 2 || merges != 2 {
		t.Errorf("run_usage_models = %d member and %d merge rows; want 2 and 2", members, merges)
	}
	for limit, want := range map[int64]bool{370: true, 371: false} {
		used, _, err := quota.Check(env.ctx, env.Store, env.Now(), testInstance, testProjectID, settings.Effective{MonthlyTokenQuota: &limit})
		if err != nil || used != want {
			t.Errorf("quota of %d used up = %v, %v; want %v", limit, used, err, want)
		}
	}
}

// An "A only" review says why in its footer. An off row that an earlier,
// failed launch left (chain B removed before the retry) is not this
// review's: the comment is today's.
func TestE2EPanelOffNote(t *testing.T) {
	for _, stale := range []bool{false, true} {
		e := newE2E(t)
		env := e.env
		globalChainB(t, env) // no floor: the image is refused
		head := sha('a')
		servePublish(env, 1, head)
		id := env.Enqueue(1, head)
		if stale {
			fails := 0
			env.Kube.PrependReactor("create", "jobs", func(act k8stesting.Action) (bool, runtime.Object, error) {
				if act.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Name == kube.JobName(id) && fails == 0 {
					fails++
					return true, nil, errorString("the API server is down")
				}
				return false, nil, nil
			})
			dispatch(t, env)
			if env.Job(id) != nil || fails != 1 {
				t.Fatalf("the first launch did not fail\n%s", env.Logs())
			}
			saveChainB(t, env, store.Chain{Scope: store.ScopeGlobal})
			env.SetNow(env.Now().Add(time.Hour))
		}
		dispatch(t, env)
		if env.Job(id) == nil {
			t.Fatalf("stale %v: no Job\n%s", stale, env.Logs())
		}
		if ms := members(t, env, id); len(ms) != 1 || ms[0].State != store.MemberOff {
			t.Fatalf("stale %v: members = %+v; want the off row", stale, ms)
		}
		if code := e.postResult(t, id, string(env.Secret(id).Data["result-token"]), clean(t, head)); code != http.StatusOK {
			t.Fatalf("result: status %d", code)
		}
		publish(t, env)
		wantPublished(t, env, id)
		comment := createdComment(t, env, 1)
		note := policy.PanelOffLine("the review image does not support it")
		if got := strings.Contains(comment, note); got == stale {
			t.Errorf("stale %v: comment holds %q = %v:\n%s", stale, note, got, comment)
		}
		if strings.Contains(comment, "Panel:") {
			t.Errorf("stale %v: an A only comment names a panel:\n%s", stale, comment)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// A panel's inline threads hide its models as its comment does; without a
// panel a thread's text is as the model wrote it.
func TestInlinePanelHidesModels(t *testing.T) {
	for _, panel := range []bool{false, true} {
		h := newHarness(t)
		b, _ := json.Marshal(map[string]any{"id": fpBlocking[:6], "fingerprint": fpBlocking, "file": "db/query.go", "line": 11,
			"severity": "blocking", "title": "Calls " + panelBModel, "explanation": "Ask " + panelBProvider + " less."})
		o := securityReview(t, h.head, "has_blocking_issues", b)
		o.Hide = func(s string) string {
			return policy.HideModels(s, []policy.ModelRef{{Provider: panelBProvider, Model: panelBModel}}, nil)
		}
		if panel {
			o.Panel = &render.PanelNames{A: "a", B: "b"}
		}
		h.sync(o)
		if len(h.fi.posted) == 0 || len(h.fi.posted[0]) == 0 {
			t.Fatalf("panel %v: no thread posted", panel)
		}
		body := h.fi.posted[0][0].Body
		if got := strings.Contains(body, panelBModel) || strings.Contains(body, panelBProvider); got == panel {
			t.Errorf("panel %v: thread names a model = %v:\n%s", panel, got, body)
		}
	}
}
