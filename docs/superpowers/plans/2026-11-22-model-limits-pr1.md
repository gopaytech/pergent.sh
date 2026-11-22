# Model limits, PR 1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Admins cap the monthly tokens of one global model (for everyone, or per team), team maintainers cap their own team models, and every review, request and API review skips or holds on a used-up limit, through the API alone.

**Architecture:** A new `model_limits` table holds the limits, and the usage rows get the owner team they had when they were written. `quota.ReadLimits` turns the limits and this month's usage per entry into two sets (model limit used up, team limit used up), which `chain.LoadSet` puts on `chain.Input`, so every caller of the chain (dispatch, panel members, chain B, requests by hand, API reviews) skips those entries with no new call site. The resolver holds a review when only quotas and limits stop it, an admin's start goes past the admin limits, and the dashboard gets the routes, the audit entries and the list fields.

**Tech Stack:** Go 1.24, Postgres through pgx (tests on the existing `pergent-sh-pg` container, Postgres 16 in CI, 13 supported), `net/http` ServeMux, `openapi.yaml` checked by `internal/dashboard/openapi_test.go`.

---

**Spec:** `docs/superpowers/specs/2026-11-22-model-limits-design.md`. Read it in full with this plan. Its "Rollout" section is the scope: this is PR 1. PR 2 (the Models page, the team card, the chain tags and the **Waiting for a start** text in `web/`) is not in this plan.

## Spec notes (the code and the spec differ; the plan follows the code)

| # | Spec says | Code says | This plan |
|---|---|---|---|
| 1 | Migration `0050` | The built-in rules branch (`feat/choose-built-in-rules`, not merged yet) already has `0050_built_in_rule_names.sql` | `0051_model_limits.sql`. `Migrate` applies by version and skips done ones, so the 2 branches can merge in either order. If that branch is dropped first, rename the file to `0050` and move the test's `newStoreAt(t, 50)` / `applyUpTo(t, s, 51)` down by one |
| 2 | Unique on (`source`, `entry_id`, `team_id`) with NULLS NOT DISTINCT | `NULLS NOT DISTINCT` needs Postgres 15; README and `docs/setup/install.md` support 13 | `CREATE UNIQUE INDEX ... (source, entry_id, COALESCE(team_id, 0))`, the shape `model_chains_key` already uses. Team IDs are a bigserial from 1, so 0 is never a team. Upserts use `ON CONFLICT (source, entry_id, (COALESCE(team_id, 0)))` (checked on Postgres 16) |
| 3 | "A model limit sums every row of the entry" | `run_usage` and `run_usage_models` have no entry ID column. The entry is on `runs.model_entry_id`, `panel_members.model_entry_id` and `panel_judges.model_entry_id`. A panel's split run's `run_usage` rows hold the total of all 3 Jobs | The sum joins those tables. A run with `run_usage_models` rows counts from them only, each on its own entry. The migration also adds an index on `run_usage_models (run_id)` for that test. A Job P result with no per-model split counts on no entry (a gap, written in the store comment) |
| 4 | `PUT, DELETE /api/teams/{id}/models/{modelId}/limit` | The team routes name the entry `{model}` (`api_team_models.go:26-29`, openapi `TeamModelId`) | `{model}` |
| 5 | "a maintainer writing an admin limit is 403" | Every admin route answers a non-admin 404, never 403 (`CLAUDE.md`, `Dashboard.adminRoute`, `routes_test.go`) | The 403 is on the team route: `PUT .../models/{model}/limit` with a global entry's ID is 403 `only admins set a team's limit on a global model`. The admin routes stay 404 for everyone else |
| 6 | The **Waiting for a start** line is decided server-side when a limit took part | The line is drawn in `web/src/pages/RepositoryDetail.tsx:167` from `WaitingView.reason`; the API only passes `runs.skip_reason` through | PR 1 adds the server signal: a new wait reason `limit_used_up` (`store.WaitLimit`), written by `Store.HoldForLimit` when a limit took part. PR 2 draws the text for it and shows its **Start review** to admins only. Until PR 2, a maintainer sees **Start review** on such a change and gets the 409 |
| 7 | Only "an admin's start" | `pastQuota` covers an admin's **Re-run** too (`api_commands.go:246`, `:300`), and `actions.Rerun` and `actions.Start` share `quotaUsedUp` | One function, so an admin's **Re-run** follows the same rule and can get the same 409 |
| 8 | (not said) | At dispatch an admin's start and any request by hand look the same (`quota_checked` / `trigger = 'start'`, `dispatch.go:439`). Today such a run goes past the instance quota | Such a run goes past the admin limits too (the instance quota's rule today). Member A of a panel, which passed the hold with its run (`panel.go:207`), does the same |

## Decided questions (the owner accepted all 4 defaults on 2026-11-22)

1. **A run asked for (or member A) whose entries left are stopped only by a team's own limit at dispatch.** It can't be held: `HoldForQuota` refuses a checked run or a start (`store/quota.go:21-23`), so today's code would postpone it forever. Default: it fails with `all models exhausted: team-a (team limit used up)` (Task 10). The ask-time check (Task 13) refuses it first in the normal case; this is the race.
2. **Fallback hold rule.** The spec: held "when only quotas and limits stop every entry left", failed "when a mark or a bad key is part of it", example `main (out of quota), backup (team limit used up)`. Today `tried` blocks a hold (`chain.go:108-115`). Default: `tried` is neutral. A review is held when every skip is a quota, a limit or `tried`, and at least one is a quota or a limit. In a fallback the entry fallen back from shows its live mark, so it fails as the example says; once that mark has ended (it reads `tried`) the review is held. This also changes today's instance quota in that one corner (an ended mark plus a used-up instance quota now holds instead of failing). Task 7 and Task 9 test it.
3. **The signal for the Waiting text** (spec note 6). Default: a new wait reason `limit_used_up`. The other way would be a new `runs` column, which the spec's data section does not list.
4. **An admin's Re-run** gets the same 409 as an admin's start (spec note 7).

## Global constraints

- Branch: keep working on `docs/model-limits-spec` in the model-limits worktree, or branch `feat/model-limits` off it. Never merge, push or open a PR without the owner's word.
- **Never start a container.** Database tests run only when `nc -z localhost 55432` succeeds. If it fails, say so and leave the DB tests to CI.
- `DBTEST` in this plan means, typed in full in the same command:
  `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m`
  Without the variable the store, chain, actions, quota (DB part), dashboard and engine tests skip and print a fast `ok` that proves nothing. Another session may use the same DB: if engine hits the timeout with no `--- FAIL`, rerun that package alone with `-timeout 60m`.
- Each new test must be seen to FAIL, for the reason the step says, before its code. A build failure counts only when it is the missing name the step names (check `grep -E -- '--- FAIL|build failed|undefined'`).
- `internal/engine/testdata/dispatch-job.golden.json` must not change: no new `store.Run` field and no new Job input. `TestDispatchJobUnchanged` runs in the final gate.
- The migration only adds. The forge texts (`quotaDescription`, `quotaNote`, `whyQuota`) do not change and never name a limit or a number.
- No key, base URL or secret in an API answer, a log line or an audit entry. Audit details name the entry, the team and the number only.
- Admin routes go through `Dashboard.adminRoute` and carry `x-pergent-access: admin`. Team routes are session routes and check in the handler (`teamFor`).
- No release: no version bump, tag, chart version or `defaultImage` move. Upgrade notes go under `## Unreleased` in `docs/upgrading.md`.
- Before every commit, from the repository root, run the private-name scan the controller's brief gives (a `git grep` for internal names over the tree, plans excluded) and expect no output; run `git diff --name-only origin/main | xargs grep -nE '@gmail\.com|/Users/|/private/tmp/'` and expect no output; and run the same `grep`s over each new untracked file, since `git grep` misses them. The repository is public.
- Commit as the repository's own address: `git -c user.email="$(git config --local user.email)" commit ...`. Conventional subject. Last line `Co-Authored-By: Claude <model that wrote it> <noreply@anthropic.com>`. Run tests and the commit as separate commands.
- `gofmt -l .` prints nothing before each commit. `go build ./cmd/...` writes a binary into the cwd: use `go vet ./...` instead.

## Existing code each task leans on

| What | Where | Note |
|---|---|---|
| The owner team resolver | `Store.OwnerTeam`, `internal/store/teams.go:289-303` | The repository's own `team_repos` row with `owner`, else the instance's (`project = ''`), case-insensitive. Task 2 turns its SQL into `ownerTeamSQL` so the usage writes reuse it. No second resolver |
| Instance quota today | `quota.Check` (`internal/quota/quota.go:59-80`), called in `dispatch.go:440` and `actions.quotaUsedUp` (`actions.go:178-198`) | The instance quota is a flag on `chain.Input` (`InstanceQuotaUsedUp`); the resolver skips global entries for it |
| How marks and reasons skip entries | `chain.skipReason`, `internal/chain/chain.go:123-146` | Order: removed, not allowed, not owner, same model, live mark (only for chains of 2 or more), tried, no key, instance quota. Limits go last |
| Hold vs fail | `chain.Resolve`, `chain.go:107-116` | Today: hold only when every skip is `instance_quota` |
| Where the chain is read | `chain.LoadSet`, `internal/chain/load.go:32-90`; callers in `engine/chain.go`, `engine/panel.go`, `actions/actions.go`, `dashboard/api_chains.go:510` | Task 8 reads the limits there, once, for all of them |
| The hold | `dispatcher.holdForQuota`, `internal/engine/quota.go:30-54`; `Store.HoldForQuota`, `internal/store/quota.go:17-28` | Same status, same once-a-month note |
| Usage writes | `RecordUsage` (`store/usage.go:42-51`), `usageInsertSQL` (`:74-87`), the merge insert (`:123-132`), the member insert (`:199-207`) | Each `INSERT ... SELECT ... FROM runs r` gets `owner_team_id` |
| Requests by hand | `actions.Rerun` / `actions.Start` (`actions.go:112-247`), callers `dashboard/api_commands.go:244-263`, `:298-312`, `engine/commands.go:622-641`, `:696-710` | A status not handled falls through to "queued": every caller must handle a new status |
| Team route checks | `Dashboard.teamFor(w, r, need)`, `api_team_models.go:65-84` | Non-member 404; viewer or other team's maintainer 403 on a write |
| Audit | `Dashboard.audit(ctx, se, action, instance, project, detail)`, `api_org.go:67`; actor `actorLabel` | Instance and project `""` for these entries |
| Quota numbers in JSON | `quotaValue(raw)`, `api_quota.go:31-46`; `config.MaxMonthlyTokenQuota` | Reads the JSON text, refuses fractions, exponents, strings, 0 and negatives |
| Store test helpers | `newStore`, `newStoreAt`, `applyUpTo`, `schemaColumns`, `schemaIndexes`, `dispatched`, `reported`, `usageSeq`, `newTeam`, `panelMembersWithEntries`, `doneMember`, `mr1`, `t0`, `hash` | `internal/store/*_test.go` |
| Engine test helpers | `newTestEnv`, `newE2E`, `quotaPushEnv`, `servePush`, `wantHeld`, `seedUsageOf`, `teamOwning`, `addTeamModel`, `globalRef`, `saveChain`, `dispatchMR`, `wantJobKey`, `newFallbackE2E`, `e.attempt`, `failedOn`, `withTokens`, `clean`, `wantFellBack`, `newAPIE2E`, `a.ask`, `a.wantFailed`, `globalChainB`, `wantOnlyA`, `newPanelE2E`, `savePanelChain`, `splitPanel`, `releasedPanel`, `panelJudges`, `memberOfRole`, `wantMemberKey`, `ownerTeam`, `quotaCommands`, `publishedHead`, `wantOutcome`, `novemberMid` | `internal/engine/*_test.go` |
| Dashboard test helpers | `newEnv`, `newTeamsEnv` (Payments owns `main/acme/payments`, Widgets owns `main/acme/widgets`), `te.addTeamModel`, `e.addModel`, `e.operator()`, `e.send`, `e.get`, `decode[T]`, `wantCode`, `errorOf`, `auditDetails`, `e.usageRun`, `runFix`, `usageSkill`, `e.exec`, `e.waitingChange`, `buttonURL`, `head`, `modelNamed`, `teamURL` | `internal/dashboard/*_test.go` |

---

## Task 1: Migration `0051_model_limits.sql`

**Files:**
- Create: `internal/store/migrations/0051_model_limits.sql`
- Create: `internal/store/limits_test.go`

**Step 1: Write the failing tests**

`internal/store/limits_test.go`:

