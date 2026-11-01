# Skills Library (4a) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Admins write, install from git, update and delete library skills in the dashboard, set a default skill list, and turn on detected repository skills; reviews read all of it from Postgres, record each library skill's version, and a repository with no skills gets no review.

**Architecture:** Migration `0014` adds the library, its versions, skill fetches, the default list, `repo_skills`, per-run versions and a `skipped` run state. The engine resolves a review's skills from the store in one read transaction and from four repository folders in D23 order; `skills_dir` and the chart's `skills:` are deleted. Installing reads a connected instance's repository in the pod through new path-based forge reads, or a public `https` URL in a short Kubernetes Job (a git init container plus a `pergent-sh fetch-skills` container) that posts the files to a one-time-token callback.

**Tech Stack:** Go 1.24.8, pgx v5, client-go, React 18 + TS + TanStack Query 5 + Vitest, Helm.

**Spec:** `docs/superpowers/specs/2026-11-01-skills-library-design.md` (D1 to D25, the edge-case table, the API and page tables). Read it with the dashboard, organization and commands designs, whose constraints still hold.

## Global Constraints

- Branch `feat/skills-library` off `main` in `/Users/zufar.dhiyaullhaq/Documents/personal/github/pergent.sh`. The pergent CLI is not changed. Ships as app and chart **0.3.0**; the release itself waits for the owner.
- Skill name: `^[A-Za-z0-9][A-Za-z0-9._-]*$`, fixed after create or install; a frontmatter `name`, when present, must equal it (D4).
- Limits: **128 KiB** per skill, **200** library skills; frontmatter must parse; `paths` and `exclude` are string lists, `cross_batch` is a boolean, other fields are ignored (D6).
- Budget: saving the default list or a repository's `skills` is refused with 400 naming the total when the library text alone is over **900 KiB** (`inputs.ConfigMapLimit`) (D6).
- Repository folders in precedence order: `.pergent/skills`, `.agents/skills`, `.claude/skills`, `.github/skills`, then the library (D23). `.pergent/skills` always runs (D21); the other three run only when named in `repoSkills` / `repo_skills` (D22, D25).
- A repository's `skills` is null (the default list) or a non-empty list of library names (D20).
- Exact messages:
  - `skill "x" is not in the skill library`
  - `skill "x" is turned on but not found in the repository`
  - `this repository has no skills`
  - `name already in the library`
  - `removed at the source`
  - `renamed at the source`
  - `a private repository on <instance>`
  - `the skill contains a secret pergent.sh holds`
  - `skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`
- No skills at all: no Job, no comment, no status; run state `skipped`; "Needs attention" `this repository has no skills`.
- Who: any admin (`requireAdmin`) writes the library and the default list (D3). Maintainers and admins of a repository change its `skills` and `repoSkills`. Viewers only read.
- Audit actions: `skill_created`, `skill_updated`, `skill_installed`, `skill_source_updated`, `skill_copied`, `skill_deleted`; `defaults_updated` names `skills`. Never content (D10).
- Skill text is shown in `<pre>` or `<textarea>` only, never as HTML or markdown.
- Public `https` URLs only, fetched in a Job with no token under the review-Job NetworkPolicy (label `app.kubernetes.io/managed-by: pergent-sh`). The pod never fetches an admin-typed URL (D18).
- One replica; the engine loop never waits on a handler; no second way to run or publish a review.
- **Go suite:** `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:pergent@localhost:55432/pergent_test?sslmode=disable' go test -count=1 ./...` (container `pergent-sh-pg` up; store and e2e tests must not skip), then `go vet ./...`. "With the DB env" below means that variable set.
- **Web checks:** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`.
- **Chart checks:** `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml` and `helm lint charts/pergent-sh`.
- Private-name scan prints nothing: `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`.
- Commits: `git -c user.email=zufardhiyaulhaq@gmail.com commit`, conventional subjects, message ends `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never push, tag or release. Never run `go clean`, `docker builder prune` or any cache or disk cleanup.

## Review Focus

