# Model limits, PR 2 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** The dashboard shows and sets the model limits that PR 1 added to the API: the admin **Models** list gets each entry's tokens, its model limit and a **Team limits** list, a team's page gets a **Model limits** card, every chain editor tags an entry at its limit, and **Waiting for a start** says when a limit holds a change.

**Architecture:** One new field component, `LimitField` (a number is a `PUT {monthlyTokens}`, empty is a `DELETE`), sits beside the existing `QuotaLine` bar on the admin **Models** rows, in their folded **Team limits** list and on the new team card (`pages/TeamLimits.tsx`). `entryStateText` learns the two limit states, and the waiting row reads the `limit_used_up` reason. One small server change: the global and team chain views read the limits (`quota.ReadLimits`) as the repository view already does, so their editors get the tags too.

**Tech Stack:** React 18, TanStack Query 5, Vite 6, Vitest 5 with Testing Library and `web/src/test/fakeApi.ts`; for Task 3 only, Go 1.24 and `internal/dashboard` tests on the existing `pergent-sh-pg` Postgres.

---

**Spec:** `docs/superpowers/specs/2026-11-22-model-limits-design.md`, its "Dashboard" section and the Rollout row for PR 2. PR 1's plan, `docs/superpowers/plans/2026-11-22-model-limits-pr1.md`, ends with "Notes for PR 2": this plan covers each of them. PR 1 (#94, #95) is merged on `main`.

## Spec notes (the code and the spec differ; the plan follows the code)

| # | Spec says | Code says | This plan |
|---|---|---|---|
| 1 | "Admin Models page" | There is no page of that name. The models are the **Models** card of the admin **Settings** page (`/admin/settings`, `pages/AdminSettings.tsx` `Models` and `ModelRow`); `docs/concepts/models.md` calls it "the admin **Models** list" | The rows of that card get the tokens, the field and the **Team limits** list |
| 2 | The new tags sit "beside the existing `out of quota` and `no usable key`" | `entryStateText` (`web/src/ChainEditor.tsx:16`) writes `exhausted until ... (quota)` and `key can't be read` | `model limit used up` and `team limit used up`, beside the texts the code has |
| 3 | "Chain editors tag an entry skipped right now" (all of them) | Only the repository chain view reads the limits (`repoShown` calls `chain.LoadSet`). The global and team chain views build their `chain.Input` with `chainEntries` (`internal/dashboard/api_chains.go:177`), which reads no limits, so they would never send `model_limit` or `team_limit`. PR 1's "Notes for PR 2" says so | **A server change (Task 3):** `writeGlobalChain` reads the model limits, `writeTeamChain` the team's limits too, with `quota.ReadLimits`. No new route, field or enum value: `openapi.yaml` only changes the `state` description |
| 4 | `PUT`, `DELETE /api/teams/{id}/models/{modelId}/limit` | The route is `.../models/{model}/limit` (PR 1 spec note 4) | `paths.apiTeamModelLimit(team, model)` |
| 5 | The team card shows "each global model with an admin limit for this team", editable by admins | The team route answers an admin 400 `the model is not one of this team's models` for a global entry (`teamModelTarget`, `api_limits.go`) | On the team card an admin saves a global row with the admin route `PUT`/`DELETE /api/admin/models/{id}/team-limits/{teamId}` |
| 6 | A global model's model limit shows only as `room left` or `used up` | `TeamGlobalLimitView.modelLimit` has a third value, `none` | `none` shows no line |
| 7 | The admin's 409 is "shown beside the button" | It already is: **Start review** (`RepositoryDetail.tsx`) and **Re-run** (`ReviewDetail.tsx`) put the server's text in an `ErrorBox` | No code. Task 4 adds a test for each that pins the team-limit text. Those 2 tests pass at once |
| 8 | Bars "amber from 80% and red when used up, as for the quotas" | `QuotaLine` (`web/src/quota.tsx`) draws exactly that, and with no limit shows the usage with no bar | Every bar here is a `QuotaLine` |

## Open questions (each has a default the plan uses)

1. **The server change of spec note 3.** The brief asked for web only unless a task truly needs a server change. Without it the global and team chain editors never show the new tags. Default: do Task 3 (one helper and 2 calls in `internal/dashboard`, one DB test). If the owner wants web only, drop Task 3 and the chain sentence in Task 8; the repository chain editors still get the tags.
2. **Who sees a limit tag in a chain.** A team's chain is read by every member, viewers too, and a repository's chain by everyone who sees the repository (PR 1 already sends the tags there). The tags show a state, never a number. Default: every reader of the chain sees the tags; only the **Model limits** card, which has the numbers, is for maintainers and admins.
3. **Adding a team limit from the team page.** Default: no. An admin adds team limits on the admin **Models** list; on the team page an admin changes or removes the ones that exist.
4. **A confirmation before removing a limit.** Default: none. A limit is set again in one step, and each change is audited.
5. **PR 1's deferred minors in web and docs.** No file in the repository lists them (not the PR 1 plan, not its commits). Default: none folded in. Name them and they can go into Task 8 if they are in the docs it touches.

## Global constraints

- Work in the `pergent.sh-limits-web` worktree on branch `feat/model-limits-web`. Never merge, push, tag or open a PR without the owner's word.
- **Never start a container.** Task 3's database test runs only when `nc -z localhost 55432` succeeds. If it fails, say so and leave it to CI.
- `DBTEST` in this plan means, typed in full in the same command:
  `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m`
  Without the variable the dashboard tests skip and print a fast `ok` that proves nothing.
- **Owner rule: run only the focused tests locally.** Each task names its test files. The full Go suite runs on CI after the push. The final gate runs the whole web suite (fast) and the focused Go tests of Task 3 only.
- Each new test must be seen to FAIL, for the reason the step says, before its code. The 2 tests of Task 4 Step 5 are the only exception: they pin text the code already shows, and the step says so.
- `web/` has no prettier. Format by hand in the style of the file; never run `prettier --write`. `npm run lint` is the gate.
- No new API field, route or enum value: `openapi.yaml` changes only the `ChainEntryView.state` description (Task 3). No migration, no config key, no release (no version bump, tag or chart change). Upgrade notes go under `## Unreleased` in `docs/upgrading.md`.
- No key, base URL or secret in any new page text. The team card never shows a number of a global entry's model limit.
- Before every commit, from the repository root: run the private-name scan the controller's brief gives (a `git grep` for internal names over the tree, plans excluded) and expect no output; run `git diff --name-only origin/main -- . ':!docs/superpowers/plans' | xargs grep -nE '@gmail\.com|/Users/|/private/tmp/'` and expect no output; run the same `grep` on each new untracked file, since `git grep` misses them. The repository is public.
- Commit as the repository's own address: `git -c user.email="$(git config --local user.email)" commit ...`. Conventional subject. Last line `Co-Authored-By: Claude <model that wrote it> <noreply@anthropic.com>`. Run tests and the commit as separate commands.

## Existing code each task leans on

| What | Where | Note |
|---|---|---|
| The bar | `QuotaLine` in `web/src/quota.tsx` | `use={{ used, limit }}`, `ariaLabel` names the `meter`; classes `warn` (80%) and `full` |
| Parsing a number typed in | `parseQuota`, `quotaText` in `web/src/quota.tsx` | Same range as a limit (1 to 1,000,000,000,000,000); `null` is empty, `undefined` is wrong |
| The quota field to copy | `TokenQuota` in `web/src/pages/Integrations.tsx:228` | Text input, `Save`, a `meta problem` line, an `ErrorBox` |
| Errors | `ErrorBox`, `Loading` in `web/src/ui.tsx` | `ErrorBox` shows the server's `error` text with `role="alert"` |
| Writes | `useWrite` in `web/src/layout/write.ts` | Sends the CSRF token, retries once on a stale one |
| Chain tags | `entryStateText` in `web/src/ChainEditor.tsx:16` | A state it doesn't know is shown raw (`return e.state`) |
| Waiting row | `Waiting` in `web/src/pages/RepositoryDetail.tsx:126-195` | Lines 167-169: the reason line and who gets **Start review** |
| Admin model rows | `ModelRow` in `web/src/pages/AdminSettings.tsx:735`; `changed()` invalidates the model list and the chains | `modelsKey = ["admin-models"]`, `chainKey = ["admin-chain"]` |
| Team page | `TeamModels` and `useTeamChanged` in `web/src/pages/TeamModels.tsx` | `TeamModelsView.role` is `"admin"` for an admin, `canEdit` is maintainer or admin |
| Tests | `fakeApi` (`web/src/test/fakeApi.ts`: an unknown route answers 404), `renderApp` (`web/src/test/render.tsx`), fixtures in `web/src/test/fixtures.ts` | A page that asks a route the test does not list gets 404 and shows an extra `alert`: add the route to the file's `routes()` helper |
| Existing tests to follow | `web/src/pages/TokenQuota.test.tsx` (bars, waiting), `AdminSettings.test.tsx` "AdminSettings models", `TeamModels.test.tsx`, `ChainEditor.test.tsx` | |
| Chain views (Task 3) | `chainEntries`, `writeGlobalChain` in `internal/dashboard/api_chains.go`; `teamShown`, `writeTeamChain` in `internal/dashboard/api_team_models.go`; `quota.ReadLimits` in `internal/quota/limits.go` | `chain.EntryReason` already returns `model_limit` and `team_limit` once the `Input` carries the maps |

