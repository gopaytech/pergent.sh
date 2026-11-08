# Usage You Can See (Project 1) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Admins see where model tokens go, by repository, skill, merge request, model and day, what was wasted and what each skill gives back, with a CSV export and a private Prometheus endpoint.

**Architecture:** pergent adds cache and reasoning tokens to each skill's `tokens` in its result. pergent.sh records one `run_usage` row per run and skill the Job actually ran, from the posted body after the callback accepts it, and fills rows for older results in the background. Every view, the CSV, the review page's token sum and the metrics read that table. Metrics are served on their own port, off by default, through their own `ClusterIP` Service.

**Tech Stack:** Go 1.x, Postgres (pgx), React 18 + react-query 5 + vitest, Helm. No new Go or npm dependency: the Prometheus text format is written by hand.

**Design:** `docs/superpowers/specs/2026-11-08-usage-visibility-vision.md` (Project 1). Read it first. Every "the owner's decision" there is fixed; do not reopen it.

**Rules for whoever runs this plan:**
- Never start a container (no `docker`, `orbctl`, or anything that wakes OrbStack). The store, dashboard and engine tests skip without `PERGENT_SH_TEST_DATABASE_URL`; they run in CI only. Say so when you report.
- Never merge, push, tag or release without the owner asking. Commit with `git -c user.email=zufardhiyaulhaq@gmail.com commit`, and end every commit message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Before every commit in either repo, run the public-name scan and expect no output:
  `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`
- After each task that changes code, break the new code on purpose once (a condition flipped, a field dropped) and check a test fails, then put it back. Note it in the PR body.
- Three pull requests, in order: PR 1 in pergent (Tasks 1–3), PR 2 in pergent.sh (Tasks 4–15), PR 3 in pergent.sh (Tasks 16–21). PR 2 doesn't wait for PR 1's release.

---

## PR 1: pergent reports cache and reasoning tokens

Repository: `~/Documents/personal/github/pergent`, a new branch `feat/usage-tokens` from `origin/master`. Check the pin first: pergent.sh pins pergent `v0.8.0`; work on `master`.

`internal/runner/parser.go` already decodes `reasoning`, `cache.read` and `cache.write` (`openCodeTokens`, lines 46–55). Only `Usage` drops them. Every sum goes through `Usage.Add`, so nothing in `internal/review/` changes.

### Task 1: Count cache and reasoning tokens in `Usage`

**Files:**
- Modify: `internal/runner/parser.go` (the `Usage` type and `Add`, about lines 85–94; the `Transcript.Usage` comment, about 133–135; the `step_finish` sum, about 197–207)
- Test: `internal/runner/parser_test.go` (`TestParseOutput_Usage`, about 234–244)

**Step 1: Change the test to want the new fields**

In `TestParseOutput_Usage`, add `"reasoning":4` to the first step's tokens and change the expectation:

```go
	stdout := `{"type":"step_start","part":{"type":"step-start"}}
{"type":"step_finish","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":99,"input":10,"output":3,"reasoning":4,"cache":{"read":50,"write":1}}}}
{"type":"step_start","part":{"type":"step-start"}}
{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":200,"input":20,"output":5}}}
`
	got := ParseOutput([]byte(stdout))
	if want := (Usage{Input: 30, Output: 8, Reasoning: 4, CacheRead: 50, CacheWrite: 1}); got.Usage != want {
```

**Step 2: Run it to see it fail**

Run: `go test ./internal/runner/ -run TestParseOutput_Usage`
Expected: FAIL to compile, `unknown field Reasoning in struct literal`.

**Step 3: Add the fields**

```go
// Usage is the tokens a run consumed, summed over every finished step.
// Input and Output are what opencode reports as input and output; Reasoning,
// CacheRead and CacheWrite are reported apart and never added to them.
type Usage struct {
	Input, Output                    int
	Reasoning, CacheRead, CacheWrite int
}

// Add returns u plus o.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input: u.Input + o.Input, Output: u.Output + o.Output,
		Reasoning: u.Reasoning + o.Reasoning, CacheRead: u.CacheRead + o.CacheRead, CacheWrite: u.CacheWrite + o.CacheWrite,
	}
}
```

In the `step_finish` case, after the two existing lines:

```go
					t.Usage.Input += event.Part.Tokens.Input
					t.Usage.Output += event.Part.Tokens.Output
					t.Usage.Reasoning += event.Part.Tokens.Reasoning
					t.Usage.CacheRead += event.Part.Tokens.Cache.Read
					t.Usage.CacheWrite += event.Part.Tokens.Cache.Write
```

Update the `Transcript.Usage` comment to name all five fields.

**Step 4: Run the runner and review tests**

Run: `go test ./internal/runner/ ./internal/review/`
Expected: PASS. `TestRun_UsageAcrossAttempts`, `TestRun_UsageSummedPerSkill` and `TestRun_UsageCountsEveryRun` compare whole values and still pass.

**Step 5: Extend the retry test**

In `internal/runner/runner_test.go`, give `usageStep` a variant that sets `"cache":{"read":R}` on one attempt and assert `CacheRead` is summed across attempts in `TestRun_UsageAcrossAttempts`. Run `go test ./internal/runner/`; expect PASS.

**Step 6: Commit**

```bash
git add internal/runner/
git -c user.email=zufardhiyaulhaq@gmail.com commit -m "feat(runner): count reasoning and cache tokens apart from input and output"
```

### Task 2: Put them in the result

**Files:**
- Modify: `internal/result/result.go` (`Tokens`, about 145–149; `buildSkill`, about 220)
- Test: `internal/result/result_test.go` (about 51 and 78–81)

**Step 1: Write the failing assertions**

Set `Usage: runner.Usage{Input: 8, Output: 3, Reasoning: 2, CacheRead: 40, CacheWrite: 5}` at line 51, and replace the token assertion:

```go
	tok := sk["tokens"].(map[string]any)
	want := map[string]any{"input": float64(8), "output": float64(3), "reasoning": float64(2), "cache_read": float64(40), "cache_write": float64(5)}
	if !reflect.DeepEqual(tok, want) {
		t.Errorf("tokens = %v, want %v", tok, want)
	}
```

Add a second case with all three at zero and assert the keys are present with `0`: pergent.sh reads a missing key as "unknown" and a zero as "none", so the keys are never omitted.

**Step 2: Run it to see it fail**

Run: `go test ./internal/result/ -run Tokens -v` (or the test's own name)
Expected: FAIL, `tokens = map[input:8 output:3]`.

**Step 3: Implement**

```go
// Tokens is the model usage of a skill's runs. Input and Output keep their
// meaning from the first version of the schema; the other three are counted
// apart and never included in them.
type Tokens struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}
```

```go
		Tokens: Tokens{Input: s.Usage.Input, Output: s.Usage.Output, Reasoning: s.Usage.Reasoning,
			CacheRead: s.Usage.CacheRead, CacheWrite: s.Usage.CacheWrite},
```

**Step 4: Run**

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: PASS, no gofmt output.

**Step 5: Commit**

```bash
git add internal/result/
git -c user.email=zufardhiyaulhaq@gmail.com commit -m "feat(result): report reasoning and cache tokens per skill"
```

### Task 3: Document it, open PR 1

**Files:**
- Modify: `docs/agent-result-schema.md` (example `tokens` lines, about 86 and 116; the `tokens` row, about 193)

**Step 1:** Change both example lines to the five keys, for example `"tokens": { "input": 52310, "output": 1840, "reasoning": 0, "cache_read": 41200, "cache_write": 3100 }`.

**Step 2:** Replace the `tokens` row:

```
| `tokens` | object | Token counts summed over every run of the skill: batch, repair, verification and cross-batch runs, retries included. `input` and `output` are what the provider reports as input and output. `reasoning`, `cache_read` and `cache_write` are counted apart and never included in `input` or `output`; they are new in pergent v0.9.0, and an older pergent leaves them out. An additive field: `schema_version` stays `1`, and a reader that doesn't know them can ignore them. A provider that reports no usage gives zeros. |
```

Use the real next version number in place of `v0.9.0`: check `git tag --sort=-v:refname | head -1` after `git fetch --tags`.

**Step 3:** Run `go test ./...`, the name scan, then commit `docs(result): document the new token counts` and open the PR (only when the owner asks). After it merges, the owner releases it; that release is what Task 15 waits for.

---

## PR 2: pergent.sh records usage and shows it to admins

Repository: `~/Documents/personal/github/pergent.sh-usage`, branch `feat/usage` from current `origin/main` (the vision doc is untracked there; commit it in the first commit of this PR).

### Task 4: The `run_usage` table

**Files:**
- Create: `internal/store/migrations/0032_run_usage.sql` (check `ls internal/store/migrations | tail -1` first; use the next free number)

**Step 1: Write the migration**

```sql
-- Model usage per run and skill, for the admin usage views, the CSV export,
-- the review page's token sum and the metrics. One row per skill the Job ran,
-- written when a usable result arrives and filled in from older results in
-- the background (runs.usage_recorded says which runs are done). It only
-- adds: the previous release still runs on the migrated database and ignores
-- both.
--
-- cache_read, cache_write and reasoning are NULL when the Job's pergent
-- didn't report them (before pergent's cache and reasoning counts), and 0
-- when it reported none.
CREATE TABLE run_usage (
    run_id       uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    skill        text        NOT NULL,
    instance     text        NOT NULL,
    project_id   bigint      NOT NULL,
    project_path text        NOT NULL,
    mr_iid       bigint      NOT NULL,
    trigger      text        NOT NULL,
    incremental  boolean     NOT NULL,
    model        text        NOT NULL,
    verdict      text        NOT NULL,
    reported_at  timestamptz NOT NULL,
    tokens_in    bigint      NOT NULL CHECK (tokens_in >= 0),
    tokens_out   bigint      NOT NULL CHECK (tokens_out >= 0),
    cache_read   bigint      CHECK (cache_read >= 0),
    cache_write  bigint      CHECK (cache_write >= 0),
    reasoning    bigint      CHECK (reasoning >= 0),
    findings     integer     NOT NULL,
    blocking     integer     NOT NULL,
    PRIMARY KEY (run_id, skill)
);

CREATE INDEX run_usage_reported_idx ON run_usage (reported_at);
CREATE INDEX run_usage_project_idx ON run_usage (instance, project_id, reported_at);

ALTER TABLE runs ADD COLUMN usage_recorded boolean NOT NULL DEFAULT false;
CREATE INDEX runs_usage_pending_idx ON runs (created_at) WHERE NOT usage_recorded AND result IS NOT NULL;
```

Runs are never deleted (no `DELETE FROM runs` exists), and neither `DeleteProject` nor `DeleteInstanceData` touches this table, so usage outlives a switched-off repository or a removed instance, as decided. Do not add `run_usage` to either function.

**Step 2:** `go build ./... && go test ./internal/store/` (skips without a database; `TestMigrationNumbersAreUnique` checks the number in CI).

**Step 3: Commit** `feat(store): add the run_usage table` together with the vision doc.

### Task 5: Read usage out of a result

**Files:**
- Create: `internal/usage/usage.go`
- Test: `internal/usage/usage_test.go`

`internal/usage` turns a result body into rows. It imports `internal/store` (for the row type) and `internal/result` (for `ImpactSkill`); nothing imports it except `internal/callback`, `cmd/pergent-sh` and `internal/dashboard`.

**Step 1: Write the failing tests**

```go
package usage

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func i64(v int64) *int64 { return &v }

func TestFromResultCountsEverySkill(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","skills":[
	 {"name":"security","verdict":"has_blocking_issues","findings":[{"severity":"blocking"},{"severity":"suggestion"}],
	  "tokens":{"input":100,"output":20,"reasoning":3,"cache_read":400,"cache_write":7}},
	 {"name":"style","verdict":"failed","findings":[],"tokens":{"input":5,"output":1}}]}`)
	got, err := FromResult(body, store.Run{Trigger: store.TriggerPush})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.SkillUsage{
		{Skill: "security", Verdict: "has_blocking_issues", In: 100, Out: 20, Reasoning: i64(3), CacheRead: i64(400), CacheWrite: i64(7), Findings: 2, Blocking: 1},
		{Skill: "style", Verdict: "failed", In: 5, Out: 1},
	}
	if !equal(got, want) {
		t.Errorf("FromResult = %+v\nwant %+v", got, want)
	}
}

