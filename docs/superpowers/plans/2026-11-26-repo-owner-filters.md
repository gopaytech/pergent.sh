# Owner teams on the lists, and filters by team: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show each repository's owner team on the Repositories list, add search, a team filter and sort to it, turn both Teams pages into sortable tables, and filter the Reviews page by owner team.

**Architecture:** One new store call reads the owners of many repositories with the existing `ownerTeamSQL`. The dashboard adds `ownerTeam` to every `RepositoryView`, and `GET /api/reviews` takes `?team=`, which only narrows the session's visibility. Search, filter and sort of the Repositories and Teams tables run in the browser through one shared search hook and one shared sort hook.

**Tech Stack:** Go (net/http, pgx), PostgreSQL; React 18, TanStack Query, react-router 7, vitest with `fakeApi` and `renderApp`.

**Spec:** `docs/superpowers/specs/2026-11-26-repo-owner-filters-design.md`

## Global Constraints

- Every team filter means **owner team only**. A filter never widens what a session can read.
- The owner rule lives only in `ownerTeamSQL` (`internal/store/teams.go:301`): own entry first (case-insensitive), else the instance entry.
- Search, filter and sort live in the address: `?q=`, `?team=<id>|none`, `?sort=<key>` or `?sort=<key>-desc`, all written with `{ replace: true }`.
- Copy, verbatim: "⚠ No owner" (admin), "None" (non-admin), "N repositories have no owner. Show them" (1: "1 repository has no owner. Show it"), "Owns every repository on <instance>", "No repository matches.", "No team matches.", "Clear", "12 of 40 shown", "All teams", "No owner".
- No public repo may hold a work name: fixtures use `acme/*`, teams `devs`, `ops`, `payments`.
- Locally run only the focused test files named in each step. The full run is CI's. Never start containers; Go DB tests need `PERGENT_SH_TEST_DATABASE_URL` and skip without it, so CI is the proof.
- Easy English in comments, copy and docs; `python3 ~/Documents/personal/github/workflow/skills/prose-slop-check/check.py` passes on docs/upgrading.md.
- Commits signed off (`git commit -s`), ending with the `Co-Authored-By` line.

## Review Focus

1. **A team is renamed or removed while the Repositories list is cached.** Expected: the list shows the new name or "No owner" after the team change, not a stale owner. Test in Task 6.
2. **`?team=` names a team that is no longer in the list** (removed, or a stale shared link). Expected: Repositories treats it as All teams and the select shows All teams; Reviews does not send it and shows all reviews, not a 400 error. Tests in Tasks 5 and 7.
3. **`?sort=` holds an unknown key** (`?sort=bogus`, `?sort=-desc`). Expected: the default sort, name A to Z, and no crash. Test in Task 4.
4. **Search text with characters that mean something in a regex or URL** (`(`, `%`, `+`, leading spaces). Expected: a plain substring match on the trimmed text, no crash. Test in Task 5.
5. **A long team name** in the Owner team column. Expected: it wraps inside the cell and the table does not grow past the page. CSS in Task 5 (`td.owner { max-width: 220px; overflow-wrap: anywhere }`), checked by eye in the preview.

---

## File map

| File | Change |
|---|---|
| `internal/store/teams.go` | + `OwnerTeams` |
| `internal/store/teams_test.go` | + `TestOwnerTeams` |
| `internal/dashboard/view.go` | `RepositoryView.OwnerTeam`, `OwnerTeamView.ViaInstance` |
| `internal/dashboard/api_read.go` | `repositoryViews` fills the owner; `ownerViews`; `reviews` reads `?team=`; `narrowToTeam` |
| `internal/dashboard/api_read_test.go` | + `TestRepositoriesOwnerTeam`, `TestReviewsTeamFilter` |
| `openapi.yaml` | `RepositoryView.ownerTeam`, `OwnerTeamView.viaInstance`, `team` query param of `/api/reviews` |
| `docs/upgrading.md` | 2 bullets under `## Unreleased` |
| `web/src/api.ts` | `RepositoryView.ownerTeam`, `OwnerTeamView.viaInstance` |
| `web/src/test/fixtures.ts` | `ownerTeam` on every `RepositoryView` fixture |
| `web/src/useSearchText.ts` | new: the hook moved out of `AuditLog.tsx` |
| `web/src/TableSort.tsx` (+ `.test.tsx`) | new: `useTableSort`, `SortHeader`, `compareRows` |
| `web/src/pages/Repositories.tsx` (+ test) | column, No owner, admin line, toolbar, sort |
| `web/src/pages/MyTeams.tsx`, `Teams.tsx` (+ tests) | tables |
| `web/src/pages/teamText.ts` | removed if unused |
| `web/src/pages/Team.tsx` | team saves and removes also refresh `["repositories"]` |
| `web/src/pages/Reviews.tsx`, `ReviewList.tsx` (+ test) | team dropdown, `team` in the filter |
| `web/src/styles.css` | toolbar, sort buttons, owner cell |