```go
package store

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Migration 0051 only adds: every column and index of 0050 is still there,
// unchanged, beside the new table and columns.
func TestModelLimitsMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0051_model_limits.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	// A foreign key's ON DELETE is not a delete.
	text := regexp.MustCompile(`(?i)ON\s+DELETE\s+CASCADE`).ReplaceAllString(sql.String(), "")
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`, `(?i)\bUPDATE\b`,
		`(?i)\bALTER\s+COLUMN\b`, `(?i)\bSET\s+NOT\s+NULL\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0051 matches %s; it may only add", bad)
		}
	}
	for _, stmt := range strings.Split(text, ";") {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") && strings.Contains(up, "NOT NULL") && !strings.Contains(up, "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}

	s := newStoreAt(t, 50)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	applyUpTo(t, s, 51)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0051 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0051 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"model_limits.source", "model_limits.entry_id", "model_limits.team_id", "model_limits.monthly_tokens",
		"model_limits.updated_at", "model_limits.updated_by", "run_usage.owner_team_id", "run_usage_models.owner_team_id"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0051", col)
		}
	}
	for _, col := range []string{"run_usage.owner_team_id", "run_usage_models.owner_team_id"} {
		if after[col].Nullable != "YES" {
			t.Errorf("%s = %+v; want nullable: the previous release writes none", col, after[col])
		}
	}
}

// One limit per entry and team, the model limit (no team) included; a team
// entry has no model limit; the number is in the quotas' range.
func TestModelLimitsKey(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	ins := func(source string, teamID any, n int64) error {
		_, err := s.pool.Exec(ctx, `INSERT INTO model_limits (source, entry_id, team_id, monthly_tokens, updated_at, updated_by)
			VALUES ($1, 'e1', $2, $3, now(), 'test')`, source, teamID, n)
		return err
	}
	if err := ins("global", nil, 10); err != nil {
		t.Fatalf("model limit: %v", err)
	}
	if err := ins("global", team, 10); err != nil {
		t.Fatalf("team limit: %v", err)
	}
	wantCode := func(what string, err error, code string) {
		t.Helper()
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("%s = %v; want SQLSTATE %s", what, err, code)
		}
	}
	wantCode("a second model limit", ins("global", nil, 20), "23505")
	wantCode("a second team limit", ins("global", team, 20), "23505")
	wantCode("a team entry's model limit", ins("team", nil, 10), "23514")
	wantCode("0 tokens", ins("team", team, 0), "23514")
	wantCode("over the maximum", ins("team", team, 1_000_000_000_000_001), "23514")
}

// The previous release writes usage rows with no owner team: they still go
// in, with none.
func TestModelLimitsMigrationPreviousReleaseInsert(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "h1", `{"schema_version":1,"status":"completed","skills":[]}`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid, trigger,
			incremental, provider, model, verdict, reported_at, tokens_in, tokens_out, findings, blocking, source, team_id)
		VALUES ($1::uuid, 'security', $2, 7, 'acme/widgets', 3, 'push', false, '', 'gpt', 'no_blocking_issues', now(),
			1, 1, 0, 0, 'global', NULL)`, id, mr1.Instance); err != nil {
		t.Fatalf("the previous release's insert: %v", err)
	}
	var owner *int64
	if err := s.pool.QueryRow(ctx, `SELECT owner_team_id FROM run_usage WHERE run_id = $1::uuid`, id).Scan(&owner); err != nil || owner != nil {
		t.Errorf("owner_team_id = %v, %v; want NULL", owner, err)
	}
}
```

**Step 2: Run them and watch them fail**

```bash
nc -z localhost 55432 && DBTEST ./internal/store/ -run 'TestModelLimits'
```

Expected: FAIL. `TestModelLimitsMigrationOnlyAdds` with `open migrations/0051_model_limits.sql: no such file or directory`; the other two with `relation "model_limits" does not exist` / `column "owner_team_id" does not exist`.

**Step 3: Write the migration**

`internal/store/migrations/0051_model_limits.sql`:

```sql
-- Model limits. It only adds, so the previous release still runs on the
-- migrated database and ignores all of it.
--
-- model_limits: a monthly token limit on a chain entry, named as chain
-- entries name it (source and entry_id, no foreign key: a removed entry
-- leaves its rows unused). team_id NULL is the model limit: every token on a
-- global entry, on every instance and repository. A team ID is that team's
-- limit on the entry: the tokens of the repositories it owns. A team entry
-- has no model limit (its team's limit covers every use of it), so only a
-- global row may have no team. Removing a team removes its rows.
-- One row per entry and team: NULLS NOT DISTINCT needs Postgres 15 and
-- pergent.sh supports 13, so the key is on COALESCE(team_id, 0), as
-- model_chains_key is (team IDs start at 1).
--
-- run_usage.owner_team_id and run_usage_models.owner_team_id: the owner team
-- of the run's repository when the row was written, NULL for none. A team
-- limit counts these, so a repository that moves to another team leaves its
-- earlier tokens with the old one. The previous release writes NULL.
--
-- run_usage_models (run_id): the limit sums leave out the run_usage rows of a
-- run that has rows per model.
CREATE TABLE model_limits (
    source         text        NOT NULL CHECK (source IN ('global', 'team')),
    entry_id       text        NOT NULL,
    team_id        bigint      REFERENCES teams (id) ON DELETE CASCADE,
    monthly_tokens bigint      NOT NULL CHECK (monthly_tokens BETWEEN 1 AND 1000000000000000),
    updated_at     timestamptz NOT NULL,
    updated_by     text        NOT NULL,
    CHECK (source = 'global' OR team_id IS NOT NULL)
);
CREATE UNIQUE INDEX model_limits_key ON model_limits (source, entry_id, COALESCE(team_id, 0));

ALTER TABLE run_usage ADD COLUMN owner_team_id bigint;
ALTER TABLE run_usage_models ADD COLUMN owner_team_id bigint;
CREATE INDEX run_usage_models_run_idx ON run_usage_models (run_id);
```

**Step 4: Run them and watch them pass, then the whole store package**

```bash
DBTEST ./internal/store/ -run 'TestModelLimits'
DBTEST ./internal/store/
```

Expected: `ok  github.com/gopaytech/pergent.sh/internal/store` both times.

**Step 5: Commit**

```bash
git add internal/store/migrations/0051_model_limits.sql internal/store/limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(store): model_limits table and owner team on usage rows" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 2: Usage rows record the owner team

**Files:**
- Modify: `internal/store/teams.go:289-303` (`OwnerTeam` reads a shared SQL fragment)
- Modify: `internal/store/usage.go:42-51`, `:74-87`, `:123-132`, `:199-207` (4 inserts)
- Test: `internal/store/limits_test.go`

**Step 1: Write the failing tests** (append to `limits_test.go`; add `"time"` to its imports)

```go
// setOwner makes team cover (instance, project) alone, with the owner tick.
func setOwner(t *testing.T, s *Store, team int64, instance, project string) {
	t.Helper()
	ok, err := s.UpdateTeam(ctx, team, func(tm *Team) error {
		tm.Repos = []TeamRepo{{Instance: instance, Project: project, Owner: true}}
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
}

// dropRepos takes every repository off team.
func dropRepos(t *testing.T, s *Store, team int64) {
	t.Helper()
	if ok, err := s.UpdateTeam(ctx, team, func(tm *Team) error { tm.Repos = nil; return nil }); err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
}

// ownerOf is the owner_team_id of run id's run_usage rows (nil: none). Every
// row of a run has the same.
func ownerOf(t *testing.T, s *Store, id string) *int64 {
	t.Helper()
	var owner *int64
	if err := s.pool.QueryRow(ctx, `SELECT owner_team_id FROM run_usage WHERE run_id = $1::uuid LIMIT 1`, id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

func wantOwner(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if (want == 0) != (got == nil) || (got != nil && *got != want) {
		t.Errorf("%s: owner_team_id = %v, want %d (0: none)", what, got, want)
	}
}

// A usage row keeps the owner team of its repository when it is written: the
// repository's own owner, else its instance's, else none. A repository that
// moves to another team leaves its earlier rows with the old one.
func TestUsageRecordsOwnerTeam(t *testing.T) {
	s := newStore(t)
	row := skillRow("security", "no_blocking_issues", 1, 1)
	none := seedUsage(t, s, usageMR(7, 1), t0, TriggerPush, false, "gpt", row)

	platform, payments, moved := newTeam(t, s, "Platform"), newTeam(t, s, "Payments"), newTeam(t, s, "Moved")
	setOwner(t, s, platform, mr1.Instance, "")
	byInstance := seedUsage(t, s, usageMR(7, 2), t0, TriggerPush, false, "gpt", row)
	setOwner(t, s, payments, mr1.Instance, "ACME/p7") // seedUsage writes project_path acme/p7
	byRepo := seedUsage(t, s, usageMR(7, 3), t0, TriggerPush, false, "gpt", row)
	dropRepos(t, s, payments)
	setOwner(t, s, moved, mr1.Instance, "acme/p7")
	after := seedUsage(t, s, usageMR(7, 4), t0, TriggerPush, false, "gpt", row)

	wantOwner(t, "no owner", ownerOf(t, s, none), 0)
	wantOwner(t, "the instance's owner", ownerOf(t, s, byInstance), platform)
	wantOwner(t, "the repository's own owner, ignoring case", ownerOf(t, s, byRepo), payments)
	wantOwner(t, "after the move", ownerOf(t, s, after), moved)
	// The earlier row stays with the team it had.
	wantOwner(t, "before the move, read again", ownerOf(t, s, byRepo), payments)
}

// judgedPanel is a panel review of head on mr1 taken to the end: members A
// (global m1) and B (team t1 of team 4) report their usage, the run is
// released, and Job P, judging with both, reports its own with a row per
// model. It returns the run.
func judgedPanel(t *testing.T, s *Store, head string) string {
	t.Helper()
	id, a, b := panelMembersWithEntries(t, s, head)
	doneMember(t, s, a.ID, `{"status":"completed"}`)
	doneMember(t, s, b.ID, `{"status":"completed"}`)
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 100, Out: 10}, {Skill: "style", In: 7, Out: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMemberUsage(ctx, b.ID, []SkillUsage{{Skill: "security", In: 50, Out: 5}}); err != nil {
		t.Fatal(err)
	}
	if released, _, err := s.ReleasePanel(ctx, id, t0); err != nil || !released {
		t.Fatalf("ReleasePanel = %v, %v", released, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "a", RunEntry{Source: SourceGlobal, ID: "m1", Name: "main", Length: 1}, "openai", "gpt-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "b", RunEntry{Source: SourceTeam, ID: "t1", Name: "team-b", TeamID: 4, Length: 1}, "anthropic", "sonnet-b"); err != nil {
		t.Fatal(err)
	}
	token := "p-" + head
	if err := s.MarkDispatched(ctx, id, "job-p-"+head, "base-sha", "start-sha", hash(token), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, token, []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "has_blocking_issues", In: 20, Out: 2}},
		[]ModelUsage{
			{Skill: "security", Role: "a", Provider: "openai", Model: "gpt-a", In: 12, Out: 1},
			{Skill: "security", Role: "b", Provider: "anthropic", Model: "sonnet-b", In: 8, Out: 1},
		}); err != nil {
		t.Fatal(err)
	}
	return id
}

// A panel's rows, members and Job P's, keep the owner team too.
func TestPanelUsageRecordsOwnerTeam(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	setOwner(t, s, team, mr1.Instance, "acme/widgets")
	id := judgedPanel(t, s, "a1")
	wantOwner(t, "run_usage", ownerOf(t, s, id), team)
	rows, err := s.pool.Query(ctx, `SELECT part, owner_team_id FROM run_usage_models WHERE run_id = $1::uuid`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var part string
		var owner *int64
		if err := rows.Scan(&part, &owner); err != nil {
			t.Fatal(err)
		}
		wantOwner(t, part+" row", owner, team)
		n++
	}
	if n != 5 { // A: 2 skills, B: 1, P: 2 roles
		t.Errorf("%d model rows, want 5", n)
	}
}
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/store/ -run 'TestUsageRecordsOwnerTeam|TestPanelUsageRecordsOwnerTeam'
```

Expected: FAIL with `owner_team_id = <nil>, want <team>` on every owned case (nothing writes the column yet).

**Step 3: Write the code**

`internal/store/teams.go`, replace `OwnerTeam` (lines 289-303) with:

```go
// ownerTeamSQL is the team that owns the repository whose instance and path
// the SQL expressions instance and project give, NULL for none: the owner of
// its own entry (matched ignoring case), else of the instance entry.
// OwnerTeam and the usage writes read it, so the 2 never disagree.
func ownerTeamSQL(instance, project string) string {
	return `(SELECT o.team_id FROM team_repos o WHERE o.owner AND o.instance = ` + instance +
		` AND (lower(o.project) = lower(` + project + `) OR o.project = '') ORDER BY o.project = '' LIMIT 1)`
}

// OwnerTeam returns the team that owns project on instance (ownerTeamSQL).
// ok is false when neither its own entry nor the instance entry has an owner.
func (s *Store) OwnerTeam(ctx context.Context, instance, project string) (teamID int64, ok bool, err error) {
	var id *int64
	if err := s.pool.QueryRow(ctx, `SELECT `+ownerTeamSQL("$1::text", "$2::text"), instance, project).Scan(&id); err != nil {
		return 0, false, err
	}
	if id == nil {
		return 0, false, nil
	}
	return *id, true, nil
}
```

(`errors` and `pgx` stay imported in `teams.go`: other functions use them. If `go vet` says otherwise, drop the unused import.)

`internal/store/usage.go`: in each of the 4 inserts add the column `owner_team_id` last in the column list and `ownerTeamSQL("r.instance", "r.project_path")` last in the `SELECT`:

1. `RecordUsage` (lines 42-51): column list ends `reasoning, findings, blocking, source, team_id, owner_team_id)`; the select ends with
   ```go
   CASE WHEN r.model_source = 'team' THEN r.model_team_id END, `+ownerTeamSQL("r.instance", "r.project_path")+`
   FROM runs r WHERE r.id = $1::uuid
   ```
2. `usageInsertSQL` (lines 74-87): the same 2 edits. The `ON CONFLICT ... DO UPDATE` stays: a row keeps the owner it was first written with.
3. The merge insert in `recordMergeUsage` (lines 123-132): column list ends `reasoning, owner_team_id)`, select ends `$11::bigint, ` + `ownerTeamSQL("r.instance", "r.project_path")`. `r` is already `runs r`.
4. The member insert in `RecordMemberUsage` (lines 199-207): it selects `FROM panel_members m WHERE m.id = $1::uuid`. Make it `FROM panel_members m JOIN runs r ON r.id = $9::uuid WHERE m.id = $1::uuid`, column list ends `reasoning, owner_team_id)`, select ends `$8::bigint, ` + `ownerTeamSQL("r.instance", "r.project_path")`. `$9` is already the target run.

Update the doc comment of `RecordUsage` and `usageInsertSQL`: "... and whose key paid (the run's entry: global, or a team and its ID), and the owner team of its repository now (`ownerTeamSQL`), which a team limit counts by."

An API review's run has instance `''`, which no `team_repos` row has, so it gets no owner team.

**Step 4: Run and watch them pass, then the store package**

```bash
DBTEST ./internal/store/ -run 'OwnerTeam|Owner|Usage'
DBTEST ./internal/store/
```

Expected: `ok`. `TestOwnerMostSpecific` and `TestOneOwnerPerLevel` still pass: `OwnerTeam` answers as before.

**Step 5: Commit**

```bash
git add internal/store/teams.go internal/store/usage.go internal/store/limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(store): usage rows keep the owner team they had" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 3: Store the limits

**Files:**
- Create: `internal/store/limits.go`
- Test: `internal/store/limits_test.go`

**Step 1: Write the failing tests** (append)

```go
// A limit is set, replaced and cleared per entry and team; clearing names the
// number it had; ModelLimits lists every one, by entry and team.
func TestSetAndClearModelLimit(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := EntryRef{Source: SourceGlobal, ID: "m1"}
	tm := EntryRef{Source: SourceTeam, ID: "t1"}
	for _, l := range []ModelLimit{
		{Ref: g, MonthlyTokens: 100, UpdatedAt: t0, UpdatedBy: "root (password)"},
		{Ref: g, TeamID: team, MonthlyTokens: 50, UpdatedAt: t0, UpdatedBy: "root (password)"},
		{Ref: g, MonthlyTokens: 200, UpdatedAt: t0.Add(time.Minute), UpdatedBy: "alice@example.com"}, // replaces the first
		{Ref: tm, TeamID: team, MonthlyTokens: 7, UpdatedAt: t0, UpdatedBy: "alice@example.com"},
	} {
		if err := s.SetModelLimit(ctx, l); err != nil {
			t.Fatalf("SetModelLimit(%+v): %v", l, err)
		}
	}
	got, err := s.ModelLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelLimit{
		{Ref: g, MonthlyTokens: 200, UpdatedAt: t0.Add(time.Minute), UpdatedBy: "alice@example.com"},
		{Ref: g, TeamID: team, MonthlyTokens: 50, UpdatedAt: t0, UpdatedBy: "root (password)"},
		{Ref: tm, TeamID: team, MonthlyTokens: 7, UpdatedAt: t0, UpdatedBy: "alice@example.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("ModelLimits = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i].Ref != want[i].Ref || got[i].TeamID != want[i].TeamID || got[i].MonthlyTokens != want[i].MonthlyTokens ||
			!got[i].UpdatedAt.Equal(want[i].UpdatedAt) || got[i].UpdatedBy != want[i].UpdatedBy {
			t.Errorf("limit %d = %+v; want %+v", i, got[i], want[i])
		}
	}
	if old, found, err := s.ClearModelLimit(ctx, g, 0); err != nil || !found || old != 200 {
		t.Errorf("ClearModelLimit(model) = %d, %v, %v; want 200, true", old, found, err)
	}
	if _, found, err := s.ClearModelLimit(ctx, g, 0); err != nil || found {
		t.Errorf("ClearModelLimit again = %v, %v; want not found", found, err)
	}
	if got, _ := s.ModelLimits(ctx); len(got) != 2 {
		t.Errorf("after the clear: %+v; want the 2 team limits", got)
	}
}

// Removing a team removes its limits, on global and team entries.
func TestDeleteTeamDropsItsLimits(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := EntryRef{Source: SourceGlobal, ID: "m1"}
	for _, l := range []ModelLimit{{Ref: g, MonthlyTokens: 100}, {Ref: g, TeamID: team, MonthlyTokens: 50}} {
		l.UpdatedAt, l.UpdatedBy = t0, "test"
		if err := s.SetModelLimit(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.DeleteTeam(ctx, team, nil); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	if got, err := s.ModelLimits(ctx); err != nil || len(got) != 1 || got[0].TeamID != 0 {
		t.Errorf("ModelLimits = %+v, %v; want the model limit alone", got, err)
	}
}
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/store/ -run 'TestSetAndClearModelLimit|TestDeleteTeamDropsItsLimits'
```

Expected: build failure `undefined: ModelLimit` / `s.SetModelLimit undefined`.

**Step 3: Write the code**

`internal/store/limits.go`:

```go
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelLimit is a monthly token limit on a chain entry. TeamID 0 is the model
// limit, on every use of a global entry; a team's ID is that team's limit on
// the entry, on the repositories it owns. UpdatedBy is the actor as the audit
// log names it.
type ModelLimit struct {
	Ref           EntryRef
	TeamID        int64
	MonthlyTokens int64
	UpdatedAt     time.Time
	UpdatedBy     string
}

// ModelLimits returns every limit, by source, entry and team, the model limit
// of an entry first.
func (s *Store) ModelLimits(ctx context.Context) ([]ModelLimit, error) {
	rows, err := s.pool.Query(ctx, `SELECT source, entry_id, COALESCE(team_id, 0), monthly_tokens, updated_at, updated_by
		FROM model_limits ORDER BY source, entry_id, COALESCE(team_id, 0)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelLimit
	for rows.Next() {
		var l ModelLimit
		if err := rows.Scan(&l.Ref.Source, &l.Ref.ID, &l.TeamID, &l.MonthlyTokens, &l.UpdatedAt, &l.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetModelLimit stores l in place of the limit its entry and team had. The
// caller checks the entry and the team exist; a team that doesn't is a
// foreign key error.
func (s *Store) SetModelLimit(ctx context.Context, l ModelLimit) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO model_limits (source, entry_id, team_id, monthly_tokens, updated_at, updated_by)
		VALUES ($1, $2, NULLIF($3::bigint, 0), $4, $5, $6)
		ON CONFLICT (source, entry_id, (COALESCE(team_id, 0))) DO UPDATE SET monthly_tokens = EXCLUDED.monthly_tokens,
			updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		l.Ref.Source, l.Ref.ID, l.TeamID, l.MonthlyTokens, l.UpdatedAt, l.UpdatedBy)
	return err
}

// ClearModelLimit removes the limit of ref for team (0: the model limit). old
// is the number it had; found is false when there was none.
func (s *Store) ClearModelLimit(ctx context.Context, ref EntryRef, team int64) (old int64, found bool, err error) {
	err = s.pool.QueryRow(ctx, `DELETE FROM model_limits WHERE source = $1 AND entry_id = $2 AND COALESCE(team_id, 0) = $3
		RETURNING monthly_tokens`, ref.Source, ref.ID, team).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return old, true, nil
}
```

`DeleteTeam` needs no change: `team_id` cascades (`ON DELETE CASCADE`). Add one sentence to its doc comment (`teams.go:305`): "its model limits go with it (ON DELETE CASCADE)".

**Step 4: Run and watch them pass**

```bash
DBTEST ./internal/store/ -run 'TestSetAndClearModelLimit|TestDeleteTeamDropsItsLimits'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/store/limits.go internal/store/limits_test.go internal/store/teams.go
git -c user.email="$(git config --local user.email)" commit -m "feat(store): set, list and clear model limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 4: Store: tokens per entry and owner team

**Files:**
- Modify: `internal/store/limits.go`
- Test: `internal/store/limits_test.go`

**Step 1: Write the failing tests** (append; add `"fmt"` to the imports)

```go
// entryUsage records a review of a new change on mr1's project that ran on
// ref and reported in and out tokens at at (at most t0 + 1h, the token's
// expiry). It returns the run.
func entryUsage(t *testing.T, s *Store, ref EntryRef, at time.Time, in, out int64) string {
	t.Helper()
	n := usageSeq.Add(1)
	head := fmt.Sprintf("limit%d", n)
	mr := MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 1000 + n}
	id := dispatched(t, s, mr, head, "tok-"+head)
	if ref.ID != "" {
		if err := s.SetRunEntry(ctx, id, RunEntry{Source: ref.Source, ID: ref.ID, Name: "n-" + ref.ID, Position: 1, Length: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := s.StoreResult(ctx, id, "tok-"+head, []byte(`{"schema_version":1,"status":"completed","skills":[]}`), at); err != nil || res != ResultStored {
		t.Fatalf("StoreResult = %v, %v", res, err)
	}
	if err := s.RecordUsage(ctx, id, []SkillUsage{skillRow("security", "no_blocking_issues", in, out)}, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// The sums: per entry and owner team, from the 1st of the month in UTC on; a
// row with no owner team counts under 0; a run that recorded no entry counts
// on none.
func TestEntryTokensSince(t *testing.T) {
	s := newStore(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := EntryRef{Source: SourceGlobal, ID: "m1"}
	tm := EntryRef{Source: SourceTeam, ID: "t9"}
	entryUsage(t, s, g, t0, 3, 0) // before the team owned the repository
	team := newTeam(t, s, "Payments")
	setOwner(t, s, team, mr1.Instance, "acme/widgets")
	entryUsage(t, s, g, t0, 10, 5)
	entryUsage(t, s, tm, t0, 7, 0)
	entryUsage(t, s, g, since, 1, 0)                // the first second of the month counts
	entryUsage(t, s, g, since.Add(-time.Second), 99, 0) // the last second of the month before does not
	entryUsage(t, s, EntryRef{}, t0, 50, 0)         // no entry

	got, err := s.EntryTokensSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := []EntryTokens{
		{Ref: g, Tokens: 3},
		{Ref: g, OwnerTeam: team, Tokens: 16},
		{Ref: tm, OwnerTeam: team, Tokens: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("EntryTokensSince = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v; want %+v", i, got[i], want[i])
		}
	}
}

// A panel counts each Job on its own entry: the members on theirs, Job P's
// roles on their judges'. Its run_usage rows, the total of all 3 Jobs, are
// not counted again on the run's entry.
func TestEntryTokensSincePanel(t *testing.T) {
	s := newStore(t)
	id := judgedPanel(t, s, "a1")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET model_source = 'global', model_entry_id = 'm1' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.EntryTokensSince(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []EntryTokens{
		{Ref: EntryRef{Source: SourceGlobal, ID: "m1"}, Tokens: 110 + 8 + 13}, // A's 2 skills, P's role a
		{Ref: EntryRef{Source: SourceTeam, ID: "t1"}, Tokens: 55 + 9},        // B, P's role b
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("EntryTokensSince = %+v; want %+v", got, want)
	}
}
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/store/ -run 'TestEntryTokensSince'
```

Expected: build failure `undefined: EntryTokens`.

**Step 3: Write the code** (append to `internal/store/limits.go`)

```go
// EntryTokens is what one chain entry used in the repositories of one owner
// team (OwnerTeam 0: rows with none, API reviews among them).
type EntryTokens struct {
	Ref       EntryRef
	OwnerTeam int64
	Tokens    int64
}

// entryTokensSQL sums the tokens (in plus out) reported from $1 on, per
// entry and owner team. A run with rows per model (a review panel's members
// and Job P) counts from those, each on its own entry (panel_members,
// panel_judges), and its run_usage rows, the panel's total, are left out.
// Any other run counts its run_usage rows on the run's entry. A run with no
// entry (before chains) counts on none, and so do Job P's tokens when its
// result split none per model.
const entryTokensSQL = `SELECT source, entry_id, COALESCE(owner_team_id, 0), sum(tokens)::bigint FROM (
		SELECT COALESCE(r.model_source, 'global') AS source, r.model_entry_id AS entry_id, u.owner_team_id,
			u.tokens_in + u.tokens_out AS tokens
		FROM run_usage u JOIN runs r ON r.id = u.run_id
		WHERE u.reported_at >= $1 AND COALESCE(r.model_entry_id, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM run_usage_models m WHERE m.run_id = u.run_id)
		UNION ALL
		SELECT p.model_source, p.model_entry_id, m.owner_team_id, m.tokens_in + m.tokens_out
		FROM run_usage_models m JOIN panel_members p ON p.id = m.member_id
		WHERE m.part = 'member' AND m.reported_at >= $1 AND p.model_entry_id <> ''
		UNION ALL
		SELECT j.model_source, j.model_entry_id, m.owner_team_id, m.tokens_in + m.tokens_out
		FROM run_usage_models m JOIN panel_judges j ON j.run_id = m.run_id AND j.role = m.role
		WHERE m.part = 'merge' AND m.reported_at >= $1 AND j.model_entry_id <> '') x
	GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`

// EntryTokensSince returns what each entry used from since on, by reported
// time (entryTokensSQL), by source, entry and owner team.
func (s *Store) EntryTokensSince(ctx context.Context, since time.Time) ([]EntryTokens, error) {
	rows, err := s.pool.Query(ctx, entryTokensSQL, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryTokens
	for rows.Next() {
		var e EntryTokens
		if err := rows.Scan(&e.Ref.Source, &e.Ref.ID, &e.OwnerTeam, &e.Tokens); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

**Step 4: Run and watch them pass**

```bash
DBTEST ./internal/store/ -run 'TestEntryTokensSince'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/store/limits.go internal/store/limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(store): tokens per entry and owner team this month" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 5: Store: the `limit_used_up` wait

**Files:**
- Modify: `internal/store/runs.go:45-60` (wait reasons)
- Modify: `internal/store/quota.go:12-28`
- Test: `internal/store/quota_test.go`

**Step 1: Write the failing test** (append to `internal/store/quota_test.go`)

```go
// A limit holds a run as the quota does, with its own reason: an automatic
// run waits, a run asked for or a start is left alone, and a start takes the
// waiting run.
func TestHoldForLimit(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "h1")
	if ok, err := s.HoldForLimit(ctx, id); err != nil || !ok {
		t.Fatalf("HoldForLimit = %v, %v", ok, err)
	}
	if r := getRun(t, s, id); r.State != Waiting || r.SkipReason != WaitLimit || WaitLimit != "limit_used_up" {
		t.Fatalf("run = %s/%q; want waiting/limit_used_up", r.State, r.SkipReason)
	}
	if got, err := s.ClaimQueued(ctx, 10, t0); err != nil || len(got) != 0 {
		t.Fatalf("ClaimQueued = %v, %v; a held run is never claimed", got, err)
	}
	started, ok, err := s.StartRun(ctx, mr1, "acme/widgets", "h1", "main")
	if err != nil || !ok || started != id {
		t.Fatalf("StartRun = %s, %v, %v; want the held run", started, ok, err)
	}
	if ok, err := s.HoldForLimit(ctx, id); err != nil || ok {
		t.Errorf("HoldForLimit of a start = %v, %v; want left alone", ok, err)
	}
}
```

**Step 2: Run and watch it fail**

```bash
DBTEST ./internal/store/ -run 'TestHoldForLimit'
```

Expected: build failure `s.HoldForLimit undefined`.

**Step 3: Write the code**

`internal/store/runs.go`, after `WaitQuota`:

```go
	// WaitLimit: the monthly token quotas and limits stop every entry of the
	// run's chain, and a model limit or a team limit is one of them. It is
	// held, noted and started as WaitQuota is; only the dashboard tells them
	// apart.
	WaitLimit = "limit_used_up"
```

`internal/store/quota.go`, replace `HoldForQuota` with:

```go
// HoldForQuota moves a queued, or claimed and not yet dispatched, run to
// Waiting with the reason WaitQuota. A run the quota was looked at for
// (QuotaChecked: asked for, or started) is not held, nor is a start, whatever
// its flag: the previous release makes starts without it. ok is false when the
// run was in another state or unknown; a malformed id is an error.
func (s *Store) HoldForQuota(ctx context.Context, id string) (ok bool, err error) {
	return s.holdFor(ctx, id, WaitQuota)
}

// HoldForLimit is HoldForQuota with the reason WaitLimit: a model limit or a
// team limit took part.
func (s *Store) HoldForLimit(ctx context.Context, id string) (ok bool, err error) {
	return s.holdFor(ctx, id, WaitLimit)
}

// holdFor is HoldForQuota's hold, with reason.
func (s *Store) holdFor(ctx context.Context, id, reason string) (bool, error) {
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND dispatched_at IS NULL AND NOT quota_checked
		  AND trigger <> 'start'`, id, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
```

**Step 4: Run it and the quota tests**

```bash
DBTEST ./internal/store/ -run 'HoldFor|StartRun'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/store/runs.go internal/store/quota.go internal/store/quota_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(store): hold a run for a used-up model limit" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 6: `quota`: which limits are used up

**Files:**
- Create: `internal/quota/limits.go`
- Create: `internal/quota/limits_test.go` (no database)

**Step 1: Write the failing test**

```go
package quota

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// Usage per entry: the total counts every row, a team's sum its own rows
// only; a limit is used up at or above it; only the owner team's limits are
// read, and with no owner team none is.
func TestLimitsFrom(t *testing.T) {
	g := store.EntryRef{Source: store.SourceGlobal, ID: "g"}
	h := store.EntryRef{Source: store.SourceGlobal, ID: "h"}
	tm := store.EntryRef{Source: store.SourceTeam, ID: "t"}
	u := usageOf([]store.EntryTokens{
		{Ref: g, OwnerTeam: 7, Tokens: 60}, {Ref: g, Tokens: 40}, {Ref: tm, OwnerTeam: 7, Tokens: 9}, {Ref: h, OwnerTeam: 8, Tokens: 5},
	})
	for _, c := range []struct {
		ref  store.EntryRef
		team int64
		want int64
	}{{g, 0, 100}, {g, 7, 60}, {g, 8, 0}, {tm, 7, 9}, {tm, 0, 9}, {h, 7, 0}} {
		if got := u.Used(c.ref, c.team); got != c.want {
			t.Errorf("Used(%s, %d) = %d, want %d", c.ref.ID, c.team, got, c.want)
		}
	}
	limits := []store.ModelLimit{
		{Ref: g, MonthlyTokens: 100},            // at the limit: used up
		{Ref: g, TeamID: 7, MonthlyTokens: 61},  // one under: room
		{Ref: tm, TeamID: 7, MonthlyTokens: 9},  // used up
		{Ref: h, TeamID: 8, MonthlyTokens: 5},   // another team's
		{Ref: h, MonthlyTokens: 6},              // room
	}
	l := limitsFrom(limits, u, 7)
	if !l.Model[g] || l.Model[h] || l.Team[g] || !l.Team[tm] || l.Team[h] {
		t.Errorf("team 7: %+v", l)
	}
	if l := limitsFrom(limits, u, 0); len(l.Team) != 0 || !l.Model[g] {
		t.Errorf("no owner team: %+v; want the model limit only", l)
	}
	if l := limitsFrom(limits, u, 8); !l.Team[h] {
		t.Errorf("team 8: %+v; want its limit on h used up", l)
	}
	if !needsUsage(limits, 0) || needsUsage([]store.ModelLimit{{Ref: h, TeamID: 8, MonthlyTokens: 5}}, 7) {
		t.Error("needsUsage: a model limit needs the usage, another team's limit alone does not")
	}
}
```

**Step 2: Run and watch it fail**

```bash
go test -count=1 ./internal/quota/ -run TestLimitsFrom
```

Expected: build failure `undefined: usageOf`.

**Step 3: Write the code**

`internal/quota/limits.go`:

```go
package quota

import (
	"context"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// EntryUsage is what each chain entry used in a month: in all (every row)
// and in the repositories of each owner team. A row with no owner team (an
// API review, a repository with none, a row from before model limits) counts
// in the total only.
type EntryUsage struct {
	total  map[store.EntryRef]int64
	byTeam map[store.EntryRef]map[int64]int64
}

// Used is what ref used in the repositories team owns, or with team 0 in all.
func (u EntryUsage) Used(ref store.EntryRef, team int64) int64 {
	if team == 0 {
		return u.total[ref]
	}
	return u.byTeam[ref][team]
}

// UsedUp reports whether l is reached: usage at or above it, as for the
// quotas.
func (u EntryUsage) UsedUp(l store.ModelLimit) bool { return u.Used(l.Ref, l.TeamID) >= l.MonthlyTokens }

func usageOf(rows []store.EntryTokens) EntryUsage {
	u := EntryUsage{total: map[store.EntryRef]int64{}, byTeam: map[store.EntryRef]map[int64]int64{}}
	for _, r := range rows {
		u.total[r.Ref] += r.Tokens
		if r.OwnerTeam == 0 {
			continue
		}
		if u.byTeam[r.Ref] == nil {
			u.byTeam[r.Ref] = map[int64]int64{}
		}
		u.byTeam[r.Ref][r.OwnerTeam] += r.Tokens
	}
	return u
}

// ReadEntryUsage reads what each entry used in the month of now.
func ReadEntryUsage(ctx context.Context, st *store.Store, now time.Time) (EntryUsage, error) {
	rows, err := st.EntryTokensSince(ctx, MonthStart(now))
	if err != nil {
		return EntryUsage{}, err
	}
	return usageOf(rows), nil
}

// Limits is which entries a limit stops this month for one owner team's
// repositories: Model, the entries whose model limit is used up; Team, those
// whose limit for the team is used up.
type Limits struct {
	Model map[store.EntryRef]bool
	Team  map[store.EntryRef]bool
}

// ReadLimits reads which limits are used up in the month of now for the
// repositories ownerTeam owns. With ownerTeam 0 (no owner team, an API
// review) no team limit applies. It reads the usage only when a limit
// applies, so with none it makes one query.
func ReadLimits(ctx context.Context, st *store.Store, now time.Time, ownerTeam int64) (Limits, error) {
	limits, err := st.ModelLimits(ctx)
	if err != nil || !needsUsage(limits, ownerTeam) {
		return Limits{}, err
	}
	u, err := ReadEntryUsage(ctx, st, now)
	if err != nil {
		return Limits{}, err
	}
	return limitsFrom(limits, u, ownerTeam), nil
}

// needsUsage reports whether one of limits applies to ownerTeam's
// repositories: a model limit, or the team's own.
func needsUsage(limits []store.ModelLimit, ownerTeam int64) bool {
	for _, l := range limits {
		if l.TeamID == 0 || l.TeamID == ownerTeam {
			return true
		}
	}
	return false
}

func limitsFrom(limits []store.ModelLimit, u EntryUsage, ownerTeam int64) Limits {
	var out Limits
	add := func(m map[store.EntryRef]bool, ref store.EntryRef) map[store.EntryRef]bool {
		if m == nil {
			m = map[store.EntryRef]bool{}
		}
		m[ref] = true
		return m
	}
	for _, l := range limits {
		switch {
		case !u.UsedUp(l):
		case l.TeamID == 0:
			out.Model = add(out.Model, l.Ref)
		case l.TeamID == ownerTeam:
			out.Team = add(out.Team, l.Ref)
		}
	}
	return out
}
```

Update the package comment of `internal/quota/quota.go` (lines 1-5): add "and, for model limits, which entries a model limit or a team limit stops (limits.go)".

**Step 4: Run and watch it pass**

```bash
go test -count=1 ./internal/quota/
```

Expected: `ok` (the DB tests of the package skip here; this one needs none).

**Step 5: Commit**

```bash
git add internal/quota/limits.go internal/quota/limits_test.go internal/quota/quota.go
git -c user.email="$(git config --local user.email)" commit -m "feat(quota): which model and team limits are used up" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 7: `chain`: limit reasons, the hold rule, an admin's start

**Files:**
- Modify: `internal/chain/chain.go` (reasons 18-26, `Input` 38-56, `Decision` 65-80, `Resolve` 82-117, `skipReason` 123-146, `reasonWords` 173-185)
- Test: `internal/chain/chain_test.go`

**Step 1: Write the failing tests** (append to `chain_test.go`)

```go
// A used-up model limit skips a global entry, a used-up team limit any
// entry: the next entry runs.
func TestResolveLimitsSkip(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"), g("c"))
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
	d := Resolve(in)
	if d.Ref != g("c") || d.Position != 3 || d.FirstSkip != ReasonModelLimit || d.Skips[1].Reason != ReasonTeamLimit {
		t.Fatalf("decision = %+v", d)
	}
}

// The order: a mark, then the instance quota, then the model limit, then the
// team limit.
func TestResolveLimitOrder(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"))
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	in.TeamLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	if d := Resolve(in); d.FirstSkip != ReasonModelLimit {
		t.Errorf("model before team: first skip %q", d.FirstSkip)
	}
	in.InstanceQuotaUsedUp = true
	if d := Resolve(in); d.FirstSkip != ReasonInstanceQuota {
		t.Errorf("quota before limits: first skip %q", d.FirstSkip)
	}
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(time.Hour))}
	if d := Resolve(in); d.FirstSkip != store.ClassQuota {
		t.Errorf("mark first: first skip %q", d.FirstSkip)
	}
}

// A team entry has no model limit: only its team's limit stops it.
func TestResolveModelLimitIsGlobalOnly(t *testing.T) {
	in := input(store.ScopeTeam, tm("a"))
	in.ModelLimitUsedUp = map[store.EntryRef]bool{tm("a"): true}
	if d := Resolve(in); d.Ref != tm("a") {
		t.Fatalf("decision = %+v; want the team entry to run", d)
	}
}

// A 1-entry chain ignores marks, not limits: at a limit it is held.
func TestResolveLimitsHoldOneEntryChain(t *testing.T) {
	in := input(store.ScopeGlobal, g("a"))
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassAuth, nil)}
	if d := Resolve(in); !d.Runs() {
		t.Fatalf("a mark stopped a 1-entry chain: %+v", d)
	}
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	if d := Resolve(in); !d.Hold || !d.Limited || d.Exhausted {
		t.Fatalf("decision = %+v; want a hold a limit took part in", d)
	}
}

// Held when quotas and limits alone stop the entries this review has not
// tried; Limited when a limit is one of them. A mark or a bad key fails it,
// and names the limits too.
func TestResolveHoldRules(t *testing.T) {
	for _, c := range []struct {
		name          string
		edit          func(in *Input)
		hold, limited bool
		text          string
	}{
		{"quota only", func(in *Input) { in.InstanceQuotaUsedUp = true }, true, false, ""},
		{"quota and a team limit", func(in *Input) {
			in.InstanceQuotaUsedUp = true
			in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
		}, true, true, ""},
		{"tried and a team limit", func(in *Input) {
			in.Tried = map[store.EntryRef]bool{g("a"): true}
			in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
		}, true, true, ""},
		{"tried only", func(in *Input) {
			in.Tried = map[store.EntryRef]bool{g("a"): true, tm("b"): true}
		}, false, false, "all models exhausted: n-a (already tried), n-b (already tried)"},
		{"a mark and a team limit", func(in *Input) {
			in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(time.Hour))}
			in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
		}, false, false, "all models exhausted: n-a (out of quota), n-b (team limit used up)"},
		{"no key and a model limit", func(in *Input) {
			m := in.Entries[tm("b")]
			m.Key = ""
			in.Entries[tm("b")] = m
			in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
		}, false, false, "all models exhausted: n-a (model limit used up), n-b (no usable key)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := input(store.ScopeTeam, g("a"), tm("b"))
			c.edit(&in)
			d := Resolve(in)
			if d.Hold != c.hold || d.Limited != c.limited || d.Exhausted == c.hold || d.Runs() {
				t.Fatalf("decision = %+v; want hold %v, limited %v", d, c.hold, c.limited)
			}
			if c.text != "" && FailureText(d) != c.text {
				t.Errorf("FailureText = %q\nwant %q", FailureText(d), c.text)
			}
		})
	}
}

// An admin's start goes past the instance quota, the model limits and the
// team limits on global entries; a team's own limit on its team entry stays.
func TestPastAdminLimits(t *testing.T) {
	in := input(store.ScopeRepo, tm("a"), g("b"))
	in.SavedByAdmin, in.InstanceQuotaUsedUp = true, true
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("b"): true}
	in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("a"): true, g("b"): true}
	if d := Resolve(in); !d.Hold {
		t.Fatalf("before: %+v; want a hold", d)
	}
	past := in
	past.PastAdminLimits()
	if d := Resolve(past); d.Ref != g("b") || d.FirstSkip != ReasonTeamLimit {
		t.Fatalf("past the admin limits: %+v; want the global entry, the team's own limit still skipping its entry", d)
	}
	if !in.TeamLimitUsedUp[g("b")] || !in.ModelLimitUsedUp[g("b")] {
		t.Error("PastAdminLimits changed the maps of the Input it was copied from")
	}
	own := input(store.ScopeTeam, tm("a"))
	own.TeamLimitUsedUp = map[store.EntryRef]bool{tm("a"): true}
	own.PastAdminLimits()
	if d := Resolve(own); !d.Hold {
		t.Fatalf("only the team's own entry: %+v; want still held", d)
	}
	own.IgnoreLimits()
	if d := Resolve(own); !d.Runs() {
		t.Fatalf("IgnoreLimits: %+v; want it to run", d)
	}
}

func TestReasonWordsLimits(t *testing.T) {
	if ReasonWords(ReasonModelLimit) != "model limit used up" || ReasonWords(ReasonTeamLimit) != "team limit used up" {
		t.Errorf("words = %q, %q", ReasonWords(ReasonModelLimit), ReasonWords(ReasonTeamLimit))
	}
}
```

**Step 2: Run and watch them fail**

```bash
go test -count=1 ./internal/chain/ -run 'Limit|HoldRules|PastAdmin'
```

Expected: build failure `in.ModelLimitUsedUp undefined` / `undefined: ReasonModelLimit`.

**Step 3: Write the code** in `internal/chain/chain.go`

1. Reasons, after `ReasonSameModel`:

```go
	ReasonModelLimit = "model_limit" // a global entry whose model limit is used up this month
	ReasonTeamLimit  = "team_limit"  // the owner team's limit on the entry is used up this month
```

2. `Input`, after `InstanceQuotaUsedUp`, and extend its doc comment ("ModelLimitUsedUp and TeamLimitUsedUp are the entries whose model limit, or whose limit for OwnerTeam, is used up this month; LoadSet reads them."):

```go
	ModelLimitUsedUp    map[store.EntryRef]bool
	TeamLimitUsedUp     map[store.EntryRef]bool
```

3. `Decision`: add `Limited bool` after `Hold`, and change its doc comment to: "Otherwise exactly one of Wait (the chain is empty: wait as with no model), Hold (only quotas and limits stopped the entries it has not tried: hold on them; Limited is set when a model or team limit is one of them) and Exhausted (fail) is set."

4. In `Resolve`, replace lines 108-116 with:

```go
	d.Hold, d.Limited = holds(d.Skips)
	d.Exhausted = !d.Hold
	return d
}

// holds reports whether skips hold the review rather than fail it: a quota or
// a limit, which frees up, stopped at least one entry, and every other entry
// is one this review already tried. A mark, a bad key or any other reason
// fails it. limited is whether a model or team limit is among them.
func holds(skips []Skip) (hold, limited bool) {
	for _, s := range skips {
		switch s.Reason {
		case ReasonModelLimit, ReasonTeamLimit:
			hold, limited = true, true
		case ReasonInstanceQuota:
			hold = true
		case ReasonTried:
		default:
			return false, false
		}
	}
	return hold, hold && limited
}
```

5. `skipReason`, the last switch:

```go
	switch {
	case in.Tried[ref]:
		return ReasonTried
	case m.Key == "":
		return ReasonKeyUnusable
	case ref.Source == store.SourceGlobal && in.InstanceQuotaUsedUp:
		return ReasonInstanceQuota
	case ref.Source == store.SourceGlobal && in.ModelLimitUsedUp[ref]:
		return ReasonModelLimit
	case in.TeamLimitUsedUp[ref]:
		return ReasonTeamLimit
	}
	return ""
```

and its doc comment ends: "the instance quota and the limits come last, so a hold waits only for entries that could run once they free up. A limit stops a 1-entry chain too: marks come from provider errors, limits are a budget."

6. `reasonWords`: add `ReasonModelLimit: "model limit used up"` and `ReasonTeamLimit: "team limit used up"`.

7. The 2 methods, after `EntryReason`:

```go
// PastAdminLimits drops what an admin's start goes past: the instance quota,
// the model limits and the team limits on global entries. A team's own limit
// on its team entry stays: that is the team's money. The maps of the Input in
// was copied from are not changed.
func (in *Input) PastAdminLimits() {
	in.InstanceQuotaUsedUp, in.ModelLimitUsedUp = false, nil
	var own map[store.EntryRef]bool
	for ref := range in.TeamLimitUsedUp {
		if ref.Source == store.SourceTeam {
			if own == nil {
				own = map[store.EntryRef]bool{}
			}
			own[ref] = true
		}
	}
	in.TeamLimitUsedUp = own
}

// IgnoreLimits drops every model and team limit: Job P's judge ends a review
// that has started, as a running review finishes after a quota runs out.
func (in *Input) IgnoreLimits() { in.ModelLimitUsedUp, in.TeamLimitUsedUp = nil, nil }
```

**Step 4: Run the chain package**

```bash
go test -count=1 ./internal/chain/
```

Expected: `ok` (the pure tests run; the DB tests in `load_test.go` skip). `TestResolveOnlyQuotaBlockedHolds` still passes (a mark and the quota: exhausted).

**Step 5: Commit**

```bash
git add internal/chain/chain.go internal/chain/chain_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(chain): skip entries at their model or team limit, and hold on them" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 8: Every chain read carries the limits

`LoadSet` reads the limits, so dispatch, members, chain B, requests by hand and API reviews skip used-up entries with no other change. The engine tests here prove it end to end before the code exists.

**Files:**
- Modify: `internal/chain/load.go:14-90`
- Test: `internal/chain/load_test.go`
- Create: `internal/engine/e2e_limits_test.go`
- Modify: `internal/engine/e2e_quota_test.go:186-200` (`TestQuotaReadErrorRetries` uses the new `dropTable`)

**Step 1: Write the failing tests**

`internal/chain/load_test.go`, append (add `"crypto/sha256"`, `"fmt"`, `"time"` to the imports if missing):

```go
// spent records a review of acme/api that ran on ref and used tokens now.
func spent(t *testing.T, st *store.Store, iid int64, ref store.EntryRef, tokens int64) {
	t.Helper()
	mr := store.MRKey{Instance: inst, ProjectID: 1, IID: iid}
	id, _, err := st.EnqueueRun(ctx, mr, project, fmt.Sprintf("spent%d", iid), "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimQueued(ctx, 10, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunEntry(ctx, id, store.RunEntry{Source: ref.Source, ID: ref.ID, Name: ref.ID, Position: 1, Length: 1}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("tok-" + id))
	if err := st.MarkDispatched(ctx, id, "job-"+id, "base", "start", sum[:], now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if out, err := st.StoreResult(ctx, id, "tok-"+id, []byte(`{"schema_version":1,"status":"completed","skills":[]}`), now); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := st.RecordUsage(ctx, id, []store.SkillUsage{{Skill: "a", Verdict: "no_blocking_issues", In: tokens}}, nil); err != nil {
		t.Fatal(err)
	}
}

func limit(t *testing.T, st *store.Store, ref store.EntryRef, team, tokens int64) {
	t.Helper()
	if err := st.SetModelLimit(ctx, store.ModelLimit{Ref: ref, TeamID: team, MonthlyTokens: tokens, UpdatedAt: now, UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
}

// Load reads the limits of this month: the model limits, and the owner
// team's limits only. A repository with no owner team gets no team limit.
func TestLoadReadsLimits(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	spent(t, e.st, 1, e.a, 10)
	spent(t, e.st, 2, e.b, 10)
	limit(t, e.st, e.a, 0, 10)      // used up
	limit(t, e.st, e.b, e.team, 10) // the owner team's, used up
	in, err := Load(ctx, e.st, e.reg, now, store.Run{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !in.ModelLimitUsedUp[e.a] || in.ModelLimitUsedUp[e.b] || !in.TeamLimitUsedUp[e.b] {
		t.Fatalf("owned: model %v, team %v", in.ModelLimitUsedUp, in.TeamLimitUsedUp)
	}
	other := settings.Effective{Instance: inst, Project: "acme/other"}
	in, err = Load(ctx, e.st, e.reg, now, store.Run{}, other)
	if err != nil {
		t.Fatal(err)
	}
	if !in.ModelLimitUsedUp[e.a] || len(in.TeamLimitUsedUp) != 0 {
		t.Fatalf("no owner: model %v, team %v; want the model limit only", in.ModelLimitUsedUp, in.TeamLimitUsedUp)
	}
}
```

`internal/engine/e2e_limits_test.go`:

```go
package engine

import (
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// limitSeeds numbers the changes seedEntryUsage reviews, so each seed is a
// change of its own.
var limitSeeds atomic.Int64

// seedEntryUsage records a review of another change of acme/widgets that ran
// on ref and reported tokens tokens at the engine's clock. Like seedUsageOf it
// claims every queued run, so call it before queueing the change's own.
func seedEntryUsage(t *testing.T, env *testEnv, ref store.EntryRef, tokens int64) {
	t.Helper()
	id := env.Enqueue(100+limitSeeds.Add(1), sha('9'))
	if _, err := env.Store.ClaimQueued(env.ctx, 100, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.SetRunEntry(env.ctx, id, store.RunEntry{Source: ref.Source, ID: ref.ID, Name: ref.ID, Position: 1, Length: 1}); err != nil {
		t.Fatal(err)
	}
	seedUsageOf(t, env, id, env.Now(), tokens)
}

// setLimit stores a limit of tokens on ref for team (0: the model limit).
func setLimit(t *testing.T, env *testEnv, ref store.EntryRef, team, tokens int64) {
	t.Helper()
	if err := env.Store.SetModelLimit(env.ctx, store.ModelLimit{Ref: ref, TeamID: team, MonthlyTokens: tokens,
		UpdatedAt: env.Now(), UpdatedBy: "test"}); err != nil {
		t.Fatalf("SetModelLimit: %v", err)
	}
}

// configRef is the config file's entry, the global chain of a test env.
func configRef() store.EntryRef { return store.EntryRef{Source: store.SourceGlobal, ID: store.ConfigModelID} }

// configName is the config file's entry's name.
func configName(t *testing.T, env *testEnv) string {
	t.Helper()
	for _, m := range env.Engine.Instances.GlobalModels() {
		if m.ID == store.ConfigModelID {
			return m.Name
		}
	}
	t.Fatal("no config entry")
	return ""
}

// dropTable drops table name in the env's schema, so reading it fails.
func dropTable(t *testing.T, env *testEnv, name string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv(testDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	if _, err := conn.Exec(env.ctx, `DROP TABLE `+name); err != nil {
		t.Fatal(err)
	}
}

// A global entry at its model limit and a team entry at its team's limit are
// skipped: the review runs on the next entry.
func TestLimitsRunNextEntry(t *testing.T) {
	env := newTestEnv(t)
	env.SetNow(novemberMid)
	team := teamOwning(t, env, "Payments")
	g := globalRef(t, env, "gpt-g", "key-g")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	b := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		Entries: []store.EntryRef{g, a, b}, SavedByAdmin: true})
	seedEntryUsage(t, env, g, 10)
	seedEntryUsage(t, env, a, 10)
	setLimit(t, env, g, 0, 10)
	setLimit(t, env, a, team, 10)

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-team-b")
	if r := env.Run(id); r.ChainPosition != 3 || r.FallbackClass != chain.ReasonModelLimit {
		t.Errorf("run entry = position %d, first skip %q; want 3 and %q", r.ChainPosition, r.FallbackClass, chain.ReasonModelLimit)
	}
}

// A fallback whose entry fallen back from still has its mark fails, naming the
// limit of the entry left.
func TestFallbackFailsOnMarkAndLimit(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	env.SetNow(novemberMid)
	team := ownerTeam(t, env)
	seedEntryUsage(t, env, refs[1], 10)
	setLimit(t, env, refs[1], team, 10)
	head := sha('a')
	servePublish(env, 1, head)
	root := env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)
	dispatch(t, env)
	publish(t, env)

	as, err := env.Store.Attempts(env.ctx, root)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want 2", len(as), err)
	}
	want := "all models exhausted: team-a (out of quota), team-b (team limit used up)"
	if last := as[1]; last.FailureReason != want || last.State != store.Published {
		t.Errorf("last attempt = %s %q; want published %q", last.State, last.FailureReason, want)
	}
	if n := len(env.Jobs()); n != 1 {
		t.Errorf("%d Jobs, want 1", n)
	}
}

// An API review has no owner team: a team's used-up limit does not stop it.
func TestAPIReviewIgnoresTeamLimits(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	team := teamOwning(t, env, "Payments")
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), team, 10)
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job; run = %+v\n%s", env.Run(id), env.Logs())
	}
}

// An API review at the model limit fails, naming the entry: it is never held.
func TestAPIReviewAtModelLimitFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), 0, 10)
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatal("a review at the model limit got a Job")
	}
	publish(t, env)
	a.wantFailed(t, id, reviewapi.StepStarting, "all models exhausted: "+configName(t, env)+" (model limit used up)")
}

// When the usage can't be read, the dispatch is tried again; nothing starts
// unchecked.
func TestLimitReadErrorRetries(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	setLimit(t, env, configRef(), 0, 100)
	dropTable(t, env, "run_usage_models")
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
	if r := env.Run(id); r.State != store.Queued || r.Attempts != 1 {
		t.Fatalf("run = %s attempts %d, want queued 1; logs:\n%s", r.State, r.Attempts, env.Logs())
	}
	if logs := env.Logs(); !strings.Contains(logs, "read the model limits") {
		t.Errorf("the retry isn't about the limits; logs:\n%s", logs)
	}
}

var _ = http.MethodPost // used by later tasks' tests in this file
```

(Drop the last line once a later task uses `net/http` in this file; it keeps the import used meanwhile.)

In `e2e_quota_test.go`, `TestQuotaReadErrorRetries` replaces its 12 lines of `pgx` setup with `dropTable(t, env, "run_usage")`. Its `os` and `pgx` imports stay: `TestUnflaggedStartDispatchesOnUsedUpQuota` (line ~317) still uses them.

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/chain/ -run TestLoadReadsLimits
DBTEST ./internal/engine/ -run 'TestLimitsRunNextEntry|TestFallbackFailsOnMarkAndLimit|TestAPIReviewIgnoresTeamLimits|TestAPIReviewAtModelLimitFails|TestLimitReadErrorRetries|TestQuotaReadErrorRetries'
```

Expected: `TestLoadReadsLimits` FAIL (`owned: model map[], team map[]`). `TestLimitsRunNextEntry` FAIL (`the Job's key = "key-g"`), `TestFallbackFailsOnMarkAndLimit` FAIL (2 Jobs, the second attempt ran team-b), `TestAPIReviewAtModelLimitFails` FAIL (got a Job), `TestLimitReadErrorRetries` FAIL (1 job created). `TestAPIReviewIgnoresTeamLimits` and `TestQuotaReadErrorRetries` PASS (they guard against over-reach and the refactor).

**Step 3: Write the code** in `internal/chain/load.go`

Rename today's `LoadSet` body to `loadSet` (same signature), and add:

```go
// LoadSet is Load for one chain set. ChainA is Load. ChainB is a review
// panel's second chain: ... (today's comment, unchanged) ...
// Both carry the limits used up this month (quota.ReadLimits): the model
// limits, and the owner team's limits (none without an owner team, so none
// for an API review). A limit read that fails is the load's error, so no
// review starts unchecked.
func LoadSet(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective, set store.ChainSet) (Input, error) {
	in, err := loadSet(ctx, st, reg, now, run, repo, set)
	if err != nil {
		return Input{}, err
	}
	lim, err := quota.ReadLimits(ctx, st, now, in.OwnerTeam)
	if err != nil {
		return Input{}, fmt.Errorf("read the model limits: %w", err)
	}
	in.ModelLimitUsedUp, in.TeamLimitUsedUp = lim.Model, lim.Team
	return in, nil
}
```

Import `github.com/gopaytech/pergent.sh/internal/quota`. Change `Load`'s comment "The caller sets InstanceQuotaUsedUp." to "The caller sets InstanceQuotaUsedUp; the limits are read here." The limits are read for every entry with a limit, not only those in `Refs`, so `dispatcher.chainA`'s pinned client model (Task 12) finds its own.

**Step 4: Run and watch them pass, then the 3 packages**

```bash
DBTEST ./internal/chain/
DBTEST ./internal/engine/ -run 'TestLimits|TestFallbackFailsOnMarkAndLimit|TestAPIReview|TestLimitReadErrorRetries|TestQuotaReadErrorRetries'
DBTEST ./internal/actions/ ./internal/quota/
```

Expected: `ok` for each.

**Step 5: Commit**

```bash
git add internal/chain/load.go internal/chain/load_test.go internal/engine/e2e_limits_test.go internal/engine/e2e_quota_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(chain): every chain read carries this month's used-up limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 9: Dispatch holds with the limit reason

**Files:**
- Modify: `internal/engine/quota.go:25-54`
- Modify: `internal/engine/dispatch.go:445`, `:478`
- Test: `internal/engine/e2e_limits_test.go`

**Step 1: Write the failing tests** (append)

```go
// wantLimitHeld checks run id waits because a limit took part, with no Job.
func wantLimitHeld(t *testing.T, env *testEnv, id string) {
	t.Helper()
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitLimit {
		t.Fatalf("run = %s/%q, want waiting/%s; logs:\n%s", r.State, r.SkipReason, store.WaitLimit, env.Logs())
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
}

// A push whose only entry is at its model limit is held as for a used-up
// quota: the same pending status and note, naming no limit. A 1-entry chain
// ignores marks, not limits.
func TestModelLimitHoldsPush(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	seedEntryUsage(t, env, configRef(), 100)
	setLimit(t, env, configRef(), 0, 100)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)

	wantLimitHeld(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: "+quotaDescription {
		t.Errorf("statuses = %v", got)
	}
	if ns := notes(env, 3); len(ns) != 1 || !strings.Contains(ns[0], quotaNote) {
		t.Fatalf("notes = %v", ns)
	}
}

// A fallback whose entries left are stopped by limits alone is held once the
// mark of the entry it fell back from has ended.
func TestFallbackHeldOnLimit(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	env.SetNow(novemberMid)
	team := ownerTeam(t, env)
	seedEntryUsage(t, env, refs[1], 10)
	setLimit(t, env, refs[1], team, 10)
	head := sha('a')
	servePublish(env, 1, head)
	root := env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)
	env.SetNow(novemberMid.Add(2 * time.Hour)) // team-a's quota mark (1h) has ended: it reads "tried"
	dispatch(t, env)

	as, err := env.Store.Attempts(env.ctx, root)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want 2", len(as), err)
	}
	if last := as[1]; last.State != store.Waiting || last.SkipReason != store.WaitLimit {
		t.Errorf("last attempt = %s/%q; want waiting/%s", last.State, last.SkipReason, store.WaitLimit)
	}
	if n := len(env.Jobs()); n != 1 {
		t.Errorf("%d Jobs, want 1", n)
	}
}
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/engine/ -run 'TestModelLimitHoldsPush|TestFallbackHeldOnLimit'
```

Expected: FAIL with `run = waiting/"quota_used_up", want waiting/limit_used_up` (the hold works since Task 7 and 8, with the old reason).

**Step 3: Write the code**

`internal/engine/quota.go`, `holdForQuota` takes `limited bool`:

```go
// holdForQuota makes the run wait because the monthly token quotas stop it,
// or with limited, the quotas and the limits with a model or team limit among
// them: no Job, the pending status quotaDescription on its head and, the
// first time in the month on this change, quotaNote. The texts are the same
// either way, since anyone reads them on a public repository; only the wait
// reason (store.WaitLimit) tells the dashboard that a limit took part. A push
// or a request after the quota has room starts the review. A status or note
// that can't be posted is logged: the run waits either way.
func (d *dispatcher) holdForQuota(ctx context.Context, plat Platform, limited bool) error {
	e, run := d.e, d.run
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	hold, reason := e.Store.HoldForQuota, store.WaitQuota
	if limited {
		hold, reason = e.Store.HoldForLimit, store.WaitLimit
	}
	held, err := hold(sctx, run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("hold the run for the quota: %w", err))
	}
	if !held {
		// Superseded meanwhile, or asked for while it was being checked: back
		// to the queue, which a superseded run doesn't go to.
		return d.postpone(ctx, e.now())
	}
	d.log.Info("run waits: the monthly token quota is used up", "reason", reason)
	// ... the rest of the function is unchanged ...
```

`internal/engine/dispatch.go`: line 445 `return d.holdForQuota(ctx, plat, false)` (the repository quota); line 478 `return d.holdForQuota(ctx, plat, dec.Limited)`. Extend the comment above `askedFor` (line 427): "The resolver also stops entries at their model or team limit, and holds when only quotas and limits are left."

**Step 4: Run them, the quota tests and the golden test**

```bash
DBTEST ./internal/engine/ -run 'TestModelLimitHoldsPush|TestFallbackHeldOnLimit|Quota|TestDispatchJobUnchanged|Fallback'
```

Expected: `ok`. `git status --short internal/engine/testdata` prints nothing.

**Step 5: Commit**

```bash
git add internal/engine/quota.go internal/engine/dispatch.go internal/engine/e2e_limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(engine): a limit-held run waits with its own reason" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 10: A run asked for, and member A, go past the admin limits

**Files:**
- Modify: `internal/engine/chain.go:44-62` (`resolve`, new `pastAdmin`)
- Modify: `internal/engine/panel.go:207-229` (`resolveA`)
- Test: `internal/engine/e2e_limits_test.go`

**Step 1: Write the failing tests** (append)

```go
// An admin's start of a limit-held change goes past the model limit and runs
// on the first global entry with no mark; the team entry at its team's own
// limit stays skipped.
func TestStartGoesPastAdminLimits(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	team := teamOwning(t, env, "Payments")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	g := globalRef(t, env, "gpt-g", "key-g")
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		Entries: []store.EntryRef{a, g}, SavedByAdmin: true})
	seedEntryUsage(t, env, a, 10)
	seedEntryUsage(t, env, g, 10)
	setLimit(t, env, a, team, 10) // the team's own
	setLimit(t, env, g, 0, 10)    // an admin's
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantLimitHeld(t, env, id)

	if started, ok, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil || !ok || started != id {
		t.Fatalf("StartRun = %s, %v, %v", started, ok, err)
	}
	dispatch(t, env)
	wantJobKey(t, env, id, "key-g")
}

// A start whose chain is only the team's entry at the team's own limit can't
// be held (a start never is) and can't run: it fails, naming the limit.
func TestStartStoppedByTeamOwnLimitFails(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	team := teamOwning(t, env, "Payments")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{a}})
	seedEntryUsage(t, env, a, 10)
	setLimit(t, env, a, team, 10)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantLimitHeld(t, env, id)

	if _, ok, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil || !ok {
		t.Fatalf("StartRun = %v, %v", ok, err)
	}
	dispatch(t, env)
	if r := env.Run(id); r.FailureReason != "all models exhausted: team-a (team limit used up)" {
		t.Fatalf("run = %s %q; want the exhausted failure\n%s", r.State, r.FailureReason, env.Logs())
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d Jobs, want 0", n)
	}
}

// Member A passed the hold with its run: a model limit used up since the
// split does not stop it.
func TestMemberAGoesPastAdminLimits(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e := newE2E(t)
	env := e.env
	env.SetNow(novemberMid)
	env.Engine.ScanImage = testScanImage
	teamOwning(t, env, "Payments")
	savePanelChain(t, env, "team-b")
	seedEntryUsage(t, env, configRef(), 10)
	id := splitPanel(t, env, 1, sha('a'))
	setLimit(t, env, configRef(), 0, 10)
	dispatch(t, env)
	wantMemberKey(t, env, memberOfRole(t, env, id, "a"), testLLMKey)
}
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/engine/ -run 'TestStartGoesPastAdminLimits|TestStartStoppedByTeamOwnLimitFails|TestMemberAGoesPastAdminLimits'
```

Expected: `TestStartGoesPastAdminLimits` FAIL (`no job`: the start is held again and postponed), `TestStartStoppedByTeamOwnLimitFails` FAIL (empty failure reason, the run is back in the queue), `TestMemberAGoesPastAdminLimits` FAIL (member A failed with `all models exhausted: ... (model limit used up)`).

**Step 3: Write the code**

`internal/engine/chain.go`, replace `resolve` (lines 44-62):

```go
// resolve picks the run's entry. instanceUsedUp is whether the instance's
// token quota stops global entries. A run somebody asked for (askedFor) is
// never held: when only quotas and limits stop its entries, it goes past
// what an admin's start goes past (pastAdmin), as before chains it ran on the
// first global entry.
func (d *dispatcher) resolve(ctx context.Context, repo settings.Effective, instanceUsedUp, askedFor bool) (chain.Decision, error) {
	in, err := d.e.chainFor(ctx, d.run, repo)
	if err != nil {
		return chain.Decision{}, err
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
	if dec.Hold && askedFor {
		dec = pastAdmin(in)
	}
	d.attempt = len(in.Tried) + 1
	d.entry, d.scope = dec, in.Scope
	return dec, nil
}

// pastAdmin resolves in past the instance quota and the admin limits
// (chain.Input.PastAdminLimits), for a run that must not be held: one
// somebody asked for, or member A, which passed the hold with its run. When a
// team's own limit still stops every entry left, it fails: HoldForQuota never
// holds such a run, so a hold would only come back to the queue.
func pastAdmin(in chain.Input) chain.Decision {
	in.PastAdminLimits()
	dec := chain.Resolve(in)
	if dec.Hold {
		dec.Hold, dec.Limited, dec.Exhausted = false, false, true
	}
	return dec
}
```

`internal/engine/panel.go`, in `resolveA` (lines 223-227) replace

```go
	dec := chain.Resolve(in)
	if dec.Hold {
		in.InstanceQuotaUsedUp = false
		dec = chain.Resolve(in)
	}
```

with

```go
	dec := chain.Resolve(in)
	if dec.Hold {
		dec = pastAdmin(in)
	}
```

and change its doc comment's "and no hold for the instance quota" to "and no hold: it goes past the instance quota and the admin limits (pastAdmin)".

**Step 4: Run them, then the panel and quota tests**

```bash
DBTEST ./internal/engine/ -run 'Start|Member|Panel|Quota|Limit'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/engine/chain.go internal/engine/panel.go internal/engine/e2e_limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(engine): a run asked for goes past the admin limits, never a team's own" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 11: Chain B turns the panel off; Job P's judge ignores limits

**Files:**
- Modify: `internal/engine/panel.go:120-141` (`panelOffReason`), `:539-560` (`resolveMerge`)
- Test: `internal/engine/panel_test.go:427-455` (`TestPanelOffReason`), `internal/engine/e2e_limits_test.go`

**Step 1: Write the failing tests**

In `TestPanelOffReason`, add these cases to the table:

```go
		{"model limit only", []string{chain.ReasonModelLimit}, panelOffQuota},
		{"team limit and same model", []string{chain.ReasonTeamLimit, chain.ReasonSameModel}, panelOffQuota},
		{"quota and a team limit", []string{chain.ReasonInstanceQuota, chain.ReasonTeamLimit}, panelOffQuota},
		{"a limit and a mark", []string{chain.ReasonTeamLimit, store.ClassQuota}, panelOffNone},
```

Append to `e2e_limits_test.go`:

```go
// Chain B with no entry left under its limits turns the panel off with the
// quota's reason; the review runs on chain A alone.
func TestDispatchPanelLimitStopsB(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 0, 0)
	b := globalChainB(t, env)
	seedEntryUsage(t, env, b, 10)
	setLimit(t, env, b, 0, 10)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantOnlyA(t, env, id, "the token quota stopped model B")
}

// Job P's judge does not check limits: it ends a review that has started.
func TestPanelJudgeIgnoresLimits(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	env.SetNow(novemberMid)
	head := sha('a')
	id, a, b := releasedPanel(t, e, head, withTokens(t, clean(t, head), 5, 5), withTokens(t, clean(t, head), 5, 5))
	team := ownerTeam(t, env)
	setLimit(t, env, store.EntryRef{Source: a.Entry.Source, ID: a.Entry.ID}, team, 1) // used up by member A's own tokens
	setLimit(t, env, store.EntryRef{Source: b.Entry.Source, ID: b.Entry.ID}, team, 1)

	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	if js := panelJudges(t, env, id); len(js) != 2 || js["a"].Name != "team-a" || js["b"].Name != "team-b" {
		t.Errorf("judges = %+v; want both, limits or not", js)
	}
}
```

(`releasedPanel` returns the members as they were when they started; `Entry` is set on them by then. If it is empty, read them again with `memberOfRole(t, env, id, "a")`.)

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/engine/ -run 'TestPanelOffReason|TestDispatchPanelLimitStopsB|TestPanelJudgeIgnoresLimits'
```

Expected: `TestPanelOffReason` FAIL on the 3 limit cases (`= "no entry of chain B can be used", want "the token quota stopped model B"`); `TestDispatchPanelLimitStopsB` FAIL (the off row's reason is `no entry of chain B can be used`); `TestPanelJudgeIgnoresLimits` FAIL (`judges = map[]`).

**Step 3: Write the code**

`panelOffReason`: the quota case becomes

```go
		case chain.ReasonInstanceQuota, chain.ReasonModelLimit, chain.ReasonTeamLimit:
			quota = true
```

and its comment: "The quota is the reason when only the quota, the limits and A's model stopped the entries ...".

`resolveMerge`, right after `inA.Tried, inB.Tried = ...` (line 557):

```go
	// The judge ends a review that has started: no limit stops it.
	inA.IgnoreLimits()
	inB.IgnoreLimits()
```

and add to its doc comment: "Limits are not checked: its tokens count on its entries all the same."

**Step 4: Run them and the panel tests**

```bash
DBTEST ./internal/engine/ -run 'Panel'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/engine/panel.go internal/engine/panel_test.go internal/engine/e2e_limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(engine): limits turn chain B off; Job P's judge ignores them" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 12: An API client's pinned model at its model limit fails

**Files:**
- Modify: `internal/engine/apireview.go:117-131`
- Test: `internal/engine/e2e_limits_test.go`

**Step 1: Write the failing test** (append)

```go
// A client that pinned a model at its model limit fails, naming the entry: a
// pinned model is a chain of 1, and an API review is never held.
func TestAPIReviewPinnedModelAtLimitFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), 0, 10)
	id := a.ask(t, "route:1", twoFiles())
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = store.ConfigModelID }); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatal("a pinned model at its model limit got a Job")
	}
	publish(t, env)
	a.wantFailed(t, id, reviewapi.StepStarting, "all models exhausted: "+configName(t, env)+" (model limit used up)")
}
```

**Step 2: Run and watch it fail**

```bash
DBTEST ./internal/engine/ -run TestAPIReviewPinnedModelAtLimitFails
```

Expected: FAIL `a pinned model at its model limit got a Job`.

**Step 3: Write the code** in `dispatchAPI`, right after the `if client.ModelID != "" && (...) { ... }` block (line 131), before the `switch`:

```go
	if d.stage == stageReview && client.ModelID != "" {
		// A pinned model is a chain of 1 (chainA): its model limit stops it,
		// and an API review is never held, so it fails.
		in, err := d.chainA(ctx, settings.Effective{})
		if err != nil {
			return d.retry(ctx, fmt.Errorf("read the model chain: %w", err))
		}
		if dec := chain.Resolve(in); dec.Hold {
			d.log.Warn("the model chosen for this API client is at its model limit", "model_id", client.ModelID)
			return d.fail(ctx, chain.FailureText(dec))
		}
	}
```

**Step 4: Run it and the API tests**

```bash
DBTEST ./internal/engine/ -run 'API'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/engine/apireview.go internal/engine/e2e_limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(engine): a pinned API model at its model limit fails" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 13: Requests by hand ask the limits; an admin's 409

**Files:**
- Modify: `internal/actions/actions.go` (statuses 60-75, `RerunResult` 95-101, `Rerun` 133-150, `quotaUsedUp` 170-198 becomes `refusal`, `Start` 222-238)
- Modify: `internal/actions/chain_quota_test.go:16-61` (`teamChainModels` calls a new helper), `internal/actions/quota_test.go:56-81` (`seedUsage` calls a new helper)
- Create: `internal/actions/limits_test.go`
- Modify: `internal/dashboard/api_commands.go` (const ~32, rerun switch ~247-263, start switch ~302-312)
- Modify: `internal/engine/commands.go:631-632`, `:704-705`
- Test: `internal/dashboard/quota_test.go`, `internal/engine/e2e_limits_test.go`

**Step 1: Refactor the 2 test helpers (no behaviour change)**

In `quota_test.go`, `seedUsage(t, st, iid, at, tokens)` becomes a call to

```go
// seedUsageOn is seedUsage of a run that ran on ref (nil: no entry, as
// before chains).
func seedUsageOn(t *testing.T, st *store.Store, iid int64, at time.Time, tokens int64, ref *store.EntryRef)
```

which is today's body plus, right after `ClaimQueued`:

```go
	if ref != nil {
		if err := st.SetRunEntry(ctx, id, store.RunEntry{Source: ref.Source, ID: ref.ID, Name: ref.ID, Position: 1, Length: 1}); err != nil {
			t.Fatal(err)
		}
	}
```

In `chain_quota_test.go`, move `teamChainModels`'s body into

```go
// chainModels is a registry over st where team Payments owns main/acme/web
// and its chain is the global entry "global" (teams may use it), then, with
// teamEntry, the team's own "team-a"; and the team and refs.
type chainModels struct {
	reg          *instances.Registry
	team         int64
	global, own  store.EntryRef // own is zero without the team entry
}

func newChainModels(t *testing.T, st *store.Store, teamEntry bool) chainModels
```

and `teamChainModels` returns `newChainModels(t, st, teamEntry).reg`. Run `DBTEST ./internal/actions/` and expect `ok` before going on.

**Step 2: Write the failing tests**

`internal/actions/limits_test.go`:

```go
package actions

import (
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// limitRepo is main/acme/web with roomy caps and no quota.
func limitRepo() settings.Effective {
	return settings.Effective{Instance: "main", Project: "acme/web", RerunLimitPerMR: 10, RerunLimitPerRepo: 50}
}

func setLimit(t *testing.T, st *store.Store, ref store.EntryRef, team, tokens int64) {
	t.Helper()
	if err := st.SetModelLimit(ctx, store.ModelLimit{Ref: ref, TeamID: team, MonthlyTokens: tokens, UpdatedAt: quotaNow, UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
}

// With no quota used up, a maintainer's re-run and start ask the limits: they
// go ahead while an entry has room, and are refused as for the quota when
// limits alone stop every entry.
func TestRequestsByHandAskTheLimits(t *testing.T) {
	st := newStore(t)
	m := newChainModels(t, st, true)
	seedUsageOn(t, st, 1000, quotaNow.Add(-time.Hour), 10, &m.global)
	seedUsageOn(t, st, 1001, quotaNow.Add(-time.Hour), 10, &m.own)
	setLimit(t, st, m.global, 0, 10)

	req := rerunRequest(limitRepo())
	req.Models = m.reg
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQueued {
		t.Fatalf("team entry with room: Rerun = %+v, %v; want queued", res, err)
	}
	setLimit(t, st, m.own, m.team, 10)
	req.Head = "head2"
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("every entry at a limit: Rerun = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
	sreq := startRequest(limitRepo())
	sreq.Models, sreq.Head = m.reg, "head3"
	if res, err := Start(ctx, st, sreq); err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("Start = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
}

// An admin's request goes past the admin limits to the first global entry
// with no mark, and is refused, naming the team, only when nothing but team
// entries at their team's own limit is left.
func TestAdminRequestStopsAtTeamOwnLimit(t *testing.T) {
	st := newStore(t)
	m := newChainModels(t, st, true)
	seedUsageOn(t, st, 1000, quotaNow.Add(-time.Hour), 10, &m.global)
	seedUsageOn(t, st, 1001, quotaNow.Add(-time.Hour), 10, &m.own)
	setLimit(t, st, m.global, 0, 10)
	setLimit(t, st, m.global, m.team, 10)
	setLimit(t, st, m.own, m.team, 10)

	req := rerunRequest(limitRepo())
	req.Models, req.PastQuota = m.reg, true
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQueued {
		t.Fatalf("a global entry past the admin limits: Rerun = %+v, %v; want queued", res, err)
	}
	if err := st.SaveChain(ctx, store.Chain{Scope: store.ScopeTeam, TeamID: m.team, Entries: []store.EntryRef{m.own}}, quotaNow); err != nil {
		t.Fatal(err)
	}
	req.Head = "head2"
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunTeamLimit || res.Team != "Payments" {
		t.Fatalf("only the team's entry: Rerun = %+v, %v; want RerunTeamLimit for Payments", res, err)
	}
	sreq := startRequest(limitRepo())
	sreq.Models, sreq.PastQuota, sreq.Head = m.reg, true, "head3"
	if res, err := Start(ctx, st, sreq); err != nil || res.Status != RerunTeamLimit || res.Team != "Payments" {
		t.Fatalf("Start = %+v, %v; want RerunTeamLimit for Payments", res, err)
	}
}
```

Append to `internal/dashboard/quota_test.go`:

```go
// An admin's start of a change whose chain is only a team entry at its
// team's own limit is refused with the team's name; a maintainer's gets the
// quota's refusal. The change stays waiting.
func TestStartAtTeamOwnLimit(t *testing.T) {
	te := newTeamsEnv(t) // Widgets owns main/acme/widgets, the button repository
	m := te.addTeamModel(te.otherMaintainer, te.widgets, "wid-a", teamKeyOne)
	ref := store.EntryRef{Source: store.SourceTeam, ID: m.ID}
	if err := te.st.SaveChain(ctx, store.Chain{Scope: store.ScopeTeam, TeamID: te.widgets, Entries: []store.EntryRef{ref}}, te.clock.now()); err != nil {
		t.Fatal(err)
	}
	used := te.usageRun(runFix{project: buttonProject, number: 99, head: head('9')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	te.exec(`UPDATE runs SET model_source = 'team', model_entry_id = $2 WHERE id = $1::uuid`, used, m.ID)
	if err := te.st.SetModelLimit(ctx, store.ModelLimit{Ref: ref, TeamID: te.widgets, MonthlyTokens: 10, UpdatedAt: te.clock.now(), UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	id := te.waitingChange(head('1'), store.WaitLimit)

	rec := te.send(te.admin, "POST", buttonURL("start"), `{"head":"`+head('1')+`"}`)
	wantCode(t, rec, "admin start", http.StatusConflict)
	if got := errorOf(t, rec); got != "every model of the chain is at the team's own limit: a maintainer of Widgets can raise it on the team page" {
		t.Errorf("error = %q", got)
	}
	rec = te.send(te.otherMaintainer, "POST", buttonURL("start"), `{"head":"`+head('1')+`"}`)
	wantCode(t, rec, "maintainer start", http.StatusConflict)
	if got := errorOf(t, rec); got != msgQuotaUsedUp {
		t.Errorf("maintainer's error = %q; want %q", got, msgQuotaUsedUp)
	}
	rows := te.runRows()
	if i := slices.IndexFunc(rows, func(r runRow) bool { return r.ID == id }); i < 0 || rows[i].State != "waiting" {
		t.Errorf("runs = %+v; want the change still waiting", rows)
	}
}
```

Append to `internal/engine/e2e_limits_test.go`:

```go
// A writer's review command is refused when only limits stop every entry:
// 👎 and the quota's note, as for a used-up quota.
func TestReviewCommandRefusedAtModelLimit(t *testing.T) {
	e := quotaCommands(t, 0)
	env := e.env
	publishedHead(t, env, sha('a'))
	seedEntryUsage(t, env, configRef(), 5) // 5 of the repository's 10: room left
	setLimit(t, env, configRef(), 0, 5)

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, quotaNote)
	wantOutcome(t, env, "refused: "+whyQuota, 1)
}
```

**Step 3: Run and watch them fail**

```bash
DBTEST ./internal/actions/ -run 'TestRequestsByHandAskTheLimits|TestAdminRequestStopsAtTeamOwnLimit'
DBTEST ./internal/dashboard/ -run TestStartAtTeamOwnLimit
DBTEST ./internal/engine/ -run TestReviewCommandRefusedAtModelLimit
```

Expected: actions build failure `undefined: RerunTeamLimit` (comment out the admin test's 2 `RerunTeamLimit` checks to see `TestRequestsByHandAskTheLimits` fail with `Rerun = {Status:0 ...}; want RerunQuotaUsedUp`, then put them back). Dashboard FAIL (`admin start = 202`). Engine FAIL (reactions `eyes, thumbsup` / no refusal outcome).

**Step 4: Write the code**

`internal/actions/actions.go`:

1. After `RerunQuotaUsedUp`:

```go
	// RerunTeamLimit: an admin's request, and the chain has nothing left but
	// team entries at their team's own monthly limit, which is the team's
	// money; nothing was queued. RerunResult.Team names the team.
	RerunTeamLimit
```

2. `RerunResult` gets `Team string` with "Team is the owner team of a RerunTeamLimit."

3. In `Rerun`, replace the `used, err := quotaUsedUp(...)` block (lines 135-150) with:

```go
	refuse, team, err := refusal(ctx, st, req.PastQuota, req.Now, req.MR, req.Repo, req.Models)
	if err != nil {
		return RerunResult{}, err
	}
	if refuse != RerunQueued {
		// A review of the head that is queued or running is on its way: the
		// request is answered as any request for it is, not refused.
		active, err := st.ReviewActiveForRerun(ctx, req.MR, req.Head, req.TargetBranch, !req.FailedOnly)
		if err != nil {
			return RerunResult{}, fmt.Errorf("look for the head's review: %w", err)
		}
		if active {
			return RerunResult{Status: RerunAlreadyQueued}, nil
		}
		return RerunResult{Status: refuse, Team: team}, nil
	}
```

and the same shape in `Start` (lines 224-238, with `ReviewActiveForStart`).

4. Replace `quotaUsedUp` (lines 170-198) with:

```go
// refusal is why a request for the change mr is refused before anything is
// queued: RerunQuotaUsedUp, RerunTeamLimit (team names the owner team), or
// RerunQueued when it may go ahead. The repository's quota counts every token
// and refuses. Then the chain in effect is read as the dispatcher reads it,
// with the instance quota and the limits (owner ruling 2026-11-20): a request
// is refused when they alone stop every entry, and, under a used-up instance
// quota, whenever no entry is left. models nil refuses on the instance quota
// as before chains, and reads no limit. An admin's request (pastQuota) is
// adminRefusal's.
func refusal(ctx context.Context, st *store.Store, pastQuota bool, now time.Time, mr store.MRKey, repo settings.Effective, models *instances.Registry) (RerunStatus, string, error) {
	if pastQuota {
		return adminRefusal(ctx, st, now, mr, repo, models)
	}
	repoUsed, instanceUsed, err := quota.Check(ctx, st, now, mr.Instance, mr.ProjectID, repo)
	if err != nil {
		return 0, "", fmt.Errorf("check the token quota: %w", err)
	}
	switch {
	case repoUsed, models == nil && instanceUsed:
		return RerunQuotaUsedUp, "", nil
	case models == nil:
		return RerunQueued, "", nil
	}
	// A new review: no attempt of it ran yet.
	in, err := chain.Load(ctx, st, models, now, store.Run{MR: mr}, repo)
	if err != nil {
		return 0, "", fmt.Errorf("read the model chain: %w", err)
	}
	in.InstanceQuotaUsedUp = instanceUsed
	if dec := chain.Resolve(in); dec.Hold || (instanceUsed && !dec.Runs()) {
		return RerunQuotaUsedUp, "", nil
	}
	return RerunQueued, "", nil
}

// adminRefusal is refusal for an admin's request: it goes past every quota
// and the admin limits (chain.Input.PastAdminLimits), so the dispatcher runs
// it on the first entry with room, else on the first global entry with no
// mark. It is refused only when nothing but team entries at their team's own
// limit is left. A chain left with no entry for another reason goes ahead and
// fails at dispatch, as before.
func adminRefusal(ctx context.Context, st *store.Store, now time.Time, mr store.MRKey, repo settings.Effective, models *instances.Registry) (RerunStatus, string, error) {
	if models == nil {
		return RerunQueued, "", nil
	}
	in, err := chain.Load(ctx, st, models, now, store.Run{MR: mr}, repo)
	if err != nil {
		return 0, "", fmt.Errorf("read the model chain: %w", err)
	}
	in.PastAdminLimits()
	if !chain.Resolve(in).Hold {
		return RerunQueued, "", nil
	}
	t, _, err := st.Team(ctx, in.OwnerTeam)
	if err != nil {
		return 0, "", fmt.Errorf("read the owner team: %w", err)
	}
	return RerunTeamLimit, t.Name, nil
}
```

Update the doc comments of `Rerun` and `Start` ("then the token quota and the model limits (an admin's request goes past the quotas and the admin limits, not a team's own) ...") and of `RerunRequest.PastQuota` ("goes past a used-up quota and the admin limits").

`internal/dashboard/api_commands.go`: add the constant

```go
	msgTeamLimitUsedUp = "every model of the chain is at the team's own limit: a maintainer of %s can raise it on the team page"
```

and in both `rerun` and `start`, after `case actions.RerunQuotaUsedUp:`

```go
	case actions.RerunTeamLimit:
		writeError(w, http.StatusConflict, fmt.Sprintf(msgTeamLimitUsedUp, res.Team))
```

`internal/engine/commands.go`: both `case actions.RerunQuotaUsedUp:` (lines 631 and 704) become `case actions.RerunQuotaUsedUp, actions.RerunTeamLimit:`. A command is never an admin's request, so the second never comes, but a status left out would read as queued.

**Step 5: Run them, then the 3 packages' quota and command tests**

```bash
DBTEST ./internal/actions/
DBTEST ./internal/dashboard/ -run 'Quota|Start|Rerun'
DBTEST ./internal/engine/ -run 'Command|Quota|Limit'
```

Expected: `ok` for each.

**Step 6: Commit**

```bash
git add internal/actions/ internal/dashboard/api_commands.go internal/dashboard/quota_test.go internal/engine/commands.go internal/engine/e2e_limits_test.go
git -c user.email="$(git config --local user.email)" commit -m "feat(actions): requests by hand ask the limits; an admin stops at a team's own" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 14: Dashboard: the waiting reason and the chain states

**Files:**
- Modify: `internal/dashboard/outcome.go:71-73`
- Modify: `internal/dashboard/view.go:581-595` (`WaitingView` comment), `:1570-1590` (`ChainEntryView` comment)
- Modify: `openapi.yaml` (`WaitingView.reason` enum ~5952, `ChainEntryView.state` enum ~6699)
- Test: `internal/dashboard/quota_test.go`, `internal/dashboard/openapi_test.go:1445-1446`

**Step 1: Write the failing tests**

Append to `quota_test.go`:

```go
// A run a limit holds reads as one the quota holds in the runs lists.
func TestJudgeLimitWaiting(t *testing.T) {
	got := judge(store.RunDigest{Outcome: OutcomeWaiting, State: store.Waiting, SkipReason: store.WaitLimit})
	if got.outcome != OutcomeWaiting || got.reason != reasonQuotaUsedUp || got.status != "" {
		t.Errorf("judge = %+v; want waiting / %q", got, reasonQuotaUsedUp)
	}
}
```

In `TestOpenAPIEnumsMatchCode`, the `ChainEntryView` `state` case gets `chain.ReasonModelLimit, chain.ReasonTeamLimit` at its end, and add a case:

```go
		{"WaitingView", "reason", []string{store.WaitUntrusted, store.WaitCheckFailed, store.WaitQuota, store.WaitLimit}},
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/dashboard/ -run 'TestJudgeLimitWaiting|TestOpenAPIEnumsMatchCode'
```

Expected: `TestJudgeLimitWaiting` FAIL (reason `waiting for someone to start the review` or the like); `TestOpenAPIEnumsMatchCode` FAIL on `ChainEntryView.state` and `WaitingView.reason`.

**Step 3: Write the code**

`outcome.go`: `if r.SkipReason == store.WaitQuota || r.SkipReason == store.WaitLimit {`.

`openapi.yaml`:
- `WaitingView.reason`: `enum: [untrusted_author, trust_check_failed, quota_used_up, limit_used_up]`, and the description ends: "... or the monthly token quota is used up (an admin starts it). `limit_used_up` is that hold when a model limit or a team limit took part."
- `ChainEntryView.state`: `enum: [ready, exhausted, key_rejected, removed, not_allowed, not_owner, key_unusable, model_limit, team_limit]`, and the description gets: "`model_limit` and `team_limit`: its model limit, or the owner team's limit on it, is used up this month (a repository's chain only)."

`view.go`: the `WaitingView` comment lists `"limit_used_up"` (held like `quota_used_up`, a limit took part); the `ChainEntryView` comment lists `"model_limit"` and `"team_limit"`.

`stateOf` needs no change: it passes these reasons through.

**Step 4: Run the dashboard openapi and outcome tests**

```bash
DBTEST ./internal/dashboard/ -run 'OpenAPI|Judge|Outcome'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/dashboard/outcome.go internal/dashboard/view.go internal/dashboard/quota_test.go internal/dashboard/openapi_test.go openapi.yaml
git -c user.email="$(git config --local user.email)" commit -m "feat(dashboard): the limit wait reason and chain states in the API" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 15: Admin routes for model limits and team limits

**Files:**
- Create: `internal/dashboard/api_limits.go`
- Modify: `internal/dashboard/api_operator.go:75-80` (routes)
- Modify: `openapi.yaml` (2 paths after `/api/admin/models/{id}/mark` ~3763; parameter `TeamId` after `ModelId` ~4915; schema `ModelLimitRequest`)
- Create: `internal/dashboard/api_limits_test.go`

**Step 1: Write the failing tests**

`internal/dashboard/api_limits_test.go`:

```go
package dashboard

import (
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func modelLimitURL(id string) string { return modelsURL + "/" + id + "/limit" }

func teamLimitURL(id string, team int64) string {
	return modelsURL + "/" + id + "/team-limits/" + strconv.FormatInt(team, 10)
}

// storedLimits is every limit in the store.
func (e *env) storedLimits() []store.ModelLimit {
	e.t.Helper()
	got, err := e.st.ModelLimits(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// An admin sets and clears the model limit of a stored entry and of the
// config file's: 204 each time, a clear of none too; each set and each real
// clear is audited with the entry, all teams and the number.
func TestAdminModelLimit(t *testing.T) {
	t.Setenv("PERGENT_TEST_LLM_KEY", "config-key")
	e := newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	})
	c := e.operator()
	m := e.addModel("openai", "gpt-5", "", llmKeyThree)
	name := modelNamed(t, decode[ModelsAdminView](t, e.get(c, modelsURL), "GET models", http.StatusOK), m.ID).Name
	for _, id := range []string{m.ID, store.ConfigModelID} {
		wantCode(t, e.send(c, "PUT", modelLimitURL(id), `{"monthlyTokens":5000}`), "PUT "+id, http.StatusNoContent)
	}
	wantCode(t, e.send(c, "PUT", modelLimitURL(m.ID), `{"monthlyTokens":6000}`), "PUT again", http.StatusNoContent)
	if got := e.storedLimits(); len(got) != 2 || got[0].Ref.ID != store.ConfigModelID || got[1].MonthlyTokens != 6000 || got[1].UpdatedBy != "root (password)" {
		t.Errorf("stored = %+v", got)
	}
	wantCode(t, e.send(c, "DELETE", modelLimitURL(m.ID), ""), "DELETE", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", modelLimitURL(m.ID), ""), "DELETE none", http.StatusNoContent)
	wantCode(t, e.send(c, "PUT", modelLimitURL("no-such-model"), `{"monthlyTokens":1}`), "unknown entry", http.StatusNotFound)
	if got, want := auditDetails(e, "model_limit_set"), []string{
		name + " for all teams: monthly_tokens=5000", "anthropic/from-config for all teams: monthly_tokens=5000", name + " for all teams: monthly_tokens=6000",
	}; !slices.Equal(got, want) {
		t.Errorf("model_limit_set = %q; want %q", got, want)
	}
	if got := auditDetails(e, "model_limit_cleared"); !slices.Equal(got, []string{name + " for all teams: monthly_tokens=6000"}) {
		t.Errorf("model_limit_cleared = %q", got)
	}
}

// The number is a whole number from 1 to 10^15, written as a JSON number;
// anything else is 400 with the range and changes nothing.
func TestModelLimitBounds(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	m := e.addModel("openai", "gpt-5", "", llmKeyThree)
	for _, body := range []string{`{"monthlyTokens":0}`, `{"monthlyTokens":-1}`, `{"monthlyTokens":1000000000000001}`,
		`{"monthlyTokens":1.5}`, `{"monthlyTokens":1e3}`, `{"monthlyTokens":"5"}`, `{"monthlyTokens":null}`, `{}`, `{"monthlyTokens":5,"x":1}`} {
		rec := e.send(c, "PUT", modelLimitURL(m.ID), body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if got := errorOf(t, rec); got != msgBadLimit {
			t.Errorf("%s: error = %q", body, got)
		}
	}
	if got := e.storedLimits(); len(got) != 0 {
		t.Fatalf("stored = %+v; want none", got)
	}
	for _, body := range []string{`{"monthlyTokens":1}`, `{"monthlyTokens":1000000000000000}`} {
		wantCode(t, e.send(c, "PUT", modelLimitURL(m.ID), body), body, http.StatusNoContent)
	}
}

// An admin sets a team's limit on a global entry, teams may use it or not;
// an unknown team is 404; the route is not found for anyone else.
func TestAdminTeamLimit(t *testing.T) {
	te := newTeamsEnv(t)
	m := te.addModel("openai", "gpt-5", "", llmKeyThree)
	name := modelNamed(t, decode[ModelsAdminView](t, te.get(te.admin, modelsURL), "GET models", http.StatusOK), m.ID).Name
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(m.ID, te.payments), `{"monthlyTokens":300}`), "PUT", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(m.ID, 999999), `{"monthlyTokens":300}`), "unknown team", http.StatusNotFound)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL("no-such-model", te.payments), `{"monthlyTokens":300}`), "unknown entry", http.StatusNotFound)
	for name, c := range map[string]*http.Cookie{"maintainer": te.maintainer, "viewer": te.viewer} {
		wantCode(t, te.send(c, "PUT", teamLimitURL(m.ID, te.payments), `{"monthlyTokens":1}`), name, http.StatusNotFound)
		wantCode(t, te.send(c, "PUT", modelLimitURL(m.ID), `{"monthlyTokens":1}`), name, http.StatusNotFound)
	}
	if got := te.storedLimits(); len(got) != 1 || got[0].TeamID != te.payments || got[0].MonthlyTokens != 300 {
		t.Errorf("stored = %+v", got)
	}
	wantCode(t, te.send(te.admin, "DELETE", teamLimitURL(m.ID, te.payments), ""), "DELETE", http.StatusNoContent)
	if got := auditDetails(te.env, "model_limit_set"); !slices.Equal(got, []string{name + " for Payments: monthly_tokens=300"}) {
		t.Errorf("model_limit_set = %q", got)
	}
	if got := auditDetails(te.env, "model_limit_cleared"); !slices.Equal(got, []string{name + " for Payments: monthly_tokens=300"}) {
		t.Errorf("model_limit_cleared = %q", got)
	}
}
```

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/dashboard/ -run 'TestAdminModelLimit|TestModelLimitBounds|TestAdminTeamLimit'
```

Expected: build failure `undefined: msgBadLimit`; once that constant exists, FAIL with 404 / 405 on every `PUT` (no route).

**Step 3: Write the code**

`internal/dashboard/api_limits.go`:

```go
package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgBadLimit       = "monthlyTokens must be a whole number from 1 to 1000000000000000"
	msgNotTeamModel   = "the model is not one of this team's models"
	msgAdminLimit     = "only admins set a team's limit on a global model"
	msgTeamLimitsRead = "only maintainers of this team see its model limits"
)

// globalEntry is the global entry id, the config file's included (config,
// config-1 to config-4).
func (d *Dashboard) globalEntry(id string) (instances.Model, bool) {
	for _, m := range d.registry.GlobalModels() {
		if m.ID == id {
			return m, true
		}
	}
	return instances.Model{}, false
}

// limitBody reads {"monthlyTokens": n} (quotaValue: a whole number from 1 to
// config.MaxMonthlyTokenQuota). On anything else it has answered 400.
func limitBody(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var in struct {
		MonthlyTokens json.RawMessage `json:"monthlyTokens"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadLimit)
		return 0, false
	}
	n, ok := quotaValue(in.MonthlyTokens)
	if !ok || n == nil {
		writeError(w, http.StatusBadRequest, msgBadLimit)
		return 0, false
	}
	return *n, true
}

// limitDetail is how a limit is audited: the entry's name, the team's name
// ("" for the model limit: all teams) and the number. Never a key.
func limitDetail(entry, team string, n int64) string {
	if team == "" {
		team = "all teams"
	}
	return fmt.Sprintf("%s for %s: monthly_tokens=%d", entry, team, n)
}

// setLimit stores l, answers 204 and audits it.
func (d *Dashboard) setLimit(w http.ResponseWriter, r *http.Request, l store.ModelLimit, entry, team string) {
	ctx, se := r.Context(), session(r)
	l.UpdatedAt, l.UpdatedBy = d.now(), actorLabel(se.Kind, se.Username)
	if err := d.Store.SetModelLimit(ctx, l); err != nil {
		d.Log.Error("dashboard: setting a model limit failed", "source", l.Ref.Source, "model_id", l.Ref.ID, "team", l.TeamID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.audit(ctx, se, "model_limit_set", "", "", limitDetail(entry, team, l.MonthlyTokens))
	w.WriteHeader(http.StatusNoContent)
}

// clearLimit removes the limit of ref for team (0: the model limit): 204
// whether there was one or not; one removed is audited with its number.
func (d *Dashboard) clearLimit(w http.ResponseWriter, r *http.Request, ref store.EntryRef, teamID int64, entry, team string) {
	ctx, se := r.Context(), session(r)
	old, found, err := d.Store.ClearModelLimit(ctx, ref, teamID)
	if err != nil {
		d.Log.Error("dashboard: clearing a model limit failed", "source", ref.Source, "model_id", ref.ID, "team", teamID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if found {
		d.audit(ctx, se, "model_limit_cleared", "", "", limitDetail(entry, team, old))
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT and DELETE /api/admin/models/{id}/limit ---

func (d *Dashboard) putModelLimit(w http.ResponseWriter, r *http.Request) {
	m, ok := d.globalEntry(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	n, ok := limitBody(w, r)
	if !ok {
		return
	}
	d.setLimit(w, r, store.ModelLimit{Ref: chain.RefOf(m), MonthlyTokens: n}, m.Name, "")
}

func (d *Dashboard) deleteModelLimit(w http.ResponseWriter, r *http.Request) {
	m, ok := d.globalEntry(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	d.clearLimit(w, r, chain.RefOf(m), 0, m.Name, "")
}

// --- PUT and DELETE /api/admin/models/{id}/team-limits/{teamId} ---

// teamLimitTarget is the global entry {id} and the team {teamId}; ok is false
// when it has answered (404 for either unknown).
func (d *Dashboard) teamLimitTarget(w http.ResponseWriter, r *http.Request) (instances.Model, store.Team, bool) {
	m, ok := d.globalEntry(r.PathValue("id"))
	teamID, err := strconv.ParseInt(r.PathValue("teamId"), 10, 64)
	if !ok || err != nil || teamID <= 0 {
		notFound(w)
		return instances.Model{}, store.Team{}, false
	}
	t, found, err := d.Store.Team(r.Context(), teamID)
	if err != nil {
		d.failed(w, r, "reading the team", err)
		return instances.Model{}, store.Team{}, false
	}
	if !found {
		notFound(w)
		return instances.Model{}, store.Team{}, false
	}
	return m, t, true
}

func (d *Dashboard) putTeamLimit(w http.ResponseWriter, r *http.Request) {
	m, t, ok := d.teamLimitTarget(w, r)
	if !ok {
		return
	}
	n, ok := limitBody(w, r)
	if !ok {
		return
	}
	d.setLimit(w, r, store.ModelLimit{Ref: chain.RefOf(m), TeamID: t.ID, MonthlyTokens: n}, m.Name, t.Name)
}

func (d *Dashboard) deleteTeamLimit(w http.ResponseWriter, r *http.Request) {
	m, t, ok := d.teamLimitTarget(w, r)
	if !ok {
		return
	}
	d.clearLimit(w, r, chain.RefOf(m), t.ID, m.Name, t.Name)
}
```

`api_operator.go`, after the `.../mark` route:

```go
	d.adminRoute(mux, "PUT /api/admin/models/{id}/limit", d.putModelLimit)
	d.adminRoute(mux, "DELETE /api/admin/models/{id}/limit", d.deleteModelLimit)
	d.adminRoute(mux, "PUT /api/admin/models/{id}/team-limits/{teamId}", d.putTeamLimit)
	d.adminRoute(mux, "DELETE /api/admin/models/{id}/team-limits/{teamId}", d.deleteTeamLimit)
```

`openapi.yaml`, after `/api/admin/models/{id}/mark`:

```yaml
  /api/admin/models/{id}/limit:
    put:
      operationId: setModelLimit
      tags: [Instances and settings]
      summary: Set a global entry's model limit
      description: |
        Admins only. The most tokens (input plus output) reviews may spend on
        the entry in a UTC calendar month, on every instance and repository,
        API reviews included. When it is used up, reviews skip the entry.
        `{id}` takes `config` and `config-1` to `config-4` too. A number out
        of range is 400 with the range. Audited as `model_limit_set`.
      x-pergent-access: admin
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: '#/components/parameters/ModelId'
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/ModelLimitRequest'}
      responses:
        '204':
          description: The limit is set.
        '400': {$ref: '#/components/responses/BadRequest'}
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
    delete:
      operationId: clearModelLimit
      tags: [Instances and settings]
      summary: Remove a global entry's model limit
      description: |
        Admins only. 204 whether it had one or not; a limit removed is audited
        (`model_limit_cleared`, with the number it had).
      x-pergent-access: admin
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: '#/components/parameters/ModelId'
      responses:
        '204':
          description: No model limit now.
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
  /api/admin/models/{id}/team-limits/{teamId}:
    put:
      operationId: setTeamLimit
      tags: [Instances and settings]
      summary: Set a team's limit on a global entry
      description: |
        Admins only. The most tokens the repositories the team owns may spend
        on the entry in a UTC calendar month, whether teams may use the entry
        or not (the global chain reaches it too). When it is used up, those
        repositories skip the entry. A team limit may be larger than the
        model limit. 404 for an unknown entry or team. Audited as
        `model_limit_set`, with the team's name.
      x-pergent-access: admin
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: '#/components/parameters/ModelId'
        - $ref: '#/components/parameters/TeamId'
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/ModelLimitRequest'}
      responses:
        '204':
          description: The limit is set.
        '400': {$ref: '#/components/responses/BadRequest'}
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
    delete:
      operationId: clearTeamLimit
      tags: [Instances and settings]
      summary: Remove a team's limit on a global entry
      description: Admins only. 204 whether it had one or not; a limit removed is audited (`model_limit_cleared`).
      x-pergent-access: admin
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: '#/components/parameters/ModelId'
        - $ref: '#/components/parameters/TeamId'
      responses:
        '204':
          description: No team limit now.
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
```

Under `components.parameters`, after `ModelId`:

```yaml
    TeamId:
      name: teamId
      in: path
      required: true
      schema:
        type: integer
        format: int64
        minimum: 1
```

Under `components.schemas`, beside `InstanceTokenQuotaRequest`:

```yaml
    ModelLimitRequest:
      type: object
      required: [monthlyTokens]
      additionalProperties: false
      properties:
        monthlyTokens:
          type: integer
          format: int64
          minimum: 1
          maximum: 1000000000000000
          description: Tokens (input plus output) per UTC calendar month. Removing a limit is a `DELETE`.
```

Also extend the `ModelId` parameter's description: "`config` is the entry the config file supplies, `config-1` to `config-4` its `llm.fallbacks`."

**Step 4: Run them and the routes and openapi tests**

```bash
DBTEST ./internal/dashboard/ -run 'Limit|OpenAPI|Routes|AdminRoutes'
```

Expected: `ok`. `TestAdminRoutesAreRegisteredOnlyThroughTheHelper` passes only with both the `adminRoute` lines and the `x-pergent-access: admin` paths.

**Step 5: Commit**

```bash
git add internal/dashboard/api_limits.go internal/dashboard/api_limits_test.go internal/dashboard/api_operator.go openapi.yaml
git -c user.email="$(git config --local user.email)" commit -m "feat(dashboard): admin routes for model limits and team limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 16: Team routes: a team entry's limit, and the team's limits

**Files:**
- Modify: `internal/dashboard/api_limits.go`
- Modify: `internal/dashboard/api_team_models.go:21-34` (routes)
- Modify: `internal/dashboard/view.go` (3 view types after `TeamModelView`, ~1312)
- Modify: `internal/dashboard/openapi_test.go` (`openAPISchemas`, ~935)
- Modify: `openapi.yaml` (2 paths after `/api/teams/{id}/models/{model}/mark` ~3074; 3 schemas after `TeamModelView`)
- Test: `internal/dashboard/api_limits_test.go`

**Step 1: Write the failing tests** (append; add `"strings"` to the imports)

```go
// Who sets a team entry's limit: a maintainer of the team and an admin. A
// viewer and another team's maintainer get 403, anyone else 404; another
// team's entry is 400; a global entry is the admins' (403).
func TestTeamModelLimitAccess(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "pay-a", teamKeyOne)
	other := te.addTeamModel(te.otherMaintainer, te.widgets, "wid-a", teamKeyTwo)
	g := te.addModel("openai", "gpt-5", "", llmKeyThree)
	put := func(c *http.Cookie, model string) *httptest.ResponseRecorder {
		return te.send(c, "PUT", teamURL(te.payments, "/models/"+model+"/limit"), `{"monthlyTokens":100}`)
	}
	wantCode(t, put(te.maintainer, own.ID), "maintainer", http.StatusNoContent)
	wantCode(t, put(te.admin, own.ID), "admin", http.StatusNoContent)
	for name, c := range map[string]*http.Cookie{"viewer": te.viewer, "other maintainer": te.otherMaintainer} {
		rec := put(c, own.ID)
		wantCode(t, rec, name, http.StatusForbidden)
		if got := errorOf(t, rec); got != msgTeamWrite {
			t.Errorf("%s: error = %q", name, got)
		}
	}
	wantCode(t, put(te.other, own.ID), "a viewer of another team", http.StatusNotFound)
	for name, c := range map[string]struct {
		model string
		code  int
		msg   string
	}{
		"another team's entry": {other.ID, http.StatusBadRequest, msgNotTeamModel},
		"no such entry":        {"no-such-model", http.StatusBadRequest, msgNotTeamModel},
		"a global entry":       {g.ID, http.StatusForbidden, msgAdminLimit},
	} {
		rec := put(te.maintainer, c.model)
		wantCode(t, rec, name, c.code)
		if got := errorOf(t, rec); got != c.msg {
			t.Errorf("%s: error = %q", name, got)
		}
	}
	wantCode(t, te.send(te.maintainer, "PUT", teamURL(te.payments, "/models/"+own.ID+"/limit"), `{"monthlyTokens":0}`), "bad number", http.StatusBadRequest)
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+own.ID+"/limit"), ""), "DELETE", http.StatusNoContent)
	if got := auditDetails(te.env, "model_limit_set"); !slices.Equal(got, []string{"pay-a for Payments: monthly_tokens=100", "pay-a for Payments: monthly_tokens=100"}) {
		t.Errorf("model_limit_set = %q", got)
	}
	if got := auditDetails(te.env, "model_limit_cleared"); !slices.Equal(got, []string{"pay-a for Payments: monthly_tokens=100"}) {
		t.Errorf("model_limit_cleared = %q", got)
	}
}

// The team's limits: its own entries with their limits and its use, and each
// global entry with an admin's limit for the team. A global entry's model
// limit shows only whether it has room: its numbers count other teams.
func TestTeamLimitsView(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "pay-a", teamKeyOne)
	g := te.addModel("openai", "gpt-5", "", llmKeyThree)
	te.addModel("openai", "gpt-6", "", llmKeyThree) // no limit for the team: not listed
	gName := modelNamed(t, decode[ModelsAdminView](t, te.get(te.admin, modelsURL), "GET models", http.StatusOK), g.ID).Name
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(g.ID, te.payments), `{"monthlyTokens":1000}`), "team limit", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", modelLimitURL(g.ID), `{"monthlyTokens":100}`), "model limit", http.StatusNoContent)
	wantCode(t, te.send(te.maintainer, "PUT", teamURL(te.payments, "/models/"+own.ID+"/limit"), `{"monthlyTokens":500}`), "own", http.StatusNoContent)
	used := te.usageRun(runFix{project: "acme/payments", number: 50, head: head('5')}, usageSkill("code-review", "no_blocking_issues", 120, 30))
	te.exec(`UPDATE runs SET model_source = 'global', model_entry_id = $2 WHERE id = $1::uuid`, used, g.ID)

	for name, c := range map[string]*http.Cookie{"maintainer": te.maintainer, "admin": te.admin} {
		rec := te.get(c, teamURL(te.payments, "/limits"))
		v := decode[TeamLimitsView](t, rec, name, http.StatusOK)
		if v.Team.ID != te.payments || len(v.Models) != 1 || len(v.Global) != 1 {
			t.Fatalf("%s: view = %+v", name, v)
		}
		if m := v.Models[0]; m.ID != own.ID || m.Name != "pay-a" || m.Limit == nil || *m.Limit != 500 || m.Used != 0 {
			t.Errorf("%s: own entry = %+v", name, m)
		}
		if gl := v.Global[0]; gl != (TeamGlobalLimitView{ID: g.ID, Name: gName, Limit: 1000, Used: 150, ModelLimit: "used_up"}) {
			t.Errorf("%s: global entry = %+v", name, gl)
		}
		if strings.Contains(rec.Body.String(), `"100"`) || strings.Contains(rec.Body.String(), `:100,`) || strings.Contains(rec.Body.String(), `:100}`) {
			t.Errorf("%s: the model limit's number reached the team: %s", name, rec.Body)
		}
	}
	rec := te.get(te.viewer, teamURL(te.payments, "/limits"))
	wantCode(t, rec, "viewer", http.StatusForbidden)
	if got := errorOf(t, rec); got != msgTeamLimitsRead {
		t.Errorf("viewer: error = %q", got)
	}
	wantCode(t, te.get(te.otherMaintainer, teamURL(te.payments, "/limits")), "another team's maintainer", http.StatusNotFound)
}
```

(Add `"net/http/httptest"` to the imports for `put`'s return type.)

**Step 2: Run and watch them fail**

```bash
DBTEST ./internal/dashboard/ -run 'TestTeamModelLimitAccess|TestTeamLimitsView'
```

Expected: build failure `undefined: TeamLimitsView`.

**Step 3: Write the code**

`view.go`, after `TeamModelView`:

```go
// TeamLimitsView is GET /api/teams/{id}/limits, for the team's maintainers
// and admins: each of the team's own entries with its limit and the team's
// use of it this month, and each global entry with an admin's limit for the
// team. Never another team's numbers.
type TeamLimitsView struct {
	Team   TeamRef               `json:"team"`
	Models []TeamModelLimitView  `json:"models"`
	Global []TeamGlobalLimitView `json:"global"`
}

// TeamModelLimitView is one of the team's entries: its limit (null: none,
// which a maintainer of the team sets) and its tokens this month.
type TeamModelLimitView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Limit *int64 `json:"limit"`
	Used  int64  `json:"used"`
}

// TeamGlobalLimitView is a global entry with an admin's limit for the team:
// that limit and the team's tokens on the entry this month, read-only for a
// maintainer. ModelLimit is the state of the entry's model limit alone,
// "none", "room_left" or "used_up": its numbers count other teams.
type TeamGlobalLimitView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Limit      int64  `json:"limit"`
	Used       int64  `json:"used"`
	ModelLimit string `json:"modelLimit"`
}
```

Append to `api_limits.go` (add `"github.com/gopaytech/pergent.sh/internal/quota"` to its imports):

```go
// The state of a global entry's model limit as a team page shows it.
const (
	limitNone     = "none"
	limitRoomLeft = "room_left"
	limitUsedUp   = "used_up"
)

