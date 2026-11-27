package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/security"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A scan Job's two calls back: it downloads the source of its commit, then
// posts its envelope. Both need the scan's own token while it runs; the
// result is taken once, with every text that holds a secret withheld.

// secHit is one SARIF result with fingerprint fp and message text.
func secHit(fp, text string) map[string]any {
	return map[string]any{
		"ruleId": "injection", "message": map[string]any{"text": text},
		"fingerprints": map[string]any{"security-audit/v1": fp},
		"properties":   map[string]any{"severity": "high"},
		"locations": []any{map[string]any{"physicalLocation": map[string]any{
			"artifactLocation": map[string]any{"uri": "db/q.go"}, "region": map[string]any{"startLine": 12}}}},
	}
}

// secSARIF is a SARIF 2.1.0 log of the skill security-audit with results.
func secSARIF(t *testing.T, results ...map[string]any) json.RawMessage {
	t.Helper()
	if results == nil {
		results = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"version": "2.1.0", "runs": []any{map[string]any{
		"tool":    map[string]any{"driver": map[string]any{"name": "security-audit", "version": "abc123"}},
		"results": results,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// secBody is a complete envelope of commit with one finding (fp1) and one
// model's usage; edit, when not nil, changes it first.
func secBody(t *testing.T, commit string, edit func(env *security.Envelope)) []byte {
	t.Helper()
	tokens := security.Tokens{Input: 1000, Output: 200, Reasoning: 10, CacheRead: 500, CacheWrite: 50}
	env := security.Envelope{
		Kind: "security", Status: "complete", Commit: commit,
		Skill: &security.Skill{Name: "security-audit", Commit: "abc123"},
		Usage: security.Usage{Source: "stats", Sessions: 1, Steps: 40, Tokens: tokens,
			Models: []security.ModelUsage{{Provider: "acme", Model: "big", Steps: 40, Tokens: tokens}}},
		Coverage: &security.Coverage{Done: 3, Planned: 3},
		State:    map[string]string{"findings.json": "[]"},
		SARIF:    secSARIF(t, secHit("fp1", "SQL built from input")),
	}
	if edit != nil {
		edit(&env)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The pure part: what is kept of a body, with no database.
func TestWithholdSecurity(t *testing.T) {
	t.Parallel()
	const key = "sk-held-llm-key-0123456789"
	secrets := []string{key}

	t.Run("nothing held", func(t *testing.T) {
		raw := json.RawMessage(`{"version":"2.1.0","runs":[{"results":[{"message":{"text":"a < b"}}]}]}`)
		env := security.Envelope{Reason: "ok", State: map[string]string{"a": "b"}, SARIF: raw}
		if n := withholdSecurity(&env, secrets); n != 0 {
			t.Errorf("withheld %d; want 0", n)
		}
		if !bytes.Equal(env.SARIF, raw) || env.Reason != "ok" || env.State["a"] != "b" {
			t.Errorf("envelope changed: %+v", env)
		}
	})
	t.Run("held", func(t *testing.T) {
		raw := json.RawMessage(`{"version":"2.1.0","runs":[{"results":[{"message":{"text":"leaked ` + key + `"},
			"properties":{"` + key + `":"v","confidence":0.9,"nested":["x","` + key + `"]}}]}]}`)
		env := security.Envelope{
			Reason: "boom " + key, Skill: &security.Skill{Name: key, Commit: "c"},
			State: map[string]string{"ok.json": "[]", "bad.json": "has " + key, key: "x"},
			SARIF: raw,
		}
		if n := withholdSecurity(&env, secrets); n != 7 {
			t.Errorf("withheld %d; want 7 (reason, skill, 2 state files, message, key, list item)", n)
		}
		if env.Reason != security.Withheld || env.Skill.Name != security.Withheld {
			t.Errorf("reason %q, skill %q; want withheld", env.Reason, env.Skill.Name)
		}
		if len(env.State) != 1 || env.State["ok.json"] != "[]" {
			t.Errorf("state = %v; want only ok.json", env.State)
		}
		if strings.Contains(string(env.SARIF), key) {
			t.Fatalf("the SARIF still holds the key: %s", env.SARIF)
		}
		var doc map[string]any
		if err := json.Unmarshal(env.SARIF, &doc); err != nil {
			t.Fatalf("re-encoded SARIF: %v", err)
		}
		res := doc["runs"].([]any)[0].(map[string]any)["results"].([]any)[0].(map[string]any)
		props := res["properties"].(map[string]any)
		if res["message"].(map[string]any)["text"] != security.Withheld || props[security.Withheld] != "v" ||
			props["confidence"] != 0.9 || props["nested"].([]any)[1] != security.Withheld {
			t.Errorf("SARIF result = %v", res)
		}
	})
}

func TestSecurityResultOf(t *testing.T) {
	t.Parallel()
	const key = "sk-held-llm-key-0123456789"
	asked := time.Unix(1_700_000_000, 0)
	running := store.SecurityScan{ID: "s1", Commit: sha('c'), Provider: "acme", Model: "big"}
	cancelled := running
	cancelled.CancelRequestedAt = &asked
	many := make([]map[string]any, security.MaxFindings+1)
	for i := range many {
		many[i] = secHit(fmt.Sprintf("fp%d", i), "finding")
	}
	big := strings.Repeat("x", 600<<10)

	cases := []struct {
		name                 string
		sc                   store.SecurityScan
		body                 []byte
		status, reason       string
		findings, usage      int
		unusable, stateFiles int
	}{
		{name: "complete", sc: running, body: secBody(t, sha('c'), nil), status: "complete", findings: 1, usage: 1,
			stateFiles: 1},
		{name: "kind review", sc: running, body: secBody(t, sha('c'), func(e *security.Envelope) { e.Kind = "review" }),
			status: "failed", reason: `pergent sent a result pergent.sh couldn't use: kind must be "security"`, unusable: 1},
		{name: "not JSON", sc: running, body: []byte("{"), status: "failed", unusable: 1},
		{name: "SARIF 2.0.0", sc: running, body: secBody(t, sha('c'), func(e *security.Envelope) {
			e.SARIF = json.RawMessage(`{"version":"2.0.0","runs":[]}`)
		}), status: "failed", reason: `pergent sent a result pergent.sh couldn't use: the SARIF version must be 2.1.0, got "2.0.0"`,
			usage: 1, unusable: 1},
		{name: "capped", sc: running, body: secBody(t, sha('c'), func(e *security.Envelope) { e.SARIF = secSARIF(t, many...) }),
			status: "partial", reason: "more than 5000 findings: the first 5000 are kept", findings: security.MaxFindings,
			usage: 1, stateFiles: 1},
		{name: "cancelled", sc: cancelled, body: secBody(t, sha('c'), func(e *security.Envelope) {
			e.Status, e.Reason = "partial", "terminated"
		}), status: "partial", reason: "cancelled", findings: 1, usage: 1, stateFiles: 1},
		{name: "cancelled and failed", sc: cancelled, body: secBody(t, sha('c'), func(e *security.Envelope) {
			e.Status, e.Reason = "failed", "the model failed"
		}), status: "failed", reason: "cancelled", findings: 1, usage: 1, stateFiles: 1},
		{name: "reason one line", sc: running, body: secBody(t, sha('c'), func(e *security.Envelope) {
			e.Status, e.Reason = "partial", "line one\nline two "+strings.Repeat("y", 600)
		}), status: "partial", reason: "line one line two " + strings.Repeat("y", 500-len("line one line two ")),
			findings: 1, usage: 1, stateFiles: 1},
		{name: "reason with a secret", sc: running, body: secBody(t, sha('c'), func(e *security.Envelope) {
			e.Status, e.Reason = "failed", "boom "+key
		}), status: "failed", reason: security.Withheld, findings: 1, usage: 1, stateFiles: 1},
		{name: "big state file", sc: running, body: secBody(t, sha('c'), func(e *security.Envelope) {
			e.State = map[string]string{"big.json": big, "small.json": "[]"}
		}), status: "complete", findings: 1, usage: 1, stateFiles: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, usage, in := securityResultOf(c.sc, c.body, []string{key})
			if res.Status != c.status || (c.reason != "" && res.Reason != c.reason) {
				t.Errorf("result = %s %q; want %s %q", res.Status, res.Reason, c.status, c.reason)
			}
			if len(res.Findings) != c.findings || len(usage) != c.usage || len(res.State) != c.stateFiles {
				t.Errorf("findings %d, usage rows %d, state files %d; want %d, %d, %d",
					len(res.Findings), len(usage), len(res.State), c.findings, c.usage, c.stateFiles)
			}
			if (in.unusable != "") != (c.unusable == 1) {
				t.Errorf("unusable = %q; want unusable %v", in.unusable, c.unusable == 1)
			}
			if c.status == "failed" && c.unusable == 1 && (res.SARIF != nil || len(res.Findings) != 0 || res.State != nil) {
				t.Errorf("an unusable result kept SARIF, findings or state")
			}
			if res.Commit == "" {
				t.Errorf("no commit: the store would take it for another commit's")
			}
			if err := store.CheckSecurityResult(res); err != nil {
				t.Errorf("the store refuses it: %v", err)
			}
		})
	}
	if _, _, in := securityResultOf(running, secBody(t, sha('c'), func(e *security.Envelope) {
		e.State = map[string]string{"big.json": big}
	}), nil); in.stateDropped != 1 {
		t.Errorf("stateDropped = %d; want 1", in.stateDropped)
	}
}

// secRunning starts a scan of acme/widgets and returns it with its Job's
// result token.
func secRunning(t *testing.T, env *testEnv) (store.SecurityScan, string) {
	t.Helper()
	sc := secQueue(t, env, testProjectPath, testProjectID, 0)
	secLaunch(env)
	got := secScan(t, env, sc.ID)
	if got.State != store.SecurityRunning || secJob(t, env, sc.ID) == nil {
		t.Fatalf("scan = %s %q with no Job\n%s", got.State, got.Reason, env.Logs())
	}
	return got, string(secSecret(t, env, sc.ID).Data["result-token"])
}

// secUsageRows reports whether scan sc's hidden run has n run_usage rows.
func secUsageRows(t *testing.T, env *testEnv, sc store.SecurityScan, n int) bool {
	t.Helper()
	return querySQL(t, env, fmt.Sprintf(`SELECT count(*) = %d FROM run_usage WHERE run_id = '%s'::uuid`, n, sc.RunID))
}

func secFindings(t *testing.T, env *testEnv) []store.SecurityFinding {
	t.Helper()
	rows, _, err := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath, store.SecurityFindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A complete result is taken once: its findings and usage are stored and the
// token is spent. A review run's token is refused, and the scan's token does
// not post a review's result on its hidden run.
func TestCompleteSecurityScan(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	sc, token := secRunning(t, env)

	// A running review's token.
	env.Dispatched(1, sha('a'))
	if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, "seed-token", secBody(t, sha('c'), nil)); err != nil ||
		out != security.Unauthorized {
		t.Errorf("a review's token = %v, %v; want Unauthorized", out, err)
	}
	// The scan's token on its hidden run, through the review route.
	mux := http.NewServeMux()
	mux.Handle("POST /runs/{id}/result", &callback.Handler{Results: env.Store, Now: env.Now})
	req := httptest.NewRequest(http.MethodPost, "/runs/"+sc.RunID+"/result", strings.NewReader(`{"status":"completed"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the scan's token on its hidden run = %d; want 401", rec.Code)
	}

	out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, secBody(t, sha('c'), nil))
	if err != nil || out != security.Stored {
		t.Fatalf("CompleteSecurityScan = %v, %v; want Stored\n%s", out, err, env.Logs())
	}
	got := secScan(t, env, sc.ID)
	if got.State != store.SecurityComplete || got.NewCount != 1 || got.SkillName != "security-audit" || !got.HasSARIF {
		t.Errorf("scan = %+v", got)
	}
	if f := secFindings(t, env); len(f) != 1 || f[0].Fingerprint != "fp1" || f[0].State != "open" {
		t.Errorf("findings = %+v", f)
	}
	if !secUsageRows(t, env, sc, 1) {
		t.Errorf("usage rows: want 1")
	}
	if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, secBody(t, sha('c'), nil)); err != nil ||
		out != security.Conflict {
		t.Errorf("second post = %v, %v; want Conflict", out, err)
	}
	if strings.Contains(env.Logs(), token) {
		t.Errorf("the log holds the token")
	}
}

// Every text holding a secret is withheld before it is stored: a held LLM
// key in the reason and a SARIF message, the deploy token in a SARIF
// property key and in a state file (dropped), and the scan's own token in a
// SARIF value.
func TestSecurityWithholdsSecrets(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	sc, token := secRunning(t, env)
	hit := secHit("fp1", "SQL built from input "+testLLMKey)
	props := hit["properties"].(map[string]any)
	props[testDeployToken] = "v"
	props["exploit_scenario"] = "send " + token
	body := secBody(t, sha('c'), func(e *security.Envelope) {
		e.Status, e.Reason = "partial", "stopped: "+testLLMKey
		e.State = map[string]string{"findings.json": "[]", "notes.md": "key " + testDeployToken}
		e.SARIF = secSARIF(t, hit)
	})
	if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, body); err != nil || out != security.Stored {
		t.Fatalf("CompleteSecurityScan = %v, %v\n%s", out, err, env.Logs())
	}
	got := secScan(t, env, sc.ID)
	if got.State != store.SecurityPartial || got.Reason != security.Withheld {
		t.Errorf("scan = %s %q; want partial, withheld", got.State, got.Reason)
	}
	sarif, _, err := env.Store.SecurityScanSARIF(env.ctx, sc.ID)
	for _, s := range []string{testLLMKey, testDeployToken, token} {
		if err != nil || strings.Contains(string(sarif), s) {
			t.Errorf("stored SARIF holds %q (%v): %s", s, err, sarif)
		}
	}
	if !strings.Contains(string(sarif), security.Withheld) {
		t.Errorf("stored SARIF = %s; want the withheld mark", sarif)
	}
	f := secFindings(t, env)
	if len(f) != 1 || f[0].Title != security.Withheld || f[0].Description != security.Withheld ||
		f[0].Exploit != security.Withheld {
		t.Errorf("findings = %+v; want the title, description and exploit withheld", f)
	}
	if querySQL(t, env, `SELECT state_files ? 'notes.md' OR position('`+testDeployToken+`' in state_files::text) > 0
		FROM security_scans WHERE id = '`+sc.ID+`'::uuid`) {
		t.Errorf("the state file with the deploy token was kept")
	}
	if !querySQL(t, env, `SELECT state_files ? 'findings.json' FROM security_scans WHERE id = '`+sc.ID+`'::uuid`) {
		t.Errorf("the clean state file was dropped")
	}
	if strings.Contains(env.Logs(), testLLMKey) || strings.Contains(env.Logs(), token) {
		t.Errorf("the log holds the key or the token")
	}
}

// A result pergent.sh can't use ends the scan failed, with no findings, and
// is still taken (204), so pergent does not post it again.
func TestSecurityUnusableResult(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	cases := []struct {
		name   string
		edit   func(e *security.Envelope)
		reason string
		usage  int
	}{
		{"SARIF 2.0.0", func(e *security.Envelope) { e.SARIF = json.RawMessage(`{"version":"2.0.0","runs":[]}`) },
			`pergent sent a result pergent.sh couldn't use: the SARIF version must be 2.1.0, got "2.0.0"`, 1},
		{"kind review", func(e *security.Envelope) { e.Kind = "review" },
			`pergent sent a result pergent.sh couldn't use: kind must be "security"`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, token := secRunning(t, env)
			out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, secBody(t, sha('c'), c.edit))
			if err != nil || out != security.Stored {
				t.Fatalf("CompleteSecurityScan = %v, %v; want Stored", out, err)
			}
			got := secScan(t, env, sc.ID)
			if got.State != store.SecurityFailed || got.Reason != c.reason || got.NewCount != 0 || got.HasSARIF {
				t.Errorf("scan = %s %q, new %d, SARIF %v; want failed %q, nothing kept", got.State, got.Reason,
					got.NewCount, got.HasSARIF, c.reason)
			}
			if !secUsageRows(t, env, sc, c.usage) {
				t.Errorf("usage rows: want %d", c.usage)
			}
		})
	}
	if f := secFindings(t, env); len(f) != 0 {
		t.Errorf("findings = %d; want none", len(f))
	}
}

// A partial post after a cancel is stored partial, cancelled, whatever
// reason pergent gave.
func TestSecurityCancelledPost(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	sc, token := secRunning(t, env)
	if _, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, testProjectPath, sc.ID); err != nil {
		t.Fatalf("CancelSecurityScan: %v", err)
	}
	body := secBody(t, sha('c'), func(e *security.Envelope) { e.Status, e.Reason = "partial", "terminated" })
	if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, body); err != nil || out != security.Stored {
		t.Fatalf("CompleteSecurityScan = %v, %v", out, err)
	}
	if got := secScan(t, env, sc.ID); got.State != store.SecurityPartial || got.Reason != "cancelled" {
		t.Errorf("scan = %s %q; want partial, cancelled", got.State, got.Reason)
	}
}

// More than 5000 findings: the first 5000 are kept, the scan is partial, and
// a partial scan fixes nothing.
func TestSecurityCappedIsPartial(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	first, token := secRunning(t, env)
	old := secBody(t, sha('c'), func(e *security.Envelope) { e.SARIF = secSARIF(t, secHit("old", "an old finding")) })
	if out, err := env.Engine.CompleteSecurityScan(env.ctx, first.ID, token, old); err != nil || out != security.Stored {
		t.Fatalf("first scan = %v, %v", out, err)
	}

	sc, token := secRunning(t, env)
	many := make([]map[string]any, security.MaxFindings+1)
	for i := range many {
		many[i] = secHit(fmt.Sprintf("fp%d", i), "finding")
	}
	body := secBody(t, sha('c'), func(e *security.Envelope) { e.SARIF = secSARIF(t, many...) })
	if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, body); err != nil || out != security.Stored {
		t.Fatalf("CompleteSecurityScan = %v, %v", out, err)
	}
	got := secScan(t, env, sc.ID)
	if got.State != store.SecurityPartial || got.Reason != "more than 5000 findings: the first 5000 are kept" ||
		got.NewCount != security.MaxFindings || got.FixedCount != 0 {
		t.Errorf("scan = %s %q, new %d, fixed %d", got.State, got.Reason, got.NewCount, got.FixedCount)
	}
	open, _, err := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath,
		store.SecurityFindingFilter{State: "open", Query: "an old finding"})
	if err != nil || len(open) != 1 {
		t.Errorf("the old finding = %d rows, %v; want still open", len(open), err)
	}
}