---

### Task 1: `Store.OwnerTeams`

**Files:**
- Modify: `internal/store/teams.go` (after `OwnerTeam`, line 317)
- Test: `internal/store/teams_test.go`

**Interfaces:**
- Produces: `func (s *Store) OwnerTeams(ctx context.Context, keys []PathKey) (map[PathKey]int64, error)`. The returned keys are `PathKey{Instance, strings.ToLower(Path)}`; a key with no owner is absent. An empty `keys` returns an empty map with no query.

- [ ] **Step 1: Write the failing test** `TestOwnerTeams`, set up like `TestOwnerMostSpecific` (team A owns instance `main`; team B owns `main/acme/api` and covers `main/acme/web` without owning it):

```go
got, err := s.OwnerTeams(ctx, []PathKey{
	{"main", "ACME/api"}, {"main", "acme/web"}, {"main", "acme/other"}, {"other", "acme/api"},
})
want := map[PathKey]int64{{"main", "acme/api"}: b.ID, {"main", "acme/web"}: a.ID, {"main", "acme/other"}: a.ID}
// err == nil; maps.Equal(got, want)
// and: each key agrees with s.OwnerTeam for the same instance and path
// and: s.OwnerTeams(ctx, nil) returns an empty map, nil
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/store -run TestOwnerTeams` (with `PERGENT_SH_TEST_DATABASE_URL` set; without it the test skips, and `go vet ./internal/store` must at least fail on the missing method)
Expected: FAIL, `s.OwnerTeams undefined`.

- [ ] **Step 3: Implement `OwnerTeams`** with one query that reuses `ownerTeamSQL` as it is:

```sql
SELECT r.instance, lower(r.project), `+ownerTeamSQL("r.instance", "r.project")+`
FROM unnest($1::text[], $2::text[]) AS r(instance, project)
```

Skip rows whose team is NULL.

- [ ] **Step 4: Run the test** (same command). Expected: PASS, or SKIP locally with `go vet ./internal/store` clean.

- [ ] **Step 5: Commit** `feat(store): read the owner teams of many repositories at once`.

---

### Task 2: `ownerTeam` on every listed repository

**Files:**
- Modify: `internal/dashboard/view.go:468-481` (`RepositoryView`), `:550` (`OwnerTeamView`)
- Modify: `internal/dashboard/api_read.go:1070` (`repositoryViews`), `:1260` (`ownerTeamView`)
- Modify: `openapi.yaml:7742` and `:7823`, `web/src/api.ts:323` and `:1412`, `web/src/test/fixtures.ts`
- Test: `internal/dashboard/api_read_test.go`

**Interfaces:**
- Consumes: `Store.OwnerTeams` (Task 1), `Store.Teams`.
- Produces:
  - Go: `RepositoryView.OwnerTeam *OwnerTeamView \`json:"ownerTeam"\`` (null when none); `OwnerTeamView.ViaInstance bool \`json:"viaInstance"\`` (true when the owner comes from the instance entry).
  - `func (d *Dashboard) ownerViews(ctx context.Context, se Session, keys []RepoKey) (map[RepoKey]*OwnerTeamView, error)`.
  - TS: `RepositoryView.ownerTeam: OwnerTeamView | null`; `OwnerTeamView = { id: number; name: string; open: boolean; viaInstance: boolean }`.

> A spec deviation, to note in the PR: `viaInstance` is added to `OwnerTeamView` so the list can show "Owns every repository on <instance>" (spec section 2). It is worked out in Go from the owning team's own entries (no owner entry for that path means via the instance), so the owner rule itself stays in SQL.

- [ ] **Step 1: Write the failing test** `TestRepositoriesOwnerTeam`: `e.team("devs", members-with-the-viewer, TeamRepo{main, "acme/payments", Owner: true})` and `e.team("ops", nil, TeamRepo{main, "", Owner: true})`. Then use a third repository on another instance with no owner, and switch on `acme/payments` and `acme/widgets` on `main`.