// A re-run of failed skills is stored merged with the skills it kept, and
// those carry the earlier run's tokens: only its own skills count.
func TestFromResultRerunFailedCountsOnlyItsSkills(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","merged":true,"skills":[
	 {"name":"security","verdict":"no_blocking_issues","tokens":{"input":100,"output":20}},
	 {"name":"style","verdict":"no_blocking_issues","tokens":{"input":5,"output":1}}]}`)
	got, _ := FromResult(body, store.Run{Trigger: store.TriggerRerunFailed, SkillSubset: []string{"style"}})
	if len(got) != 1 || got[0].Skill != "style" {
		t.Errorf("FromResult = %+v, want only style", got)
	}
}

// A merged incremental result can carry the base run's impact skill as it
// was, tokens included: it isn't counted.
func TestFromResultMergedIncrementalSkipsImpact(t *testing.T) {
	body := []byte(`{"schema_version":1,"status":"completed","merged":true,"incremental":{"reviewed":[],"carried":[],"from":"abc"},"skills":[
	 {"name":"impact","verdict":"no_blocking_issues","tokens":{"input":9,"output":9}},
	 {"name":"security","verdict":"no_blocking_issues","tokens":{"input":1,"output":1}}]}`)
	got, _ := FromResult(body, store.Run{Trigger: store.TriggerPush, Incremental: true})
	if len(got) != 1 || got[0].Skill != "security" {
		t.Errorf("FromResult = %+v, want only security", got)
	}
}

func TestFromResultOddShapes(t *testing.T) {
	for _, body := range []string{`not json`, `{"skills":"x"}`, `{"skills":[{"name":""}]}`, `{"skills":[{"name":"a","tokens":{"input":-1,"output":0}}]}`} {
		got, err := FromResult([]byte(body), store.Run{})
		if err == nil && len(got) != 0 {
			t.Errorf("FromResult(%s) = %+v, want nothing", body, got)
		}
	}
}

func equal(a, b []store.SkillUsage) bool { /* compare field by field, *int64 by value */ }
```

Write `equal` in full (dereference the three pointers). Before writing the impact test, read `result.MergeIncremental` in `internal/result/result.go` and confirm the base's `impact` skill is appended as is when the new run didn't repeat it, and that a live posted body is never `merged`. If a merged result can tell a fresh impact skill from a carried one, use that instead of skipping it, and change the test.

**Step 2:** Run `go test ./internal/usage/`; expect a compile failure (`undefined: FromResult`, `undefined: store.SkillUsage`).

**Step 3: Add the row type to the store**

In a new file `internal/store/usage.go`:

```go
package store

// SkillUsage is what one skill used in one run, as the Job reported it.
// CacheRead, CacheWrite and Reasoning are nil when the Job didn't report
// them.
type SkillUsage struct {
	Skill, Verdict                   string
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning *int64
	Findings, Blocking               int
}
```

**Step 4: Implement `FromResult`**