---

## Task 0: Set up the worktree

**Step 1: `web/node_modules`.** The worktree has none. Clone another worktree's with an APFS copy when its `web/package-lock.json` is the same (never a symlink: vitest would write its cache into the other checkout):

```bash
diff -q web/package-lock.json <other worktree>/web/package-lock.json && cp -Rc <other worktree>/web/node_modules web/
```

Or `cd web && npm ci` when the network is there.

**Step 2: Baseline.**

```bash
cd web && npx vitest run src/ChainEditor.test.tsx src/pages/TeamModels.test.tsx src/pages/TokenQuota.test.tsx src/pages/AdminSettings.test.tsx src/pages/ReviewDetail.test.tsx src/api.test.ts
```

Expected: `Test Files 6 passed`. Nothing to commit.

---

## Task 1: API types, paths and fixtures

**Files:**
- Modify: `web/src/api.ts` (`WaitingView` ~31-43, `ModelAdminView` ~921-945, `EntryState` ~968-969, after `TeamModelsView` ~1298, `paths` ~1560-1564)
- Modify: `web/src/test/fixtures.ts` (`modelSonnet`, `modelOpus` ~581-609; a new `teamLimitsPayments` after `teamModelsPayments` ~1085; the type import at the top)
- Test: `web/src/api.test.ts`

**Step 1: Write the failing test and the fixtures**

In `api.test.ts`, inside `describe("paths", ...)`:

```ts
  it("builds the limit routes, the config file's entries included", () => {
    expect(paths.apiAdminModelLimit("config-1")).toBe("/api/admin/models/config-1/limit");
    expect(paths.apiAdminModelTeamLimit("m/x", 2)).toBe("/api/admin/models/m%2Fx/team-limits/2");
    expect(paths.apiTeamLimits(2)).toBe("/api/teams/2/limits");
    expect(paths.apiTeamModelLimit(2, "t-qwen")).toBe("/api/teams/2/models/t-qwen/limit");
  });
```

In `fixtures.ts`, add `limit: null, used: 0, teamLimits: [],` at the end of `modelSonnet` and of `modelOpus`, add `TeamLimitsView` to the type import from `"../api"`, and after `teamModelsPayments`:

```ts
// Payments' limits as its maintainer sees them: its own entry near its
// limit, and an admin's limit for it on a global entry whose model limit is
// used up.
export const teamLimitsPayments: TeamLimitsView = {
  team: { id: 2, name: "Payments" },
  models: [{ id: "t-qwen", name: "qwen3.5", limit: 500, used: 450 }],
  global: [{ id: "m-sonnet", name: "Claude Sonnet", limit: 1000, used: 150, modelLimit: "used_up" }],
};
```

**Step 2: Run and watch them fail**

```bash
cd web && npx vitest run src/api.test.ts
cd web && npm run typecheck
```

Expected: vitest FAIL `paths.apiAdminModelLimit is not a function`; typecheck FAIL on `limit` in `modelSonnet` (`Object literal may only specify known properties`) and on the missing `TeamLimitsView` export.

**Step 3: Write the code**

`api.ts`, `WaitingView`: the comment's reason list ends `"quota_used_up" (the monthly token quota held it; only an admin starts it) or "limit_used_up" (held the same way, and a model limit or a team limit took part).`, and:

```ts
  reason: "untrusted_author" | "trust_check_failed" | "quota_used_up" | "limit_used_up";
```

`ModelAdminView`, after `mark`:

```ts
  // limit is the model limit, tokens per UTC month on every instance and
  // repository (null: none), and used the entry's tokens this month.
  limit: number | null;
  used: number;
  // teamLimits are each team's limit on the entry, by team name.
  teamLimits: TeamLimitView[];
};

// TeamLimitView is a team's limit on a global entry, and that team's tokens
// on it this month.
export type TeamLimitView = { teamId: number; team: string; limit: number; used: number };
```

`EntryState`, with its comment:

```ts
// EntryState is what reviews do with a chain entry now. model_limit and
// team_limit: its model limit, or the owner team's limit on it, is used up
// this month.
export type EntryState =
  | "ready"
  | "exhausted"
  | "key_rejected"
  | "removed"
  | "not_allowed"
  | "not_owner"
  | "key_unusable"
  | "model_limit"
  | "team_limit";
```

After `TeamModelsView`:

```ts
// TeamLimitsView is GET /api/teams/{id}/limits, for the team's maintainers
// and admins: the team's own entries with their limits (null: none), and the
// global entries an admin limited for the team. A global entry's model limit
// is only its state (modelLimit), because its numbers count other teams.
export type TeamLimitsView = { team: TeamRef; models: TeamModelLimitView[]; global: TeamGlobalLimitView[] };
export type TeamModelLimitView = { id: string; name: string; limit: number | null; used: number };
export type TeamGlobalLimitView = {
  id: string;
  name: string;
  limit: number;
  used: number;
  modelLimit: "none" | "room_left" | "used_up";
};
```

`paths`, after `apiTeamChain`:

```ts
  // A team's limits (GET), and the limit of one of its own entries: PUT {monthlyTokens}, DELETE for none.
  apiTeamLimits: (id: number) => `/api/teams/${seg(String(id))}/limits`,
  apiTeamModelLimit: (id: number, model: string) => `/api/teams/${seg(String(id))}/models/${seg(model)}/limit`,
```

and after `apiAdminModelMark`:

```ts
  // A global entry's model limit, and a team's limit on it: PUT {monthlyTokens}, DELETE for none.
  apiAdminModelLimit: (id: string) => `/api/admin/models/${seg(id)}/limit`,
  apiAdminModelTeamLimit: (id: string, team: number) => `/api/admin/models/${seg(id)}/team-limits/${seg(String(team))}`,
```

**Step 4: Run them and the type match**

```bash
cd web && npx vitest run src/api.test.ts && npm run typecheck
```

Expected: PASS, and typecheck prints nothing. Then the openapi check, from the repository root (it compares the `required` fields and the enums of the schemas this PR uses with `api.ts`; it was seen to print `MISMATCH` before this task):

```bash
python3 - <<'EOF'
import re, yaml
spec = yaml.safe_load(open("openapi.yaml"))["components"]["schemas"]
ts = open("web/src/api.ts").read()
def ts_keys(name):
    m = re.search(r"export type %s = \{(.*?)\n?\};" % name, ts, re.S)
    if not m:
        return None
    body = re.sub(r"//[^\n]*", "", m.group(1))
    depth, keys, cur = 0, set(), ""
    for ch in body:
        if ch in "{(<[": depth += 1
        if ch in "})>]": depth -= 1
        cur += ch
        if depth == 0 and ch in ";\n,":
            k = re.match(r"\s*(\w+)\??\s*:", cur)
            if k: keys.add(k.group(1))
            cur = ""
    k = re.match(r"\s*(\w+)\??\s*:", cur)
    if k: keys.add(k.group(1))
    return keys
def ts_union(name, field):
    m = re.search(r"export type %s = \{.*?\b%s: ([^;]+);" % (name, field), ts, re.S)
    return set(re.findall(r'"(\w+)"', m.group(1))) if m else None
bad = 0
for s in ["ModelAdminView", "TeamLimitView", "TeamLimitsView", "TeamModelLimitView", "TeamGlobalLimitView", "WaitingView", "ChainEntryView"]:
    want, got = set(spec[s].get("required", [])), ts_keys(s)
    if got != want:
        bad = 1; print(s, "openapi only:", sorted(want - (got or set())), "api.ts only:", sorted((got or set()) - want))
m = re.search(r'export type EntryState =([^;]+);', ts)
if set(re.findall(r'"(\w+)"', m.group(1))) != set(spec["ChainEntryView"]["properties"]["state"]["enum"]): bad = 1; print("EntryState differs")
if ts_union("WaitingView", "reason") != set(spec["WaitingView"]["properties"]["reason"]["enum"]): bad = 1; print("WaitingView.reason differs")
if ts_union("TeamGlobalLimitView", "modelLimit") != set(spec["TeamGlobalLimitView"]["properties"]["modelLimit"]["enum"]): bad = 1; print("TeamGlobalLimitView.modelLimit differs")
print("types match" if not bad else "MISMATCH")
EOF
```