1. **A skipped review leaving an old approval.** A push, or a re-run of the same head, skipped for having no skills on an MR whose earlier or same commit the bot approved: the approval must be withdrawn, never left standing. Pinned in Task 3 `TestSkippedRunWithdrawsApprovalOfSameHead` and `TestSkippedRunOnNewHeadIsStaleApproval`.
2. **A cloned source escaping its checkout.** A public source repo whose skill file or `<name>/SKILL.md` is a symlink, or a `..` path, pointing outside the clone: the fetch Job reports a problem for it and never reads outside `/work/repo`. Pinned in Task 11 `TestFetchSkillsRefusesSymlinkOutside`.
3. **A URL carrying credentials or not https.** `https://user:token@host/x.git`, `http://`, `ssh://`, `git@host:x`, or a URL with a query or fragment: refused at start with 400, never stored, logged or shown. Pinned in Task 10 `TestValidateURL`.
4. **Two installs racing for one name.** The same name installed from two fetches, or written in the editor while a fetch is open: the second gets `name already in the library` and never overwrites. Pinned in Task 10 `TestInstallNameTakenSinceFetch`.
5. **A replayed or late fetch callback.** A second post with the same token, a post after the sweep failed the fetch, or a post over 4 MiB: 409, 409 and 413, and the stored result is unchanged. Pinned in Task 11 `TestFetchCallbackOneTime`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/store/migrations/0014_skills_library.sql` (new) | Every schema change of this piece, in one migration. |
| `internal/store/library.go` (new) | Library skills, versions, delete-in-use, resolve, budget size. |
| `internal/store/skillfetch.go` (new) | Skill fetches: create, complete once, sweep, installed-from lookup. |
| `internal/store/runs.go`, `dashboard.go`, `instances.go` | `skipped` state, `skill_versions`, `repo_skills`, default `skills`. |
| `internal/inputs/check.go` (new) | `CheckSkill`, limits, `ContainsSecret`. |
| `internal/inputs/scan.go` (new) | `RepoFolders`, `ScanFolder`, `Detect`, `DirSource`. |
| `internal/inputs/inputs.go` | `ResolveSkills` (D23); old `Resolve`, `Library`, `LoadLibrary` deleted in Task 6. |
| `internal/config/config.go`, `internal/settings/settings.go` | `repo_skills`; `skills_dir` refused; null `skills` means the defaults. |
| `internal/engine/dispatch.go` | Store-backed resolve, missing fails, zero skips, versions, the D8 re-run rule. |
| `internal/forge/forge.go`, `forge/gitlabforge/`, `forge/githubforge/`, `internal/gitlab/client.go`, `internal/github/client.go` | `forge.SourceReader`: read any repository by path at a commit. |
| `internal/skillfetch/` (new) | `Fetcher`: URL check, API path, Job path, sweep. |
| `internal/kube/fetch.go`, `internal/kube/fetch.sh` (new) | The fetch Job, its Secret, `LaunchFetch`. |
| `cmd/pergent-sh/fetch.go` (new) | `pergent-sh fetch-skills`: read a clone, post the files. |
| `internal/callback/fetch.go` (new) | `POST /skill-fetches/{id}/result`. |
| `internal/instances/instances.go` | `HeldSecrets`. |
| `internal/dashboard/api_skills.go`, `api_skill_fetch.go` (new) | Library read and write API; fetch, install and update API; D11 and D19. |
| `internal/dashboard/api_read.go`, `api_write.go`, `api_operator.go`, `view.go` | Settings, defaults, inputs view, setup checklist, run page. |
| `web/src/pages/Skills.tsx`, `SkillEditor.tsx`, `SkillInstall.tsx`, `SkillUpdates.tsx`, `web/src/diff.ts` | Skill pages and a line diff. |
| `web/src/pages/Overview.tsx`, `AdminSettings.tsx`, `RepositorySettings.tsx`, `Repositories.tsx`, `RepositoryDetail.tsx`, `ReviewDetail.tsx`, `web/src/api.ts`, `web/src/App.tsx` | Checklist, pickers, switches, warning, versions. |
| `charts/pergent-sh/` | `skills:` and its ConfigMap deleted; `PERGENT_SH_IMAGE` env. |
| `README.md` | The behaviour contract. |

---

### Task 1: Store: migration `0014`, the library and run columns

**Files:**
- Create: `internal/store/migrations/0014_skills_library.sql`, `internal/store/library.go`, `internal/store/library_test.go`
- Modify: `internal/store/instances.go` (`RepoDefaults`, `RepoDefaults()` :256, `PutRepoDefaults` :271), `internal/store/dashboard.go` (`DashboardRepo` :16-31, `dashboardRepoColumns` :33, `UpdateDashboardRepo` :126), `internal/store/runs.go` (`RunState` consts :28-33, `SetRunInputs` :614, `Run`, `StaleApprovals` :1138)

**Interfaces:**
- Produces:

```go
// 0014 creates:
//   library_skills(name PK + the name CHECK, version, content, updated_at, updated_by,
//     source_kind CHECK IN ('instance','url') NULL, source_instance, source_repo, source_url,
//     source_ref, source_path, source_commit)
//   library_skill_versions(name, version, content, sha256, source_commit, at, actor; PK (name, version); no FK)
//   skill_fetches(id uuid PK, kind, instance, repo, url, ref, from_ref, path, purpose CHECK IN ('install','update'),
//     requested_by, requested_by_id, status CHECK IN ('running','done','failed'), commit, error,
//     files jsonb, token_hash bytea, created_at, expires_at)
//   repo_defaults.skills text[]; dashboard_repos.repo_skills text[]; runs.skill_versions jsonb;
//   runs.skip_reason text; the runs state CHECK gains 'skipped'.
const Skipped RunState = "skipped"
const SourceKindInstance, SourceKindURL = "instance", "url"
type SkillSource struct{ Kind, Instance, Repo, URL, Ref, Path, Commit string }
type LibrarySkill struct {
	Name, Content, UpdatedBy string
	Version, Size            int
	UpdatedAt                time.Time
	Source                   *SkillSource // nil: written in the dashboard
}
type LibrarySkillVersion struct {
	Name, Content, Actor, Commit string
	Version                      int
	SHA256                       []byte
	At                           time.Time
}
type NewSkill struct {
	Name, Content, Actor string
	Source               *SkillSource
	At                   time.Time
}
type RepoRef struct{ Instance, Project string }
type SkillInUseError struct{ Repos []RepoRef; InDefaults bool }
var ErrSkillExists, ErrSkillNotFound, ErrStaleVersion, ErrSkillInstalled, ErrLibraryFull error
type ResolvedLibrary struct{ Skills map[string]LibrarySkill; Missing []string }