// limitOf is the limit of ref for team (0: the model limit) among limits, nil
// for none.
func limitOf(limits []store.ModelLimit, ref store.EntryRef, team int64) *int64 {
	for _, l := range limits {
		if l.Ref == ref && l.TeamID == team {
			n := l.MonthlyTokens
			return &n
		}
	}
	return nil
}

// --- PUT and DELETE /api/teams/{id}/models/{model}/limit ---

// teamModelTarget is the team {id} and its own entry {model}, for a
// maintainer of the team or an admin (teamFor). A global entry is the
// admins' (403 to anyone else); any other ID is not one of the team's
// entries (400). ok is false when it has answered.
func (d *Dashboard) teamModelTarget(w http.ResponseWriter, r *http.Request) (store.Team, instances.Model, bool) {
	t, role, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return store.Team{}, instances.Model{}, false
	}
	id := r.PathValue("model")
	for _, m := range d.registry.TeamModels(t.ID) {
		if m.ID == id {
			return t, m, true
		}
	}
	if _, global := d.globalEntry(id); global && role < RoleAdmin {
		writeError(w, http.StatusForbidden, msgAdminLimit)
	} else {
		writeError(w, http.StatusBadRequest, msgNotTeamModel)
	}
	return store.Team{}, instances.Model{}, false
}