```go
// admin: GET /api/repositories
// acme/payments -> &OwnerTeamView{ID: devs, Name: "devs", Open: true, ViaInstance: false}
// acme/widgets  -> &OwnerTeamView{ID: ops, Name: "ops", Open: true, ViaInstance: true}
// the other-instance repository -> nil
// a non-admin member of devs only: acme/widgets' owner has Open == false
// GET /api/repositories/main/acme/payments: detail.Repo.OwnerTeam equals detail.OwnerTeam
```

- [ ] **Step 2: Run it to see it fail.** `go test ./internal/dashboard -run TestRepositoriesOwnerTeam`. Expected: FAIL to compile, `OwnerTeam` is not a field of `RepositoryView`.

- [ ] **Step 3: Implement.**
  - `ownerViews` calls `OwnerTeams` once and `Store.Teams` once. `Open = se.Admin || member`.
  - `repositoryViews` sets `OwnerTeam` from it.
  - `ownerTeamView` (the repository page) sets `ViaInstance` the same way.
  - Update the openapi schemas (add `ownerTeam` nullable and `viaInstance` to `required`) and the TS types.
  - Add `ownerTeam: null` to every `RepositoryView` fixture, and give `repoPayments` `{ id: 1, name: "payments", open: true, viaInstance: false }`.

- [ ] **Step 4: Run the tests.**
  - `go test ./internal/dashboard -run 'TestRepositoriesOwnerTeam|TestSwitchOnOwnerTeam'` (PASS, or SKIP without the DB).
  - `cd web && npx tsc --noEmit` (clean).

- [ ] **Step 5: Commit** `feat(dashboard): the owner team of each listed repository`.

---

### Task 3: `GET /api/reviews?team=`

**Files:**
- Modify: `internal/dashboard/api_read.go:352` (`reviews`), with a new `narrowToTeam` beside `projectVisibility` (`:403`)
- Modify: `openapi.yaml` (the `/api/reviews` parameters), `docs/upgrading.md`
- Test: `internal/dashboard/api_read_test.go`

**Interfaces:**
- Consumes: `Store.OwnerTeams`, `Store.Projects`, `Store.Team`.
- Produces: `func (d *Dashboard) narrowToTeam(ctx context.Context, v store.Visibility, team string) (store.Visibility, error)`. `team` is a decimal id or `"none"`. `v.All` is first expanded to the stored projects (as `projectVisibility` does). It keeps the projects whose owner is that team, or that have no owner for `"none"`.

- [ ] **Step 1: Write the failing test** `TestReviewsTeamFilter`. The setup: reviews on `acme/payments` (owned by devs), `acme/widgets` (owned by ops through the instance) and `acme/tools` on instance `gh` (no owner).

```go
// admin: ?team=<devs> -> only acme/payments items; ?team=none -> only acme/tools
// admin: ?team=<devs>&project=acme/widgets -> 0 items (both filters apply)
// non-admin who sees only acme/widgets: ?team=<devs> -> 0 items, never acme/payments
// ?team=abc -> 400 msgTeamFilter ("team must be a team's id or none")
// ?team=999999 -> 400 msgNoTeam (reuse the existing constant)
// ?team=<a team that owns nothing visible> -> 200, 0 items
```

- [ ] **Step 2: Run it to see it fail.** `go test ./internal/dashboard -run TestReviewsTeamFilter`. Expected: FAIL, because `?team=` is ignored and the lists hold every review.

- [ ] **Step 3: Implement.** In `reviews`, after the `project` narrowing, run `if t := q.Get("team"); t != ""`: check it (non-number and not `none` gives 400 `msgTeamFilter`; `Store.Team` not found gives 400 `msgNoTeam`), then call `filter.Visible, err = d.narrowToTeam(ctx, filter.Visible, t)`. Document the `team` query parameter in openapi.yaml (string, `pattern: '^([0-9]+|none)$'`).

- [ ] **Step 4: Add 2 bullets under `## Unreleased`** in docs/upgrading.md:
  - "`GET /api/repositories` gives each repository its `ownerTeam` (null when none). `OwnerTeamView` gets `viaInstance`."
  - "`GET /api/reviews` takes `team`: a team's id, or `none` for repositories with no owner. It only narrows what you can already see."

  Run the prose check on the file.

- [ ] **Step 5: Run the tests.** `go test ./internal/dashboard -run 'TestReviewsTeamFilter|TestReviews'`. Expected: PASS (or SKIP without the DB).

- [ ] **Step 6: Commit** `feat(dashboard): filter reviews by owner team`.

---

### Task 4: Shared search and sort for tables

