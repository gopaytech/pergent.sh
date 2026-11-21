# Path rules, PR 1 (pergent.sh) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teams keep rule sets in the dashboard and `.pergent/rules.yaml` in the repository. pergent.sh builds one ordered rule list per review, passes it to the Job, records its hash, reviews a skill with rules in full when the hash changes, and shows which rule reviewed which file.

**Architecture:** Rule sets live in Postgres (`rule_sets`, migration `0047`) and in the config file. Every set and every repository file is checked with pergent's public `rule.Load`, the loader the CLI uses. `internal/inputs` reads the repository file at the target ref, appends the sets and hashes the list. `internal/kube` mounts it as `/in/rules.json` behind `PERGENT_RULES`. The dashboard gets a Rules library page and two settings.

**Tech Stack:** Go 1.24, pgx, `github.com/gopaytech/pergent/rule` and `render`, React + Vite + TanStack Query.

**Spec:** `docs/superpowers/specs/2026-11-20-path-rules-design.md` (agreed; read it in full, with its anti-goals). The pergent plan is pergent `docs/superpowers/plans/2026-11-21-path-rules.md`.

## Order across the repositories

1. pergent's plan merges to pergent `master` first. It adds the public package `rule/` (the spec says `internal/rule`, but pergent.sh cannot import another module's `internal/` package) and `render.Skill.FileRules`.
2. Task 1 here moves `go.mod` to that merge commit with `go get github.com/gopaytech/pergent@<merge sha>` (a pseudo-version, no `replace`; memory ruling of 2026-11-19). Every later task needs it.
3. `defaultImage` does not move: that is a release step for the owner. Until then the Job runs an image that ignores `PERGENT_RULES` (an environment variable, so it does not exit 2), and the review page says `This image does not use rules`. pergent.sh tests use fake Job results, so nothing here needs a release.

## Checked facts (origin/main `1cad1a9`, 2026-11-21)