Expected: `types match`.

**Step 5: Commit**

```bash
git add web/src/api.ts web/src/api.test.ts web/src/test/fixtures.ts
git -c user.email="$(git config --local user.email)" commit -m "feat(web): types and paths for the model limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 2: Chain editors tag the limits

**Files:**
- Modify: `web/src/ChainEditor.tsx:16-34` (`entryStateText`)
- Test: `web/src/ChainEditor.test.tsx`

Every chain editor (global, team, repository, chain B through `PanelChainCard`) draws its tags with `entryStateText`, and a state other than `ready` already gets `warn-text`. This task is that one function.

**Step 1: Write the failing test**

In `describe("ChainEditor", ...)`, after "says what reviews do with each entry now":

```ts
  it("tags an entry a limit stops this month", () => {
    const entries: ChainEntryView[] = [
      { ...entrySonnet, position: 1, state: "model_limit" },
      { ...entryTeamQwen, position: 2, state: "team_limit" },
    ];
    setup({ ...chainGlobal, readOnly: true, entries, choices: [] });
    expect(rowOf("Claude Sonnet")).toHaveTextContent("model limit used up");
    expect(within(rowOf("qwen3.5")).getByText("team limit used up")).toHaveClass("warn-text");
  });
```

**Step 2: Run and watch it fail**

```bash
cd web && npx vitest run src/ChainEditor.test.tsx
```

Expected: FAIL, the row shows the raw `model_limit`.

**Step 3: Write the code**

In `entryStateText`, after `case "key_unusable"`:

```ts
    case "model_limit":
      return "model limit used up";
    case "team_limit":
      return "team limit used up";
```

**Step 4: Run it**

```bash
cd web && npx vitest run src/ChainEditor.test.tsx
```

Expected: PASS.

**Step 5: Commit**

```bash
git add web/src/ChainEditor.tsx web/src/ChainEditor.test.tsx
git -c user.email="$(git config --local user.email)" commit -m "feat(web): chain editors tag an entry at its model or team limit" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 3: Server: the global and team chain views read the limits

**Files:**
- Modify: `internal/dashboard/api_chains.go` (a new `withLimits` beside `chainEntries` ~177; `writeGlobalChain` ~370; the `quota` import)
- Modify: `internal/dashboard/api_team_models.go` (`writeTeamChain` ~500)
- Modify: `openapi.yaml` (`ChainEntryView.state` description ~6927)
- Modify: `internal/dashboard/view.go` (`ChainEntryView` comment ~1617)
- Test: `internal/dashboard/api_limits_test.go`

This is spec note 3. The read is the one `chain.LoadSet` already does for a repository's chain. With no limit set, `quota.ReadLimits` makes one query and returns empty maps, so nothing else changes.

**Step 1: Write the failing test**

Append to `api_limits_test.go`:

```go
// entryStates is each entry's state in v, by source/id.
func entryStates(v ChainView) map[string]string {
	out := map[string]string{}
	for _, en := range v.Entries {
		out[en.Source+"/"+en.ID] = en.State
	}
	return out
}

// The global and team chain views tag an entry at its limit, as the
// resolver skips it: the global chain with the model limits alone; a team's
// chain, and the global chain it shows while it has none, with that team's
// limits too.
func TestChainViewsShowLimits(t *testing.T) {
	te := newTeamsEnv(t)
	shared, private := te.sharedAndPrivate()
	own := te.addTeamModel(te.maintainer, te.payments, "pay-a", teamKeyOne)
	decode[ChainView](t, te.sendJSON(te.admin, "PUT", adminChainURL, entryBody("global/"+shared.ID, "global/"+private.ID)), "PUT global", http.StatusOK)
	// Payments owns acme/payments, so these tokens count for Payments.
	spend := func(n int64, c byte, source, id string, tokens int64) {
		run := te.usageRun(runFix{project: "acme/payments", number: n, head: head(c)}, usageSkill("code-review", "no_blocking_issues", tokens, 0))
		te.exec(`UPDATE runs SET model_source = $2, model_entry_id = $3 WHERE id = $1::uuid`, run, source, id)
	}
	spend(50, '5', store.SourceGlobal, shared.ID, 150)
	spend(51, '6', store.SourceGlobal, private.ID, 30)
	spend(52, '7', store.SourceTeam, own.ID, 20)
	wantCode(t, te.send(te.admin, "PUT", modelLimitURL(shared.ID), `{"monthlyTokens":100}`), "model limit", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(private.ID, te.payments), `{"monthlyTokens":30}`), "team limit", http.StatusNoContent)
	wantCode(t, te.send(te.maintainer, "PUT", teamURL(te.payments, "/models/"+own.ID+"/limit"), `{"monthlyTokens":20}`), "own limit", http.StatusNoContent)

	check := func(name string, v ChainView, want map[string]string) {
		t.Helper()
		got := entryStates(v)
		for ref, state := range want {
			if got[ref] != state {
				t.Errorf("%s: %s = %q; want %q (all: %v)", name, ref, got[ref], state, got)
			}
		}
	}
	g, p, o := "global/"+shared.ID, "global/"+private.ID, "team/"+own.ID
	check("global chain", getJSON[ChainView](te.env, te.admin, adminChainURL),
		map[string]string{g: "model_limit", p: "ready"})
	check("Payments, no chain of its own", getJSON[ChainView](te.env, te.maintainer, teamURL(te.payments, "/chain")),
		map[string]string{g: "model_limit", p: "team_limit"})
	check("Widgets, no chain of its own", getJSON[ChainView](te.env, te.otherMaintainer, teamURL(te.widgets, "/chain")),
		map[string]string{g: "model_limit", p: "ready"})
	check("Payments' own chain, as saved", decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", teamURL(te.payments, "/chain"), entryBody(o, g)), "PUT team chain", http.StatusOK),
		map[string]string{o: "team_limit", g: "model_limit"})
}
```

**Step 2: Run and watch it fail**

```bash
nc -z localhost 55432
DBTEST ./internal/dashboard/ -run 'TestChainViewsShowLimits' 2>&1 | grep -E -- '--- FAIL|build failed|undefined|ok '
```

Expected: `--- FAIL: TestChainViewsShowLimits`, every state `"ready"`. If `nc` fails, write the code, check it compiles with `go vet ./internal/dashboard/`, and say the test is left to CI.

**Step 3: Write the code**

`api_chains.go`, import `"github.com/gopaytech/pergent.sh/internal/quota"`, and after `chainEntries`:

```go
// withLimits puts on in the limits used up this month for the repositories
// team owns (0: the model limits alone), so a chain view tags an entry at
// its limit as the resolver skips it (chain.EntryReason).
func (d *Dashboard) withLimits(ctx context.Context, in *chain.Input, team int64) error {
	lim, err := quota.ReadLimits(ctx, d.Store, d.now(), team)
	if err != nil {
		return err
	}
	in.ModelLimitUsedUp, in.TeamLimitUsedUp = lim.Model, lim.Team
	return nil
}
```

`writeGlobalChain`, after the `globalShown` error check:

```go
	// The global chain has no owner team: only the model limits apply.
	if err := d.withLimits(ctx, &c.in, 0); err != nil {
		d.failed(w, r, "reading the model limits", err)
		return
	}
```

`writeTeamChain` (`api_team_models.go`), after the `teamShown` error check:

```go
	// The team's repositories are what its chain, or the global one it
	// shows, runs for: its limits apply.
	if err := d.withLimits(ctx, &c.in, t.ID); err != nil {
		d.failed(w, r, "reading the model limits", err)
		return
	}
```

`openapi.yaml`, the end of the `ChainEntryView.state` description: replace ` (a repository's chain only).` (with its leading space) by `. The global chain shows the model limits only; a team's chain, and the global chain a team page shows while the team has none, also that team's limits.`

`view.go`, the `ChainEntryView` comment: after `"team_limit" (the owner team's limit on it is)` add `; the global chain reads the model limits only`.

The PUT routes end in `writeGlobalChain` and `writeTeamChain`, so their answers carry the tags too.

**Step 4: Run it, the openapi test, vet and gofmt**

```bash
DBTEST ./internal/dashboard/ -run 'TestChainViewsShowLimits|Chain|Limit' 2>&1 | grep -E -- '--- FAIL|build failed|ok '
go test -count=1 -run 'TestOpenAPI' ./internal/dashboard/
go vet ./internal/dashboard/
gofmt -l internal/
```

Expected: `ok` twice (the first takes seconds, not milliseconds: a fast `ok` means the variable was missing); `go vet` and `gofmt -l` print nothing.