```go
// Package usage reads what a review's skills used out of its result.
package usage

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

type rawResult struct {
	Merged      bool            `json:"merged"`
	Incremental json.RawMessage `json:"incremental"`
	Skills      []rawSkill      `json:"skills"`
}

type rawSkill struct {
	Name     string `json:"name"`
	Verdict  string `json:"verdict"`
	Findings []struct {
		Severity string `json:"severity"`
	} `json:"findings"`
	Tokens *struct {
		Input      int64  `json:"input"`
		Output     int64  `json:"output"`
		Reasoning  *int64 `json:"reasoning"`
		CacheRead  *int64 `json:"cache_read"`
		CacheWrite *int64 `json:"cache_write"`
	} `json:"tokens"`
}

var errNoSkills = errors.New("usage: the result has no skills")

// FromResult returns what each skill of run used, from body: the result the
// Job posted, or the one stored for run (which may be merged). Only skills
// the run itself ran count: a re-run of failed skills counts its subset, and
// a merged incremental result skips the impact skill it may have carried
// from the base run. A skill without a name or with negative counts is left
// out; a body that isn't a result is an error.
func FromResult(body []byte, run store.Run) ([]store.SkillUsage, error) {
	var res rawResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	if res.Skills == nil {
		return nil, errNoSkills
	}
	mergedIncremental := res.Merged && len(res.Incremental) > 0 && string(res.Incremental) != "null"
	var out []store.SkillUsage
	for _, s := range res.Skills {
		switch {
		case s.Name == "":
			continue
		case run.Trigger == store.TriggerRerunFailed && run.SkillSubset != nil && !slices.Contains(run.SkillSubset, s.Name):
			continue
		case mergedIncremental && s.Name == result.ImpactSkill:
			continue
		}
		u := store.SkillUsage{Skill: s.Name, Verdict: s.Verdict, Findings: len(s.Findings)}
		for _, f := range s.Findings {
			if f.Severity == "blocking" {
				u.Blocking++
			}
		}
		if t := s.Tokens; t != nil {
			if t.Input < 0 || t.Output < 0 || negative(t.Reasoning) || negative(t.CacheRead) || negative(t.CacheWrite) {
				continue
			}
			u.In, u.Out, u.Reasoning, u.CacheRead, u.CacheWrite = t.Input, t.Output, t.Reasoning, t.CacheRead, t.CacheWrite
		}
		out = append(out, u)
	}
	return out, nil
}

func negative(v *int64) bool { return v != nil && *v < 0 }
```

If `json.Unmarshal` of `"skills":"x"` errors, `TestFromResultOddShapes` is satisfied by the error; keep the test's "error or nothing" rule.

**Step 5:** `go test ./internal/usage/ && go vet ./internal/usage/`. Expect PASS.

**Step 6: Commit** `feat(usage): read each skill's usage out of a result`.

### Task 6: Store usage

**Files:**
- Modify: `internal/store/usage.go`
- Test: `internal/store/usage_test.go`

**Step 1: Write the failing test** (runs in CI only)

```go
func TestRecordUsage(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "head1", `{"schema_version":1,"status":"completed","skills":[]}`)
	rows := []SkillUsage{{Skill: "security", Verdict: "failed", In: 10, Out: 2, Findings: 1, Blocking: 1}}
	if err := s.RecordUsage(ctx, id, rows); err != nil {
		t.Fatal(err)
	}
	// A second call keeps the first rows: the background fill and the
	// callback can meet.
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", In: 99}}); err != nil {
		t.Fatal(err)
	}
	var in int64
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT u.tokens_in, r.usage_recorded FROM run_usage u JOIN runs r ON r.id = u.run_id WHERE u.run_id = $1::uuid`, id).Scan(&in, &recorded); err != nil {
		t.Fatal(err)
	}
	if in != 10 || !recorded {
		t.Errorf("tokens_in = %d, recorded = %v; want 10, true", in, recorded)
	}
}

// A run with no skills is still marked, so the background fill doesn't read
// it again on every start.
func TestRecordUsageNoSkillsMarksTheRun(t *testing.T) { /* RecordUsage(ctx, id, nil), then usage_recorded is true and no rows */ }

func TestRunsWithoutUsage(t *testing.T) { /* two reported runs, record one, RunsWithoutUsage(ctx, 10) returns the other only; a run with result NULL is never returned */ }
```

Check `reported(...)`'s real signature in `internal/store/store_test.go` (it may return the run, not its ID) and adapt.

**Step 2:** `go vet ./internal/store/` fails: `RecordUsage` undefined.

**Step 3: Implement**

```go
// RecordUsage stores what each skill of run id used, with the run's
// repository, merge request, trigger, model and time, and marks the run as
// recorded, even with no skills. A row already there is kept.
func (s *Store) RecordUsage(ctx context.Context, id string, skills []SkillUsage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	for _, u := range skills {
		if _, err := tx.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
				trigger, incremental, model, verdict, reported_at, tokens_in, tokens_out, cache_read, cache_write,
				reasoning, findings, blocking)
			SELECT r.id, $2, r.instance, r.project_id, r.project_path, r.mr_iid, r.trigger, r.incremental,
				COALESCE(r.model, ''), $3, COALESCE(r.reported_at, r.created_at), $4, $5, $6, $7, $8, $9, $10
			FROM runs r WHERE r.id = $1::uuid
			ON CONFLICT (run_id, skill) DO NOTHING`,
			id, u.Skill, u.Verdict, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning, u.Findings, u.Blocking); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET usage_recorded = true WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunsWithoutUsage returns up to limit runs with a stored result and no
// usage recorded, oldest first.
func (s *Store) RunsWithoutUsage(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runColumns+` FROM runs
		WHERE NOT usage_recorded AND result IS NOT NULL ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Run, error) { return scanRun(row) })
}
```

Use whatever `runColumns`/scan helper `runs.go` really has (`scanRun` is a guess; read `runs.go:185` onwards).

**Step 4:** `go build ./... && go vet ./internal/store/`. Commit `feat(store): record usage per run and skill`.

### Task 7: Record usage when a result is accepted

**Files:**
- Modify: `internal/callback/callback.go` (`Results` interface, about 26–31; `stored`, about 99–117)
- Modify: `internal/callback/callback_test.go` (the fake that implements `Results`, about line 51)

Usage is recorded only after `check` accepts the result: a result turned into a failure by `ReplaceWithFailure` shows as a review with no result, and its tokens as unknown, as the vision says. A failure to record is logged and never changes the answer to the Job.

**Step 1: Write the failing tests** in `callback_test.go`:
- An accepted result calls `RecordUsage` once with the run's ID and the skills of the posted body (use a body with two skills and assert both names and `In`).
- An unusable result (base/head mismatch, as an existing test does) calls `ReplaceWithFailure` and never `RecordUsage`.
- `RecordUsage` returning an error still answers 200 and logs a warning containing `usage`.

Add `recordUsage func(id string, skills []store.SkillUsage) error` and a call log to the fake, the way the fake already records `ReplaceWithFailure`.

**Step 2:** `go test ./internal/callback/` fails to compile.

**Step 3: Implement**

Add to the interface:

```go
	RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error
```

At the end of `stored`, before `h.wake()` on the accepted path:

```go
	h.recordUsage(ctx, id, body)