| Question | Answer | Where |
|---|---|---|
| How a new Job input reaches pergent | An `has*` flag per input (`hasCI`, `hasGraph`), then an env var pointing at an `/in` file, with the comment "An environment variable, not an argument: an older image ignores a variable it doesn't know, but exits 2 on an unknown flag". `PERGENT_GRAPH` and `PERGENT_CHECK_DELETED` follow it. ConfigMap items and data are listed per file. | `internal/kube/job.go:270-278,305-340,396-421,488-512` |
| Inputs size | `Inputs.Size()` sums every input; `Fit` drops CI results, dismissals, description, then previous and carried, then `ErrTooLarge`. `ConfigMapLimit = 900 * 1024`. | `internal/inputs/inputs.go:18,307-344` |
| Where dispatch reads skills | `ResolveLibrary` (408), `inputs.ResolveSkills(..., mr.StartSHA, ...)` (419), `subset` (432), `planCarried` (449), `Fit` (488), `SetRunModel` before the Job (583), `JobParams` (624, `CheckDeleted` at 645), attention cleared once a Job starts (678). | `internal/engine/dispatch.go` |
| Where incremental carry is decided | `planCarried` gates the push (cheap cases first, 54-69), then `carriedFiles` drops a skill whose hash differs (`now != was`, 205-209). The log reasons are constants at 24-45. | `internal/engine/incremental.go:24-98,182-262` |
| A failed run that shows under "Needs attention" | `failSkills` sets attention `skill_missing` and fails the run; a started Job clears it. Kinds are free strings. | `internal/engine/dispatch.go:1149-1152,678`, `internal/engine/attention.go:20-36` |
| Next migration number | The last on `origin/main` is `0046_chain_saved_by_admin.sql`. Next: **`0047_rule_sets.sql`** (the spec's `0046` is taken). | `internal/store/migrations/` |
| Where the library skill frontmatter check needs `rules` | `frontmatter` struct and `decodeFrontmatter`, which type-checks `paths`, `exclude`, `cross_batch` on the loose form and ignores other fields; `CheckSkill` calls it. | `internal/inputs/frontmatter.go:45-50,83-109`, `internal/inputs/check.go:16-50` |
| Skill library patterns to copy | Admin writes via `adminRoute` (`api_skills.go:203-208`), `refuseSkill` (check then held secrets, 227-249), 409 in use with `configReposNaming` (404-460), `lockNamedSkills` and `DeleteLibrarySkill` (`store/library.go:344-422`), `ResolveLibrary` (425-470). | as named |
| API reviews | `dispatchAPI` uses the client's library skills, builds `Inputs` by hand and "calls no forge". It reads no repository file at any ref. | `internal/engine/apireview.go:40-180` |
| Review panel | Not built: no code and no plan. | `git grep -i panel` |

## Owner decisions (2026-11-21)

The owner settled 3 points the spec left open or that the code did not allow:

1. **API reviews use the admin defaults.** An API review has no repository ref: its files come from the client's import stream, its skills from the client's library list, and it never calls a forge (`apireview.go:40-47`). So it gets the admin default rule-set list and the default **Built-in rules** switch (it already takes the dashboard's defaults for timeout and memory, `apireview.go:124-129`), no repository file, and a stored `rules_hash`. Task 12.
2. **The result shape is `files.rules: [{path, rule}]`** next to `files.reviewed`, for the files that had a rule. `schema_version` stays 1. Task 9.
3. **PR 1 hashes the rule list and the built-in switch only.** `jobs.image` joins the hash in PR 2, when the built-in texts ship inside the image. See "PR 2 follow-ups".

Task 15 amends the spec so it says what PR 1 builds.

## Judgement calls the owner accepted

- Config-file sets are read-only in the dashboard. A dashboard create or rename to a name a config set holds is 409. At dispatch a config set wins over a stored one of the same name.
- The config file holds a set's text as `content:` (the same YAML a dashboard set holds), so there is one format and one loader.
- An empty rule list adds nothing to the Job (no `PERGENT_RULES`), so the Job of a repository with no rules is byte for byte today's.
- A run with no recorded `rules_hash` (before this change) counts as a changed hash.
- A verification run whose `file-rules.md` cannot be written fails verification and keeps every finding (pergent plan, Task 7).
- `rules: yes` and other non-bool values are refused.

## Global Constraints

- Branch `feat/path-rules` from `origin/main` (`git fetch` first), after pergent's PR is merged. No release, tag, chart version, version number or `defaultImage` change.
- The migration only adds (CLAUDE.md): new table, NULL columns.
- Limits: 200 rule sets; per set and per repository file the `rule.Load` limits (16,000 characters per rule, 100 entries, 64 KB).
- Rule-set names: `inputs.ValidName` (`^[A-Za-z0-9][A-Za-z0-9._-]*$`).
- Order: repository file, then each set the repository uses, in its order, then built-ins. Settings: `rule_sets` (ordered names; `null` = the admin default list), `built_in_rules` (bool, default `true`).
- The repository file is read at `mr.StartSHA`, the same ref as repository skills. Never the change's head.
- A malformed repository file, a missing set and a stored set that no longer loads each fail the run with the problem named, and set attention `rules_invalid`. Nothing is posted as clean.
- The hash covers the list and the built-in switch. Not `jobs.image` in PR 1 (owner decision 3).
- Rule sets get the skill library's secret check on save (`inputs.ContainsSecret` with `heldSecrets`). An answer never quotes a secret or the rule text.
- Permissions: everyone with a team reads rule sets and the built-in list; admins write (`adminRoute`, 404 for a non-admin). `openapi.yaml` gets every route, schema and `x-pergent-access` tag.
- Not in this PR: built-in texts, `LICENSE`/`NOTICE`, the built-ins upgrade note (PR 2).
- Commit with the owner's personal address (`git -c user.email="$(git -C <main checkout> config --local user.email)" commit`), conventional subjects, the `Co-Authored-By` line of the model that wrote the change. Never write the address or a local path into a file.
- Never start a container. DB tests run only against the already-running `pergent-sh-pg` on port 55432 (`nc -z localhost 55432`); if it is down, say so and leave them to CI.

## Review Focus

1. **A change edits its own rules.** Dispatch must read `.pergent/rules.yaml` at `mr.StartSHA`, never `HeadSHA`. Test: `TestDispatchRulesFromTargetRef` (Task 7) asserts the ref the fake forge was asked for.
2. **A rule set deleted between save and dispatch.** A repository naming a set that is gone must fail the run, never review without it. Test: `TestDispatchMissingRuleSetFails` (Task 7).
3. **The inputs over the ConfigMap limit.** `Fit` must never drop the rules; too big fails the run with the usual reason. Test: `TestFitNeverDropsRules` (Task 5).
4. **A skill switched to `rules: false` while files are carried.** Carry for that skill must follow the skill hash only. Test: `TestCarryRulesOffSkillCarriesOverRulesChange` (Task 8).
5. **A rule set that holds a secret pergent.sh knows.** Refused on create and on update, with an answer that names no secret. Test: `TestRuleSetSecretRefused` (Task 10).

---

### Task 1: Move the pergent pin

**Files:** Modify `go.mod`, `go.sum`.

- [ ] **Step 1:** `go get github.com/gopaytech/pergent@<pergent merge sha>` then `GOFLAGS=-mod=mod GOPROXY=off go mod tidy` if the module is cached (else plain `go mod tidy`).
- [ ] **Step 2:** `go build ./... && go vet ./...` — no error. `go test ./internal/policy/... ./internal/result/... -count=1` — PASS (render changes must leave today's comments as they are).
- [ ] **Step 3: Commit** — `git commit -m "build: pin pergent with the rule package"`

### Task 2: Migration and the rule-set store

**Files:**
- Create: `internal/store/migrations/0047_rule_sets.sql`, `internal/store/rulesets.go`, `internal/store/rulesets_test.go`

**Interfaces:**
- Produces: `type RuleSet struct{ Name, Content string; Version int; CreatedAt, UpdatedAt time.Time }`; `const MaxRuleSets = 200`; `ErrRuleSetNotFound`, `ErrRuleSetExists`, `ErrRuleSetsFull`, `ErrRuleSetVersion` (stale `expect`); `type RuleSetInUseError struct{ Repos []RepoRef; InDefaults bool }`; `type MissingRuleSetError struct{ Name string }` (`Is(ErrRuleSetNotFound)`); `func (s *Store) RuleSets(ctx) ([]RuleSet, error)` (by name); `RuleSet(ctx, name) (RuleSet, error)`; `CreateRuleSet(ctx, name, content string, at time.Time) error`; `UpdateRuleSet(ctx, name, content string, expect int, at time.Time) (int, error)`; `DeleteRuleSet(ctx, name string, configRepos []RepoRef) error`; `ResolveRuleSets(ctx, names []string) (ResolvedRuleSets, error)` with `ResolvedRuleSets{Sets map[string]RuleSet; Missing []string}`, `nil` names = the default list.

- [ ] **Step 1: Write the migration:**

```sql
-- Path rules: rule sets, the settings that name them, and the hash of the
-- list each run was given. It only adds, so the previous release runs on the
-- migrated database and ignores them.
CREATE TABLE rule_sets (
    name       text        PRIMARY KEY CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*$'),
    content    text        NOT NULL,
    version    integer     NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
ALTER TABLE repo_defaults ADD COLUMN rule_sets text[];
ALTER TABLE repo_defaults ADD COLUMN built_in_rules boolean;
ALTER TABLE dashboard_repos ADD COLUMN rule_sets text[];
ALTER TABLE dashboard_repos ADD COLUMN built_in_rules boolean;
ALTER TABLE runs ADD COLUMN rules_hash text;
```

- [ ] **Step 2: Write the failing tests:** `TestRuleSetCRUD` (create v1, update with `expect` 1 gives v2, stale `expect` is `ErrRuleSetVersion`, list sorted by name); `TestRuleSetCreateTaken` (`ErrRuleSetExists`); `TestRuleSetLimit` (the 201st create is `ErrRuleSetsFull`); `TestRuleSetDeleteInUse` (named by a dashboard repo, by the defaults, or by a config repo passed in → `*RuleSetInUseError` with each, nothing deleted; free → deleted); `TestResolveRuleSets` (named sets and `Missing` in the order given, each once; `nil` reads `repo_defaults.rule_sets`); `TestRuleSetDeleteRacesSave` (a save of a list that names the set locks it `FOR KEY SHARE`, as `lockNamedSkills`).
- [ ] **Step 3: Run** — `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test ./internal/store/ -run RuleSet -count=1`. Expected: FAIL. (Without Postgres up, say so: the tests skip.)
- [ ] **Step 4: Implement** in `rulesets.go`, copying `library.go`'s locking and error shapes.
- [ ] **Step 5: Run** — same. Expected: PASS.
- [ ] **Step 6: Commit** — `git commit -m "feat(store): add rule sets"`

### Task 3: Settings columns and the run's hash

**Files:**
- Modify: `internal/store/dashboard.go:19-` (`DashboardRepo`), `internal/store/instances.go:305-` (`RepoDefaults`), `internal/store/runs.go:120-140,300-320,1218-1224` (`Run.RulesHash`, scan, setter)
- Test: `internal/store/rulesets_test.go`, `internal/store/dashboard_test.go`

**Interfaces:**
- Produces: `DashboardRepo.RuleSets []string`, `DashboardRepo.BuiltInRules *bool`; `RepoDefaults.RuleSets []string`, `RepoDefaults.BuiltInRules *bool`; `Run.RulesHash *string`; `func (s *Store) SetRunRulesHash(ctx, runID, hash string) error` (queued or running runs only, like `SetRunModel`). Saving `RuleSets` locks the named sets and returns `*MissingRuleSetError` for a name that is gone.

- [ ] **Step 1: Write the failing tests:** `TestDashboardRepoRuleSetsRoundTrip` (nil stays NULL, a list keeps its order, `BuiltInRules` nil/true/false); `TestRepoDefaultsRuleSets`; `TestSetRunRulesHash` (set on a queued run, read back by `LastReview`; a reported run is an error); `TestSaveRuleSetsMissing`.
- [ ] **Step 2: Run** — the Task 2 command with `-run 'RuleSets|RulesHash'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(store): store rule-set settings and the run's rules hash"`

### Task 4: Config file and settings

**Files:**
- Modify: `internal/config/config.go:57-,237-275,350-430,588-600,800-840`, `internal/settings/settings.go:33-60,70-150,304-345,492-560`
- Test: `internal/config/rules_test.go`, `internal/settings/settings_test.go`

**Interfaces:**
- Consumes: `rule.Load`, `rule.DashboardSource`.
- Produces: `Config.RuleSets []ConfigRuleSet` with `ConfigRuleSet{Name, Content string; Entries []rule.Entry}` (top-level `rule_sets: [{name, content}]`); `Repository.RuleSets []string` (`rule_sets`), `Repository.BuiltInRules *bool` (`built_in_rules`); `settings.KeyRuleSets = "rule_sets"`, `settings.KeyBuiltInRules = "built_in_rules"`; `Effective.RuleSets []string` (nil = default list), `Effective.BuiltInRules bool`.

- [ ] **Step 1: Write the failing tests:** `TestConfigRuleSetsLoad` (a set loads, its entries carry source `dashboard:<name>`); `TestConfigRuleSetBad` (bad name, duplicate name, or content `rule.Load` refuses → `Load` fails naming `rule_sets[1]` and the loader's message); `TestConfigRepoRuleSets` (`repositories[].rule_sets` and `built_in_rules` decode; `rule_sets: []` is an error, as for `skills`); `TestEffectiveRuleSets` (defaults: `RuleSets` nil, `BuiltInRules` true; stored defaults and a repository row override with `Overridden` set; the config file wins over the row).
- [ ] **Step 2: Run** — `go test ./internal/config/ ./internal/settings/ -run 'Rule' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement**, following `CheckDeleted` for the bool and `Skills` for the list.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(config): rule sets and the rule settings"`

### Task 5: Inputs: the `rules` frontmatter field, the repository file, the list and the hash

**Files:**
- Modify: `internal/inputs/frontmatter.go:45-50,52-109`, `internal/inputs/check.go:16-22`, `internal/inputs/inputs.go:42-76,308-345`
- Create: `internal/inputs/rules.go`, `internal/inputs/rules_test.go`

**Interfaces:**
- Consumes: `rule.Load`, `rule.EncodeList`, `rule.Hash`, `rule.RepoFile`.
- Produces:
  - `func SkillRules(content []byte) (bool, error)`: false only for `rules: false`
  - `func LoadRules(ctx context.Context, src Source, projectID int64, ref string) ([]rule.Entry, error)`: no file → `nil, nil`; malformed → `*ProblemError{Name: "rules", Path: ".pergent/rules.yaml", Problem: <loader text without the path prefix>}`; a source error passes through
  - `type NamedRuleSet struct{ Name, Content string }`; `func RuleList(repo []rule.Entry, sets []NamedRuleSet) ([]rule.Entry, error)`: appends each set's `rule.Load(content, rule.DashboardSource(name))`; a set that no longer loads → `*ProblemError{Name: name, Path: "rule set " + name, ...}`
  - `func RulesHash(list []rule.Entry, builtIn bool) string`: hex SHA-256 over `rule.Hash(list)` and the switch
  - `Inputs.Rules []byte` (the `rule.EncodeList` JSON; nil when the list is empty), counted in `Size`, never dropped by `Fit`

- [ ] **Step 1: Write the failing tests:**
  - `TestCheckSkillRulesField` (`rules: true`/`false` pass; `rules: yes`, `rules: "false"`, `rules: 1` → `rules must be true or false`); `TestSkillRules`.
  - `TestLoadRulesMissing`, `TestLoadRulesMalformed` (error text `.pergent/rules.yaml: entry 2: ...`), `TestLoadRulesReadsRef` (the fake source records the ref it was asked for).
  - `TestRuleListOrder` (repo entries, then set A, then set B, each in its own order); `TestRuleListBadStoredSet`.
  - `TestRulesHash` (stable; changes with an entry, the order and the switch).
  - `TestFitNeverDropsRules` (inputs over the limit with rules: CI results, dismissals, description and previous drop first; still over → `ErrTooLarge`, and `Rules` is never nil before that); `TestSizeCountsRules`.
- [ ] **Step 2: Run** — `go test ./internal/inputs/ -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.** In `decodeFrontmatter`, check `rules` like `cross_batch`; add `Rules *bool` to `frontmatter`; add "`rules` a boolean" to the `CheckSkill` doc. `Fit`'s error text becomes `skills, instructions and rules are %d bytes, the limit is %d`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(inputs): read path rules and hash the rule list"`

### Task 6: The Job gets the rules

**Files:**
- Modify: `internal/kube/job.go:84-130` (`JobParams`), `:270-278`, `:305-340`, `:396-421`, `:488-512`
- Test: `internal/kube/rules_test.go`

**Interfaces:**
- Consumes: `Inputs.Rules`.
- Produces: `JobParams.NoBuiltInRules bool`; ConfigMap key and file `rules.json`; env `PERGENT_RULES=/in/rules.json` when `len(Inputs.Rules) > 0`; env `PERGENT_NO_BUILT_IN_RULES=true` when `NoBuiltInRules`.

- [ ] **Step 1: Write the failing tests:** `TestJobRulesEnv` (inputs with rules → the `pergent` container has `PERGENT_RULES=/in/rules.json`, the ConfigMap item `rules.json` holds the JSON, and no argument starts with `--rules`); `TestJobNoRulesUnchanged` (no rules and built-ins on → container env, args and ConfigMap equal a Job built before the change); `TestJobNoBuiltInRules` (`PERGENT_NO_BUILT_IN_RULES=true`, no `--no-built-in-rules` argument).
- [ ] **Step 2: Run** — `go test ./internal/kube/ -run Rules -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement** next to `hasCI` and `CheckDeleted`, with the same "an older image ignores a variable" comment.
- [ ] **Step 4: Run** — `go test ./internal/kube/ -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(kube): pass the rule list to the review Job"`

### Task 7: Dispatch builds the list

**Files:**
- Modify: `internal/engine/dispatch.go:404-436,583-590,624-650,676-680,1149-1152`, `internal/engine/attention.go:20-36`
- Test: `internal/engine/rules_test.go`

**Interfaces:**
- Consumes: `Store.ResolveRuleSets`, config sets, `inputs.LoadRules`, `inputs.RuleList`, `inputs.RulesHash`, `Store.SetRunRulesHash`, `JobParams.NoBuiltInRules`.
- Produces: `attRulesInvalid = "rules_invalid"`; `func (d *dispatcher) resolveRules(ctx context.Context, plat Platform, repo settings.Effective, ref string) ([]rule.Entry, error)`; `func (d *dispatcher) failRules(ctx context.Context, repo settings.Effective, reason string) error` (attention then fail); `dispatcher.rulesHash string`.

- [ ] **Step 1: Write the failing tests** (engine e2e helpers, fake forge, fake Job):
  - `TestDispatchRulesOrder`: repository file + sets `b`, `a` (in that order) → the Job's `rules.json` lists repo entries, then `b`, then `a`.
  - `TestDispatchRulesFromTargetRef`: the forge was asked for `.pergent/rules.yaml` at `mr.StartSHA`, never at the head.
  - `TestDispatchBrokenRulesFileFails`: the run fails with `.pergent/rules.yaml: entry 1: ...`, attention `rules_invalid` with that text, no Job, nothing posted as success.
  - `TestDispatchMissingRuleSetFails`: message `rule set "gone" is not in the rule library`; attention set.
  - `TestDispatchStoredSetNoLongerLoads`: a row edited by SQL to bad content fails the run naming the set.
  - `TestDispatchConfigSetWins`: a config set and a stored set with the same name → the config set's entries.
  - `TestDispatchBuiltInSwitch`: setting off → `PERGENT_NO_BUILT_IN_RULES=true`.
  - `TestDispatchRulesHashStored`: `runs.rules_hash` equals `inputs.RulesHash(list, builtIn)` before the Job starts; an empty list still stores a hash; a change of `jobs.image` alone does not change it.
  - `TestDispatchRulesAttentionCleared`: a later run that starts a Job clears `rules_invalid`.
- [ ] **Step 2: Run** — `PERGENT_SH_TEST_DATABASE_URL=... go test ./internal/engine/ -run 'DispatchRule|DispatchBroken|DispatchMissingRule|DispatchStoredSet|DispatchConfigSet|DispatchBuiltIn' -count=1 -timeout 30m`. Expected: FAIL.
- [ ] **Step 3: Implement.** After `subset` (`dispatch.go:432`), resolve sets (config first, then `ResolveRuleSets`), read the file at `mr.StartSHA`, build the list, set `in.Rules` and `d.rulesHash`, all before `planCarried`. A source error retries, as `ResolveSkills` does. Store the hash next to `SetRunModel`; clear `attRulesInvalid` at 678; pass `NoBuiltInRules: !repo.BuiltInRules`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(engine): give each review its rule list"`

### Task 8: A rules change forces a full review for skills with rules

**Files:**
- Modify: `internal/engine/incremental.go:24-45,52-98,182-262`
- Test: `internal/engine/incremental_test.go`

**Interfaces:**
- Consumes: `store.Run.RulesHash`, `dispatcher.rulesHash`, `inputs.SkillRules`.
- Produces: `fullRules = "rules changed"`, `carryRules = "rules changed"`; `carriedFiles(last, lastHashes, hashes, changed, ds, rulesChanged bool, rulesOn map[string]bool, log)`.

- [ ] **Step 1: Write the failing tests:**
  - `TestCarryRulesChangedFullForRulesSkills`: hash differs, both skills have rules on → `planCarried` returns no files and reason `rules changed`; the log has `full review` with `reason=rules changed`, before any `ChangedSince` call.
  - `TestCarryRulesOffSkillCarriesOverRulesChange`: hash differs, skill A has rules on, B `rules: false` → only B carries; A is logged `nothing carried for a skill` with `reason=rules changed`.
  - `TestCarrySameRulesHashCarries`: the same hash carries as today.
  - `TestCarryNoRecordedHash`: a last run with NULL `rules_hash` counts as changed.
  - `TestCarrySkillRulesUnreadable`: a skill whose `rules` field can't be read counts as rules on.
- [ ] **Step 2: Run** — `go test ./internal/engine/ -run TestCarry -count=1` (pure; no database). Expected: FAIL.
- [ ] **Step 3: Implement** the cheap check in `planCarried` (every skill has rules on and the hash differs → `fullRules`), else pass the flags to `carriedFiles`, which drops a rules-on skill with `carryRules`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(engine): review skills with rules in full when the rules change"`

### Task 9: Result: read the rule per file

**Files:**
- Modify: `internal/result/result.go:30-60,155-176,248-300,613-640,810-830`
- Test: `internal/result/rules_test.go`

**Interfaces:**
- Produces: `type FileRule struct{ Path string \`json:"path"\`; Rule string \`json:"rule"\` }`; `Files.Rules []FileRule \`json:"rules,omitempty"\``; `type RulesInfo struct{ BuiltIn bool \`json:"built_in"\`; Entries int \`json:"entries"\` }`; `Result.Rules *RulesInfo \`json:"rules,omitempty"\``; `ToRender` fills `render.Skill.FileRules`.

- [ ] **Step 1: Write the failing tests:** `TestParseFileRules` (a pergent result with `files.rules` and `rules` decodes; an older one has neither); `TestToRenderFileRules`; `TestMergeIncrementalKeepsCarriedRules` (a carried file keeps the last review's rule; a reviewed file takes the new one), next to `carriedMarks`; `TestCommentRulesLine` (`policy` renders the Rules line for a result with rules, and today's comment for one without).
- [ ] **Step 2: Run** — `go test ./internal/result/ ./internal/policy/ -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS, golden files unchanged.
- [ ] **Step 5: Commit** — `git commit -m "feat(result): read the rule of each reviewed file"`

### Task 10: Rule-set API

**Files:**
- Create: `internal/dashboard/api_rulesets.go`, `internal/dashboard/api_rulesets_test.go`
- Modify: `internal/dashboard/dashboard.go:308` (register `ruleSetWriteRoutes` next to `skillWriteRoutes`, and the read routes), `internal/dashboard/view.go`, `openapi.yaml`, `internal/dashboard/openapi_test.go` (`openAPISchemas`), `internal/dashboard/api_read.go:1151-1180` (cache invalidation)

**Interfaces:**
- Routes: `GET /api/rule-sets` (session), `GET /api/rule-sets/{name}` (session), `GET /api/built-in-rules` (session), `POST /api/admin/rule-sets`, `PUT /api/admin/rule-sets/{name}`, `DELETE /api/admin/rule-sets/{name}` (admin, via `adminRoute`).
- Views: `RuleSetView{Name, Version, UpdatedAt, Entries int, Source "dashboard"|"config", InDefaults bool}`; `RuleSetDetail{RuleSetView, Content string, Notes []string}`; `RuleSetInUseView` (as `SkillInUseView`); `BuiltInRuleView{Name, Path, Rule string}`.
- Bodies: create `{name, content}`; update `{content, version}`.

- [ ] **Step 1: Write the failing tests:**
  - `TestRuleSetCreateChecks`: bad YAML, an unknown field, `api/`, a 16,001-character rule → 400 with the loader's text; a bad name → 400; an empty content → 400.
  - `TestRuleSetSecretRefused`: content with a held secret → 400 `the rule set holds a secret pergent.sh keeps; remove it`, on create and update, the answer naming no secret; held secrets unreadable → 500, nothing saved.
  - `TestRuleSetUpdateStale`: stale `version` → 409.
  - `TestRuleSetDeleteInUse409`: named by a repository, the defaults or a config repository → 409 with each listed; free → 204.
  - `TestRuleSetConfigReadOnly`: a config set is listed with `source: config`; create, update or delete of its name → 409.
  - `TestRuleSetLimit400`: the 201st → 400 `the rule library is full (200 sets)`.
  - `TestRuleSetNotes`: with a test built-in list injected (`Dashboard.builtInRules`, default `rule.Builtin()`), an entry `**/*.go` gives the note `This entry hides the built-in rule go for matching files.`; PR 1's real list gives none.
  - `TestRuleSetAudit`: `rule_set_created` (`name: v1`), `rule_set_updated` (`name: v1 -> v2`), `rule_set_deleted`; no entry holds the content.
  - `TestRuleSetReadAccess`: a team user reads; a user with no team gets what `GET /api/skills` gives them; a non-admin write is 404.
  - `TestRuleSetWriteDropsKeptInputsView`: a write invalidates the inputs cache.
  - `openapi_test.go` passes with the new routes, schemas and access tags (`routes_test.go` too).
- [ ] **Step 2: Run** — `PERGENT_SH_TEST_DATABASE_URL=... go test ./internal/dashboard/ -run 'RuleSet|OpenAPI|Routes' -count=1 -timeout 30m`. Expected: FAIL.
- [ ] **Step 3: Implement**, copying `api_skills.go`'s handlers, `refuseSkill` order (format check, then secrets) and `configReposNaming`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(dashboard): add the rule-set library API"`

### Task 11: Settings and read views

**Files:**
- Modify (the setting-touch list from memory): `internal/dashboard/view.go` (`DefaultsView`, `DefaultValues`, `InputsView:512`, `FilesView:285`), `api_read.go` (`settingViews`, `resolveInputs:1262`, the run view at 776), `api_write.go` (`msgBadSettings`, reset, `parseSettings`, a `ruleSetsSetting` like `skillsSetting:1044`), `api_operator.go` (`msgBadDefaults`, `defaultsView`, `defaultValues`, `pinnedDefaults`, `parseDefaults` incl. the `RepoDefaults` copy, `changedDefaults`), `openapi.yaml` (both request bodies, `DefaultValues`/`DefaultsView` and `required`)
- Test: `internal/dashboard/api_rulesettings_test.go`, and update the exact-body tests (`TestRepositoryDetail`, `TestAdminSettings`)

**Interfaces:**
- Settings keys `ruleSets` (`string[] | null`) and `builtInRules` (`bool`), in both the repository and the admin defaults.
- `InputsView.Rules RulesView{BuiltIn bool, Entries []RuleEntryView{Source, Path string; Off bool; Rule string}, Problem string}`: what a review of the default branch would get (repository file at the default branch, then sets; `Problem` holds the failure text instead of an error answer).
- `FilesView.Rules []FileRuleView{Path, Rule}` and `RunDetailView.RulesUsed bool` (the result has a `rules` object).

- [ ] **Step 1: Write the failing tests:** `TestRepoRuleSetsSetting` (save an ordered list, `null` resets to the default, a missing name → 400 `rule set "x" is not in the rule library`, `[]` → 400); `TestRepoBuiltInRulesSetting`; `TestDefaultsRuleSets`; `TestInputsViewRules` (order and sources; a broken repository file shows `Problem`, the skills still show); `TestRunViewFileRules` (rules merged across skills, once per path; `rulesUsed` false for an older result).
- [ ] **Step 2: Run** — `PERGENT_SH_TEST_DATABASE_URL=... go test ./internal/dashboard/ -count=1 -timeout 30m`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS, including `openapi_test.go`.
- [ ] **Step 5: Commit** — `git commit -m "feat(dashboard): rule-set settings and the rule per file"`

### Task 12: API reviews use the admin defaults

**Files:** Modify `internal/engine/apireview.go:124-180`. Test: `internal/engine/apireview_rules_test.go`.

- [ ] **Step 1:** `TestAPIReviewRules`: the Job gets the default sets' list and the default built-in switch, no repository file is read (the fake forge gets no call), and `rules_hash` is stored. `TestAPIReviewMissingDefaultSet`: the review fails with `rule set "x" is not in the rule library` and sets no attention item (API reviews set none).
- [ ] **Step 2-5:** run, implement after `BrowserDefaults`, run, commit `feat(engine): give API reviews the default rule list`.

### Task 13: Web

**Files:**
- Create: `web/src/pages/Rules.tsx`, `web/src/pages/RuleSetEditor.tsx`, their `.test.tsx`
- Modify: `web/src/App.tsx` (routes `rules`, `rules/new`, `rules/view/:name`), `web/src/layout/Layout.tsx` (a **Rules** link next to **Skills**), `web/src/api.ts`, `web/src/test/fixtures.ts`, `web/src/pages/AdminSettings.tsx`, `RepositorySettings.tsx`, `RepositoryDetail.tsx` (Skills & instructions), `ReviewDetail.tsx` (`Files`, 612-640)

- [ ] **Step 1: Write the failing tests:** `Rules.test.tsx` (lists sets with entry counts and a `config` badge; a **Built-in** tab lists each built-in glob and text, and says `No built-in rules yet.` when empty; create and delete buttons only for admins; a 409 delete shows the repositories); `RuleSetEditor.test.tsx` (save sends `{content, version}`; a 400 shows the loader's message; returned notes show under the editor); `AdminSettings.test.tsx` and `RepositorySettings.test.tsx` (choose and order sets, **Built-in rules** switch, exact PUT bodies); `RepositoryDetail.test.tsx` (the final rule list with each entry's source, and a `Problem`); `ReviewDetail.test.tsx` (a **Rule** column; `rulesUsed: false` shows `This image does not use rules`).
- [ ] **Step 2: Run** — `cd web && npx vitest run src/pages/Rules.test.tsx src/pages/RuleSetEditor.test.tsx` (never while `go test` runs). Expected: FAIL.
- [ ] **Step 3: Implement**, reusing `ui.tsx` pieces and the Skills page's layout.
- [ ] **Step 4: Run** — `npm run typecheck && npm run lint && npx vitest run && npm run build`. Expected: all pass. In a worktree with no `web/node_modules`, link another checkout's and remove the link, `tsconfig.tsbuildinfo` and `internal/web/dist/{assets,index.html}` afterwards.
- [ ] **Step 5: Commit** — `git commit -m "feat(web): rule sets, rule settings and the rule per file"`

### Task 14: Docs

**Files:**
- Create: `docs/concepts/rules.md`
- Modify: `docs/README.md` (index line), `docs/concepts/skills.md` (the `rules` field), `docs/concepts/dashboard.md` (Rules page, settings, the audit actions `rule_set_created`, `rule_set_updated`, `rule_set_deleted`), `docs/concepts/incremental-reviews.md` (the full-review reason `rules changed`), `docs/reference/configuration.md` (`rule_sets`, `repositories[].rule_sets`, `repositories[].built_in_rules`), `docs/upgrading.md` under `## Unreleased` (migration `0047` runs at startup; the 3 new config keys break rollback, since an older release refuses unknown keys, per CLAUDE.md), README only where it points at docs

- [ ] **Step 1:** `rules.md`: what a rule is (extra checks, never blocking), the file format and limits, the order, `off`, rule sets and their settings, config sets, the built-in switch and `rules: false`, that the built-in list is empty for now, that an older pergent image ignores rules and the review page says so (no version number: the release names it), the `rules_invalid` attention item, that API reviews use the admin defaults, and the full review after a rules change.
- [ ] **Step 2:** Read each changed page once for easy English and for any version number (there must be none).
- [ ] **Step 3: Commit** — `git commit -m "docs: describe path rules"`

### Task 15: Amend the spec

**Files:** Modify `docs/superpowers/specs/2026-11-20-path-rules-design.md`.

- [ ] **Step 1:** Change the spec so it matches PR 1, and add one line under its Status that says it was amended on the date of the change, after the plans:
  - `rule/` (public, `github.com/gopaytech/pergent/rule`) and `rule/builtin/` replace `internal/rule` and `internal/rule/builtin/`, because pergent.sh imports the loader.
  - The migration is `0047_rule_sets.sql`; "Checked on 2026-11-20" keeps its old line and gains `0046` as taken.
  - "Review API runs ..." becomes: API reviews read no repository file; they get the admin default rule-set list and the default built-in switch.
  - "Result": each skill's `files.rules: [{path, rule}]`, files with a rule only, `schema_version` 1.
  - "Dispatch" step 4: the hash covers the list and the built-in switch; `jobs.image` joins it in PR 2 with the built-in texts.
  - "Verification run": if `file-rules.md` cannot be written, verification fails with a note and keeps every blocking finding.
- [ ] **Step 2:** Read the Decisions table again: no row may contradict the amended text.
- [ ] **Step 3: Commit** — `git commit -m "docs(spec): amend path rules to what PR 1 builds"`

### Task 16: Verify gate and private-name scan

- [ ] **Step 1:** `nc -z localhost 55432` — if it fails, Postgres is down: say so, and do not count any store, engine or dashboard test as run.
- [ ] **Step 2:** `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./...` — every package `ok`; read the `internal/store`, `internal/engine` and `internal/dashboard` timings (minutes, not a fast fake `ok`).
- [ ] **Step 3:** `go vet ./...` — no output. `rm -f pergent-sh` if a build left a binary.
- [ ] **Step 4:** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build` — all pass (after the Go suite, never during it).
- [ ] **Step 5:** `GITHUB_ACTIONS=true GITHUB_EVENT_PATH=<file> TMPDIR=/tmp go test ./internal/config/... -count=1` — PASS.
- [ ] **Step 6:** Private-name scan, must print nothing: `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`, then `git diff --name-only origin/main | xargs grep -nE '@gmail\.com|/Users/|/private/tmp/'`, and the same `grep` over untracked files before `git add`.
- [ ] **Step 7:** Final review (`superpowers:requesting-code-review`) against `origin/main`. Before a PR, `git rev-list --count HEAD..origin/main`; if `main` moved, merge it and run the gate again. Push and the PR only when the owner asks.

## PR 2 follow-ups

- `inputs.RulesHash` gains `jobs.image`, because the built-in texts then ship inside the image and a new image can change them. Every image change then makes the next push a full review for skills with rules on; the docs say so in that PR.
- The Built-in tab shows the real texts; the built-ins upgrade note goes under `## Unreleased`.

## Review panel

The review panel (`2026-11-20-review-panel-design.md`) is agreed but not built. When it is, Job P gets the same `PERGENT_RULES` file and `PERGENT_NO_BUILT_IN_RULES` as Jobs A and B, from the one list Task 7 builds, and the run keeps one `rules_hash`. Nothing in this plan blocks that.