func (s *Store) LibrarySkills(ctx context.Context) ([]LibrarySkill, error) // by name; Content empty, Size set
func (s *Store) LibrarySkill(ctx context.Context, name string) (LibrarySkill, []LibrarySkillVersion, error) // versions newest first, Content empty
func (s *Store) LibrarySkillVersion(ctx context.Context, name string, version int) (LibrarySkillVersion, error)
func (s *Store) CreateLibrarySkill(ctx context.Context, n NewSkill) (int, error) // version = 1 + max(version) ever used for the name
func (s *Store) UpdateLibrarySkill(ctx context.Context, name, content, actor string, expect int, at time.Time) (int, error)
func (s *Store) UpdateInstalledSkill(ctx context.Context, name, content, actor string, src SkillSource, at time.Time) (int, error)
func (s *Store) DeleteLibrarySkill(ctx context.Context, name string, configRepos []RepoRef) error // row FOR UPDATE, then *SkillInUseError
func (s *Store) ResolveLibrary(ctx context.Context, names []string) (ResolvedLibrary, error) // REPEATABLE READ; nil names: repo_defaults.skills
func (s *Store) LibrarySize(ctx context.Context, names []string) (int, error)
func (s *Store) SkipRun(ctx context.Context, id, reason string, now time.Time) (bool, error) // queued or running -> skipped
// RepoDefaults gains Skills []string; DashboardRepo gains RepoSkills []string. PutRepoDefaults and
// UpdateDashboardRepo lock each named library skill FOR KEY SHARE and return ErrSkillNotFound for one missing.
// SetRunInputs(ctx, id, model string, sources map[string]string, versions map[string]int); Run gains SkillVersions map[string]int.
// StaleApprovals treats a skipped run like any other non-superseded run.
```

- [ ] **Step 1: Write the failing tests** in `internal/store/library_test.go`:

```go
func TestLibrarySkillVersions(t *testing.T)        // create v1, update(expect 1) -> 2; versions [2,1]; SHA256 == sha256(content)
func TestVersionsContinueAfterDelete(t *testing.T) // create, update, delete, create again -> 3
func TestUpdateStaleVersion(t *testing.T)          // update(expect 1) after v2 -> ErrStaleVersion
func TestUpdateInstalledRefused(t *testing.T)      // UpdateLibrarySkill on a skill with Source -> ErrSkillInstalled
func TestCreateFull(t *testing.T)                  // 200 skills, the 201st -> ErrLibraryFull
func TestDeleteInUse(t *testing.T)                 // dashboard repo, config repo, defaults -> *SkillInUseError with Repos / InDefaults
func TestDeleteRacesRepoSave(t *testing.T)         // delete || UpdateDashboardRepo naming it: exactly one nil error
func TestDeleteRacesDefaultsSave(t *testing.T)     // delete || PutRepoDefaults naming it: exactly one nil error
func TestResolveLibrary(t *testing.T)              // nil -> the defaults; ["a","gone"] -> Skills{a}, Missing ["gone"]
func TestLibrarySize(t *testing.T)                 // sum of the named skills' bytes
func TestRunSkillVersionsRoundTrip(t *testing.T)   // SetRunInputs(..., {"a":3}) -> Run.SkillVersions == {"a":3}
func TestSkipRun(t *testing.T)                     // queued -> skipped with reason; a skipped run on a new head makes StaleApprovals list the MR
```

- [ ] **Step 2: Run** `go test -count=1 -run 'TestLibrary|TestVersions|TestUpdate|TestCreateFull|TestDelete|TestResolveLibrary|TestRunSkillVersions|TestSkipRun' ./internal/store/` with the DB env. Expected: FAIL, `undefined: NewSkill`.
- [ ] **Step 3: Implement** the migration and the functions, following the store's conventions (pgx, explicit column lists, one transaction per write).
- [ ] **Step 4: Run** the Go suite. Expected: PASS, including the migration-count test (14 rows in `schema_migrations`).
- [ ] **Step 5: Commit** `feat(store): skill library, versions, fetches, default and repository skill lists`.

---

### Task 2: Inputs: checks, folder scan and `ResolveSkills`

**Files:**
- Create: `internal/inputs/check.go`, `internal/inputs/check_test.go`, `internal/inputs/scan.go`, `internal/inputs/scan_test.go`
- Modify: `internal/inputs/inputs.go` (add `ResolveSkills` and `Inputs.Versions`; `Resolve` stays until Task 6), `internal/inputs/frontmatter.go` (frontmatter struct gains `Paths, Exclude []string`, `CrossBatch bool`), `internal/inputs/inputs_test.go`

**Interfaces:**
- Produces:

```go
const MaxSkillBytes, MaxLibrarySkills = 128 << 10, 200
func CheckSkill(name string, content []byte) error
func ContainsSecret(content []byte, secrets []string) bool // empty secrets are skipped
var RepoFolders = []string{".pergent/skills", ".agents/skills", ".claude/skills", ".github/skills"}
type Found struct{ Name, Path, Problem string; Content []byte } // Name is the fallback name when Problem is set
func ScanFolder(ctx context.Context, src Source, projectID int64, dir, ref string) ([]Found, error) // *.md and <name>/SKILL.md; per-entry problems in Found; Source errors returned
type Detected struct{ Name, Folder, Path, Problem string }
func Detect(ctx context.Context, src Source, projectID int64, ref string) ([]Detected, error) // RepoFolders[1:]
type DirSource struct{ Root string } // a Source over a local tree; never follows a symlink or ".." out of Root
type LibraryEntry struct{ Content []byte; Version int }
type NotFoundError struct{ Name string } // Error(): skill "x" is turned on but not found in the repository
func ResolveSkills(ctx context.Context, src Source, lib map[string]LibraryEntry, repoSkills []string, projectID int64, ref string, previous []byte) (*Inputs, error)
// Inputs gains Versions map[string]int (library skills only). Sources[name] is SourceLibrary or the folder (a RepoFolders entry).
// A problem in .pergent/skills is an error, as today. A problem in an entry of another folder fails only when its name is turned on.
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestCheckSkill(t *testing.T)                 // table: ok; bad name; 128 KiB + 1; bad YAML; paths: "x"; cross_batch: "yes"; name mismatch
func TestContainsSecret(t *testing.T)
func TestScanFolder(t *testing.T)                 // flat and folder skills; folder without SKILL.md -> Problem
func TestDirSourceStaysInRoot(t *testing.T)       // symlink to /etc/passwd and "../x" -> not found, nothing read
func TestPrecedenceAcrossFolders(t *testing.T)    // "lint" in all four folders and the library -> Sources["lint"] == ".pergent/skills"
func TestDetectedOnlyWhenOn(t *testing.T)         // .claude/skills/a off -> absent; on -> Sources["a"] == ".claude/skills"
func TestOffMalformedDetectedSkillIgnored(t *testing.T)
func TestOnDetectedSkillMissing(t *testing.T)     // errors.As(*NotFoundError); exact message
func TestPergentFolderProblemIsFatal(t *testing.T)
func TestOffDetectedReplacesNothing(t *testing.T) // off .agents/skills/x keeps library x
func TestVersionsOnlyLibrary(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/inputs/`. Expected: FAIL, `undefined: CheckSkill`.
- [ ] **Step 3: Implement** the functions above.
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(inputs): skill checks, repository skill folders and their precedence`.

---

### Task 3: Engine reads skills from the store; no skills means no review

**Files:**
- Modify: `internal/config/config.go` (`Repository`; `fileRepository` and `repository()` :191-215 gain `RepoSkills` / `repo_skills`), `internal/config/config_test.go`, `internal/settings/settings.go` (`Effective`, `fromConfig` :311, `fromDashboard` :348; delete `LibrarySkills` :75-100), `internal/settings/settings_test.go`, `internal/engine/engine.go` (delete `Library` :74), `internal/engine/dispatch.go` (resolve at :283; delete `skills` :635-642), `internal/engine/dispatch_test.go`, `internal/engine/fakes_test.go`, `cmd/pergent-sh/main.go` (:212 stops passing `Library`)

**Interfaces:**
- Consumes: Task 1 `ResolveLibrary`, `SkipRun`, `SetAttention`; Task 2 `ResolveSkills`, `LibraryEntry`, `NotFoundError`; `(*Engine).withdrawRedecided` (publish.go:228).
- Produces: `config.Repository.RepoSkills []string` (each matches the name rule, else a config error); `settings.Effective.RepoSkills []string`; `settings.KeyRepoSkills = "repo_skills"`; `Effective.Skills` nil means the default list for config and dashboard repositories. Attention kinds `skill_missing` and `no_skills` per repository, cleared when a run of that repository dispatches a Job.

- [ ] **Step 1: Write the failing tests:**

```go
func TestRepoSkillsConfig(t *testing.T)            // repo_skills: [a] parsed; a bad name -> config error
func TestNullSkillsUsesDefaults(t *testing.T)       // config and dashboard repo, defaults [a] -> the Job ConfigMap holds a
func TestNamedSkillsExactly(t *testing.T)
func TestMissingLibrarySkillFailsRun(t *testing.T) // reason `skill "gone" is not in the skill library`; attention skill_missing; never approved
func TestNoFallbackWhenAllNamedGone(t *testing.T)  // every named skill gone -> failed, not every library skill
func TestTurnedOnRepoSkillGoneFailsRun(t *testing.T)
func TestNoSkillsSkips(t *testing.T)               // state skipped; no Launch, SetStatus or CreateComment; attention "this repository has no skills"
func TestSkippedRunWithdrawsApprovalOfSameHead(t *testing.T) // approved H, re-run of H skipped -> withdraw_pending for H
func TestSkippedRunOnNewHeadIsStaleApproval(t *testing.T)    // approved H1, push H2 skipped -> StaleApprovals lists the MR
func TestEditBetweenEnqueueAndDispatch(t *testing.T)         // the Job gets the content as it is at dispatch
```

- [ ] **Step 2: Run** `go test -count=1 -run 'TestRepoSkillsConfig|TestNullSkills|TestNamedSkills|TestMissingLibrary|TestNoFallback|TestTurnedOn|TestNoSkills|TestSkippedRun|TestEditBetween' ./internal/...` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement.** Dispatch: `ResolveLibrary(ctx, repo.Skills)`; `Missing` fails the run; `ResolveSkills`; after `subset`, zero skills → `SkipRun`, attention, and `e.withdrawRedecided(ctx, plat, run, log)` (it acts only on an approval of this head); no Job, no status.
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(engine): reviews read skills from the database; no skills means no review`.

---

### Task 4: Record versions; `review failed` after an edit is a full re-run

**Files:**
- Modify: `internal/engine/dispatch.go` (`keptSources` :152-154, the record at :367-381, `keptRun` :449), `internal/engine/rerun_test.go`

**Interfaces:**
- Consumes: Task 1 `SetRunInputs(..., versions)`, `Run.SkillVersions`; Task 2 `Inputs.Versions`.
- Produces: `keptVersions map[string]int` beside `keptSources`. `keptRun` returns the reason `a skill changed since the earlier result` when a kept library skill's current version differs from the base run's, or when the base run's `SkillVersions` is nil.

- [ ] **Step 1: Write the failing tests:**

```go
func TestRunRecordsSkillVersions(t *testing.T)              // SkillVersions == {"a": 2}
func TestRerunFailedKeepsBaseVersions(t *testing.T)         // kept skill b keeps base v1 in the merged run's record
func TestRerunFailedAfterEditIsFull(t *testing.T)           // kept skill edited -> MakeFullRerun called
func TestRerunFailedBaseWithoutVersionsIsFull(t *testing.T)
func TestRerunFailedUnchangedStaysSubset(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 -run 'TestRunRecords|TestRerunFailed' ./internal/engine/` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement** in `dispatch.go`.
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(engine): record skill versions; a re-run after an edit reviews everything`.

---

### Task 5: Dashboard reads and settings use the store

**Files:**
- Create: `internal/dashboard/api_skills.go`, `internal/dashboard/api_skills_test.go`
- Modify: `internal/dashboard/dashboard.go` (delete `Options.Library` :58-60, `library` :128 and :179), `api_read.go` (`skills` :962 moves to `api_skills.go`; `resolveInputs` :919-958; `adminSettings` :1052; `inputsCache` :836), `api_write.go` (`skillsSetting` :685-705, new `repoSkillsSetting`, `putSettings` :543), `api_operator.go` (`putDefaults` :804), `view.go` (`InputsView` :327, `InputSkill` :334, `DefaultsView` :397), `api_read_test.go`, `api_write_test.go`, `cmd/pergent-sh/main.go` (:220)

**Interfaces:**
- Consumes: Task 1 `LibrarySkills`, `LibrarySkill`, `LibrarySkillVersion`, `ResolveLibrary`, `LibrarySize`, `ErrSkillNotFound`; Task 2 `ResolveSkills`, `Detect`.
- Produces:

```go
type SkillSourceView struct{ Kind, Instance, Repo, URL, Ref, Commit string; Hidden bool } // Task 5: Repo shown to operators only; Task 10 widens it (D11)
type LibrarySkillView struct {
	Name, UpdatedBy string // UpdatedBy only for admins
	Version, Size   int
	UpdatedAt       time.Time
	InDefaults      bool
	Source          *SkillSourceView
}
type SkillVersionView struct{ Version int; At time.Time; Actor, Commit string } // Actor only for admins
type SkillDetailView struct{ LibrarySkillView; Content string; Versions []SkillVersionView }
// GET /api/skills -> []LibrarySkillView; GET /api/skills/{name} -> SkillDetailView;
// GET /api/skills/{name}/versions/{v} -> {version, at, actor?, commit, content}
// PUT /api/repositories/...: "skills" (null | non-empty names) and "repoSkills" (names; [] allowed).
// PUT /api/admin/defaults: "skills" (null | names).
// Over budget: 400 "the library skills come to N KiB; a review takes at most 900 KiB".
// InputsView gains Detected []DetectedSkillView{Name, Folder, Problem string; On bool}; InputSkill gains Folder string, Version int.
// DefaultsView gains Skills []string; the repository settings view gains repoSkills.
// inputsCache gains invalidate(), called by every library and defaults write from here on.
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestSkillsListFromStore(t *testing.T)
func TestSkillDetailActorOnlyForAdmins(t *testing.T)
func TestRepoSettingsSkillsBudget400(t *testing.T)  // the message names the total
func TestRepoSettingsUnknownSkill400(t *testing.T)
func TestRepoSkillsSettingRoundTrip(t *testing.T)   // maintainer sets ["a"]; viewer 403
func TestDefaultsSkillsRoundTrip(t *testing.T)      // admin only; audit defaults_updated names "skills"
func TestInputsViewDetectedAndFinal(t *testing.T)   // detected with On; final set with Folder and Version; a missing name shown, not dropped
func TestInputsCacheClearedOnDefaultsChange(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/dashboard/` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(dashboard): read the library, default skills and repository skills from the database`.

---

### Task 6: Delete `skills_dir`, the static library and the chart's `skills:`

**Files:**
- Modify: `internal/config/config.go` (`Config.SkillsDir` :53 deleted; `fileConfig.SkillsDir` :182 becomes a `yaml.Node` kept only to refuse it, like `Admins` :70-72; the required check :397-398 deleted), `internal/config/config_test.go`, `internal/config/testdata/*.yaml`, `internal/inputs/inputs.go` (delete `Library`, `LoadLibrary`, `Resolve`), `internal/inputs/inputs_test.go`, `cmd/pergent-sh/main.go` (:137-142; `checkSkills` :422-434 becomes a startup pass that sets `skill_missing` attention and never stops startup), `cmd/pergent-sh/main_test.go`, `charts/pergent-sh/values.yaml` (:86 text, :89, :95-96), `charts/pergent-sh/ci-values.yaml` (:11, the repository's `skills` :23, :36 on), delete `charts/pergent-sh/templates/skills-configmap.yaml`, `templates/deployment.yaml` (:27, :99-100, :106-108), `templates/_helpers.tpl` (:53-55), `charts/pergent-sh/README.md.gotmpl`, `charts/pergent-sh/README.md` (`make readme`)

**Interfaces:**
- Produces: the config error `skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`.

- [ ] **Step 1: Write the failing tests and the chart check:**

```go
func TestSkillsDirRefused(t *testing.T)                    // a config with skills_dir -> that exact error
func TestUnknownConfigSkillDoesNotStopStartup(t *testing.T) // checkSkills sets attention skill_missing, returns nil
func TestLibraryWithoutDashboard(t *testing.T)              // config without dashboard.url: the engine still resolves from the store
```

Chart check: `helm template t charts/pergent-sh -f charts/pergent-sh/ci-values.yaml | grep -cE 'skills_dir|-skills|/etc/pergent-sh/skills'`.

- [ ] **Step 2: Run** `go test -count=1 -run 'TestSkillsDirRefused|TestUnknownConfigSkill|TestLibraryWithoutDashboard' ./internal/config/ ./cmd/pergent-sh/` and the chart check. Expected: FAIL, and a count above `0`.
- [ ] **Step 3: Implement** the deletions.
- [ ] **Step 4: Run** the Go suite and the chart checks. Expected: PASS; both lints pass (the second prints the expected INFO about `existingSecret`); the chart check prints `0`.
- [ ] **Step 5: Commit** `feat!: skills_dir and the chart's skills value are removed`.

---

### Task 7: Library writes: create, edit, copy, delete; secrets refused

**Files:**
- Modify: `internal/dashboard/api_skills.go`, `internal/dashboard/api_skills_test.go`, `internal/dashboard/dashboard.go` (routes, behind `requireAdmin`), `internal/instances/instances.go` (+ `instances_test.go`)

**Interfaces:**
- Consumes: Task 1 `CreateLibrarySkill`, `UpdateLibrarySkill`, `DeleteLibrarySkill`, `SkillInUseError`, the `Err*` values; Task 2 `CheckSkill`, `ContainsSecret`; `Registry.Secrets` (:302), `LLMKeys` (:452).
- Produces:

```go
func (r *Registry) HeldSecrets(ctx context.Context) []string // every set field of every instance's Secrets, plus LLMKeys()
// POST /api/admin/skills {name, content, addToDefaults} -> 201 {version}
//   409 "name already in the library"; 400 the CheckSkill message; 400 "the library holds 200 skills, the most it can";
//   400 "the skill contains a secret pergent.sh holds"
// PUT /api/admin/skills/{name} {content, version} -> 200 {version}
//   409 "this skill changed since you opened it"; 409 "an installed skill can't be edited; copy it"
// POST /api/admin/skills/{name}/copy {name} -> 201 {version}; the copy's frontmatter name is set to the new name
// DELETE /api/admin/skills/{name} -> 204 | 409 {error, repositories: [{instance, project}], hidden: N, inDefaults: bool}
// audit: skill_created "v1", skill_updated "v3 -> v4", skill_copied "from <old>", skill_deleted; never content
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestHeldSecretsCoversEveryField(t *testing.T)    // GitLab and GitHub instances, App and token approvers, the LLM key
func TestSkillWritesNeedAdmin(t *testing.T)            // viewer and maintainer: 403 on create, edit, copy, delete
func TestSkillWritesNeedCSRF(t *testing.T)
func TestCreateSkill(t *testing.T)                     // addToDefaults true -> in the defaults; false -> not
func TestCreateChecks(t *testing.T)                    // each CheckSkill failure -> 400 with its message
func TestSaveRefusesHeldSecret(t *testing.T)           // create and update: the response, log buffer and audit hold no secret
func TestUpdateStale409(t *testing.T)
func TestUpdateInstalled409(t *testing.T)
func TestCopySetsFrontmatterName(t *testing.T)
func TestDeleteInUse409HidesUnseenRepos(t *testing.T) // platform admin: visible ones listed, the rest in hidden
func TestSkillAuditHasNoContent(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 -run 'TestHeldSecrets|TestSkillWrites|TestCreate|TestSave|TestUpdateStale|TestUpdateInstalled|TestCopy|TestDeleteInUse|TestSkillAudit' ./internal/...` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement.** The 409 list uses `Session.CanSee`; config repositories come from `Config.Repositories`.
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(dashboard): admins write, edit, copy and delete library skills`.

---

### Task 8: Repository views, review page and setup checklist

**Files:**
- Modify: `internal/dashboard/api_read.go` (`overview`, `repositoryInputs` :858, `fillSkills` :609, `attention` :232), `internal/dashboard/view.go` (`OverviewView` :69, `SkillView` :222, `InputSkill`), `internal/dashboard/outcome.go` (a skipped run's outcome), `internal/dashboard/api_read_test.go`

**Interfaces:**
- Consumes: Task 5 views; `instances.Registry.LLMStatus`, `All`; Task 1 `Skipped`.
- Produces:

```go
type SetupView struct{ LLM, Instance, Skill, Repository bool } // OverviewView.Setup *SetupView: admins only; nil when all four are true
// SkillView gains Version int; Source is "library" or a RepoFolders entry ("repo" kept for older runs)
// InputSkill gains URL string:
//   GitLab <instance url>/<project>/-/blob/<branch>/<path>; GitHub https://github.com/<owner>/<repo>/blob/<branch>/<path>; segments escaped
// a skipped run shows as "not reviewed: this repository has no skills" and is not counted as failed
// attention: items with instance "" reach every admin
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestSetupChecklist(t *testing.T)          // fresh install: all false; viewer: nil; all done: nil
func TestRunDetailSkillVersionAndFolder(t *testing.T)
func TestSkippedRunOutcome(t *testing.T)
func TestRepoSkillURLGitLab(t *testing.T)
func TestRepoSkillURLGitHub(t *testing.T)
func TestRepoSkillURLEscapesPath(t *testing.T)
func TestGlobalAttentionForPlatformAdmin(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 -run 'TestSetup|TestRunDetailSkill|TestSkippedRunOutcome|TestRepoSkillURL|TestGlobalAttention' ./internal/dashboard/` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(dashboard): setup checklist, skill versions and repository skill links`.

---

### Task 9: Forge: read any repository by path at a commit

**Files:**
- Modify: `internal/forge/forge.go`, `internal/forge/gitlabforge/gitlabforge.go` (+test), `internal/forge/githubforge/githubforge.go` (+test), `internal/gitlab/client.go` (+test), `internal/github/client.go` (+test)

**Interfaces:**
- Produces:

```go
// package forge
type SourceReader interface {
	ResolveRef(ctx context.Context, project, ref string) (commit string, err error)
	ListTreeAt(ctx context.Context, project, path, commit string) ([]TreeEntry, error)
	GetFileAt(ctx context.Context, project, path, commit string) ([]byte, bool, error)
}
var ErrSourceNotFound = errors.New("the review bot or App can't read this repository or ref")
// gitlab: func (c *Client) CommitSHA(ctx context.Context, projectID int64, ref string) (string, error) // GET /projects/:id/repository/commits/:ref
// github: func (c *Client) CommitSHA(ctx context.Context, owner, repo, ref string) (string, error)     // GET /repos/{o}/{r}/commits/{ref}
// gitlabforge: GetProject(path) -> id, then CommitSHA, ListTree, GetFile with the review bot (Reporter is enough).
// githubforge: app.Installation(owner, name) -> that installation's client; no stored project needed; not installed -> ErrSourceNotFound.
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestGitLabCommitSHA(t *testing.T)               // httptest: the ref is path-escaped; sha returned; 404 -> ErrNotFound
func TestGitHubCommitSHA(t *testing.T)
func TestGitLabSourceReader(t *testing.T)            // a project not switched on, Reporter access -> files read at the commit
func TestGitHubSourceReader(t *testing.T)
func TestGitHubSourceReaderNotInstalled(t *testing.T) // -> ErrSourceNotFound
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/gitlab/ ./internal/github/ ./internal/forge/...`. Expected: FAIL, `undefined: forge.SourceReader`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(forge): read a repository by path at a commit`.

---

### Task 10: Fetch from a connected instance; install and update

**Files:**
- Create: `internal/store/skillfetch.go`, `internal/store/skillfetch_test.go`, `internal/skillfetch/skillfetch.go`, `internal/skillfetch/skillfetch_test.go`, `internal/dashboard/api_skill_fetch.go`, `internal/dashboard/api_skill_fetch_test.go`
- Modify: `internal/dashboard/dashboard.go` (`Options.Fetcher`, routes), `internal/dashboard/api_skills.go` (D11 source view), `cmd/pergent-sh/main.go` (build the `Fetcher`; run `Sweep` once a minute beside the engine loops)

**Interfaces:**
- Consumes: Task 1 `CreateLibrarySkill`, `UpdateInstalledSkill`, `SkillSource`; Task 2 `ScanFolder`, `CheckSkill`, `ContainsSecret`; Task 7 `HeldSecrets`; Task 9 `SourceReader`; `Auth.userToken` and `provider.canSee` (dashboard/oauth.go:36).
- Produces:

```go
// package store
type FetchedFile struct{ Name, Path, Content, Problem string }
type SkillFetch struct {
	ID, Kind, Instance, Repo, URL, Ref, FromRef, Path, Purpose string // Purpose "install" | "update"
	RequestedBy                                                string
	RequestedByID                                              int64
	Status, Commit, Error                                      string // Status "running" | "done" | "failed"
	Files                                                      []FetchedFile
	CreatedAt, ExpiresAt                                       time.Time
}
func (s *Store) CreateSkillFetch(ctx context.Context, f SkillFetch, tokenHash []byte) (string, error)
func (s *Store) SkillFetch(ctx context.Context, id string) (SkillFetch, error)
func (s *Store) CompleteSkillFetch(ctx context.Context, id string, tokenHash []byte, commit, errText string, files []FetchedFile, now time.Time) (bool, error) // only from running; tokenHash nil on the API path
func (s *Store) FailStaleSkillFetches(ctx context.Context, before time.Time) (int, error)
func (s *Store) PruneSkillFetches(ctx context.Context, now time.Time) (int, error)
func (s *Store) InstalledFrom(ctx context.Context, src SkillSource) ([]LibrarySkill, error) // same kind, instance+repo or url, and ref

// package skillfetch
const MaxFetchFiles, MaxFetchBytes, FetchTimeout, FetchKeep = 200, 4 << 20, 2 * time.Minute, time.Hour
type Request struct {
	Instance, Repo, URL, Ref, FromRef, Path, Purpose, Actor string
	ActorID                                                 int64
}
type Fetcher struct {
	Store        *store.Store
	Readers      func(instance string) (forge.SourceReader, bool)
	Secrets      func(ctx context.Context) []string // Registry.HeldSecrets
	CallbackBase string                             // config callback_url
	Now          func() time.Time
	Log          *slog.Logger
	// Task 11 adds the Job fields.
}
func ValidateURL(raw string) error // https only; no userinfo, query or fragment; a host is required
func (f *Fetcher) Start(ctx context.Context, r Request) (string, error) // instance: a goroutine, at most 2 at once, FetchTimeout each; URL: an error until Task 11
func (f *Fetcher) Sweep(ctx context.Context) error // running past FetchTimeout + 1 min -> failed; past ExpiresAt -> pruned

// API (requireAdmin)
// POST /api/admin/skill-fetches {instance, repo} | {url}, ref, path, purpose, fromRef -> 202 {id}
//   D19: an operator reads any instance; a platform admin only se.Instance and only when canSee(user token) is true,
//   else 403 "you can install only from repositories of your own instance that you can see"
// GET /api/admin/skill-fetches/{id} -> {status, commit, error, skills: [{name, path, size, problem, taken}],
//   updates: [{name, path, state: "changed" | "same" | "removed at the source" | "renamed at the source", current, fetched}]}
//   only the requester sees a fetch (404 for others)
// POST /api/admin/skill-fetches/{id}/install {names, addToDefaults} -> [{name, ok, error}]; audit skill_installed
// POST /api/admin/skill-fetches/{id}/update {names} -> [{name, ok, error}]; renamed -> "renamed at the source"; audit skill_source_updated
// D11: SkillSourceView.Repo is shown for a URL source, to operators, and to a same-instance user for whom canSee is true
//   (kept 5 min per session and repo); otherwise Hidden, shown as "a private repository on <instance>"
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestCompleteSkillFetchOnce(t *testing.T)
func TestSweepFailsStaleAndPrunes(t *testing.T)
func TestValidateURL(t *testing.T)                  // ok: https; refused: userinfo, http, ssh, git@host:x, query, fragment, no host
func TestFetchInstanceReadsRef(t *testing.T)        // GitLab and GitHub fakes: commit recorded, sizes, problems; a bad ref -> Error from ErrSourceNotFound
func TestFetchMarksSecretAndOversize(t *testing.T)  // Problem set; installing that name is refused
func TestPlatformAdminCeiling(t *testing.T)         // another instance -> 403; can't see -> 403; operator -> 202
func TestInstallNameTakenSinceFetch(t *testing.T)   // name created after the fetch -> "name already in the library"; content unchanged
func TestInstallRecordsSourceAndDefaults(t *testing.T)
func TestUpdateNewRefShowsDiff(t *testing.T)        // fromRef v1.0, ref v1.1 -> "changed"; Update records ref v1.1 and the commit
func TestUpdateRemovedAndRenamed(t *testing.T)
func TestPrivateSourceHiddenFromOthers(t *testing.T)
func TestFetchOnlyRequesterSees(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/store/ ./internal/skillfetch/ ./internal/dashboard/` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement.** The API path scans with `ScanFolder` over a `SourceReader` adapter that ignores `projectID`. A file over `MaxSkillBytes`, failing `CheckSkill`, or holding a held secret gets a `Problem`.
- [ ] **Step 4: Run** the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat: install and update library skills from a connected instance`.

---

### Task 11: Fetch a public URL in a Job

**Files:**
- Create: `internal/kube/fetch.go`, `internal/kube/fetch.sh`, `internal/kube/fetch_test.go`, `cmd/pergent-sh/fetch.go`, `cmd/pergent-sh/fetch_test.go`, `internal/callback/fetch.go`, `internal/callback/fetch_test.go`
- Modify: `internal/kube/launcher.go`, `internal/skillfetch/skillfetch.go` (+test), `cmd/pergent-sh/main.go` (subcommand `fetch-skills` beside `serve` :107; route `POST /skill-fetches/{id}/result`), `charts/pergent-sh/templates/deployment.yaml` (env `PERGENT_SH_IMAGE` set to the container's own image expression)

**Interfaces:**
- Consumes: Task 10 `CreateSkillFetch`, `CompleteSkillFetch`, `ValidateURL`, `MaxFetchBytes`, `Fetcher`; Task 2 `DirSource`, `ScanFolder`; `kube.lockedDown`, `labelManagedBy`.
- Produces:

```go
type FetchJobParams struct {
	Name, Namespace, FetchID, GitImage, Image, URL, Ref, Path, ResultURL string
	Timeout                                                              time.Duration
}
func FetchJobName(fetchID string) string // "pergent-fetch-" + the first 12 hex characters
func BuildFetchJob(p FetchJobParams) *batchv1.Job
//   init "clone": GitImage (jobs.image), sh -ec fetch.sh: a depth-1 fetch of FETCH_REF with no credentials and hooks off;
//     writes /work/commit, or /work/error "could not fetch <url> at <ref>; the URL, branch or tag may be wrong, or outside jobEgress.cidrs" and exits 0
//   main "send": Image (PERGENT_SH_IMAGE), pergent-sh fetch-skills; PERGENT_SH_FETCH_TOKEN from the Job's Secret
//   no service account token, lockedDown(), activeDeadlineSeconds = Timeout, the managed-by label
func BuildFetchSecret(p FetchJobParams, owner *batchv1.Job, token string) *corev1.Secret
func (l *Launcher) LaunchFetch(ctx context.Context, p FetchJobParams, token string) error
// pergent-sh fetch-skills --dir /work/repo --path <folder> --commit-file /work/commit --error-file /work/error --result-url <url>
//   posts {"commit","error","files":[{"name","path","content","problem"}]} with Authorization: Bearer <token>
// callback.FetchHandler: 401 bad token; 409 not running; 413 over MaxFetchBytes; 204 stored
// Fetcher gains Launcher interface{ LaunchFetch(...) error }, Namespace, GitImage, Image (from PERGENT_SH_IMAGE).
// A URL fetch with Image empty -> 400 "installing from a URL needs PERGENT_SH_IMAGE (set by the chart)"
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestBuildFetchJob(t *testing.T)               // no token env in "clone"; managed-by label; no SA token; deadline; the URL only in env
func TestFetchScriptHasNoCredentials(t *testing.T) // fetch.sh holds no GIT_TOKEN; core.hooksPath /dev/null; --depth=1
func TestFetchSkillsPostsFiles(t *testing.T)       // temp clone -> httptest receives the files and the commit
func TestFetchSkillsPostsError(t *testing.T)       // /work/error present -> its text posted as error
func TestFetchSkillsRefusesSymlinkOutside(t *testing.T)
func TestFetchCallbackOneTime(t *testing.T)        // second post 409; post after the sweep 409; over 4 MiB 413; the store unchanged
func TestStartURLLaunchesJob(t *testing.T)         // fake launcher gets the URL; the only secret is the fetch token
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/kube/ ./internal/callback/ ./internal/skillfetch/ ./cmd/pergent-sh/` with the DB env. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite and the chart checks. Expected: PASS.
- [ ] **Step 5: Commit** `feat: fetch skills from a public git URL in a Job`.

---

### Task 12: Web: Skills list, editor, copy and delete

**Files:**
- Create: `web/src/pages/SkillEditor.tsx`, `web/src/pages/SkillEditor.test.tsx`
- Modify: `web/src/pages/Skills.tsx`, `web/src/pages/Skills.test.tsx`, `web/src/api.ts`, `web/src/App.tsx` (routes `skills/new`, `skills/:name`), `web/src/test/fixtures.ts`, `web/src/styles.css`

**Interfaces:**
- Consumes: the Task 5 and Task 7 API shapes.
- Produces: `LibrarySkill`, `SkillDetail`, `SkillSource` TS types in `api.ts`, mirroring the Go views.

- [ ] **Step 1: Write the failing tests:**

```ts
test("lists name, version, source and the default badge")
test("an admin sees New skill, Install from git, Edit, Copy and Delete; a viewer sees none")
test("an installed skill has Copy and Check for updates, and no Edit")
test("create posts name, content and addToDefaults; Add to default skills is off at first")
test("a stale save shows 'changed since you opened it' with Reload")
test("a delete 409 lists the repositories, the default list and '+2 repositories you can't see'")
test("renders skill content as text") // "<script>x</script>" and "# h" appear verbatim in <pre>
test("a hidden source shows 'a private repository on main'")
```

- [ ] **Step 2: Run** `cd web && npx vitest run src/pages/Skills.test.tsx src/pages/SkillEditor.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the web checks and the Go suite (`make web` embeds the build). Expected: PASS.
- [ ] **Step 5: Commit** `feat(web): skill library list and editor`.

---

### Task 13: Web: Install from git and Check for updates

**Files:**
- Create: `web/src/diff.ts`, `web/src/diff.test.ts`, `web/src/pages/SkillInstall.tsx`, `web/src/pages/SkillInstall.test.tsx`, `web/src/pages/SkillUpdates.tsx`, `web/src/pages/SkillUpdates.test.tsx`
- Modify: `web/src/App.tsx` (routes `skills/install`, `skills/updates`), `web/src/api.ts`, `web/src/test/fixtures.ts`

**Interfaces:**
- Consumes: the Task 10 API shapes.
- Produces: `export type DiffLine = { kind: "same" | "add" | "del"; text: string }` and `export function lineDiff(a: string, b: string): DiffLine[]`.

- [ ] **Step 1: Write the failing tests:**

```ts
test("lineDiff marks added and removed lines")
test("install: instance and repository, or URL; ref; folder; polls until done; lists skills with size and problem; Add to default skills is off at first")
test("a taken name can't be ticked and says 'name already in the library'")
test("the install page says installed skills are readable by every signed-in user")
test("the URL help says the Job reaches only jobEgress.cidrs")
test("updates: a diff per skill; 'removed at the source'; 'renamed at the source' with Update disabled")
test("updates: a new tag is sent as ref, with fromRef")
```

- [ ] **Step 2: Run** `cd web && npx vitest run src/diff.test.ts src/pages/SkillInstall.test.tsx src/pages/SkillUpdates.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the web checks and the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(web): install skills from git and check for updates`.

---

### Task 14: Web: defaults, repository skills, checklist, warning, versions

**Files:**
- Modify: `web/src/pages/AdminSettings.tsx`, `RepositorySettings.tsx`, `Repositories.tsx`, `Overview.tsx`, `RepositoryDetail.tsx`, `ReviewDetail.tsx` and their tests, `web/src/api.ts`, `web/src/test/fixtures.ts`

**Interfaces:**
- Consumes: the Task 5 and Task 8 API shapes.

- [ ] **Step 1: Write the failing tests:**

```ts
test("the default skills picker shows a running total against 900 KiB, and the 400 message")
test("repository settings: 'Use the default skills (a, b)' or 'Choose skills', with a running total")
test("detected skills have switches, off at first; .pergent/skills is shown as always on; a note says only the SKILL.md text is used")
test("Add repository warns when the default skill list is empty")
test("the overview checklist ticks done items, links each page, and is gone when all are done")
test("Skills & instructions shows the folder, the version and a file link")
test("the review page shows 'library v3' and 'repo (.claude/skills)'")
```

- [ ] **Step 2: Run** `cd web && npx vitest run`. Expected: FAIL for these tests.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the web checks and the Go suite. Expected: PASS.
- [ ] **Step 5: Commit** `feat(web): default and repository skills, setup checklist, skill versions`.

---

### Task 15: README, end-to-end test and release notes draft

**Files:**
- Create: `internal/engine/e2e_skills_test.go`
- Modify: `README.md`

- README: "Skills and instructions" (the three sources, the D23 order, `repoSkills`, no skills means no review); the roles table; the Dashboard pages (Skills, Install from git, Check for updates, Settings, the setup checklist); the audit actions; the config reference (`repositories[].repo_skills`; `skills_dir` removed); "Exposing pergent.sh" lists `/skill-fetches/` on the public host; `jobEgress.cidrs` for public sources; the access a private source needs (GitLab review bot Reporter, GitHub review App installed); the SQL step to remove a version by hand (D5); an **Upgrading to 0.3.0** note. Quote the code's messages exactly.
- Release notes draft, handed to the owner, not stored in the repository: breaking changes (no `skills:` value or `skills_dir`, and a config with `skills_dir` is refused; null `skills` means the default list; migration `0014` runs at startup).

- [ ] **Step 1: Write the test** `TestEndToEndSkills` (GitLab fake): install `lint` from a fixture repository at commit C1 → a review's Job ConfigMap holds its text and the run records `{"lint":1}` → the fixture moves to C2, Check for updates, Update → the next review records `{"lint":2}` and the run page shows version 2 → with the default list emptied and no repository skills, a push is `skipped` with no status.
- [ ] **Step 2: Run** `go test -count=1 -run TestEndToEndSkills ./internal/engine/` with the DB env. Expected: PASS. A failure is a bug in an earlier task: fix it with a failing test in that task's package first.
- [ ] **Step 3: Update** the README, then run the private-name scan. Expected: no output.
- [ ] **Step 4: Commit** `docs: skill library, installs and repository skills; end-to-end test`.

---

### Task 16: Full verify gate

- [ ] **Step 1:** `docker ps --filter name=pergent-sh-pg --format '{{.Status}}'` prints `Up ...`. If not, stop and say so.
- [ ] **Step 2:** The Go suite. Expected: `ok` for every package; `go test -count=1 -v ./internal/store/ ./internal/engine/ 2>&1 | grep -c -- '--- SKIP'` (with the DB env) prints `0`; `go vet ./...` prints nothing.
- [ ] **Step 3:** The web checks. Expected: no type or lint errors, every vitest file passes, the build succeeds.
- [ ] **Step 4:** The chart checks. Expected: `1 chart(s) linted, 0 chart(s) failed`, twice.
- [ ] **Step 5:** The private-name scan. Expected: no output.
- [ ] **Step 6:** `superpowers:requesting-code-review` on the branch, against the spec's anti-goals and this plan's Review Focus. Fix findings test-first, then repeat Steps 2 to 5.