```

```go
// recordUsage stores what the accepted result's skills used. Usage is for
// the admin views only: a failure is logged and never changes the review.
func (h *Handler) recordUsage(ctx context.Context, id string, body []byte) {
	run, found, err := h.Results.Run(ctx, id)
	if err != nil || !found {
		h.log().Warn("couldn't record the result's usage", "run", id, "found", found, "error", errText(err))
		return
	}
	skills, err := usage.FromResult(body, run)
	if err != nil {
		h.log().Warn("couldn't read the result's usage", "run", id, "error", err.Error())
		skills = nil
	}
	if err := h.Results.RecordUsage(ctx, id, skills); err != nil {
		h.log().Warn("couldn't record the result's usage", "run", id, "error", err.Error())
	}
}
```

Write `errText(err error) string` (`""` for nil) if the package has none. `*store.Store` already satisfies the interface after Task 6; `go build ./...` proves it at `internal/server/server.go:89`.

**Step 4:** `go test ./internal/callback/ && go build ./...`. Break it once (record before `check`), see the unusable-result test fail, restore.

**Step 5: Commit** `feat(callback): record usage when a result is accepted`.

### Task 8: Fill in usage from older results, in the background

**Files:**
- Create: `internal/usage/backfill.go`
- Test: `internal/usage/backfill_test.go`
- Modify: `cmd/pergent-sh/main.go` (the `loops` list, about line 330)

It runs as a loop after the server listens, never in the migration and never before `net.Listen`: parsing every stored result (each up to 10 MB) before listening could outlast the startup probe and crash-loop the single replica.

**Step 1: Write the failing test** with a fake store:

```go
type fakeRuns struct {
	pending  [][]store.Run
	recorded map[string][]store.SkillUsage
}

func (f *fakeRuns) RunsWithoutUsage(ctx context.Context, limit int) ([]store.Run, error) {
	if len(f.pending) == 0 {
		return nil, nil
	}
	page := f.pending[0]
	f.pending = f.pending[1:]
	return page, nil
}

func (f *fakeRuns) RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error {
	f.recorded[id] = skills
	return nil
}

func TestBackfillRecordsEveryPageAndMarksOddResults(t *testing.T) {
	f := &fakeRuns{recorded: map[string][]store.SkillUsage{}, pending: [][]store.Run{
		{{ID: "a", Result: []byte(`{"skills":[{"name":"s","verdict":"failed","tokens":{"input":1,"output":1}}]}`)}},
		{{ID: "b", Result: []byte(`not json`)}},
	}}
	n := Backfill(context.Background(), f, slog.New(slog.DiscardHandler))
	if n != 2 || len(f.recorded["a"]) != 1 || f.recorded["b"] != nil {
		t.Errorf("n = %d, recorded = %+v", n, f.recorded)
	}
	if _, ok := f.recorded["b"]; !ok {
		t.Error("an odd result wasn't marked, so it would be read again on every start")
	}
}
```

Check the real field name of a run's ID in `store.Run`.

**Step 2: Implement**

```go
// Runs is the part of the store Backfill needs.
type Runs interface {
	RunsWithoutUsage(ctx context.Context, limit int) ([]store.Run, error)
	RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// backfillPage is how many results are read at once: each can be 10 MB.
const backfillPage = 20

// Backfill records the usage of every run whose result was stored before
// usage was recorded, page by page, until none is left or ctx ends. A result
// it can't read is marked with no usage. It returns how many runs it marked.
func Backfill(ctx context.Context, st Runs, log *slog.Logger) int {
	done := 0
	for ctx.Err() == nil {
		runs, err := st.RunsWithoutUsage(ctx, backfillPage)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("couldn't read the runs whose usage isn't recorded yet", "error", err.Error())
			}
			return done
		}
		if len(runs) == 0 {
			return done
		}
		for _, run := range runs {
			skills, err := FromResult(run.Result, run)
			if err != nil {
				skills = nil
			}
			if err := st.RecordUsage(ctx, run.ID, skills); err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't record a run's usage", "run", run.ID, "error", err.Error())
				}
				return done
			}
			done++
		}
	}
	return done
}
```

**Step 3: Wire it** in `main.go`, next to the other loops:

```go
	loops = append(loops, func(ctx context.Context) {
		if n := usage.Backfill(ctx, st, log); n > 0 {
			log.Info("recorded the usage of earlier reviews", "runs", n)
		}
	})
```

It returns when done, which `together` allows.

**Step 4:** `go test ./internal/usage/ && go build ./...`. Commit `feat(usage): fill in usage from earlier results in the background`.

### Task 9: The usage queries

**Files:**
- Modify: `internal/store/usage.go`
- Test: `internal/store/usage_test.go`

All periods are `reported_at >= since`. Totals, rankings and medians use `tokens_in + tokens_out` (the owner's decision); cache and reasoning are summed apart, with a count of rows where they are unknown. Days are cut at midnight UTC.

**Step 1: Write the failing tests** (CI only). Seed with a helper that inserts runs with `reported(...)` and rows with `RecordUsage`, then:
- `TestUsageByRepository`: two repositories; busiest first; reviews counted once per run, not per skill.
- `TestUsageDaily`: rows at 23:30 and 00:30 UTC fall on two days.
- `TestUsageRepositoryBySkillMRModel`: per skill, per merge request (top 50, busiest first, with the latest title), per model.
- `TestUsageWasted`: failed skills' tokens only (a passing skill in the same run isn't counted); re-runs (`trigger <> 'push'`) counted apart; a reported run with `result IS NULL` counts as no result; a superseded run with `result IS NULL` and a `dispatched_at` counts as superseded; a superseded run that kept its result counts in neither.
- `TestUsageMediansAndOutliers`: full and incremental medians apart; `rerun_failed` runs in neither (they cover a subset); a full run at more than three times the full median is an outlier, one at exactly three times isn't.
- `TestUsageCacheUnknown`: a row with NULL `cache_read` adds to the unknown count, not to the sum.

**Step 2: Implement.** Types:

```go
// TokenSum is input and output tokens, plus the counts reported apart and how
// many rows didn't report them.
type TokenSum struct {
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning int64
	Unknown                          int64 // rows without cache and reasoning counts
}

type RepoUsage struct {
	Instance    string
	ProjectID   int64
	ProjectPath string
	Tokens      TokenSum
	Reviews     int64
}

type DayUsage struct {
	Day    time.Time // midnight UTC
	Tokens TokenSum
}

type NamedUsage struct { // a skill, a model
	Name   string
	Tokens TokenSum
	Runs   int64
	Failed int64 // runs where this skill failed (skills only)
}

type MRUsage struct {
	IID     int64
	Title   string
	WebURL  string
	Tokens  TokenSum
	Reviews int64
}

type WastedUsage struct {
	Failed     TokenSum // tokens of skills that failed
	Reruns     TokenSum // tokens of re-runs, not counted as waste
	RerunCount int64
	NoResult   int64 // reviews that left no result
	Superseded int64 // reviews stopped by a newer push before their result
}

type ReviewCost struct {
	RunID       string
	IID         int64
	Incremental bool
	Total       int64
	At          time.Time
}

type CostPerReview struct {
	FullMedian, IncrementalMedian *float64 // nil without reviews of that kind
	Outliers                          []ReviewCost
}
```

The sum columns, shared by every query:

```go
const tokenSumColumns = `COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0),
	COALESCE(sum(cache_read), 0), COALESCE(sum(cache_write), 0), COALESCE(sum(reasoning), 0),
	count(*) FILTER (WHERE cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)`
```

Queries (each a method; scan with `pgx.CollectRows` as `AuditLog` does). In the SQL below, `<tokenSumColumns>` stands for that constant, spliced into the Go string with `+`:

```sql
-- UsageByRepository(ctx, since)
SELECT instance, project_id, (array_agg(project_path ORDER BY reported_at DESC))[1], <tokenSumColumns>,
       count(DISTINCT run_id)
FROM run_usage WHERE reported_at >= $1
GROUP BY instance, project_id
ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, instance, project_id