// The envelope's commit and usage source are stored; 2 models give 2 usage
// rows; a state file over 512 KiB is not kept, the others are.
func TestSecurityResultFields(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)

	t.Run("fields", func(t *testing.T) {
		sc, token := secRunning(t, env)
		body := secBody(t, sha('c'), func(e *security.Envelope) {
			e.Usage.Source = "stream"
			e.Usage.Models = append(e.Usage.Models, security.ModelUsage{Provider: "acme", Model: "small", Steps: 2,
				Tokens: security.Tokens{Input: 10, Output: 2}})
			e.State = map[string]string{"big.json": strings.Repeat("x", 600<<10), "findings.json": "[]"}
		})
		if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, body); err != nil || out != security.Stored {
			t.Fatalf("CompleteSecurityScan = %v, %v", out, err)
		}
		got := secScan(t, env, sc.ID)
		if got.State != store.SecurityComplete || got.UsageSource != "stream" || got.CoverageDone == nil ||
			*got.CoverageDone != 3 {
			t.Errorf("scan = %+v", got)
		}
		if !secUsageRows(t, env, sc, 2) {
			t.Errorf("usage rows: want 2")
		}
		state, ok, err := env.Store.LastCompleteSecurityState(env.ctx, testInstance, testProjectPath)
		if err != nil || !ok || len(state) != 1 || state["findings.json"] != "[]" {
			t.Errorf("state = %d files, %v, %v; want only findings.json", len(state), ok, err)
		}
	})
	t.Run("another commit", func(t *testing.T) {
		sc, token := secRunning(t, env)
		if out, err := env.Engine.CompleteSecurityScan(env.ctx, sc.ID, token, secBody(t, sha('d'), nil)); err != nil ||
			out != security.Stored {
			t.Fatalf("CompleteSecurityScan = %v, %v", out, err)
		}
		got := secScan(t, env, sc.ID)
		if got.State != store.SecurityFailed ||
			got.Reason != "pergent sent a result pergent.sh couldn't use: the result is for another commit" {
			t.Errorf("scan = %s %q", got.State, got.Reason)
		}
	})
	t.Run("too large", func(t *testing.T) {
		sc, token := secRunning(t, env)
		if err := env.Engine.SecurityTooLarge(env.ctx, sc.ID, "wrong"); err != nil {
			t.Fatal(err)
		}
		if got := secScan(t, env, sc.ID); got.State != store.SecurityRunning {
			t.Fatalf("a wrong token ended the scan: %s", got.State)
		}
		if err := env.Engine.SecurityTooLarge(env.ctx, sc.ID, token); err != nil {
			t.Fatal(err)
		}
		if got := secScan(t, env, sc.ID); got.State != store.SecurityFailed ||
			got.Reason != "the scan Job sent a result over 12 MiB" {
			t.Errorf("scan = %s %q", got.State, got.Reason)
		}
	})
}