**Files:**
- Create: `web/src/useSearchText.ts` (move `useSearchText` from `web/src/pages/AuditLog.tsx:88-114` unchanged; `AuditLog.tsx` imports it)
- Create: `web/src/TableSort.tsx`, test `web/src/TableSort.test.tsx`
- Modify: `web/src/styles.css` (`.sort-btn`: an inherit-font button with no border, background or padding, and a small arrow)

**Interfaces:**
- Produces:
  - `export function useSearchText(): [typed: string, setTyped: (v: string) => void, sent: string]` (as today).
  - `export type SortState<K extends string> = { key: K; desc: boolean }`.
  - `export function useTableSort<K extends string>(keys: readonly K[], fallback: K, numeric: readonly K[]): [SortState<K>, (key: K) => void]`. It reads `?sort=`. An unknown value means `{ key: fallback, desc: false }`. Toggle: the same key flips; a new numeric key starts `desc: true`; a new text key starts `desc: false`. The fallback ascending removes `sort` from the address. It writes with `{ replace: true }` and keeps other params.
  - `export function SortHeader<K extends string>({ label, sortKey, sort, onSort, num }: { label: string; sortKey: K; sort: SortState<K>; onSort: (k: K) => void; num?: boolean })`. It renders a `<th aria-sort="ascending|descending|none">` holding `<button type="button" className="sort-btn">`.
  - `export function compareRows<T>(a: T, b: T, value: (r: T) => string | number, desc: boolean, name: (r: T) => string): number`. Ties fall back to `name` ascending.

- [ ] **Step 1: Write the failing tests** in `TableSort.test.tsx`. Use a tiny route rendered with a `MemoryRouter` at `/x?sort=...`:

```ts
// no sort param -> header "Name" aria-sort="ascending", others "none"
// click "Reviews" -> ?sort=reviews-desc, aria-sort="descending"; click again -> ?sort=reviews
// click "Name" while on reviews -> sort param removed (fallback ascending)
// ?sort=bogus and ?sort=-desc -> treated as name ascending (Review Focus 3)
// other params (?q=pay) survive a toggle
// compareRows: equal values order by name ascending
```

- [ ] **Step 2: Run them to see them fail.** `cd web && npx vitest run src/TableSort.test.tsx src/pages/AuditLog.test.tsx`. Expected: TableSort FAIL (module missing); AuditLog PASS.

- [ ] **Step 3: Implement** both files and the move.

- [ ] **Step 4: Run** the same command. Expected: all PASS. Also `npx tsc --noEmit` clean.

- [ ] **Step 5: Commit** `refactor(dashboard): shared table search and sort`.

---

### Task 5: Repositories page

**Files:**
- Modify: `web/src/pages/Repositories.tsx`, `web/src/styles.css`
- Test: `web/src/pages/Repositories.test.tsx`

**Interfaces:**
- Consumes: `useSearchText`, `useTableSort`, `SortHeader`, `compareRows` (Task 4); `RepositoryView.ownerTeam` (Task 2).
- Produces: none used by later tasks. Sort keys: `name | reviews | approved | failed`, fallback `name`, numeric `reviews | approved | failed`.

- [ ] **Step 1: Write the failing tests** (a new `describe("Repositories: owner, search, filter, sort")`). The fixtures: `repoPayments` (owner payments), `repoWidgets` (no owner, on), `repoToolsPaused` (no owner, paused).

```ts
// Owner team column: "payments" links to /teams/1 when open; plain text when open is false
// viaInstance owner: cell has title "Owns every repository on main"
// admin: widgets row shows "⚠ No owner" linking to /repos/main/acme/widgets?tab=settings
// non-admin (viewer): widgets row shows "None", no link, no "⚠"
// admin: "1 repository has no owner. Show it" (paused tools not counted); click -> ?team=none, rows = widgets + tools, line hidden
// viewer: no such line
// search "PAY" -> only acme/payments; "12 of 40"-style text reads "1 of 3 shown"; search "(" or "%" -> no crash, "No repository matches." (Review Focus 4)
// Clear -> all 3 rows, address has no q or team
// team select options: "All teams", "No owner", "payments"; choose payments -> ?team=1
// ?team=99 (not in list) -> select shows "All teams", all rows (Review Focus 2)
// click "Reviews" header -> rows ordered by last7.reviews descending; again -> ascending
// "+ Add repository" with ?q=pay -> address keeps q=pay and adds add=1
```

- [ ] **Step 2: Run them to see them fail.** `cd web && npx vitest run src/pages/Repositories.test.tsx`. Expected: the new tests FAIL; the old ones PASS.