-- UsageDaily(ctx, since, instance, projectID): instance "" means every repository
SELECT ((reported_at AT TIME ZONE 'UTC')::date)::timestamp AT TIME ZONE 'UTC' AS day, <tokenSumColumns>
FROM run_usage WHERE reported_at >= $1 AND ($2 = '' OR (instance = $2 AND project_id = $3))
GROUP BY day ORDER BY day

-- UsageBySkill(ctx, instance, projectID, since)
SELECT skill, <tokenSumColumns>, count(*), count(*) FILTER (WHERE verdict = 'failed')
FROM run_usage WHERE instance = $1 AND project_id = $2 AND reported_at >= $3
GROUP BY skill ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, skill

-- UsageByModel: the same with `model` in place of skill, count(DISTINCT run_id), Failed 0

-- UsageByMR(ctx, instance, projectID, since, limit 50)
SELECT u.mr_iid, (array_agg(r.title ORDER BY r.created_at DESC))[1], (array_agg(r.web_url ORDER BY r.created_at DESC))[1],
       <tokenSumColumns with u. prefixes>, count(DISTINCT u.run_id)
FROM run_usage u JOIN runs r ON r.id = u.run_id
WHERE u.instance = $1 AND u.project_id = $2 AND u.reported_at >= $3
GROUP BY u.mr_iid ORDER BY COALESCE(sum(u.tokens_in), 0) + COALESCE(sum(u.tokens_out), 0) DESC LIMIT $4
```

`UsageWasted` is two queries. The first, over `run_usage`, sums failed skills and re-runs apart:

```sql
SELECT COALESCE(sum(tokens_in)  FILTER (WHERE verdict = 'failed'), 0),
       COALESCE(sum(tokens_out) FILTER (WHERE verdict = 'failed'), 0),
       COALESCE(sum(cache_read) FILTER (WHERE verdict = 'failed'), 0),
       COALESCE(sum(cache_write) FILTER (WHERE verdict = 'failed'), 0),
       COALESCE(sum(reasoning)  FILTER (WHERE verdict = 'failed'), 0),
       count(*) FILTER (WHERE verdict = 'failed' AND (cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)),
       COALESCE(sum(tokens_in)  FILTER (WHERE trigger <> 'push'), 0),
       COALESCE(sum(tokens_out) FILTER (WHERE trigger <> 'push'), 0),
       COALESCE(sum(cache_read) FILTER (WHERE trigger <> 'push'), 0),
       COALESCE(sum(cache_write) FILTER (WHERE trigger <> 'push'), 0),
       COALESCE(sum(reasoning)  FILTER (WHERE trigger <> 'push'), 0),
       count(*) FILTER (WHERE trigger <> 'push' AND (cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)),
       count(DISTINCT run_id) FILTER (WHERE trigger <> 'push')
FROM run_usage WHERE instance = $1 AND project_id = $2 AND reported_at >= $3
```

The second, over `runs`, counts the reviews whose tokens are unknown:

```sql
SELECT count(*) FILTER (WHERE result IS NULL AND failure_reason <> '' AND state IN ('reported', 'published', 'publish_failed')),
       count(*) FILTER (WHERE state = 'superseded' AND result IS NULL AND dispatched_at IS NOT NULL)