// The source is the platform's archive of the scan's own commit, read with
// the review bot or App; only the running scan's token opens it. A source
// over the limit fails the scan and deletes its Job.
func TestOpenSecuritySource(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	gh := addGitHub(t, env, false)
	env.Engine.Archives = func(instance string) (forge.ArchiveReader, bool) {
		p, ok := env.Engine.Platforms.Get(instance)
		if !ok {
			return nil, false
		}
		r, ok := p.Forge.(forge.ArchiveReader)
		return r, ok
	}
	// The review bot reads the repository by its path.
	env.GitLab.Reply(http.MethodGet, "/projects/acme%2Fwidgets", http.StatusOK,
		map[string]any{"id": testProjectID, "path_with_namespace": testProjectPath})
	env.GitLab.Handle(http.MethodGet, projectPath()+"/repository/archive.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sha") != sha('c') {
			http.Error(w, "wrong commit", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "gitlab-archive")
	})
	read := func(t *testing.T, rc io.ReadCloser) string {
		t.Helper()
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("GitLab", func(t *testing.T) {
		sc, token := secRunning(t, env)
		rc, out, err := env.Engine.OpenSecuritySource(env.ctx, sc.ID, token)
		if err != nil || out != security.Stored {
			t.Fatalf("OpenSecuritySource = %v, %v\n%s", out, err, env.Logs())
		}
		if got := read(t, rc); got != "gitlab-archive" {
			t.Errorf("archive = %q", got)
		}
		env.Dispatched(2, sha('a'))
		if _, out, err := env.Engine.OpenSecuritySource(env.ctx, sc.ID, "seed-token"); err != nil ||
			out != security.Unauthorized {
			t.Errorf("a review's token = %v, %v; want Unauthorized", out, err)
		}
		if _, out, err := env.Engine.OpenSecuritySource(env.ctx, "not-a-scan", token); err != nil || out != security.Unauthorized {
			t.Errorf("no such scan = %v, %v; want Unauthorized", out, err)
		}

		// Over the limit: the scan fails and its Job goes.
		if err := env.Engine.SecuritySourceTooLarge(env.ctx, sc.ID, token); err != nil {
			t.Fatal(err)
		}
		if got := secScan(t, env, sc.ID); got.State != store.SecurityFailed || got.Reason != "the source is over the size limit" {
			t.Errorf("scan = %s %q", got.State, got.Reason)
		}
		if secJob(t, env, sc.ID) != nil {
			t.Errorf("the Job of a scan with too much source was not deleted")
		}
		if _, out, err := env.Engine.OpenSecuritySource(env.ctx, sc.ID, token); err != nil || out != security.Conflict {
			t.Errorf("after it ended = %v, %v; want Conflict", out, err)
		}
	})
	t.Run("GitLab can't read it", func(t *testing.T) {
		sc, token := secRunning(t, env)
		env.GitLab.Reply(http.MethodGet, projectPath()+"/repository/archive.tar.gz", http.StatusNotFound, `{"message":"404"}`)
		_, _, err := env.Engine.OpenSecuritySource(env.ctx, sc.ID, token)
		if !errors.Is(err, security.ErrNoSource) || !errors.Is(err, forge.ErrSourceNotFound) {
			t.Errorf("err = %v; want ErrNoSource and ErrSourceNotFound", err)
		}
		if got := secScan(t, env, sc.ID); got.State != store.SecurityRunning {
			t.Errorf("scan = %s; want still running (pergent ends it)", got.State)
		}
		secFinish(t, env, sc, store.SecurityResult{Status: store.SecurityFailed})
	})
	t.Run("GitHub", func(t *testing.T) {
		sc, err := env.Store.QueueSecurityScan(env.ctx, store.SecurityScanRequest{Instance: ghInstance, Project: ghRepoPath,
			ProjectID: ghRepoID, Branch: "main", Commit: sha('e'), StartedBy: "maya", TimeoutSeconds: 3600}, env.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Hour)); err != nil || !ok {
			t.Fatalf("claim = %v, %v", ok, err)
		}
		const token = "gh-scan-token"
		if err := env.Store.StartSecurityJob(env.ctx, sc.ID, store.SecurityJobStart{JobName: "j", TokenHash: hashToken(token),
			Deadline: env.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "github-archive")
		}))
		defer download.Close()
		gh.Handle(http.MethodGet, "/repos/"+ghRepoPath+"/tarball/"+sha('e'), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, download.URL+"/a.tar.gz", http.StatusFound)
		})
		rc, out, err := env.Engine.OpenSecuritySource(env.ctx, sc.ID, token)
		if err != nil || out != security.Stored {
			t.Fatalf("OpenSecuritySource = %v, %v\n%s", out, err, env.Logs())
		}
		if got := read(t, rc); got != "github-archive" {
			t.Errorf("archive = %q", got)
		}
	})
}