- [ ] **Step 3: Implement.**
  - The column goes after Repository, as `<td className="owner">`.
  - The No owner line sits above the card (admins only, active repositories only).
  - The toolbar is a `pad search` row inside the card: search, team select, then "N of M shown" while filtering.
  - The headers use `SortHeader`.
  - The empty match shows a `.pad.empty` with a Clear button.
  - `setParams` for add changes only `add`.
  - CSS: `td.owner { max-width: 220px; overflow-wrap: anywhere }` (Review Focus 5).

- [ ] **Step 4: Run** `npx vitest run src/pages/Repositories.test.tsx src/pages/Reviews.test.tsx` and `npx tsc --noEmit`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(dashboard): owner team, search, team filter and sort on the repository list`.

---

### Task 6: Teams pages as tables

**Files:**
- Modify: `web/src/pages/MyTeams.tsx`, `web/src/pages/Teams.tsx`, `web/src/pages/Team.tsx`
- Delete: `web/src/pages/teamText.ts`, if `grep -rn teamText web/src` shows no other user
- Test: `web/src/pages/MyTeams.test.tsx`, `web/src/pages/Teams.test.tsx`, `web/src/pages/Team.test.tsx`

**Interfaces:**
- Consumes: Task 4's hooks. Sort keys: `team | repos | owned | members`, fallback `team`, numeric `repos | owned | members`.

- [ ] **Step 1: Write the failing tests.**

```ts
// /teams: headers Team, Your role, Repositories, Owner of, Members; a row shows "admin", "4", "4", "6"
// a team with 1 whole-instance entry shows "+1 instance" under its Repositories number
// search "pay" -> one row; no match -> "No team matches." + Clear
// click "Members" -> most members first
// /admin/teams: no "Your role" column, no "Remove" button anywhere in the table; "Create a team" form still above
// empty states unchanged ("You are on no team yet…", "No teams yet…")
// Team page: saving a rename and removing a team each invalidate ["repositories"] (Review Focus 1)
```

- [ ] **Step 2: Run them to see them fail.** `cd web && npx vitest run src/pages/MyTeams.test.tsx src/pages/Teams.test.tsx src/pages/Team.test.tsx`. Expected: the new tests FAIL.

- [ ] **Step 3: Implement.** Build both tables. In `Teams.tsx`, the counts come from `team.repos` (as `TeamCard` does today); drop `TeamCard` and its remove mutation. In `Team.tsx`, add `["repositories"]` to the invalidations on save and on remove.

- [ ] **Step 4: Run** the same command and `npx tsc --noEmit`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(dashboard): teams as sortable tables`.

---

### Task 7: Team filter on the Reviews page

**Files:**
- Modify: `web/src/pages/Reviews.tsx`, `web/src/pages/ReviewList.tsx:8-24`
- Test: `web/src/pages/Reviews.test.tsx`

**Interfaces:**
- Consumes: `RepositoryView.ownerTeam` (Task 2); `GET /api/reviews?team=` (Task 3).
- Produces: `ReviewFilter = { project?: string; outcome?: string; days: number; team?: string }`. `team` goes in the query key and the request.

- [ ] **Step 1: Write the failing tests.**

```ts
// a "Team" select before "Repository": "All teams", "No owner", "payments"
// choose payments -> request /api/reviews?team=1&days=90; the Repository select lists only acme/payments
// with ?project=acme/widgets, choosing payments clears project (widgets is not owned by payments)
// ?team=99 (not an owning team in the list) -> request has no team param, select shows "All teams" (Review Focus 2)
// the empty-list Clear link goes to /reviews when only team is set
```

- [ ] **Step 2: Run them to see them fail.** `cd web && npx vitest run src/pages/Reviews.test.tsx`. Expected: the new tests FAIL.

- [ ] **Step 3: Implement.** Add `team` to `set`, the select, the narrowed `projects`, `ReviewFilter`, and `clearTo`.

- [ ] **Step 4: Run** `npx vitest run src/pages/Reviews.test.tsx src/pages/RepositoryDetail.test.tsx` and `npx tsc --noEmit` and `npx eslint src`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(dashboard): filter the review list by owner team`.

---

### Task 8: Look at it, then hand over

- [ ] **Step 1:** Check by eye in a local preview (the temporary `web/preview.html` + `src/preview.tsx` fetch stub; delete both before committing):
  - Repositories as admin and as a viewer, with a long team name;
  - both Teams pages;
  - Reviews with a team picked.
- [ ] **Step 2:** Run the name scan on `git diff origin/main` for work names; it must find nothing.
- [ ] **Step 3:** Stop. Pushing and opening the PR wait for the owner's word; CI then runs the full Go (with the DB) and web suites.