func (d *Dashboard) putTeamModelLimit(w http.ResponseWriter, r *http.Request) {
	t, m, ok := d.teamModelTarget(w, r)
	if !ok {
		return
	}
	n, ok := limitBody(w, r)
	if !ok {
		return
	}
	d.setLimit(w, r, store.ModelLimit{Ref: chain.RefOf(m), TeamID: t.ID, MonthlyTokens: n}, m.Name, t.Name)
}

func (d *Dashboard) deleteTeamModelLimit(w http.ResponseWriter, r *http.Request) {
	t, m, ok := d.teamModelTarget(w, r)
	if !ok {
		return
	}
	d.clearLimit(w, r, chain.RefOf(m), t.ID, m.Name, t.Name)
}

// --- GET /api/teams/{id}/limits ---

// teamLimits is the team's limits (TeamLimitsView): 404 for a non-member, 403
// for a viewer.
func (d *Dashboard) teamLimits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, role, ok := d.teamFor(w, r, RoleViewer)
	if !ok {
		return
	}
	if role < RoleMaintainer {
		writeError(w, http.StatusForbidden, msgTeamLimitsRead)
		return
	}
	limits, err := d.Store.ModelLimits(ctx)
	if err != nil {
		d.failed(w, r, "reading the model limits", err)
		return
	}
	usage, err := quota.ReadEntryUsage(ctx, d.Store, d.now())
	if err != nil {
		d.failed(w, r, "reading the token usage", err)
		return
	}
	v := TeamLimitsView{Team: TeamRef{ID: t.ID, Name: t.Name}, Models: []TeamModelLimitView{}, Global: []TeamGlobalLimitView{}}
	for _, m := range d.registry.TeamModels(t.ID) {
		ref := chain.RefOf(m)
		v.Models = append(v.Models, TeamModelLimitView{ID: m.ID, Name: m.Name, Limit: limitOf(limits, ref, t.ID), Used: usage.Used(ref, t.ID)})
	}
	for _, m := range d.registry.GlobalModels() {
		ref := chain.RefOf(m)
		team := limitOf(limits, ref, t.ID)
		if team == nil {
			continue
		}
		state := limitNone
		if ml := limitOf(limits, ref, 0); ml != nil {
			state = limitRoomLeft
			if usage.UsedUp(store.ModelLimit{Ref: ref, MonthlyTokens: *ml}) {
				state = limitUsedUp
			}
		}
		v.Global = append(v.Global, TeamGlobalLimitView{ID: m.ID, Name: m.Name, Limit: *team, Used: usage.Used(ref, t.ID), ModelLimit: state})
	}
	writeJSON(w, http.StatusOK, v)
}
```

`api_team_models.go` `teamRoutes`, after the `.../mark` route:

```go
	route("PUT /api/teams/{id}/models/{model}/limit", d.putTeamModelLimit)
	route("DELETE /api/teams/{id}/models/{model}/limit", d.deleteTeamModelLimit)
	route("GET /api/teams/{id}/limits", d.teamLimits)