Mutation check (put the code back after each): in `writeTeamChain` pass `0` for `t.ID`, and the test fails on `Payments, no chain of its own`; drop the call in `writeGlobalChain`, and it fails on `global chain`. Grep for `--- FAIL` and `build failed`.

**Step 5: Commit**

```bash
git add internal/dashboard/api_chains.go internal/dashboard/api_team_models.go internal/dashboard/api_limits_test.go internal/dashboard/view.go openapi.yaml
git -c user.email="$(git config --local user.email)" commit -m "feat(dashboard): the global and team chain views tag entries at their limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 4: Waiting for a start: the limit line, Start for admins only

**Files:**
- Modify: `web/src/pages/RepositoryDetail.tsx:126-170` (`Waiting`)
- Test: `web/src/pages/TokenQuota.test.tsx` (`describe("Token quota: waiting changes")`), `web/src/pages/ReviewDetail.test.tsx` (`describe("ReviewDetail buttons")`)

Today a maintainer gets **Start review** on a `limit_used_up` row and the server refuses it with 409 (PR 1 spec note 6).

**Step 1: Write the failing test**

In `describe("Token quota: waiting changes", ...)`, after the `untrusted` constant:

```ts
  const limited: WaitingView = { ...held, number: 13, head: "b".repeat(40), reason: "limit_used_up" };
  const teamLimitText = "every model of the chain is at the team's own limit: a maintainer of Payments can raise it on the team page";

  it("says no model has room on a limit-held change, and offers Start to admins only", async () => {
    const fake = fakeApi({
      ...pageRoutes({ ...repoDetail(repoPayments), waiting: [limited] }),
      "POST /api/reviews/main/acme/payments/13/start": { status: 202, body: { status: "queued" } },
    });
    const first = renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(box).toHaveTextContent("No model in the chain has room this month.");
    expect(box).not.toHaveTextContent("Monthly token quota used up.");
    await userEvent.click(within(box).getByRole("button", { name: "Start review of !13" }));
    expect(fake.find("POST", "/api/reviews/main/acme/payments/13/start")[0].body).toEqual({ head: "b".repeat(40) });
    first.unmount();

    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: { ...repoDetail(repoPayments, "maintainer"), waiting: [limited] } },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: noReviews },
    });
    renderApp("/repos/main/acme/payments");
    const seen = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(seen).toHaveTextContent("No model in the chain has room this month.");
    expect(within(seen).queryByRole("button")).not.toBeInTheDocument();
  });
```

**Step 2: Run and watch it fail**

```bash
cd web && npx vitest run src/pages/TokenQuota.test.tsx -t "limit-held"
```

Expected: FAIL, the text `No model in the chain has room this month.` is not there.

**Step 3: Write the code**

In `RepositoryDetail.tsx`, above `function Waiting`:

```tsx
// heldForTokens says a change waits because the tokens ran out: a quota, or
// a model or team limit with it. Only an admin starts one.
const heldForTokens = (w: WaitingView) => w.reason === "quota_used_up" || w.reason === "limit_used_up";
```

After the `quota_used_up` line:

```tsx
            {w.reason === "limit_used_up" && <div className="meta">No model in the chain has room this month.</div>}