FROM runs WHERE instance = $1 AND project_id = $2 AND created_at >= $3
```

Check `dispatched_at` is nullable in `0001_init.sql`; if it is `NOT NULL` with a zero default, compare against that instead.

```sql
-- UsageCostPerReview(ctx, instance, projectID, since)
WITH per_run AS (
    SELECT run_id, bool_or(incremental) AS incremental, max(mr_iid) AS iid, max(reported_at) AS at,
           sum(tokens_in + tokens_out) AS total
    FROM run_usage
    WHERE instance = $1 AND project_id = $2 AND reported_at >= $3 AND trigger <> 'rerun_failed'
    GROUP BY run_id
), medians AS (
    SELECT incremental, percentile_cont(0.5) WITHIN GROUP (ORDER BY total) AS median FROM per_run GROUP BY incremental
)
SELECT p.run_id::text, p.iid, p.incremental, p.total, p.at, m.median
FROM per_run p JOIN medians m USING (incremental)
ORDER BY p.total DESC
```

Read every row once: take each kind's median from the joined column, and keep a row as an outlier when `total > 3 * median`, at most 20 of them. (One query; no second pass.)

**Step 3:** `go vet ./internal/store/ && go build ./...`. Commit `feat(store): usage queries for the admin views`.

### Task 10: The admin usage API

**Files:**
- Create: `internal/dashboard/api_usage.go`
- Modify: `internal/dashboard/view.go` (new view types), `internal/dashboard/api_read.go` (`repository()`: one more `CutSuffix` branch, next to `/skill-results`), the route registration in the operator or org routes
- Modify: `internal/dashboard/openapi_test.go` (`openAPISchemas`: one entry per new view type)
- Modify: `openapi.yaml`
- Test: `internal/dashboard/api_usage_test.go`

Two routes:
- `GET /api/admin/usage?days=N`, registered with `d.adminRoute`: totals, daily totals over every repository, and the repositories busiest first.
- `GET /api/repositories/{instance}/{rest...}/usage?days=N`: one repository's detail, through `repository()`'s `CutSuffix`, starting with the `se.Admin` check that `repositoryTeams` uses (`api_org.go:709`): a non-admin gets `notFound(w)`, the same 404 as any unknown path.

`days` uses `parseDays(r, 30, 90)` with the message `"days must be a number from 1 to 90"`, the way Overview does.

**Step 1: View types** in `view.go` (reuse the existing `TokensView` for `in`/`out` where it fits; add the rest):

```go
// UsageTokensView is tokens used: input and output, which every total and
// ranking adds up, and the counts reported apart. Unknown counts the
// review-and-skill rows whose pergent didn't report the apart counts.
type UsageTokensView struct {
	In         int64 `json:"in"`
	Out        int64 `json:"out"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
	Unknown    int64 `json:"unknown"`
}

// UsageDayView is one UTC day's usage.
type UsageDayView struct {
	Day    string          `json:"day"` // YYYY-MM-DD
	Tokens UsageTokensView `json:"tokens"`
}

// UsageRepositoryRowView is one repository in GET /api/admin/usage.
// SwitchedOn is false for a repository switched off since, or on an
// instance that was removed: its usage is kept.
type UsageRepositoryRowView struct {
	Instance   string          `json:"instance"`
	Project    string          `json:"project"`
	SwitchedOn bool            `json:"switchedOn"`
	Tokens     UsageTokensView `json:"tokens"`
	Reviews    int64           `json:"reviews"`
}

// UsageOverviewView is GET /api/admin/usage (admins).
type UsageOverviewView struct {
	Days         int                      `json:"days"`
	Tokens       UsageTokensView          `json:"tokens"`
	Reviews      int64                    `json:"reviews"`
	Daily        []UsageDayView           `json:"daily"`
	Repositories []UsageRepositoryRowView `json:"repositories"`
}

// UsageNamedView is one skill or model of a repository's usage.
type UsageNamedView struct {
	Name   string          `json:"name"`
	Tokens UsageTokensView `json:"tokens"`
	Runs   int64           `json:"runs"`
	Failed int64           `json:"failed"`
}

// UsageMRView is one merge request of a repository's usage.
type UsageMRView struct {
	IID     int64           `json:"iid"`
	Title   string          `json:"title"`
	WebURL  string          `json:"webURL"`
	Tokens  UsageTokensView `json:"tokens"`
	Reviews int64           `json:"reviews"`
}

// UsageWastedView is a repository's wasted tokens: those of skills that
// failed. Re-runs are shown apart and aren't waste. NoResult and Superseded
// count reviews whose tokens are unknown.
type UsageWastedView struct {
	Failed     UsageTokensView `json:"failed"`
	Reruns     UsageTokensView `json:"reruns"`
	RerunCount int64           `json:"rerunCount"`
	NoResult   int64           `json:"noResult"`
	Superseded int64           `json:"superseded"`
}

// UsageOutlierView is a review that used more than three times its kind's
// median.
type UsageOutlierView struct {
	RunID       string `json:"runID"`
	IID         int64  `json:"iid"`
	Incremental bool   `json:"incremental"`
	Total       int64  `json:"total"`
	At          string `json:"at"`
}

// UsageRepositoryView is GET /api/repositories/{instance}/{project}/usage
// (admins).
type UsageRepositoryView struct {
	Days              int                `json:"days"`
	Daily             []UsageDayView     `json:"daily"`
	Skills            []UsageNamedView   `json:"skills"`
	Models            []UsageNamedView   `json:"models"`
	MergeRequests     []UsageMRView      `json:"mergeRequests"`
	Wasted            UsageWastedView    `json:"wasted"`
	FullMedian        *float64           `json:"fullMedian"`
	IncrementalMedian *float64           `json:"incrementalMedian"`
	Outliers          []UsageOutlierView `json:"outliers"`
}
```

Slices are always non-nil in the JSON (`make(..., 0)`), as other views do.

**Step 2: Write the failing tests** (CI only) in `api_usage_test.go`, using `newEnv`, `e.operator()`, `e.get`, `decode`:
- An admin gets 200 with repositories busiest first and `switchedOn` true for `acme/widgets`; a repository with usage rows but not in the config is listed with `switchedOn` false.
- A viewer (`e.signInRole(store.RoleViewer)`) gets 404 on both routes, with the same body as an unknown path.
- `days=0`, `days=91` and `days=x` get 400.
- The repository route returns each section, and an outlier is listed.

To decide `switchedOn`, use the same list the Repositories page reads for switched-on repositories (find it in `api_read.go`; it comes from the settings registry), keyed by instance and lower-cased project path.

**Step 3: Implement** `api_usage.go`: parse `days`, call the Task 9 queries, map to views, `writeJSON`. Register `d.adminRoute(mux, "GET /api/admin/usage", d.usageOverview)` with the other admin routes. In `repository()`, add next to the Skill results branch:

```go
	if rest, ok := strings.CutSuffix(rest, "/usage"); ok {
		... resolve repo exactly as the /skill-results branch does, then d.repositoryUsage(w, r, se, repo)
	}
```

`repositoryUsage` starts with `if !se.Admin { notFound(w); return }`, then resolves the project ID with `d.projectID(ctx, se, repo.Instance, repo.Project)` as `repositorySkillResults` does.

**Step 4: OpenAPI.** Add both operations to `openapi.yaml`, modelled on `GET /api/audit` (literal path, `x-pergent-access: admin`) and on the repository Teams sub-route (`x-pergent-pattern: GET /api/repositories/{instance}/{rest...}`, its access value). Add every new view type to `components.schemas` and to `openAPISchemas` in `openapi_test.go`. Run:

`go test ./internal/dashboard/ -run 'TestOpenAPI'`
Expected: PASS (these tests need no database). They fail until every route, sub-route and view type is documented.

**Step 5:** `go vet ./... && go build ./...`. Commit `feat(dashboard): usage API for admins`.

### Task 11: CSV export

**Files:**
- Modify: `internal/dashboard/api_usage.go`, `openapi.yaml`
- Modify: `internal/store/usage.go` (`UsageRows`)
- Test: `internal/dashboard/api_usage_test.go`, plus a database-free test of the cell escaping

Route: `GET /api/admin/usage.csv?days=N&instance=I&project=P`, `adminRoute`. `instance` and `project` are optional and go together (one without the other is a 400). Header `Content-Type: text/csv; charset=utf-8` and `Content-Disposition: attachment; filename="pergent-usage-<days>d.csv"`.

Columns, one row per run and skill, oldest first: `reported_at` (RFC 3339 UTC), `instance`, `repository`, `merge_request`, `trigger`, `incremental`, `model`, `skill`, `verdict`, `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, `reasoning` (empty when unknown), `findings`, `blocking`. No titles, no text a person wrote. Dismissals are not on these rows (the owner's decision): they belong to a change.

**Step 1: Write the failing tests:**

```go
// A cell a spreadsheet would read as a formula is written as text.
func TestCSVCell(t *testing.T) {
	for in, want := range map[string]string{"acme/widgets": "acme/widgets", "=cmd": "'=cmd", "+1": "'+1", "-x": "'-x", "@a": "'@a", "\tx": "'\tx"} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}
```

and, with the database: the header row, one row per seeded run and skill, an empty `cache_read` for a NULL, and 404 for a viewer.

**Step 2: Implement** with `encoding/csv`, writing every string through `csvCell`:

```go
// csvCell keeps a spreadsheet from reading a cell as a formula.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
```

Stream rows from `UsageRows` (a `pgx.Rows` loop, not a slice) so a 90-day export doesn't sit in memory. Cap at 200,000 rows and end with no error row; document the cap in the README.

**Step 3:** `go test ./internal/dashboard/ -run 'TestCSVCell|TestOpenAPI'`, `go build ./...`. Commit `feat(dashboard): export usage as CSV`.

### Task 12: The review page reads the usage table

**Files:**
- Modify: `internal/dashboard/api_read.go` (the run handler around :594, `fillSkills` around :655–666)
- Modify: `internal/store/usage.go` (`RunUsage`)
- Test: `internal/dashboard/api_read_test.go`

**Step 1: Failing test** (CI only): a `rerun_failed` run whose stored, merged result carries a kept skill with `tokens.input: 100` and its own skill with `tokens.input: 5`, with usage recorded for the own skill only. `GET /api/runs/{id}` must show `tokens.in == 5`. Today it shows 105.

**Step 2: Implement**

```go
// RunUsage returns what run id used, summed over its recorded skills; found
// is false when its usage isn't recorded yet.
func (s *Store) RunUsage(ctx context.Context, id string) (in, out int64, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0), bool_or(true)
		FROM run_usage WHERE run_id = $1::uuid`, id).Scan(&in, &out, &found)
	return in, out, found, err
}
```

(`bool_or` over no rows is NULL; scan into a `*bool` and treat NULL as not found.) In the run handler, after `fillSkills`, when `found`, overwrite `view.Tokens` with the table's sums. When not found (a result not yet filled in by the background fill), keep today's sum. The view's JSON doesn't change.

**Step 3:** Commit `fix(dashboard): count a re-run's own tokens only on the review page`.

### Task 13: Skill cost on the Skill results tab, for admins

**Files:**
- Modify: `internal/store/usage.go` (`UsageOnDecidedChanges`), `internal/dashboard/api_skill_results.go`, `view.go`, `openapi.yaml`, `openapi_test.go`
- Test: `internal/dashboard/api_skill_results_test.go`

The tab counts per change once a change is merged or closed. The tokens beside it are those used on the same changes, per skill.

**Step 1: Find the changes the tab counts.** Read `internal/store/fates.go`: `countedFates` (about :219) selects `finding_fates` rows decided in the period. Check whether `fate_jobs` (or another table) lists every merged or closed change, including one with no findings. If it does, take the period's decided changes from there, so a skill that found nothing on a change still has that change's tokens counted. If it doesn't, take them from `finding_fates` and say in the README that a change with no findings isn't counted.

**Step 2: Query**

```sql
WITH decided AS (<the decided changes of the period: instance, project_id, mr_iid>)
SELECT u.skill, COALESCE(sum(u.tokens_in), 0), COALESCE(sum(u.tokens_out), 0)
FROM run_usage u JOIN decided d USING (instance, project_id, mr_iid)
WHERE u.instance = $1 AND u.project_id = $2
GROUP BY u.skill
```

**Step 3: API.** Today the route returns a list of rows (check `SkillResultView` and the handler's `writeJSON`). Don't change that response. Add a sibling sub-route `GET /api/repositories/{instance}/{rest...}/skill-results/tokens?days=30|90`, admin only (`se.Admin`, else `notFound`), returning `[]SkillTokensView{Skill string; Tokens TokensView}` with `TokensView` the existing `{in, out}` type. Mind the `CutSuffix` order in `repository()`: check `/skill-results/tokens` before `/skill-results`. Document it in `openapi.yaml`.

**Step 4: Tests** (CI only): an admin gets each skill's tokens on decided changes only (an open merge request's tokens are left out); a viewer gets 404; `days=7` gets 400.

**Step 5:** `go test ./internal/dashboard/ -run TestOpenAPI`, `go build ./...`. Commit `feat(dashboard): tokens per skill on decided changes, for admins`.

### Task 14: The dashboard pages

**Files:**
- Modify: `web/src/api.ts` (types beside the other view types, with a comment naming route and caller; `paths` entries)
- Create: `web/src/pages/Usage.tsx`, `web/src/pages/UsageRepository.tsx`, `web/src/pages/Usage.test.tsx`
- Modify: `web/src/App.tsx` (two routes under `AdminOnly`), `web/src/layout/Layout.tsx` (`adminItems`), `web/src/pages/AdminHome.tsx` (`areas`)
- Modify: `web/src/pages/SkillResults.tsx`, `web/src/pages/RepositoryDetail.test.tsx` (its `describe("Skill results")`)
- Modify: `web/src/test/fixtures.ts`

Rules the pages follow:
- CSP allows no inline style, so no `style=` attributes. Show a day's share as `<progress value={total} max={peak}>` in a table cell.
- Every number has a `className="num"` cell. Format with the existing number helper if `ui.tsx` has one; otherwise `toLocaleString()`.
- Text from the API (titles, project paths, skill names, model names) is rendered as text only, never as HTML.
- Unknown cache and reasoning counts read "unknown for N reviews", never as zero.

**Step 1: Types and paths** in `api.ts`, matching Task 10's JSON exactly (`UsageTokensView`, `UsageDayView`, `UsageRepositoryRowView`, `UsageOverviewView`, `UsageNamedView`, `UsageMRView`, `UsageWastedView`, `UsageOutlierView`, `UsageRepositoryView`, `SkillTokensView`). Paths:

```ts
  apiUsage: (days: number) => `/api/admin/usage?days=${days}`,
  apiRepoUsage: (instance: string, project: string, days: number) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/usage?days=${days}`,
  apiUsageCSV: (days: number, instance?: string, project?: string) =>
    `/api/admin/usage.csv${query([["days", String(days)], ["instance", instance], ["project", project]])}`,
  apiRepoSkillTokens: (instance: string, project: string, days: 30 | 90) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/skill-results/tokens${days === 30 ? "" : `?days=${days}`}`,
```

**Step 2: Write the failing tests** in `Usage.test.tsx`, in the style of `AuditLog.test.tsx` (`fakeApi`, `renderApp`, fixtures `operator` and `viewer`):
- `/admin/usage` as `operator`: the heading "Usage", repositories in the order the API gives, a "switched off" pill on a switched-off one, and the period select calls `/api/admin/usage?days=7` after choosing "Last 7 days".
- As `viewer`: "Not found", and no call to `/api/admin/usage`.
- The "Download CSV" link is an `<a>` whose `href` is `/api/admin/usage.csv?days=30` (a same-origin GET sends the session cookie; no CSRF token is needed for a GET).
- `/admin/usage/main/acme/payments`: sections Skills, Merge requests, Models, Daily, Wasted, Cost per review; a merge request title `<img src=x onerror=alert(1)>` renders as text, not an `img`.
- A 500 renders the `ErrorBox` alert.
- Unknown cache counts render "unknown for 3 reviews".

Add fixtures `usageOverview` and `usageRepository` to `fixtures.ts`.

**Step 3:** `cd web && npx vitest run src/pages/Usage.test.tsx`. Expect FAIL (no route).

**Step 4: Implement the pages.** `Usage.tsx` mirrors `Overview.tsx`'s period select (`periods = [7, 30, 90]`, default 30), `useQuery({ queryKey: ["usage", days], ... })`, `Loading`/`ErrorBox`, a totals card, a Daily table, and a Repositories table whose rows link to `/admin/usage/<instance>/<project>`. `UsageRepository.tsx` reads `instance` and `params["*"]` the way the repository detail route does, with `queryKey: ["usage", instance, project, days]`, and one table per section; merge request titles link with `ExternalLink` to `webURL`; outliers link to the review page. Both pages carry a line saying days are cut at midnight UTC and that totals add input and output only.

Routes in `App.tsx`, under `AdminOnly` like `admin/audit`: `admin/usage` and `admin/usage/:instance/*`. Add `{ to: "/admin/usage", label: "Usage", icon: "📈" }` to `adminItems` and `["/admin/usage", "Usage", "Where model tokens go."]` to `AdminHome`'s `areas`.

**Step 5: Skill results tab.** In `SkillResults.tsx`, when `useMe().admin`, fetch `apiRepoSkillTokens(instance, project, days)` with `queryKey: ["skill-tokens", instance, project, days]` and render, below the existing table, a section "Skill cost on these changes": skill, tokens in, tokens out, findings fixed (summed over the skill's rows of the existing table), and tokens per fixed finding (`(in + out) / fixed`, a dash when none fixed). A non-admin never fetches it. Update `RepositoryDetail.test.tsx`: the existing cell assertion stays unchanged (the table itself doesn't change); add an admin test for the new section and a viewer test that finds no "Skill cost" heading and no call to the tokens route.

**Step 6:** `cd web && npm run typecheck && npm run lint && npx vitest run`. Expect PASS. Then `make web` from the repo root (CI checks the built `internal/web/dist` is committed: run `git status` and add it).

**Step 7: Commit** `feat(web): admin usage pages and skill cost on Skill results`.

### Task 15: Docs, then open PR 2

**Files:** `README.md`, `openapi.yaml` (already done), and, only after the owner has released PR 1's pergent: `go.mod` and `internal/config/config.go`'s `defaultImage` together.

**Step 1: README.**
- In `## 📊 Dashboard` → `### Two areas`, add Usage (`/admin/usage`) to the admin pages.
- A new `### Usage` subsection after `### Audit log`, in the same voice: what it shows; admins only; input plus output in every total; cache and reasoning apart and "unknown" for reviews by an older pergent; days in UTC; wasted means failed skills, with re-runs, no-result and superseded reviews shown apart; full and incremental medians apart and the 3x rule; switched-off repositories keep their usage; the CSV and its columns and row cap; usage is never pruned.
- `## 📈 Skill results`: one paragraph on the admin-only "Skill cost on these changes" section, including Task 13's finding about changes with no findings.
- `## 🌐 HTTP routes`: the three new routes.
- `## ⬆️ Upgrading`: a new `### Upgrading to the next release` above the newest section: migration `0032` adds a table and a column, dropping nothing, the previous release keeps working; usage of earlier reviews is filled in in the background after the upgrade, and the Usage page fills up as it goes; cache and reasoning counts need the pergent release that reports them.

**Step 2: Pin bump (only when the owner says PR 1's pergent is released).** Move `go.mod`'s `github.com/gopaytech/pergent` and `defaultImage` to that version in one commit, run `go mod tidy`, and add a bullet to the upgrading section.

**Step 3: Checks.** `gofmt -l . ; go vet ./... ; go test ./... ; (cd web && npm run typecheck && npm run lint && npx vitest run) ; helm lint charts/pergent-sh`, and the name scan. Say in the PR which tests skip locally (store, dashboard, engine, and so all database-backed usage tests).

**Step 4:** Commit `docs: usage for admins`; open PR 2 only when the owner asks.

---

## PR 3: pergent.sh metrics

Branch `feat/usage-metrics` from `main` after PR 2 merged.

### Task 16: `metrics_listen` in the config

**Files:**
- Modify: `internal/config/config.go` (`Config`, `fileConfig` (strict decoding: both), the copy at about :468, validation)
- Test: `internal/config/config_test.go`

Rules: empty means off (the default). Otherwise it must parse with `net.SplitHostPort`, have a port, and differ from `listen`. Tests: empty is accepted and off; `":9090"` is accepted; `"9090"`, `":8080"` (same as `listen`) and `"x:y"` are refused with a message naming `metrics_listen`.

Commit `feat(config): metrics_listen`.

### Task 17: The metrics text

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go`
- Modify: `internal/store/usage.go` (`MetricsTotals`)

Everything is computed from the database at each scrape, so counters survive restarts and include earlier reviews (the owner's decision). Written by hand in the Prometheus text format, version 0.0.4; no new dependency.

Metrics, all counters:

```
pergent_tokens_total{instance,repository,skill,model,kind}        kind: input|output|reasoning|cache_read|cache_write
pergent_reviews_total{instance,repository,trigger,incremental}
pergent_reviews_no_result_total{instance,repository}
pergent_reviews_superseded_total{instance,repository}
pergent_skill_runs_total{instance,repository,skill,verdict}
pergent_findings_total{instance,repository,skill,severity}       severity: blocking|suggestion
pergent_dismissals_total{instance,repository,skill}
```

Labels are bounded: `repository` is the project path only for a repository switched on now, otherwise `"(switched off)"`; `skill` is the skill name only for a skill in the library or a repository's own skill list, otherwise `"(other)"`. No merge request numbers, no titles, no text a person wrote. Dismissals come from the `dismissals` table, which loses a repository's rows when it is switched off: say in the README that this counter can drop, which Prometheus reads as a reset.

**Step 1: Failing tests** (no database): a `Write(w io.Writer, s Snapshot)` that prints `# HELP` and `# TYPE ... counter` once per metric, sorts series, and escapes label values (`\` → `\\`, `"` → `\"`, newline → `\n`):

```go
func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Errorf("escapeLabel = %q", got)
	}
}
```

and a golden-style test of a small `Snapshot` against an exact expected string.

**Step 2: Implement** `Snapshot` (plain slices of label sets and values), `Write`, and a `Handler(source func(ctx) (Snapshot, error), log)` that answers `GET /metrics` with `Content-Type: text/plain; version=0.0.4; charset=utf-8`, a 10-second query timeout, and a 500 with no body detail on error (logged).

**Step 3: Store:** `MetricsTotals(ctx)` runs one grouped query per metric over `run_usage`, `runs` and `dismissals`; mapping repository and skill names to the bounded labels happens in `cmd/pergent-sh`, which knows the switched-on repositories and the skill library (use the same sources the dashboard uses).

Commit `feat(metrics): usage counters in the Prometheus text format`.

### Task 18: Serve metrics on their own port

**Files:**
- Modify: `cmd/pergent-sh/main.go`

When `cfg.MetricsListen != ""`: `net.Listen` it before serving (fail start-up with a clear error if the port is taken), build an `http.Server` with a mux holding only `GET /metrics`, and add a loop to `loops` that serves it and shuts it down when ctx ends (the shape of `serveUntilDone`, with its own 20-second shutdown). Nothing else is ever served on that port, and `/metrics` is never added to the main handler: the `public_url` routes must not reach it.

Test: a `cmd/pergent-sh` test that starts the metrics server on `127.0.0.1:0` with a fake source and gets 200 with the exposition, and that `/healthz` and `/api/` on that port are 404.

Commit `feat: serve metrics on metrics_listen`.

### Task 19: Chart

**Files:**
- Modify: `charts/pergent-sh/values.yaml`, `templates/_helpers.tpl` (`pergent-sh.config`, `pergent-sh.validate`), `templates/deployment.yaml` (ports)
- Create: `templates/metrics-service.yaml`, `templates/servicemonitor.yaml`
- Modify: `charts/pergent-sh/ci-values.yaml`, `.github/workflows/ci.yml` (chart greps), then `make readme`

**values.yaml**, after `service:`:

```yaml
# Prometheus metrics: token, review and finding counters read from the database at each scrape. Off by default. They are served on their own port, through their own ClusterIP Service, never through the main Service, which may be a LoadBalancer.
metrics:
  # -- Serve /metrics on metrics.port.
  enabled: false
  # -- Port of the metrics server and its Service.
  port: 9090
  serviceMonitor:
    # -- Create a ServiceMonitor (needs the Prometheus Operator's CRDs).
    enabled: false
    # -- Scrape interval.
    interval: 60s
    # -- Extra labels, for a Prometheus that selects ServiceMonitors by label.
    labels: {}
```

**`pergent-sh.config`**: when `.Values.metrics.enabled`, `set $config "metrics_listen" (printf ":%d" (int .Values.metrics.port))`.

**`pergent-sh.validate`**: `fail` when `.Values.config.metrics_listen` is set ("metrics_listen is set by the chart: use metrics.enabled and metrics.port"), and when `metrics.port` is 8080.

**deployment.yaml** ports, under the `http` port:

```yaml
            {{- if .Values.metrics.enabled }}
            - name: metrics
              containerPort: {{ .Values.metrics.port }}
              protocol: TCP
            {{- end }}
```

**metrics-service.yaml**, whole file under `{{- if .Values.metrics.enabled }}`: `name: {{ include "pergent-sh.fullname" . }}-metrics`, the usual labels plus `app.kubernetes.io/component: metrics`, `type: ClusterIP` (fixed, never from values), one port `metrics` → `targetPort: metrics`, the usual selector.

**servicemonitor.yaml**, under `{{- if and .Values.metrics.enabled .Values.metrics.serviceMonitor.enabled }}`: `apiVersion: monitoring.coreos.com/v1`, selecting `app.kubernetes.io/component: metrics` and the selector labels, endpoint `port: metrics`, `path: /metrics`, `interval: {{ .Values.metrics.serviceMonitor.interval }}`, labels merged from `.Values.metrics.serviceMonitor.labels`.

**CI** (`ci.yml`, after the existing greps):
- default render has no `-metrics` Service and no `metrics_listen`;
- `--set metrics.enabled=true` renders `metrics_listen: :9090`, a `ClusterIP` Service named `t-pergent-sh-metrics`, and a `containerPort: 9090`;
- `--set metrics.enabled=true --set service.type=LoadBalancer` still renders the metrics Service as `ClusterIP`;
- `--set config.metrics_listen=:9090` fails.

Run the same `helm lint` and `helm template ... | grep` commands locally, then `make readme`. Commit `feat(chart): optional metrics port, Service and ServiceMonitor`.

### Task 20: Docs

`README.md`: the Config reference row for `metrics_listen`; a `### Metrics` subsection (what each metric counts, the bounded labels, "(switched off)" and "(other)", the dismissals counter that can drop, how to scrape it inside the cluster, never through `public_url`); `## 🌐 HTTP routes` (the metrics port's one route); the upgrading section (off by default; nothing to do). Commit `docs: metrics`.

### Task 21: Checks, then open PR 3

`gofmt -l . ; go vet ./... ; go test ./... ; helm lint charts/pergent-sh ; helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml`, the chart greps, the name scan. Open the PR only when the owner asks; say which tests skip locally.