```

`openapi_test.go` `openAPISchemas`: add

```go
	"TeamLimitsView":          outputSchema(TeamLimitsView{}),
	"TeamModelLimitView":      outputSchema(TeamModelLimitView{}),
	"TeamGlobalLimitView":     outputSchema(TeamGlobalLimitView{}),
```

and in `TestOpenAPIEnumsMatchCode`: `{"TeamGlobalLimitView", "modelLimit", []string{limitNone, limitRoomLeft, limitUsedUp}},`.

`openapi.yaml`, after `/api/teams/{id}/models/{model}/mark`:

```yaml
  /api/teams/{id}/models/{model}/limit:
    put:
      operationId: setTeamModelLimit
      tags: [Organization]
      summary: Set the limit of a team's own entry
      description: |
        For the team's maintainers and admins (a viewer of the team or a
        maintainer of another team 403, anyone else 404). The most tokens
        reviews may spend on the entry in a UTC calendar month; at it, they
        skip the entry. An entry of another team is 400 (`the model is not one
        of this team's models`); a global entry's limits are the admins'
        (403 `only admins set a team's limit on a global model`). Audited as
        `model_limit_set`.
      x-pergent-access: session
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: '#/components/parameters/NumericId'
        - $ref: '#/components/parameters/TeamModelId'
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/ModelLimitRequest'}
      responses:
        '204':
          description: The limit is set.
        '400': {$ref: '#/components/responses/BadRequest'}
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
    delete:
      operationId: clearTeamModelLimit
      tags: [Organization]
      summary: Remove the limit of a team's own entry
      description: As setting one. 204 whether it had one or not; a limit removed is audited (`model_limit_cleared`).
      x-pergent-access: session
      security:
        - sessionCookie: []
          csrfToken: []
      parameters:
        - $ref: '#/components/parameters/NumericId'
        - $ref: '#/components/parameters/TeamModelId'
      responses:
        '204':
          description: No limit now.
        '400': {$ref: '#/components/responses/BadRequest'}
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
  /api/teams/{id}/limits:
    get:
      operationId: getTeamLimits
      tags: [Organization]
      summary: A team's model limits
      description: |
        For the team's maintainers and admins: a viewer of the team gets 403
        (`only maintainers of this team see its model limits`), anyone else
        404. Each of the team's own entries with its limit, and each global
        entry with an admin's limit for the team. A global entry's model
        limit shows only as `none`, `room_left` or `used_up`, because its
        numbers count other teams.
      x-pergent-access: session
      security:
        - sessionCookie: []
      parameters:
        - $ref: '#/components/parameters/NumericId'
      responses:
        '200':
          description: The team's limits.
          content:
            application/json:
              schema: {$ref: '#/components/schemas/TeamLimitsView'}
        '401': {$ref: '#/components/responses/Unauthorized'}
        '403': {$ref: '#/components/responses/Forbidden'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
```

Schemas, after `TeamModelView`:

```yaml
    TeamLimitsView:
      description: A team's model limits. Never another team's numbers.
      type: object
      required: [team, models, global]
      properties:
        team: {$ref: '#/components/schemas/TeamRef'}
        models:
          type: array
          items: {$ref: '#/components/schemas/TeamModelLimitView'}
        global:
          type: array
          items: {$ref: '#/components/schemas/TeamGlobalLimitView'}
    TeamModelLimitView:
      description: One of the team's own entries and its limit.
      type: object
      required: [id, name, limit, used]
      properties:
        id: {type: string}
        name: {type: string}
        limit:
          type: [integer, 'null']
          format: int64
          description: Tokens per UTC month; null for none.
        used:
          type: integer
          format: int64
          description: The team's tokens on the entry this month.
    TeamGlobalLimitView:
      description: A global entry with an admin's limit for the team. Read-only for maintainers.
      type: object
      required: [id, name, limit, used, modelLimit]
      properties:
        id: {type: string}
        name: {type: string}
        limit: {type: integer, format: int64}
        used:
          type: integer
          format: int64
          description: The team's tokens on the entry this month.
        modelLimit:
          type: string
          enum: [none, room_left, used_up]
          description: The entry's model limit, without its numbers.
```

(Match how the file writes a nullable integer: copy the shape `QuotaUseView.limit` uses.)

**Step 4: Run them, the routes and openapi tests, and the team tests**

```bash
DBTEST ./internal/dashboard/ -run 'Limit|OpenAPI|Routes|Team'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/dashboard/api_limits.go internal/dashboard/api_limits_test.go internal/dashboard/api_team_models.go internal/dashboard/view.go internal/dashboard/openapi_test.go openapi.yaml
git -c user.email="$(git config --local user.email)" commit -m "feat(dashboard): team routes for a team entry's limit and the team's limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 17: The admin model list shows `limit`, `used` and `teamLimits`

**Files:**
- Modify: `internal/dashboard/api_models.go:77-112` (`modelLists`, `globalView`)
- Modify: `internal/dashboard/view.go:1514-1540` (`ModelAdminView`, new `TeamLimitView`)
- Modify: `internal/dashboard/openapi_test.go` (`openAPISchemas`)
- Modify: `openapi.yaml` (`ModelAdminView`, new `TeamLimitView`)
- Test: `internal/dashboard/api_limits_test.go`

**Step 1: Write the failing test** (append)

```go
// The admin list: each global entry's model limit and its tokens this month,
// and each team's limit on it with that team's tokens, by team name.
func TestModelListShowsLimits(t *testing.T) {
	te := newTeamsEnv(t)
	g := te.addModel("openai", "gpt-5", "", llmKeyThree)
	plain := te.addModel("openai", "gpt-6", "", llmKeyThree)
	wantCode(t, te.send(te.admin, "PUT", modelLimitURL(g.ID), `{"monthlyTokens":1000}`), "model limit", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(g.ID, te.widgets), `{"monthlyTokens":50}`), "Widgets", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(g.ID, te.payments), `{"monthlyTokens":400}`), "Payments", http.StatusNoContent)
	used := te.usageRun(runFix{project: "acme/payments", number: 50, head: head('5')}, usageSkill("code-review", "no_blocking_issues", 120, 30))
	te.exec(`UPDATE runs SET model_source = 'global', model_entry_id = $2 WHERE id = $1::uuid`, used, g.ID)

	v := decode[ModelsAdminView](t, te.get(te.admin, modelsURL), "GET models", http.StatusOK)
	got := modelNamed(t, v, g.ID)
	if got.Limit == nil || *got.Limit != 1000 || got.Used != 150 {
		t.Errorf("limit, used = %v, %d; want 1000, 150", got.Limit, got.Used)
	}
	want := []TeamLimitView{
		{TeamID: te.payments, Team: "Payments", Limit: 400, Used: 150},
		{TeamID: te.widgets, Team: "Widgets", Limit: 50, Used: 0},
	}
	if !slices.Equal(got.TeamLimits, want) {
		t.Errorf("teamLimits = %+v; want %+v", got.TeamLimits, want)
	}
	if p := modelNamed(t, v, plain.ID); p.Limit != nil || p.Used != 0 || p.TeamLimits == nil || len(p.TeamLimits) != 0 {
		t.Errorf("an entry with no limit = %+v; want null, 0 and []", p)
	}
}
```

**Step 2: Run and watch it fail**

```bash
DBTEST ./internal/dashboard/ -run TestModelListShowsLimits
```

Expected: build failure `got.Limit undefined (type ModelAdminView has no field or method Limit)`.

**Step 3: Write the code**

`view.go`, in `ModelAdminView` add (and to its comment: "Limit is its model limit (null: none) and Used its tokens this month on every instance; TeamLimits each team's limit on it, by team name."):

```go
	Limit      *int64          `json:"limit"`
	Used       int64           `json:"used"`
	TeamLimits []TeamLimitView `json:"teamLimits"`
```

and after it:

```go
// TeamLimitView is a team's limit on a global entry and that team's tokens on
// it this month.
type TeamLimitView struct {
	TeamID int64  `json:"teamId"`
	Team   string `json:"team"`
	Limit  int64  `json:"limit"`
	Used   int64  `json:"used"`
}
```

`api_models.go`: `modelLists` gains

```go
	teams  map[int64]string
	limits []store.ModelLimit
	usage  quota.EntryUsage
```

filled in `modelLists()` (keep the `teams` it reads already in `l.teams`; then `l.limits, err = d.Store.ModelLimits(ctx)` and `l.usage, err = quota.ReadEntryUsage(ctx, d.Store, d.now())`, each returning its error as the others do). `globalView` adds, before `return v`:

```go
	v.Limit, v.Used, v.TeamLimits = limitOf(l.limits, ref, 0), l.usage.Used(ref, 0), []TeamLimitView{}
	for _, lim := range l.limits {
		if lim.Ref == ref && lim.TeamID != 0 {
			v.TeamLimits = append(v.TeamLimits, TeamLimitView{TeamID: lim.TeamID, Team: l.teams[lim.TeamID],
				Limit: lim.MonthlyTokens, Used: l.usage.Used(ref, lim.TeamID)})
		}
	}
	slices.SortFunc(v.TeamLimits, func(a, b TeamLimitView) int { return strings.Compare(strings.ToLower(a.Team), strings.ToLower(b.Team)) })
```

A view built with an empty `modelLists{}` (the answer of `POST /api/admin/models`) gets `limit: null`, `used: 0`, `teamLimits: []`, as the schema wants.

`openapi_test.go` `openAPISchemas`: `"TeamLimitView": outputSchema(TeamLimitView{}),`.

`openapi.yaml` `ModelAdminView`: add `limit, used, teamLimits` to `required`, and:

```yaml
        limit:
          type: [integer, 'null']
          format: int64
          description: The model limit, tokens per UTC month on every instance and repository; null for none.
        used:
          type: integer
          format: int64
          description: The entry's tokens this month, every review and API review included.
        teamLimits:
          type: array
          items: {$ref: '#/components/schemas/TeamLimitView'}
          description: Each team's limit on the entry, by team name.
```

and the schema:

```yaml
    TeamLimitView:
      description: A team's limit on a global entry.
      type: object
      required: [teamId, team, limit, used]
      properties:
        teamId: {type: integer, format: int64}
        team: {type: string}
        limit: {type: integer, format: int64}
        used:
          type: integer
          format: int64
          description: The tokens of the repositories the team owns on the entry this month.
```

**Step 4: Run it and the models and openapi tests**

```bash
DBTEST ./internal/dashboard/ -run 'Model|Limit|OpenAPI'
```

Expected: `ok`.

**Step 5: Commit**

```bash
git add internal/dashboard/api_models.go internal/dashboard/view.go internal/dashboard/openapi_test.go internal/dashboard/api_limits_test.go openapi.yaml
git -c user.email="$(git config --local user.email)" commit -m "feat(dashboard): the admin model list shows limits and their use" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 18: Docs

**Files:**
- Modify: `docs/concepts/token-quota.md` (a new section `## Model and team limits` before `## Good to know`)
- Modify: `docs/concepts/models.md:115-131` (Fallback: the hold line and the skip reasons)
- Modify: `docs/concepts/dashboard.md` (the team routes bullet ~168-175, the audit log paragraph ~322)
- Modify: `docs/reference/http-routes.md:27`, `:30`
- Modify: `openapi.yaml` (`rerunReview` ~1025-1031 and `startReview` ~1072-1083 descriptions)
- Modify: `docs/upgrading.md` (under `## Unreleased`)

No test. The prose rules: easy English, no em dashes, no `→` in a UI path (write "the admin **Models** page"), log texts and API values in backticks.

**Step 1: `docs/concepts/token-quota.md`**, the new section:

```markdown
## Model and team limits

A quota caps a repository or an instance. A **limit** caps one model entry. Limits are optional: with none set, nothing changes.

| Limit | Set by | Counts | When it is used up |
| --- | --- | --- | --- |
| Model limit | An admin, on a global entry (the config file's entries too) | Every token on the entry, on every instance and repository, API reviews included | Reviews skip the entry |
| Team limit on a global entry | An admin, whether teams may use the entry or not | Tokens on the entry in the repositories the team owns | Those repositories skip the entry |
| Team limit on a team entry | The team's maintainers, or an admin | Tokens on the team's entry | Reviews skip the entry |

- Each limit is its own number, from 1 to 1,000,000,000,000,000. An entry is skipped when any limit on it is used up. A team limit may be larger than the model limit: then the model limit stops the entry first.
- **The owner team** is the one models use ([Models](models.md#the-owner-team)). Each usage row keeps the owner team it had when the review finished, so a repository that moves to another team leaves its earlier tokens with the old team. A repository with no owner team, and an API review, has no team limit.
- Usage is the same measure as the quotas: input plus output tokens of reviews that finished in the current UTC month, dropped attempts included, on the entry that spent them. A review panel counts each Job on its own entry.
- **The check** runs where the chain picks an entry, after the quotas. A review skips an entry at its limit and runs on the next one. A chain of 1 entry ignores marks, not limits: at its limit it is held.
- **When only the quotas and the limits stop the entries a review has not tried yet**, it is held as for a used-up quota: the same pending status and note, which name no limit and no number. The API gives the change waiting for a start the reason `limit_used_up` (`quota_used_up` when only quotas stopped it). When a mark or an unusable key is part of it, the review fails with `all models exhausted:`, such as `all models exhausted: main (out of quota), backup (team limit used up)`.
- **A request by hand** (`@<bot> review`, a maintainer's **Re-run** or **Start review**) asks the chain the same way, and is refused with `the monthly token quota is used up` when no entry has room.
- **An admin's Start review or Re-run** runs on the first entry with room. When none has room, it goes past the model limits and the team limits on global entries, and runs on the first global entry with no mark. It does not go past a team's own limit on its team entry, which is the team's money: when nothing else is left, the button answers 409 `every model of the chain is at the team's own limit: a maintainer of <team> can raise it on the team page`.
- **A review asked for with room left** goes past the admin limits in the same way if they run out before it is dispatched. If only a team's own limit is left by then, it fails with `all models exhausted:`.
- **API reviews** count toward the model limit and no team limit. An API review skips an entry at its model limit, and one with no entry left, or whose pinned model is at its limit, fails with `all models exhausted:`.
- **Chain B** skips an entry at its limit. When no entry of chain B is left, the panel is off for that review with `the token quota stopped model B`, and the review runs on chain A alone. Job P's judge does not check limits: it ends a review that has started.
- A limit lowered below its usage stops the entry at the next dispatch. A limit raised or removed has room at once, but a held change waits for its next push, a command or an admin's start.
- **When the usage can't be read** at dispatch, the dispatch is tried again with the usual backoff, and then the review fails. No review starts unchecked. A limit can be overshot, as a quota can.
- Setting and removing a limit is audited as `model_limit_set` and `model_limit_cleared`, with the entry's name, the team (or `all teams`) and the number, such as `main for Payments: monthly_tokens=5000000`.
- The routes are in [HTTP routes](../reference/http-routes.md). There is no config-file key for limits.
```

In the `## Good to know` list, change "Rolling back removes the quota." to "Rolling back removes the quota and the limits." with the existing sentence after it.

**Step 2: `docs/concepts/models.md`**

- Replace "**When only the instance token quota stops the entries**, the review is held for the quota, as before ([Monthly token quota](token-quota.md))." with "**When only the token quotas and the model limits stop the entries this review has not tried**, the review is held as for a used-up quota ([Model and team limits](token-quota.md#model-and-team-limits))."
- The skip reasons sentence ends: "..., `instance token quota used up`, `model limit used up`, `team limit used up`."
- Under `## Chain B`, if it names the quota's reason, add "or a limit".

**Step 3: `docs/concepts/dashboard.md`**

After the team routes table (the line "The answer never depends on whether the team exists."), add:

```markdown
  A team's limits follow the same rules, with 2 more: `GET /api/teams/{id}/limits` is for the team's maintainers and admins, and a viewer gets 403 `only maintainers of this team see its model limits`. A maintainer sets the limit of the team's own entries (`PUT` and `DELETE /api/teams/{id}/models/{model}/limit`); naming another team's entry is 400 `the model is not one of this team's models`, and naming a global entry is 403 `only admins set a team's limit on a global model`. A global entry's model limit shows to the team only as `room_left` or `used_up`, because its numbers count other teams ([Model and team limits](token-quota.md#model-and-team-limits)).
```

In the audit log paragraph, after "an instance's token quota (...)", add "the model limits (`model_limit_set`, `model_limit_cleared`: the entry, the team or `all teams`, and the number),".

**Step 4: `docs/reference/http-routes.md`**

- Row 27: add "`PUT` and `DELETE /api/admin/models/{id}/limit`, `PUT` and `DELETE /api/admin/models/{id}/team-limits/{teamId}`" to the routes, and "set or remove its model limit or a team's limit on it (`{"monthlyTokens"}`, 204)" to the text.
- Row 30: add "`PUT` and `DELETE /api/teams/{id}/models/{model}/limit`, `GET /api/teams/{id}/limits`" to the routes, and "A team entry's limit and the team's limits are for its maintainers and admins only ([Model and team limits](../concepts/token-quota.md#model-and-team-limits))." to the text.

**Step 5: `openapi.yaml`** — `rerunReview` and `startReview`: after "an admin's goes past it." add "An admin's goes past the model limits too, and is 409 (`every model of the chain is at the team's own limit: ...`) when every entry left is a team entry at its team's own limit."

**Step 6: `docs/upgrading.md`**, under `## Unreleased`:

```markdown
**Model limits.** An admin can cap the monthly tokens of one global model, for everyone or for one team, and a team's maintainers can cap their own team models: [Model and team limits](concepts/token-quota.md#model-and-team-limits). The admin **Models** page and the team page show them in a later change; this release has the API.

- **Migration `0051_model_limits.sql`** runs at startup. It adds the table `model_limits`, the NULL columns `run_usage.owner_team_id` and `run_usage_models.owner_team_id`, and one index, and drops nothing. Back up Postgres first, as for any migration. The previous release keeps working on the migrated database and ignores them.
- **Nothing changes by default.** No limit is set after the upgrade.
- **The upgrade month.** Team limits count only the reviews that finished after the upgrade, because older usage rows have no owner team. Model limits count the whole month.
- **Rolling deploy.** Pods of the previous release start reviews without the new checks until they are replaced.
- **Rolling back** needs no config change: there is no config key. The limits stay in the database, unused. A change held by a limit shows in the previous release as waiting for a start, with no reason line, and its **Start review** starts it.
```

**Step 7: Check and commit**

```bash
grep -n '—\|→' docs/concepts/token-quota.md docs/concepts/models.md docs/concepts/dashboard.md docs/upgrading.md docs/reference/http-routes.md | grep -v '^.*`.*→.*`' ; true
DBTEST ./internal/dashboard/ -run 'OpenAPI'
```

Expected: the `grep` prints no new line from this change; `ok`.

```bash
git add docs/concepts/token-quota.md docs/concepts/models.md docs/concepts/dashboard.md docs/reference/http-routes.md docs/upgrading.md openapi.yaml
git -c user.email="$(git config --local user.email)" commit -m "docs: model and team limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 19: Final gate and review

**Step 1: The whole suite, with the database**

```bash
nc -z localhost 55432
PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./...
go vet ./...
gofmt -l .
git status --short internal/engine/testdata
```

Expected: every package `ok`; the store, chain, actions, quota, dashboard and engine lines take seconds to minutes (a fast `ok` on them means the variable was missing); `go vet` and `gofmt -l` print nothing; the golden file is unchanged. Report a failure by test name.

`web/` and the chart did not change, so their gates do not run. Check: `git diff --name-only origin/main -- web charts` prints nothing.

**Step 2: The CI environment check** (memory: GitHub Actions sets these for every test)

```bash
PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' GITHUB_ACTIONS=true TMPDIR=/tmp go test -count=1 -timeout 30m ./internal/dashboard/ ./internal/engine/ ./internal/store/ ./internal/chain/ ./internal/actions/ ./internal/quota/
```

Expected: `ok` for each.

**Step 3: Mutation spot checks** (each must make a named test fail; put the code back after each)

| Change | Test that must fail |
|---|---|
| Delete the `ModelLimitUsedUp` case in `chain.skipReason` | `TestResolveLimitsSkip`, `TestModelLimitHoldsPush` |
| In `chain.holds`, make `ReasonTried` return `false, false` | `TestResolveHoldRules/tried_and_a_team_limit`, `TestFallbackHeldOnLimit` |
| In `LoadSet`, drop the `ReadLimits` call | `TestLoadReadsLimits`, `TestLimitsRunNextEntry` |
| In `pastAdmin`, drop `in.PastAdminLimits()` | `TestStartGoesPastAdminLimits` |
| In `resolveMerge`, drop `IgnoreLimits` | `TestPanelJudgeIgnoresLimits` |
| In `adminRefusal`, return `RerunQueued` always | `TestAdminRequestStopsAtTeamOwnLimit`, `TestStartAtTeamOwnLimit` |
| In `ownerTeamSQL`, drop `OR o.project = ''` | `TestUsageRecordsOwnerTeam` |
| In `entryTokensSQL`, drop the `NOT EXISTS` line | `TestEntryTokensSincePanel` |

Grep each run for `--- FAIL` and `build failed`: a mutation that leaves a variable unused fails the build and looks like a survivor.

**Step 4: Review**

Use `superpowers:requesting-code-review` on the diff against `origin/main` (dispatch the reviewer on opus, or self-review against its checklist when no Agent tool is there; say which). Check against the spec's anti-goals and these points by hand:

- No forge text names a limit or a number: `git diff origin/main -- internal/engine | grep -n 'limit' | grep -i 'description\|note\|reply'` shows nothing new in a posted text.
- A failed, held or skipped review never gets `success` or an approval (the fallback tests check the status of the failed one).
- No key or base URL in a new log line, audit detail or view: read every `d.Log` and `d.audit` call in `api_limits.go`.
- `openapi.yaml` request bodies match the handlers by hand (`ModelLimitRequest`: `monthlyTokens` only, `additionalProperties: false`, as `decodeStrict` refuses other fields).
- Every caller of `actions.Rerun` and `actions.Start` handles `RerunTeamLimit` (`git grep -n 'RerunQuotaUsedUp' -- '*.go' ':!*_test.go'` and check `RerunTeamLimit` is beside each).

**Step 5: The private-name scan and the last commit**

Run the private-name scan from the Global constraints, and:

```bash
git diff --name-only origin/main | xargs grep -nE '@gmail\.com|/Users/|/private/tmp/'
```

Expected: both print nothing. Commit any review fix as `fix: <what>`. Do not push: the owner merges.

## Notes for PR 2 (not this plan)

- `web/src/pages/RepositoryDetail.tsx:167-169`: a `limit_used_up` row shows `No model in the chain has room this month.` and its **Start review** is for admins only, as `quota_used_up`.
- `web/src/api.ts`: `WaitingView.reason` adds `"limit_used_up"`; `EntryState` adds `"model_limit" | "team_limit"`; `ModelAdminView` gains `limit`, `used`, `teamLimits`; the new `TeamLimitsView` types.
- `web/src/ChainEditor.tsx` `entryStateText`: `model_limit` reads `model limit used up`, `team_limit` reads `team limit used up`. The repository chain view sends these states from PR 1 on; the global and team chain views (`chainEntries`, `api_chains.go:178`) read no limits yet, so PR 2 adds `quota.ReadLimits` there for their tags.
- The admin Models page field and **Team limits** list, and the team page card **Model limits**, bars amber from 80% and red when used up.
- `docs/concepts/token-quota.md` then says the **Waiting for a start** line, and `models.md` gets the 2 states in its table.