```

and the button's condition becomes:

```tsx
          {!paused && (role === "admin" || (role === "maintainer" && !heldForTokens(w))) && (
```

**Step 4: Run it**

```bash
cd web && npx vitest run src/pages/TokenQuota.test.tsx
```

Expected: PASS, the quota tests too.

**Step 5: Pin the admin's 409 texts (spec note 7)**

These pass at once: the `ErrorBox` beside each button already shows the server's text. They keep it so.

In `TokenQuota.test.tsx`, in the same `describe`:

```ts
  it("shows an admin why a Start was refused at a team's own limit", async () => {
    fakeApi({
      ...pageRoutes({ ...repoDetail(repoPayments), waiting: [limited] }),
      "POST /api/reviews/main/acme/payments/13/start": { status: 409, body: { error: teamLimitText } },
    });
    renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    await userEvent.click(within(box).getByRole("button", { name: "Start review of !13" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent(teamLimitText);
  });
```

In `ReviewDetail.test.tsx`, in `describe("ReviewDetail buttons", ...)` after "shows a closed change's refusal as text":

```ts
  it("shows an admin's refusal at a team's own limit as text", async () => {
    const text = "every model of the chain is at the team's own limit: a maintainer of Payments can raise it on the team page";
    fakeApi({ ...base(allCan), ["POST " + rerunUrl]: { status: 409, body: { error: text } } });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run review" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(text);
  });
```

```bash
cd web && npx vitest run src/pages/TokenQuota.test.tsx src/pages/ReviewDetail.test.tsx
```

Expected: PASS.

**Step 6: Commit**

```bash
git add web/src/pages/RepositoryDetail.tsx web/src/pages/TokenQuota.test.tsx web/src/pages/ReviewDetail.test.tsx
git -c user.email="$(git config --local user.email)" commit -m "feat(web): a limit-held change says no model has room, and only admins start it" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 5: Models list: tokens, bar and the model limit field

**Files:**
- Create: `web/src/LimitField.tsx`
- Modify: `web/src/quota.tsx` (a `limitProblem` beside `quotaProblem`)
- Modify: `web/src/pages/AdminSettings.tsx` (`ModelRow` ~735-850; imports)
- Test: `web/src/pages/AdminSettings.test.tsx` (`describe("AdminSettings models")`)

**Step 1: Write the failing tests**

In `describe("AdminSettings models", ...)`:

```ts
  it("shows each entry's tokens this month against its model limit, amber from 80% and red when used up", async () => {
    fakeApi(
      listed({
        models: [
          { ...modelSonnet, used: 1234 },
          { ...modelOpus, limit: 5000, used: 4200 },
          { ...modelOpus, id: "m-full", name: "Full", limit: 100, used: 100 },
        ],
      }),
    );
    renderApp("/admin/settings");
    const sonnet = await entry("Claude Sonnet");
    expect(sonnet).toHaveTextContent("Tokens this month: 1,234");
    expect(within(sonnet).queryByRole("meter")).not.toBeInTheDocument();
    expect(within(sonnet).getByLabelText("Model limit of Claude Sonnet")).toHaveValue("");
    const opus = await entry("Claude Opus");
    expect(opus).toHaveTextContent("Tokens this month: 4,200 of 5,000");
    expect(within(opus).getByRole("meter", { name: "Tokens of Claude Opus this month" })).toHaveClass("warn");
    expect(within(opus).getByLabelText("Model limit of Claude Opus")).toHaveValue("5000");
    expect(within(await entry("Full")).getByRole("meter", { name: "Tokens of Full this month" })).toHaveClass("full");
  });

  it("sets a model limit with a PUT, stripped of separators, and removes it when emptied", async () => {
    const fake = fakeApi({
      ...listed({ models: [modelSonnet, { ...modelOpus, limit: 5000, used: 10 }] }),
      "PUT /api/admin/models/m-opus/limit": { status: 204 },
      "DELETE /api/admin/models/m-opus/limit": { status: 204 },
    });
    renderApp("/admin/settings");
    const opus = await entry("Claude Opus");
    const input = within(opus).getByLabelText("Model limit of Claude Opus");
    const save = within(opus).getByRole("button", { name: "Save the model limit of Claude Opus" });
    await userEvent.clear(input);
    await userEvent.type(input, "1,000,000");
    await userEvent.click(save);
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/models/m-opus/limit")).toHaveLength(1));
    const [put] = fake.find("PUT", "/api/admin/models/m-opus/limit");
    expect(put.body).toEqual({ monthlyTokens: 1000000 });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    // The list is read again, so the bar follows.
    await vi.waitFor(() => expect(fake.find("GET", "/api/admin/models").length).toBeGreaterThanOrEqual(2));
    await userEvent.clear(input);
    await userEvent.click(save);
    await vi.waitFor(() => expect(fake.find("DELETE", "/api/admin/models/m-opus/limit")).toHaveLength(1));
  });

  it("refuses a limit that is not a whole number, and shows the server's refusal", async () => {
    const fake = fakeApi({
      ...listed(),
      "PUT /api/admin/models/m-opus/limit": { status: 400, body: { error: "monthlyTokens must be a whole number from 1 to 1000000000000000" } },
    });
    renderApp("/admin/settings");
    const opus = await entry("Claude Opus");
    const input = within(opus).getByLabelText("Model limit of Claude Opus");
    await userEvent.type(input, "5M");
    await userEvent.click(within(opus).getByRole("button", { name: "Save the model limit of Claude Opus" }));
    expect(await within(opus).findByText("The limit is a whole number from 1 to 1000000000000000, or empty.")).toBeInTheDocument();
    expect(fake.find("PUT", "/api/admin/models/m-opus/limit")).toHaveLength(0);
    await userEvent.clear(input);
    await userEvent.type(input, "7");
    await userEvent.click(within(opus).getByRole("button", { name: "Save the model limit of Claude Opus" }));
    expect(await within(opus).findByRole("alert")).toHaveTextContent("monthlyTokens must be a whole number");
  });

  it("limits the config file's entry too", async () => {
    const config: ModelAdminView = { ...modelSonnet, id: "config", source: "config", default: true, name: "anthropic/claude-sonnet" };
    const fake = fakeApi({ ...listed({ canSetDefault: false, models: [config] }), "PUT /api/admin/models/config/limit": { status: 204 } });
    renderApp("/admin/settings");
    const row = await entry("anthropic/claude-sonnet");
    await userEvent.type(within(row).getByLabelText("Model limit of anthropic/claude-sonnet"), "900");
    await userEvent.click(within(row).getByRole("button", { name: "Save the model limit of anthropic/claude-sonnet" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/models/config/limit")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/models/config/limit")[0].body).toEqual({ monthlyTokens: 900 });
  });
```

Two existing assertions forbid any input or button on a row. Change them in the same step (they then fail for the right reason only once the field exists, and pass after Step 3):

- In "lists each model by its name, ...", replace `expect(within(opus).queryByRole("textbox")).not.toBeInTheDocument();` with:

  ```ts
      // The only text input is the model limit: none for a key.
      expect(within(opus).getAllByRole("textbox").map((e) => e.getAttribute("aria-label"))).toEqual(["Model limit of Claude Opus"]);
  ```

- In "gives the config file's entry no controls, and says where to change it", replace `expect(within(row).queryByRole("button")).not.toBeInTheDocument();` with:

  ```ts
      // Only its limits can be changed here.
      expect(within(row).queryByRole("button", { name: /^(Edit|Remove|Set) / })).not.toBeInTheDocument();
  ```

**Step 2: Run and watch them fail**

```bash
cd web && npx vitest run src/pages/AdminSettings.test.tsx -t "AdminSettings models"
```

Expected: the 4 new tests FAIL (no `Tokens this month`, no `Model limit of ...` field), and "lists each model" FAILS (`getAllByRole("textbox")` finds none).

**Step 3: Write the code**

`quota.tsx`, after `quotaProblem`:

```ts
export const limitProblem = `The limit is a whole number from 1 to ${quotaMax}, or empty.`;
```

`web/src/LimitField.tsx`:

```tsx
import { useMutation } from "@tanstack/react-query";
import { useState } from "react";

import { useWrite } from "./layout/write";
import { limitProblem, parseQuota, quotaText } from "./quota";
import { ErrorBox } from "./ui";

// LimitField sets one monthly token limit at path: a number is a PUT of
// {monthlyTokens}, empty a DELETE (no limit). A value the API would refuse
// is caught before any request. label names the input and saveLabel its
// button; onSaved runs once the server took it. Give it a key of the limit,
// so a new value from the server starts it again.
export default function LimitField({
  label,
  saveLabel,
  hint,
  limit,
  path,
  onSaved,
}: {
  label: string;
  saveLabel: string;
  hint?: string;
  limit: number | null;
  path: string;
  onSaved: () => void;
}) {
  const write = useWrite();
  const [text, setText] = useState(quotaText(limit));
  const [problem, setProblem] = useState("");
  const save = useMutation({
    mutationFn: (n: number | null) => (n === null ? write<void>("DELETE", path) : write<void>("PUT", path, { monthlyTokens: n })),
    onSuccess: () => onSaved(),
  });
  const submit = () => {
    const n = parseQuota(text);
    if (n === undefined) {
      setProblem(limitProblem);
      return;
    }
    setProblem("");
    save.mutate(n);
  };
  return (
    <div>
      <div className="search">
        <label>
          Monthly token limit{" "}
          <input
            type="text"
            inputMode="numeric"
            aria-label={label}
            placeholder="no limit"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        </label>
        <button type="button" className="btn small" aria-label={saveLabel} disabled={save.isPending} onClick={submit}>
          Save
        </button>
      </div>
      {hint && <div className="meta">{hint}</div>}
      {problem && <div className="meta problem">{problem}</div>}
      {save.isError && <ErrorBox error={save.error} />}
    </div>
  );
}
```

`AdminSettings.tsx`: import `LimitField from "../LimitField"` and `{ QuotaLine } from "../quota"`. In `ModelRow`, after `changed`:

```tsx
  // A limit moves the bars here, the chains' tags and the team pages.
  const limitsChanged = () => {
    changed();
    client.invalidateQueries({ queryKey: ["team-chain"] });
    client.invalidateQueries({ queryKey: ["team-limits"] });
  };
```

and in the JSX, after the `following` line and before the mark (so every entry has it, the config file's included):

```tsx
      <QuotaLine ariaLabel={`Tokens of ${m.name} this month`} use={{ used: m.used, limit: m.limit }} />
      <LimitField
        key={String(m.limit)}
        label={`Model limit of ${m.name}`}
        saveLabel={`Save the model limit of ${m.name}`}
        hint="Tokens per calendar month (UTC), input plus output, on every instance and repository. Empty: no limit."
        limit={m.limit}
        path={paths.apiAdminModelLimit(m.id)}
        onSaved={limitsChanged}
      />
```

**Step 4: Run them**

```bash
cd web && npx vitest run src/pages/AdminSettings.test.tsx && npm run typecheck && npm run lint
```

Expected: PASS; typecheck and lint print no error.

**Step 5: Commit**

```bash
git add web/src/LimitField.tsx web/src/quota.tsx web/src/pages/AdminSettings.tsx web/src/pages/AdminSettings.test.tsx
git -c user.email="$(git config --local user.email)" commit -m "feat(web): the models list shows each entry's tokens and sets its model limit" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 6: Models list: the Team limits list

**Files:**
- Modify: `web/src/pages/AdminSettings.tsx` (new `TeamLimits`, `TeamLimitRow`, `AddTeamLimit` after `ModelRow`; `ModelRow` renders `TeamLimits`; imports `type TeamLimitView`, `type TeamView`)
- Test: `web/src/pages/AdminSettings.test.tsx` (a new `describe("AdminSettings team limits")`; add `teamEmpty`, `teamPayments` to the fixtures import)

The list is folded until opened, and it reads the teams (`GET /api/teams`, key `["teams"]`, as the **Teams** page) only then, so no other test of the page needs a new route.

**Step 1: Write the failing tests**

After `describe("AdminSettings models", ...)`:

```ts
describe("AdminSettings team limits", () => {
  const opusLimited: ModelAdminView = { ...modelOpus, limit: 5000, used: 4200, teamLimits: [{ teamId: 2, team: "Payments", limit: 400, used: 350 }] };
  const page = (extra: Parameters<typeof fakeApi>[0] = {}) =>
    fakeApi({
      ...routes(operator),
      "GET /api/admin/models": { body: { ...models, models: [modelSonnet, opusLimited] } },
      "GET /api/teams": { body: [teamPayments, teamEmpty] },
      ...extra,
    });
  // open unfolds the team limits of the entry name and returns the list.
  const open = async (name: string) => {
    const row = await screen.findByRole("group", { name });
    await userEvent.click(within(row).getByRole("button", { name: /^Team limits/ }));
    return within(row).getByRole("group", { name: `Team limits on ${name}` });
  };

  it("folds the team limits until opened, then lists each with its team's tokens", async () => {
    const fake = page();
    renderApp("/admin/settings");
    const row = await screen.findByRole("group", { name: "Claude Opus" });
    const toggle = within(row).getByRole("button", { name: "Team limits (1)" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(within(row).queryByRole("group", { name: "Team limits on Claude Opus" })).not.toBeInTheDocument();
    // The teams are read only when the list is opened.
    expect(fake.find("GET", "/api/teams")).toHaveLength(0);
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const payments = within(within(row).getByRole("group", { name: "Team limits on Claude Opus" })).getByRole("group", { name: "Team Payments" });
    expect(payments).toHaveTextContent("Tokens this month: 350 of 400");
    expect(within(payments).getByRole("meter", { name: "Tokens of Payments on Claude Opus this month" })).toHaveClass("warn");
    expect(within(payments).getByLabelText("Limit of Payments on Claude Opus")).toHaveValue("400");
    expect(within(await screen.findByRole("group", { name: "Claude Sonnet" })).getByRole("button", { name: "Team limits (0)" })).toBeInTheDocument();
  });

  it("changes and removes a team's limit with the admin routes", async () => {
    const fake = page({
      "PUT /api/admin/models/m-opus/team-limits/2": { status: 204 },
      "DELETE /api/admin/models/m-opus/team-limits/2": { status: 204 },
    });
    renderApp("/admin/settings");
    const payments = within(await open("Claude Opus")).getByRole("group", { name: "Team Payments" });
    const input = within(payments).getByLabelText("Limit of Payments on Claude Opus");
    await userEvent.clear(input);
    await userEvent.type(input, "800");
    await userEvent.click(within(payments).getByRole("button", { name: "Save the limit of Payments on Claude Opus" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/models/m-opus/team-limits/2")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/models/m-opus/team-limits/2")[0].body).toEqual({ monthlyTokens: 800 });
    await userEvent.click(within(payments).getByRole("button", { name: "Remove the limit of Payments on Claude Opus" }));
    await vi.waitFor(() => expect(fake.find("DELETE", "/api/admin/models/m-opus/team-limits/2")).toHaveLength(1));
  });

  it("adds a limit for a team that has none, and offers no team twice", async () => {
    const fake = page({ "PUT /api/admin/models/m-opus/team-limits/3": { status: 204 } });
    renderApp("/admin/settings");
    const list = await open("Claude Opus");
    const pick = await within(list).findByRole("combobox", { name: "Team to limit on Claude Opus" });
    // Payments has a limit on it already: only Platform is offered.
    expect(within(pick).getAllByRole("option").map((o) => o.textContent)).toEqual(["Pick a team", "Platform"]);
    const add = within(list).getByRole("button", { name: "Add a team limit on Claude Opus" });
    await userEvent.click(add);
    expect(within(list).getByText("Pick a team and type its limit: a whole number from 1 to 1000000000000000.")).toBeInTheDocument();
    expect(fake.find("PUT", "/api/admin/models/m-opus/team-limits/3")).toHaveLength(0);
    await userEvent.selectOptions(pick, "Platform");
    await userEvent.type(within(list).getByLabelText("New team limit on Claude Opus"), "300");
    await userEvent.click(add);
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/models/m-opus/team-limits/3")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/models/m-opus/team-limits/3")[0].body).toEqual({ monthlyTokens: 300 });
  });

  it("shows the error when the teams can't be read", async () => {
    page({ "GET /api/teams": { status: 500, body: { error: "boom" } } });
    renderApp("/admin/settings");
    expect(await within(await open("Claude Opus")).findByRole("alert")).toHaveTextContent("boom");
  });
});
```

**Step 2: Run and watch them fail**

```bash
cd web && npx vitest run src/pages/AdminSettings.test.tsx -t "AdminSettings team limits"
```

Expected: 4 FAIL, no `Team limits (1)` button.

**Step 3: Write the code**

In `ModelRow`, right after the `LimitField` of Task 5:

```tsx
      <TeamLimits m={m} onChanged={limitsChanged} />
```

After `ModelRow`:

```tsx
const addLimitProblem = "Pick a team and type its limit: a whole number from 1 to 1000000000000000.";

// TeamLimits is each team's limit on a global entry, folded until opened:
// the limit and that team's tokens on the entry this month, changed or
// removed in place, and a form to add one for a team with none.
function TeamLimits({ m, onChanged }: { m: ModelAdminView; onChanged: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button type="button" className="btn small" aria-expanded={open} onClick={() => setOpen(!open)}>
        Team limits ({m.teamLimits.length})
      </button>
      {open && (
        <div className="stack" role="group" aria-label={`Team limits on ${m.name}`}>
          {m.teamLimits.length === 0 && <div className="meta">No team has a limit on it.</div>}
          {m.teamLimits.map((t) => (
            <TeamLimitRow key={t.teamId} m={m} t={t} onChanged={onChanged} />
          ))}
          <AddTeamLimit m={m} onAdded={onChanged} />
        </div>
      )}
    </div>
  );
}

function TeamLimitRow({ m, t, onChanged }: { m: ModelAdminView; t: TeamLimitView; onChanged: () => void }) {
  const write = useWrite();
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminModelTeamLimit(m.id, t.teamId)),
    onSuccess: () => onChanged(),
  });
  return (
    <div role="group" aria-label={`Team ${t.team}`}>
      <div className="row">
        <b className="grow">{t.team}</b>
        <button
          type="button"
          className="btn small"
          aria-label={`Remove the limit of ${t.team} on ${m.name}`}
          disabled={remove.isPending}
          onClick={() => remove.mutate()}
        >
          Remove
        </button>
      </div>
      <QuotaLine ariaLabel={`Tokens of ${t.team} on ${m.name} this month`} use={{ used: t.used, limit: t.limit }} />
      <LimitField
        key={t.limit}
        label={`Limit of ${t.team} on ${m.name}`}
        saveLabel={`Save the limit of ${t.team} on ${m.name}`}
        limit={t.limit}
        path={paths.apiAdminModelTeamLimit(m.id, t.teamId)}
        onSaved={onChanged}
      />
      {remove.isError && <ErrorBox error={remove.error} />}
    </div>
  );
}

// AddTeamLimit gives a team with no limit on the entry one. The teams are
// read when the list is opened.
function AddTeamLimit({ m, onAdded }: { m: ModelAdminView; onAdded: () => void }) {
  const write = useWrite();
  const teams = useQuery({ queryKey: ["teams"], queryFn: () => api.get<TeamView[]>("/api/teams") });
  const [team, setTeam] = useState("");
  const [text, setText] = useState("");
  const [problem, setProblem] = useState("");
  const add = useMutation({
    mutationFn: ({ id, n }: { id: number; n: number }) => write<void>("PUT", paths.apiAdminModelTeamLimit(m.id, id), { monthlyTokens: n }),
    onSuccess: () => {
      setTeam("");
      setText("");
      onAdded();
    },
  });
  if (teams.isPending) return <Loading />;
  if (teams.isError) return <ErrorBox error={teams.error} />;
  const limited = new Set(m.teamLimits.map((t) => t.teamId));
  const free = teams.data.filter((t) => !limited.has(t.id));
  if (free.length === 0) {
    return <div className="meta">{teams.data.length === 0 ? "No team yet." : "Every team has a limit on it."}</div>;
  }
  const submit = () => {
    const n = parseQuota(text);
    if (team === "" || n === null || n === undefined) {
      setProblem(addLimitProblem);
      return;
    }
    setProblem("");
    add.mutate({ id: Number(team), n });
  };
  return (
    <div>
      <div className="search">
        <select aria-label={`Team to limit on ${m.name}`} value={team} onChange={(e) => setTeam(e.target.value)}>
          <option value="">Pick a team</option>
          {free.map((t) => (
            <option key={t.id} value={String(t.id)}>
              {t.name}
            </option>
          ))}
        </select>
        <input
          type="text"
          inputMode="numeric"
          aria-label={`New team limit on ${m.name}`}
          placeholder="tokens per month"
          value={text}
          onChange={(e) => setText(e.target.value)}
        />
        <button type="button" className="btn small" aria-label={`Add a team limit on ${m.name}`} disabled={add.isPending} onClick={submit}>
          Add
        </button>
      </div>
      {problem && <div className="meta problem">{problem}</div>}
      {add.isError && <ErrorBox error={add.error} />}
    </div>
  );
}
```

Import `parseQuota` with `QuotaLine` from `"../quota"`.

**Step 4: Run them**

```bash
cd web && npx vitest run src/pages/AdminSettings.test.tsx && npm run typecheck && npm run lint
```

Expected: PASS, the Task 5 tests and the edited config-entry test too.

**Step 5: Commit**

```bash
git add web/src/pages/AdminSettings.tsx web/src/pages/AdminSettings.test.tsx
git -c user.email="$(git config --local user.email)" commit -m "feat(web): the models list adds, changes and removes team limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 7: Team page: the Model limits card

**Files:**
- Create: `web/src/pages/TeamLimits.tsx`
- Modify: `web/src/pages/TeamModels.tsx` (render the card after the **Models** card; `useTeamChanged` also reads `["team-limits", team]` again)
- Modify: `web/src/pages/TeamModels.test.tsx` (its `routes()` gets the limits route, so the maintainers' tests see no 404 `alert`)
- Test: `web/src/pages/TeamLimits.test.tsx`

**Step 1: Write the failing tests**

`web/src/pages/TeamLimits.test.tsx`:

```ts
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { TeamLimitsView, TeamModelsView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { chainTeam, maintainer, myTeams, ssoAdmin, teamLimitsPayments, teamModelQwen, teamModelsPayments, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const routes = (me = maintainer, models: TeamModelsView = teamModelsPayments, limits: TeamLimitsView = teamLimitsPayments) => ({
  "GET /api/me": { body: me },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2/models": { body: models },
  "GET /api/teams/2/chain": { body: chainTeam },
  "GET /api/teams/2/panel-chain": { body: { ...chainTeam, source: "global", team: "", entries: [] } },
  "GET /api/teams/2/limits": { body: limits },
});

const card = () => screen.findByRole("region", { name: "Model limits" });

describe("TeamModels: model limits", () => {
  it("shows a maintainer the team's own limits to set, and an admin's limit read-only", async () => {
    fakeApi(routes());
    renderApp("/teams/2");
    const c = await card();
    const own = await within(c).findByRole("group", { name: "qwen3.5 limit" });
    expect(own).toHaveTextContent("Tokens this month: 450 of 500");
    expect(within(own).getByRole("meter", { name: "Tokens of qwen3.5 this month" })).toHaveClass("warn");
    expect(within(own).getByLabelText("Limit of qwen3.5")).toHaveValue("500");
    const sonnet = within(c).getByRole("group", { name: "Claude Sonnet limit" });
    expect(sonnet).toHaveTextContent("Tokens this month: 150 of 1,000");
    expect(sonnet).toHaveTextContent("Set by an admin.");
    expect(sonnet).toHaveTextContent("Model limit for all teams: used up");
    expect(within(sonnet).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(sonnet).queryByRole("button")).not.toBeInTheDocument();
  });

  it("sets and removes the limit of the team's own model with the team's route", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/teams/2/models/t-qwen/limit": { status: 204 },
      "DELETE /api/teams/2/models/t-qwen/limit": { status: 204 },
    });
    renderApp("/teams/2");
    const own = await within(await card()).findByRole("group", { name: "qwen3.5 limit" });
    const input = within(own).getByLabelText("Limit of qwen3.5");
    const save = within(own).getByRole("button", { name: "Save the limit of qwen3.5" });
    await userEvent.clear(input);
    await userEvent.type(input, "800");
    await userEvent.click(save);
    await waitFor(() => expect(fake.find("PUT", "/api/teams/2/models/t-qwen/limit")).toHaveLength(1));
    const [put] = fake.find("PUT", "/api/teams/2/models/t-qwen/limit");
    expect(put.body).toEqual({ monthlyTokens: 800 });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-maint");
    // The card and the team chain, whose tags move, are read again.
    await waitFor(() => expect(fake.find("GET", "/api/teams/2/limits").length).toBeGreaterThanOrEqual(2));
    await waitFor(() => expect(fake.find("GET", "/api/teams/2/chain").length).toBeGreaterThanOrEqual(2));
    await userEvent.clear(input);
    await userEvent.click(save);
    await waitFor(() => expect(fake.find("DELETE", "/api/teams/2/models/t-qwen/limit")).toHaveLength(1));
  });

  it("lets an admin change the team's limit on a global model with the admin route", async () => {
    const fake = fakeApi({
      ...routes(ssoAdmin, { ...teamModelsPayments, role: "admin" }),
      "PUT /api/admin/models/m-sonnet/team-limits/2": { status: 204 },
    });
    renderApp("/teams/2");
    const sonnet = await within(await card()).findByRole("group", { name: "Claude Sonnet limit" });
    expect(sonnet).not.toHaveTextContent("Set by an admin.");
    const input = within(sonnet).getByLabelText("Limit of Claude Sonnet");
    expect(input).toHaveValue("1000");
    await userEvent.clear(input);
    await userEvent.type(input, "2000");
    await userEvent.click(within(sonnet).getByRole("button", { name: "Save the limit of Claude Sonnet" }));
    await waitFor(() => expect(fake.find("PUT", "/api/admin/models/m-sonnet/team-limits/2")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/models/m-sonnet/team-limits/2")[0].body).toEqual({ monthlyTokens: 2000 });
  });

  it("says only whether a global model's own limit has room, and nothing when it has none", async () => {
    const limits: TeamLimitsView = {
      ...teamLimitsPayments,
      global: [
        { ...teamLimitsPayments.global[0], modelLimit: "room_left" },
        { id: "m-opus", name: "Claude Opus", limit: 50, used: 0, modelLimit: "none" },
      ],
    };
    fakeApi(routes(maintainer, teamModelsPayments, limits));
    renderApp("/teams/2");
    const c = await card();
    expect(await within(c).findByRole("group", { name: "Claude Sonnet limit" })).toHaveTextContent("Model limit for all teams: room left");
    expect(within(c).getByRole("group", { name: "Claude Opus limit" })).not.toHaveTextContent("Model limit for all teams");
  });

  it("shows a viewer no card and asks for no limits", async () => {
    const asViewer: TeamModelsView = { ...teamModelsPayments, role: "viewer", canEdit: false, models: [{ ...teamModelQwen, baseURL: "" }] };
    const fake = fakeApi(routes(viewer, asViewer));
    renderApp("/teams/2");
    await screen.findByRole("region", { name: "Team chain" });
    expect(screen.queryByRole("region", { name: "Model limits" })).not.toBeInTheDocument();
    expect(fake.find("GET", "/api/teams/2/limits")).toHaveLength(0);
  });

  it("shows nothing when the server says the session may not see the limits", async () => {
    fakeApi({ ...routes(), "GET /api/teams/2/limits": { status: 403, body: { error: "only maintainers of this team see its model limits" } } });
    renderApp("/teams/2");
    await screen.findByRole("region", { name: "Team chain" });
    await waitFor(() => expect(screen.queryByRole("region", { name: "Model limits" })).not.toBeInTheDocument());
    expect(screen.queryByText("only maintainers of this team see its model limits")).not.toBeInTheDocument();
  });
});
```

**Step 2: Run and watch them fail**

```bash
cd web && npx vitest run src/pages/TeamLimits.test.tsx
```

Expected: the first 4 FAIL, no region `Model limits`. The viewer and 403 tests pass already (no card yet); they guard Step 3.

**Step 3: Write the code**

`web/src/pages/TeamLimits.tsx`:

```tsx
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { api, isStatus, paths, type Role, type TeamGlobalLimitView, type TeamLimitsView } from "../api";
import LimitField from "../LimitField";
import { QuotaLine } from "../quota";
import { ErrorBox, Loading } from "../ui";

// modelLimitText is a global entry's model limit as a team sees it: its
// state alone, because its numbers count other teams.
const modelLimitText: Record<TeamGlobalLimitView["modelLimit"], string> = {
  none: "",
  room_left: "Model limit for all teams: room left",
  used_up: "Model limit for all teams: used up",
};

// TeamLimits is the Model limits card of a team's page, for the team's
// maintainers and admins (canEdit); a viewer sees nothing and asks nothing.
// Each of the team's own entries has its limit, which they set with the
// team's route. Each global entry an admin limited for the team shows that
// limit, read-only for a maintainer; an admin changes it with the admin
// route, since the team's route takes the team's entries only.
export default function TeamLimits({ team, role, canEdit }: { team: number; role: Role; canEdit: boolean }) {
  const client = useQueryClient();
  const key = ["team-limits", team];
  const limits = useQuery({ queryKey: key, queryFn: () => api.get<TeamLimitsView>(paths.apiTeamLimits(team)), enabled: canEdit });
  if (!canEdit || isStatus(limits.error, 403)) return null;
  // A limit moves the bars here and the tags of the chains.
  const changed = () => {
    client.invalidateQueries({ queryKey: key });
    client.invalidateQueries({ queryKey: ["team-chain", team] });
    client.invalidateQueries({ queryKey: ["team-panel-chain", team] });
    client.invalidateQueries({ queryKey: ["repo-chain"] });
    client.invalidateQueries({ queryKey: ["repo-panel-chain"] });
    client.invalidateQueries({ queryKey: ["admin-models"] });
  };
  return (
    <section className="card" aria-label="Model limits">
      <h2>Model limits</h2>
      <div className="pad meta">
        Tokens per calendar month (UTC), input plus output, that the repositories this team owns may use on a model. Reviews skip
        a model at its limit. Empty: no limit.
      </div>
      {limits.isPending && <Loading />}
      {limits.isError && <ErrorBox error={limits.error} />}
      {limits.isSuccess && limits.data.models.length === 0 && limits.data.global.length === 0 && (
        <div className="pad meta">No model of the team's own, and no admin limit for the team.</div>
      )}
      {limits.isSuccess &&
        limits.data.models.map((m) => (
          <div className="pad stack" role="group" aria-label={`${m.name} limit`} key={m.id}>
            <b>{m.name}</b>
            <QuotaLine ariaLabel={`Tokens of ${m.name} this month`} use={{ used: m.used, limit: m.limit }} />
            <LimitField
              key={String(m.limit)}
              label={`Limit of ${m.name}`}
              saveLabel={`Save the limit of ${m.name}`}
              limit={m.limit}
              path={paths.apiTeamModelLimit(team, m.id)}
              onSaved={changed}
            />
          </div>
        ))}
      {limits.isSuccess &&
        limits.data.global.map((g) => (
          <div className="pad stack" role="group" aria-label={`${g.name} limit`} key={g.id}>
            <div className="row">
              <b className="grow">{g.name}</b>
              <span className="badge">global</span>
            </div>
            <QuotaLine ariaLabel={`Tokens of ${g.name} this month`} use={{ used: g.used, limit: g.limit }} />
            {role === "admin" ? (
              <LimitField
                key={g.limit}
                label={`Limit of ${g.name}`}
                saveLabel={`Save the limit of ${g.name}`}
                limit={g.limit}
                path={paths.apiAdminModelTeamLimit(g.id, team)}
                onSaved={changed}
              />
            ) : (
              <div className="meta">Set by an admin.</div>
            )}
            {g.modelLimit !== "none" && (
              <div className={g.modelLimit === "used_up" ? "meta warn-text" : "meta"}>{modelLimitText[g.modelLimit]}</div>
            )}
          </div>
        ))}
    </section>
  );
}
```

`TeamModels.tsx`: import `TeamLimits from "./TeamLimits"`; after the `</section>` of the **Models** card:

```tsx
      <TeamLimits team={v.team.id} role={v.role} canEdit={v.canEdit} />
```

and in `useTeamChanged` (a model added or removed changes the card's list):

```tsx
    client.invalidateQueries({ queryKey: ["team-limits", team] });
```

`TeamModels.test.tsx`: add `teamLimitsPayments` to the fixtures import and `"GET /api/teams/2/limits": { body: teamLimitsPayments },` to `routes()`. The card's groups are named `<name> limit`, so `entry("qwen3.5")` still finds one group. If an old test of that file still fails on a name the card also shows, scope its query with `within(...)` of its own region; do not rename the card's labels.

**Step 4: Run them**

```bash
cd web && npx vitest run src/pages/TeamLimits.test.tsx src/pages/TeamModels.test.tsx && npm run typecheck && npm run lint
```

Expected: PASS.

**Step 5: Commit**

```bash
git add web/src/pages/TeamLimits.tsx web/src/pages/TeamLimits.test.tsx web/src/pages/TeamModels.tsx web/src/pages/TeamModels.test.tsx
git -c user.email="$(git config --local user.email)" commit -m "feat(web): the team page's Model limits card" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 8: Docs

**Files:**
- Modify: `docs/concepts/token-quota.md` (`## When a quota is used up` ~41; `## What the dashboard shows` ~60; `## Model and team limits`, the hold bullet)
- Modify: `docs/concepts/models.md` (the state table ~149-159)
- Modify: `docs/concepts/dashboard.md` (the team page bullet ~165; **The models** bullet ~221)
- Modify: `docs/upgrading.md` (`## Unreleased`, the **Model limits** paragraph)

No test. The prose rules: easy English, no em dashes, no `→` in a UI path (write "the admin **Settings** page"), page texts and API values in backticks.

**Step 1: `docs/concepts/token-quota.md`**

- In `## Model and team limits`, the bullet that starts "**When only the quotas and the limits stop ...**": after "(`quota_used_up` when only quotas stopped it)." add "The repository page lists it under **Waiting for a start** with the line `No model in the chain has room this month.`, and only admins get its **Start review** button."
- At the end of `## What the dashboard shows`, a new paragraph:

```markdown
The limits are on 2 pages. The **Models** list on the admin **Settings** page shows each global entry, the config file's included, with `Tokens this month: <used> of <limit>` and a bar when it has a model limit, and a **Monthly token limit** field: empty is no limit. Under it, **Team limits** opens the list of each team's limit on the entry, with that team's tokens, where an admin adds, changes and removes them. A team's page has a **Model limits** card for the team's maintainers and admins: each of the team's own entries with its limit, which they set, and each global entry an admin limited for the team, which only an admin changes there. A global entry's model limit shows on that card only as `Model limit for all teams: room left` or `used up`, because its numbers count other teams. A viewer sees no card. Chains tag an entry at its limit with `model limit used up` or `team limit used up`: the global chain reads the model limits, and a team's chain and a repository's chain read the owner team's limits too.
```

**Step 2: `docs/concepts/models.md`**, 2 rows at the end of the state table:

```markdown
| `model limit used up` | Its model limit is used up this month ([Model and team limits](token-quota.md#model-and-team-limits)) |
| `team limit used up` | The owner team's limit on it is used up this month; on a team's chain, that team's |
```

**Step 3: `docs/concepts/dashboard.md`**

- The "**Teams in the user area.**" bullet: after "A maintainer of the team, or an admin, changes them." add "The team's maintainers and admins also see a **Model limits** card: each team entry's limit and its tokens this month, which they set, and each global entry an admin limited for the team, which only an admin changes there. A viewer sees no card ([Model and team limits](token-quota.md#model-and-team-limits))."
- "**The models**" bullet: after "see [Models](models.md)." add "Each entry shows its tokens this month, its model limit and its **Team limits** ([Model and team limits](token-quota.md#model-and-team-limits))."

**Step 4: `docs/upgrading.md`**, under `## Unreleased`, the **Model limits** paragraph: replace "The admin **Models** page and the team page show them in a later change; this release has the API." with "The **Models** list on the admin **Settings** page and the team page show and set them, every chain tags an entry at its limit, and **Waiting for a start** says `No model in the chain has room this month.` for a change a limit holds, whose **Start review** only admins get."

**Step 5: Check and commit**

```bash
grep -rn -i 'later change\|in a later\|this release has the API' docs/concepts docs/upgrading.md docs/reference
for f in docs/concepts/token-quota.md docs/concepts/models.md docs/concepts/dashboard.md docs/upgrading.md; do python3 <workflow checkout>/skills/prose-slop-check/check.py "$f" || echo "FAIL $f"; done
```

Expected: the `grep` shows no line about the limits pages (the `dashboard.md` "a later change in the config" line about defaults is unrelated); no `FAIL` line. Warnings that were in the files before this change (`repeated-opening`, `matching-lengths`) may stay; a new one from this change is fixed.

```bash
git add docs/concepts/token-quota.md docs/concepts/models.md docs/concepts/dashboard.md docs/upgrading.md
git -c user.email="$(git config --local user.email)" commit -m "docs: the dashboard pages for model limits" -m "Co-Authored-By: Claude <model> <noreply@anthropic.com>"
```

---

## Task 9: Final gate and review

**Step 1: The web gate**

```bash
cd web && npm run typecheck && npm run lint && npx vitest run && npm run build
git status --short internal/web
```

Expected: no error, every test file passes, the build ends with the `dist` sizes; `git status` shows nothing under `internal/web` (only `.keep` is tracked and the plugin writes it back).

**Step 2: The types match** (the Task 1 Step 4 script, run again from the repository root)

Expected: `types match`.

**Step 3: The focused Go tests** (Task 3 only; the full suite runs on CI after the push)

```bash
nc -z localhost 55432
DBTEST ./internal/dashboard/ -run 'TestChainViewsShowLimits|Chain|Limit' 2>&1 | grep -E -- '--- FAIL|build failed|ok '
go test -count=1 -run 'TestOpenAPI' ./internal/dashboard/
go vet ./...
gofmt -l .
git diff --name-only origin/main -- internal/store internal/engine internal/chain internal/quota charts
```

Expected: `ok` for both (the DB one takes seconds); `go vet` and `gofmt -l` print nothing; the last command prints nothing (no store, engine, chain, quota or chart change). If Postgres is down, say so: CI runs the DB test.

**Step 4: Review**

Use `superpowers:requesting-code-review` on the diff against `origin/main` (a reviewer on opus, or a self-review against its checklist when no Agent tool is there; say which). Check by hand:

- No page text names a number of a global entry's model limit on the team page: `git diff origin/main -- web/src/pages/TeamLimits.tsx | grep -n 'modelLimit'` shows only the state text.
- A viewer of a team makes no `GET /api/teams/{id}/limits` (the test checks it) and the admin **Models** list asks `/api/teams` only when **Team limits** is open.
- Every write sends `X-CSRF-Token` (`useWrite`), and every write's error reaches an `ErrorBox`.
- Each new list or card text is shown as text, never HTML (no `dangerouslySetInnerHTML` in the diff).
- `openapi.yaml` changed only the `ChainEntryView.state` description: `git diff origin/main -- openapi.yaml`.

**Step 5: The private-name scan and the last commit**

Run the private-name scan from the Global constraints, and:

```bash
git diff --name-only origin/main -- . ':!docs/superpowers/plans' | xargs grep -nE '@gmail\.com|/Users/|/private/tmp/'
```

Expected: both print nothing. Commit any review fix as `fix: <what>`. Do not push: the owner merges.
