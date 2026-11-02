# Repository Readiness Scan (piece 3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each repository gets a Readiness tab with 4 checks (project context, ownership and process, skill fit, size and noise), scanned when it is switched on and on **Scan now**, with 3 new "Needs attention" kinds and a one-click **Use these skills**.

**Architecture:** pergent first exports its glob matcher, default exclusions and skill file selection as the public package `github.com/gopaytech/pergent/scope`; pergent.sh pins that commit. A scan is a row in `readiness_scans` (migration `0015`). A scan loop in the engine (its own goroutine, never the engine loop) claims queued scans, reads the settings checks in the pod with the review bot or App, stores them, then starts a short Job: a `clone` container (pergent image, the review Job's read-only clone token) and a `scan` container (`pergent-sh scan-repo`, a one-time result token) that posts a compact result to `POST /readiness-scans/{id}/result`. The checklist is built at read time from the stored results and the current skill settings, so a settings change re-judges skill fit and budget without a new scan.

**Tech Stack:** Go 1.24.8, pgx v5, client-go, React 18 + TS + TanStack Query 5 + Vitest.

**Spec:** `docs/superpowers/specs/2026-11-02-readiness-scan-design.md` (binding; its anti-goals are the failure bar). Read it with the dashboard, organization, commands and skills library designs, whose constraints still hold.

## Global Constraints

- **Two repositories.** Task 1 is in pergent (`/Users/zufar.dhiyaullhaq/Documents/personal/github/pergent`, branch `feat/scope` off `master`). Tasks 2 to 12 are in pergent.sh (`/Users/zufar.dhiyaullhaq/Documents/personal/github/pergent.sh`, branch `feat/readiness-scan` off `main`). No pergent release, no chart change. Ships as pergent.sh 0.5.0; the release waits for the owner.
- **The Task 1 commit must be on pergent `master` on GitHub before Task 2 pins it** (`go get` fetches it through the module proxy). That push needs the owner's go-ahead at execution time: the controller stops after Task 1 and asks.
- States, exact: `passed`, `warning`, `missing`, `unknown` (shown as "Couldn't check: <reason>"). `unknown` is never shown or counted as passed.
- Numbers: at most **2** scan Jobs at once, outside `jobs.max_running`; Scan now at most once per repository per **5 minutes**; noise = an included file over **512 KiB** (524288 bytes) or with a noise name; at most **50** noise paths, largest first, plus a count of the rest; a language is listed when it is over **10%** of source files and no assigned skill reviews any of its files; budget **900 KiB** (`inputs.ConfigMapLimit`).
- Noise names, exact (`scope.Match`): `*.pb.go`, `*_generated.*`, `*.min.js` (generated or minified name); `**/dist/**`, `**/build/**` (build output). The fix offered is a `.gitattributes` line `<pattern> linguist-generated` (never `linguist-vendored`) or an `exclude` glob on the skills that review the file.
- JSON (results, views, Job input) uses the Go field names in lowerCamel (`requiredChecks`, `waitSeconds`).
- Exact copy: `This repository will stop following the default skill list.` (confirm on Use these skills when `skills` is null); `couldn't scan files: <reason>`; `a scan is already queued or running` (409); `this repository was scanned less than 5 minutes ago; try again in <N> minutes` (429, N rounded up); `only maintainers and admins of this repository can scan it` (403).
- Needs attention kinds, exact: `no_skill_coverage`, `inputs_over_budget`, `no_project_context`. Switching a repository off drops them (as today) and deletes its scans.
- Audit: `readiness_scan_requested` (actor, repository) for an accepted Scan now only. A switch-on scan is not audited.
- The scan Job gets the read-only clone token (clone container only) and its one-time result token (scan container only). No LLM key, no write token. It carries `app.kubernetes.io/managed-by: pergent-sh`, so the review-Job NetworkPolicy applies.
- What the Job posts is untrusted text: length-capped, control characters dropped, shown as text only, never run.
- Scans exist only with the dashboard on (`dashboard.url`), like skill fetches: otherwise nothing is queued and the callback route is absent.
- **Go suite:** `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:pergent@localhost:55432/pergent_test?sslmode=disable' go test -count=1 ./...` in one command (container `pergent-sh-pg` up; store, dashboard and engine tests must not skip), then `go vet ./...`. "With the DB env" means that variable in the same command.
- **pergent suite (Task 1):** `docker run --rm -v "$PWD":/src -w /src golang:1.24-trixie sh -c 'git config --global --add safe.directory "*" && go test -count=1 ./...'` (the Mac's git is too old for the `cmd` agent tests), plus `make vet`.
- **Web checks:** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`, never while `go test` runs.
- Private-name scan prints nothing, in each repo: run the private-name scan kept in the pergent-developer memory (its pattern stays out of this public repository), excluding `go.sum`, `docs/superpowers/plans/` and `web/package-lock.json`.
- Commits: `git -c user.email=zufardhiyaulhaq@gmail.com commit`, conventional subjects, ending `Co-Authored-By: Claude <model> <noreply@anthropic.com>` for the model that wrote it. Never push, tag or release without the owner. Never run `go clean`, `docker builder prune` or any cache cleanup.

## Review Focus

1. **A repository too large to clone.** The Job is evicted at its 2 GiB disk limit: the scan ends `failed`, file checks say `couldn't scan files: the repository is too large to clone (the scan has 2 GiB of disk)`, and the settings rows still show what the pod read. Pinned in Task 7 `TestScanEvictedKeepsSettings`, Task 3 `TestChecklistFilesFailedKeepsSettings`.
2. **A check the bot can't read.** A 403 or 404 on approval rules, branch protection or the pipeline setting, an approve side that can't reach the repository, hidden GitHub required checks: the row is `unknown` with its reason, never `passed`, and no Needs attention item flips. Pinned in Task 6 `TestSettingsRefusedIsUnknown`, Task 3 `TestChecklistUnknownNeverPassed`.
3. **A user who can't see the repository.** `GET .../readiness` and `POST .../readiness/scan` answer the generic 404 with no path, commit or setting in the body, and queue nothing. Pinned in Task 9 `TestReadinessHiddenRepo404`.
4. **Scan now twice.** While a scan is queued or running: 409 with that scan. Within 5 minutes of the latest scan's queueing: 429 with `waitSeconds` and `Retry-After`. Two concurrent requests: exactly one 202. Pinned in Task 4 `TestQueueScanRace`, Task 9 `TestScanNowTwice`.
5. **A skill with no `paths`.** It covers every repository (no `no_skill_coverage`), is never listed as matching nothing, and its file count follows a review's default exclusions. Pinned in Task 2 `TestSkillWithoutPathsCovers`, Task 3 `TestFlagsNoPathsSkillCovers`.

## Builder decisions (the spec's "Left to the builder")

- **Settings reads, GitLab** (the review bot is Maintainer): approvals with `gitLabRequiredApprovals`; protection with `GET /projects/:id/protected_branches/:branch` (404 `missing`; `passed` when no push access level is Developer (30), else `warning` "Developers can push to <branch> directly"); required checks from the project's `only_allow_merge_if_pipeline_succeeds` (true `passed`, false `warning`, absent `unknown` "GitLab didn't show whether pipelines must succeed").
- **Settings reads, GitHub** (review App: Contents and Metadata read): approvals with `gitHubRequiredApprovals`, or `unknown` with `approverUnreachableNote` when the stored project's `ApproveAvailable` is false; protection from a `pull_request` or `update` rule in `GET /repos/{o}/{r}/rules/branches/{b}` or `protected` in `GET /repos/{o}/{r}/branches/{b}`; required checks from a `required_status_checks` rule or the branch's `protection.required_status_checks` contexts and checks (none shown and no rule: `unknown`).
- **Couldn't-check reasons:** `the review bot can't read the approval rules here`; `no ruleset requires reviews, and the review App can't read the classic branch protection (it needs the optional Administration: Read permission)`; `the review bot can't read branch protection here`; `the review App can't read this branch's rules`; `the review App can't see this branch's required checks`. Raw platform errors are logged (redacted), never stored or shown.
- **Job:** `jobs.image` clones, `PERGENT_SH_IMAGE` scans, as the fetch Job. Name `pergent-scan-<12hex>`; requests 100m CPU and 128Mi, memory limit 512Mi; `emptyDir` 2Gi; `activeDeadlineSeconds` 300; `backoffLimit` 0; TTL 3600 s; user 65532; read-only root filesystem; no service account token. The clone fetches the one commit at depth 1 with `core.symlinks false` and hooks off, so sizes are plain `Lstat` sizes and nothing links out of the checkout. Skill globs reach the scan container as env `PERGENT_SH_SCAN_INPUT` (JSON, at most 256 KiB, else the scan fails with `the skill library's paths are too large to send to the scan`).
- **Results:** `readiness.PodResult` and `readiness.JobResult`, `version` 1; result body limit 1 MiB (413, the scan fails). Only the latest finished scan and any queued or running one per repository are kept.
- **"Switched on for the first time":** reconcile queues a `switch-on` scan when it stores a project row that did not exist before that pass and the pass for that repository succeeded. That covers the dashboard toggle and config-file repositories alike; repositories switched on before this release are scanned once after the upgrade (next bullet).
- **Scans after the upgrade (agreed in grilling):** when the scan loop starts, it queues one `upgrade` scan for every switched-on repository that has no scan row at all, oldest project first, through the normal queue (2 at a time, the 5-minute gap counts from it, not audited). The check runs at every start, so it is idempotent and also catches a repository whose switch-on reconcile failed partway.
- **Languages:** the table in Task 2, by extension; "source files" have a listed extension and no default exclusion.
- **Lost Jobs are not retried.** Every minute the loop inspects each running scan's Job; a failed or missing Job, or a deadline passed by 1 minute, fails the scan with the matching reason. Scan now is the retry; its 5-minute gap counts from the latest scan's `queued_at`, whatever started it.
- **Skill fit after a settings change:** the Job reports file counts for every library skill and every repository-folder skill, so saving a repository's `skills` or `repoSkills`, or the default list, re-judges `no_skill_coverage` and `inputs_over_budget` from the last scan with no platform read. Languages stay as of the scan.

## File Structure

| File | Responsibility |
|---|---|
| pergent `scope/` (new); `internal/diff/classify.go`, `internal/review/select.go`, `cmd/agent.go` | Public matcher, path rules, header, `Decide`; the CLI delegates, unchanged. |
| `go.mod`, `internal/inputs/` | Pin; export `SkillScope`, `LoadInstructions`. |
| `internal/readiness/` (new) | Pure: Job types, `ScanTree`, languages, `Effective`, `ComputeFlags`, `BuildChecklist`. |
| `internal/store/migrations/0015_readiness.sql`, `internal/store/readiness.go` (new) | Scan rows. |
| `internal/kube/scan.go`, `scan.sh`, `cmd/pergent-sh/scan.go` (new) | The Job and `pergent-sh scan-repo`. |
| `internal/gitlab/client.go`, `internal/github/client.go`, `internal/engine/readiness_settings.go` (new) | Settings reads. |
| `internal/engine/readiness.go` (new), `reconcile.go`, `attention.go`, `engine.go` | Loop, completion, attention, hooks. |
| `internal/callback/readiness.go` (new), `internal/server/server.go`, `cmd/pergent-sh/main.go` | Callback and wiring. |
| `internal/dashboard/api_readiness.go` (new) and the files Task 9 names | View, Scan now, rechecks. |
| `web/src/pages/Readiness.tsx` (new), `RepositoryDetail.tsx`, `Attention.tsx`, `web/src/api.ts` | The tab. |
| `README.md` | The behaviour contract. |

---

### Task 1 (pergent repository): public `scope` package

**Files:**
- Create: `scope/match.go` (body of `internal/diff/glob.go`), `scope/rules.go`, `scope/match_test.go` (moved `glob_test.go`), `scope/rules_test.go`
- Delete: `internal/diff/glob.go`, `internal/diff/glob_test.go`
- Modify: `internal/diff/classify.go` (`Classifier` holds a `scope.Attributes`; `lockfiles`, `vendoredDirs`, the header regexp move to `scope`), `internal/review/select.go`, `cmd/agent.go:808-818`, `CLAUDE.md` (an Architecture line for `scope/`)

**Interfaces:**
- Produces:

```go
package scope // imports no pergent internal package

const (
	RuleLockfile, RuleVendored, RuleGenerated = "lockfile", "vendored", "generated"
	RuleOutOfScope, RuleExcludedBySkill = "out of scope", "excluded by skill"
)
func Match(pattern, p string) bool
func MatchAny(patterns []string, p string) bool
type Attributes struct{ /* linguist-generated patterns */ }
func ParseAttributes(gitattributes []byte) Attributes
func (a Attributes) PathRule(p string) string // lockfile, vendored, generated (by attribute) or "", in today's order; all opt-in
func HasGeneratedHeader(content string) bool // first 10 lines
func IsGeneratedHeaderLine(line string) bool
// Decide applies one skill to one file whose default exclusion is rule (optIn:
// paths may override it), in review.Select's order: "" when the skill reviews it.
func Decide(paths, exclude []string, p, rule string, optIn bool) string
```

- [ ] **Step 1: Write the failing tests** in `scope/rules_test.go`:

```go
func TestScopeImportsNoInternal(t *testing.T) // go list -deps of scope has no /internal/ (as render's test)
func TestPathRuleOrder(t *testing.T) // "vendor/go.sum" lockfile; "a/node_modules/x.js" vendored; "gen/ linguist-generated" -> "gen/a.go" generated;
 // "x linguist-generated=true" counts; "x linguist-vendored" -> ""; "main.go" -> ""
func TestDecide(t *testing.T) // non-opt-in "binary" -> "binary"; paths [*.go], "a.py" -> out of scope; exclude [*_test.go] -> excluded by skill;
 // opt-in vendored + paths [vendor/**] -> ""; opt-in lockfile, no paths -> "lockfile"; nothing -> ""
```

- [ ] **Step 2: Run** `go test ./scope/` in the trixie container. Expected: FAIL, no Go files.
- [ ] **Step 3: Implement** `scope`; `diff.Classifier.Exclusion` keeps its signature and order (deleted, binary, no content changes, `PathRule`, the patch header); `review.Select` loops over `Decide`. Rule strings stay byte-identical.
- [ ] **Step 4: Run** the pergent suite and `make vet`. Expected: PASS, with the existing `internal/diff`, `internal/review` tests and `render/testdata/golden` unchanged.
- [ ] **Step 5: Commit** `refactor: public scope package (glob matcher, default exclusions, skill file selection)`.
- [ ] **Step 6: Stop.** The controller asks the owner to merge `feat/scope` into pergent `master` and push it (`origin` is SSH). Record the SHA for Task 2.

---

### Task 2: Pin pergent; the Job-side tree scan

**Files:**
- Modify: `go.mod`, `go.sum`, `internal/inputs/frontmatter.go`, `internal/inputs/inputs.go` (`loadInstructions` → `LoadInstructions`)
- Create: `internal/readiness/result.go`, `scan.go`, `languages.go`, `scan_test.go`

**Interfaces:**
- Consumes: Task 1; `inputs.ScanFolder`, `inputs.DirSource`, `inputs.RepoFolders`.
- Produces:

```go
func SkillScope(content []byte) (paths, exclude []string, err error) // inputs
func LoadInstructions(ctx context.Context, src Source, projectID int64, ref string) ([]byte, error) // inputs

package readiness
const (ResultVersion = 1; MaxResultBytes, MaxInputBytes = 1 << 20, 256 << 10
	MaxNoisePaths, MaxPathBytes = 50, 512; NoiseSize = 512 << 10; FolderLibrary = "library")
type ScopedSkill struct{ Name string; Paths, Exclude []string; Assigned bool } // Assigned: in the library list at scan time
type JobInput struct{ Version int; Library []ScopedSkill; RepoSkills []string } // every library skill; repo_skills
type SkillFit struct {
	Name, Folder, Problem string // Folder: FolderLibrary or an inputs.RepoFolders entry
	HasPaths bool
	Files, Bytes int // Files: scope.Decide == ""; Bytes: repository skills only
}
func (f SkillFit) Covers() bool // f.Problem == "" && (f.Files > 0 || !f.HasPaths)
type Language struct{ Name string; Files int; Uncovered bool }
type NoiseFile struct{ Path, Why string; Size int64; Skills []string } // Why: "generated or minified name", "build output", "larger than 512 KiB"
type Context struct {
	AgentFile, Readme, Contributing, CodeOwners string // AgentFile: "AGENTS.md", "CLAUDE.md" or ""
	AgentFileEmpty, Instructions bool
	CodeOwnerRules int
}
type JobResult struct {
	Version int
	Commit, Error string
	Files, SourceFiles, NoiseTotal, InstructionsBytes int // InstructionsBytes: len(LoadInstructions)
	Context Context
	Skills []SkillFit
	Languages []Language
	Noise []NoiseFile
}
// ScanOutcome is what became of a posted result (engine returns it, callback reads it).
type ScanOutcome int
const (ScanStored ScanOutcome = iota + 1; ScanUnauthorized; ScanConflict)
// Effective: the skills a review would run, D23 order: every .pergent/skills skill, each
// repoSkills name from the first folder that has it, then library names not replaced.
func Effective(fits []SkillFit, library, repoSkills []string) []SkillFit
func LanguageOf(path string) string // "" for no listed extension
func ScanTree(ctx context.Context, root string, in JobInput) (JobResult, error) // never follows a link, skips .git; fills all but Version, Commit, Error
```

Per file: a NUL byte in the first 8000 bytes is the non-opt-in `binary`; else `PathRule`; else `generated` when `HasGeneratedHeader` holds for its first 10 lines. A file is **included** when an effective skill reviews it, or, with no effective skill, when it has no rule. Context: root `AGENTS.md`, then `CLAUDE.md`; a root README in any case of `README`, `README.md`, `README.rst`, `README.txt`, `README.adoc`; `CONTRIBUTING.md` (any case) at root, `.github/`, `docs/`; CODEOWNERS, first of `.github/`, root, `docs/`, `.gitlab/`, where rules are lines not blank and not starting with `#`, `[` or `^[`.

Languages (exact): Go `.go`; Python `.py`; JavaScript `.js .jsx .mjs .cjs`; TypeScript `.ts .tsx`; Java `.java`; Kotlin `.kt .kts`; Ruby `.rb`; PHP `.php`; C# `.cs`; C `.c .h`; C++ `.cc .cpp .cxx .hpp .hh`; Rust `.rs`; Swift `.swift`; Scala `.scala`; Shell `.sh .bash`; SQL `.sql`; Terraform `.tf`; Dart `.dart`; Elixir `.ex .exs`; Lua `.lua`; Vue `.vue`; Svelte `.svelte`; Objective-C `.m .mm`; Protocol Buffers `.proto`.

- [ ] **Step 1: Pin:** `go get github.com/gopaytech/pergent@<Task 1 SHA>`, then `go mod tidy`. `go.mod` shows a `v0.1.3-0.<timestamp>-<12 hex>` pseudo-version; `go build ./...` passes.
- [ ] **Step 2: Write the failing tests** over `t.TempDir()` trees:

```go
func TestScanTreeContext(t *testing.T) // empty AGENTS.md -> AgentFile, AgentFileEmpty; readme.md; docs/CONTRIBUTING.md; .gitlab/CODEOWNERS "# x","[S]","* @a" -> 1 rule
func TestSelectionMatchesReview(t *testing.T) // Files == count of scope.Decide == "" over vendor/, go.sum, .gitattributes, a header, a binary
func TestSkillWithoutPathsCovers(t *testing.T) // no paths: Covers() even when every file is vendored; Files counts only unexcluded files
func TestEffectivePrecedence(t *testing.T) // "lint" in .pergent/skills and library -> ".pergent/skills"; .claude/skills/a only when in RepoSkills
func TestNoiseOnlyWhatAReviewReads(t *testing.T) // dist/app.js listed; vendor/x.min.js not; x.pb.go with the header not; a 600 KiB .go "larger than 512 KiB"
func TestNoiseCapped(t *testing.T) // 60 noise files -> 50 largest first, NoiseTotal 60; a path over 512 bytes counted, not listed
func TestLanguagesUncovered(t *testing.T) // 8 .go + 2 .py, skill paths [*.go] -> Python Uncovered; 1 .rb in 20 -> not listed
func TestScanTreeIgnoresLinks(t *testing.T) // a symlink to /etc/passwd is neither read nor counted
func TestInstructionsBytes(t *testing.T) // == len(inputs.LoadInstructions(DirSource{root}))
```

- [ ] **Step 3: Run** `go test -count=1 ./internal/readiness/`. Expected: FAIL, `undefined: ScanTree`.
- [ ] **Step 4: Implement.**
- [ ] **Step 5: Run** `go test -count=1 ./internal/readiness/ ./internal/inputs/` and `go vet ./...`. Expected: PASS.
- [ ] **Step 6: Commit** `feat(readiness): pin pergent scope; scan a checkout for context, skill fit and noise`.

---

### Task 3: The checklist and the attention flags

**Files:**
- Create: `internal/readiness/checklist.go`, `internal/readiness/checklist_test.go`

**Interfaces:**
- Consumes: Task 2.
- Produces:

```go
const (Passed, Warning, Missing, Unknown = "passed", "warning", "missing", "unknown")
type PodItem struct{ State, Reason string; Count int } // Count: approvals only
type PodResult struct{ Version int; Approvals, Protected, RequiredChecks PodItem }
type Flags struct{ NoCoverage, NoContext, BudgetKnown bool; Budget int } // BudgetKnown false: no Job result, or an effective repo skill has a Problem
func ComputeFlags(job *JobResult, library, repoSkills []string, librarySizes map[string]int) Flags
type ChecklistInput struct {
	Pod *PodResult
	Job *JobResult // nil: no file result
	FileError string // the scan's error when Job is nil
	Library []string // the library list now: the repository's, or the defaults
	RepoSkills []string
	LibrarySizes map[string]int
	SingleApproval bool // the live single_approval item is set
}
type Item struct {
	Key, State, Reason, Why, Fix string // Reason: the couldn't-check reason or what was found
	List []string
	Noise []NoiseFile
	More int
}
type Section struct{ Key string; Items []Item } // "context", "process", "skills", "size"
type Checklist struct{ Sections []Section; Suggested []string; Budget int }
func BuildChecklist(in ChecklistInput) Checklist
```

Item keys follow the spec's table, in order: `agent_file`, `readme`, `contributing` (warning when missing), `instructions` (warning), `codeowners` (warning when missing or 0 rules), `approvals` (passed ≥ 2, warning ≤ 1), `protected`, `required_checks`, `skill_match` (missing when no effective skill covers), `skills_unmatched` (warning, listing them), `languages` (warning, listing them), `noise` (warning), `budget` (missing over 900 KiB). `Job == nil` makes every file item `unknown` with `couldn't scan files: <FileError>`; `Pod == nil` every settings item. `SingleApproval` makes `approvals` a warning whatever the pod read. The `noise` fix is one `.gitattributes` line per distinct pattern (`<dir>/** linguist-generated` for build output, else the path) and, per reviewing skill, `exclude: [<path>]`. `Suggested` is library skills not assigned with `Files > 0`. `Why` copy is the builder's: one sentence on what the gap costs a review.

- [ ] **Step 1: Write the failing tests:**

```go
func TestChecklistOrderAndKeys(t *testing.T) // 4 sections, the 13 keys in order
func TestChecklistUnknownNeverPassed(t *testing.T) // table: Pod nil, Job nil, each PodItem Unknown -> no affected item is Passed
func TestChecklistFilesFailedKeepsSettings(t *testing.T) // Job nil, FileError "x": file items Unknown "couldn't scan files: x"; pod items as read
func TestChecklistLiveSingleApproval(t *testing.T) // approvals Count 3 Passed + SingleApproval -> Warning
func TestChecklistSuggested(t *testing.T) // unassigned with Files>0 suggested; assigned or Files 0 not
func TestChecklistNoiseFix(t *testing.T) // dist/a.js -> "dist/** linguist-generated"; never "linguist-vendored"
func TestFlagsNoPathsSkillCovers(t *testing.T) // one assigned no-paths skill with Files 0 -> NoCoverage false
func TestFlagsZeroAssigned(t *testing.T) // no effective skill -> NoCoverage false (no_skills is the review's item)
func TestFlagsBudget(t *testing.T) // InstructionsBytes + effective repo Bytes + library sizes; a Problem -> BudgetKnown false
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/readiness/`. Expected: FAIL, `undefined: BuildChecklist`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same. Expected: PASS.
- [ ] **Step 5: Commit** `feat(readiness): checklist and attention flags`.

---

### Task 4: Store: migration `0015` and scan rows

**Files:**
- Create: `internal/store/migrations/0015_readiness.sql`, `internal/store/readiness.go`, `internal/store/readiness_test.go`

**Interfaces:**
- Produces:

```go
// 0015: readiness_scans(id uuid PK, instance, project, branch, commit, state CHECK IN
//   ('queued','running','done','failed'), started_by, queued_at, started_at, finished_at,
//   deadline, job_name, error, pod_result jsonb, job_result jsonb, token_hash bytea);
//   UNIQUE (instance, lower(project)) WHERE state IN ('queued','running');
//   INDEX (instance, lower(project), queued_at DESC).
type ScanState string
const (ScanQueued, ScanRunning, ScanDone, ScanFailed ScanState = "queued", "running", "done", "failed")
const (StartedBySwitchOn, StartedByUpgrade = "switch-on", "upgrade")
type ReadinessScan struct {
	ID, Instance, Project, Branch, Commit, StartedBy, JobName, Error string
	State ScanState
	QueuedAt, StartedAt, FinishedAt, Deadline time.Time
	Pod, Job json.RawMessage
}
type QueueOutcome int
const (ScanQueuedNow QueueOutcome = iota + 1; ScanBusy; ScanTooSoon)
func (s *Store) QueueReadinessScan(ctx context.Context, instance, project, by string, now time.Time, gap time.Duration) (ReadinessScan, QueueOutcome, error) // Busy: the active scan; TooSoon: the latest; gap 0 never refuses
func (s *Store) ClaimReadinessScan(ctx context.Context, limit int, now, deadline time.Time) (ReadinessScan, bool, error) // oldest queued -> running while fewer than limit run; FOR UPDATE SKIP LOCKED
func (s *Store) SetReadinessPod(ctx context.Context, id, branch, commit string, pod json.RawMessage) error
func (s *Store) StartReadinessJob(ctx context.Context, id, jobName string, tokenHash []byte, deadline time.Time) error
func (s *Store) ReadinessScan(ctx context.Context, id string) (ReadinessScan, error) // ErrReadinessScanNotFound
func (s *Store) ReadinessTokenIs(ctx context.Context, id string, hash []byte) (bool, error)
// FinishReadinessScan ends a running scan (done when errText is "", else failed), spending the token when
// hash is set (nil: a pod-side end), and deletes the repository's older finished scans in the same transaction.
func (s *Store) FinishReadinessScan(ctx context.Context, id string, hash []byte, job json.RawMessage, errText string, now time.Time) (bool, error)
func (s *Store) RunningReadinessScans(ctx context.Context) ([]ReadinessScan, error)
func (s *Store) ReadinessScans(ctx context.Context, instance, project string) (latest, active *ReadinessScan, err error) // case-insensitive project
func (s *Store) DeleteReadinessScans(ctx context.Context, instance, project string) error
func (s *Store) UnscannedProjects(ctx context.Context) ([]ProjectRef, error) // switched-on projects with no readiness_scans row, oldest project first; ProjectRef{Instance, Project}, reusing an existing type if one fits
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestQueueScanRace(t *testing.T) // 10 goroutines, one repo: exactly one ScanQueuedNow, 9 ScanBusy with its ID
func TestQueueScanGap(t *testing.T) // latest queued t0 and finished: t0+4m gap 5m -> ScanTooSoon; t0+5m -> queued; gap 0 -> queued
func TestQueueScanCaseInsensitive(t *testing.T) // "Acme/Web" active blocks "acme/web"
func TestClaimRespectsLimit(t *testing.T) // 3 queued, limit 2 -> 2 claims by queued_at, third false
func TestFinishSpendsToken(t *testing.T) // right hash -> true, done; again -> false; wrong hash -> false, unchanged
func TestFinishPrunesOlder(t *testing.T) // two finished, a third finishes -> only the third remains
func TestUnscannedProjects(t *testing.T) // 3 switched on, 1 scanned, 1 switched off -> the other 2, oldest first
func TestReadinessScansLatestAndActive(t *testing.T)
func TestDeleteReadinessScans(t *testing.T)
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Scan|Readiness|Claim|Finish' ./internal/store/` with the DB env. Expected: FAIL, `undefined: QueueReadinessScan`.
- [ ] **Step 3: Implement** in the store's style (explicit columns, one transaction per write, IDs from Go as `skill_fetches` does).
- [ ] **Step 4: Run** the Go suite with the DB env. Expected: PASS, including the migration-count test (15 rows).
- [ ] **Step 5: Commit** `feat(store): readiness scans (migration 0015)`.

---

### Task 5: The scan Job and `pergent-sh scan-repo`

**Files:**
- Create: `internal/kube/scan.go`, `internal/kube/scan.sh`, `internal/kube/scan_test.go`, `cmd/pergent-sh/scan.go`, `cmd/pergent-sh/scan_test.go`
- Modify: `cmd/pergent-sh/main.go` (the `scan-repo` subcommand and usage), `internal/skillfetch/job.go` (`jobText` → exported `JobText`)

**Interfaces:**
- Consumes: Task 2 `ScanTree`, `JobInput`, `JobResult`; `fetchLockedDown`, `secretEnv`, `ownerRef`, `ptr` (`internal/kube`); `postResult` (`cmd/pergent-sh/fetch.go`).
- Produces:

```go
type ScanJobParams struct {
	Name, Namespace, ScanID, GitImage, Image string
	RepoURL, GitUsername, Branch, Commit string // RepoURL never carries credentials
	ResultURL, Input string // Input: JobInput JSON
}
func ScanJobName(scanID string) string // "pergent-scan-" + first 12 hex
func BuildScanJob(p ScanJobParams) *batchv1.Job
func BuildScanSecret(p ScanJobParams, owner *batchv1.Job, gitToken, scanToken string) *corev1.Secret // keys "git-token", "scan-token"
func (l *Launcher) LaunchScan(ctx context.Context, p ScanJobParams, gitToken, scanToken string) error // suspended, Secret, unsuspend; deletes the Job on error
func (l *Launcher) ScanFailure(ctx context.Context, name string) (string, error) // why a Job ended without a result; "" while it runs or after it completed
// pergent-sh scan-repo --dir <clone> --commit-file <f> --error-file <f> --result-url <url>
// reads PERGENT_SH_SCAN_TOKEN and PERGENT_SH_SCAN_INPUT; exits 0 when posted, 1 when not.
```

`scan.sh` is `checkout.sh`'s auth header (from `GIT_USERNAME`/`GIT_TOKEN`, skipped when `GIT_TOKEN` is empty, which only the local check below uses) with `fetch.sh`'s isolation (`GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `HOME=/work`, hooks off, push URL disabled) and `core.symlinks false`. It fetches `$SCAN_COMMIT` at depth 1, falling back to `refs/heads/$SCAN_BRANCH`; it writes `the default branch moved during the scan; scan again` to `/work/error` when that head isn't `$SCAN_COMMIT`, `could not fetch the default branch` when the fetch fails, exits 0 either way, and writes the commit to `/work/commit`. `ScanFailure` maps an `Evicted` pod or an ephemeral-storage message to `the repository is too large to clone (the scan has 2 GiB of disk)`, `OOMKilled` to `the scan ran out of memory`, `DeadlineExceeded` to `the scan took longer than 5 minutes`, and any other failure or a missing Job to `the scan Job ended without a result`.

- [ ] **Step 1: Write the failing tests:**

```go
func TestBuildScanJobSecrets(t *testing.T) // clone: GIT_TOKEN from git-token, nothing from scan-token; scan: PERGENT_SH_SCAN_TOKEN from scan-token,
 // nothing from git-token; no env names an LLM key
func TestBuildScanJobLockedDown(t *testing.T) // managed-by label, user 65532, read-only root, no SA token, emptyDir 2Gi, deadline 300, backoff 0, 512Mi
func TestLaunchScanCleansUp(t *testing.T) // Secret create fails (fake clientset reactor) -> Job deleted, error returned
func TestScanFailureReasons(t *testing.T) // Evicted, ephemeral-storage, OOMKilled, DeadlineExceeded, missing Job, running -> ""
func TestScanRepoPostsResult(t *testing.T) // httptest: Bearer token, version 1, commit from the file, body under MaxResultBytes
func TestScanRepoPostsError(t *testing.T) // error file -> {"version":1,"error":...}; the tree is not read
func TestScanRepoBadInput(t *testing.T) // input not JSON -> posts error "the scan input couldn't be read"
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/kube/ ./cmd/pergent-sh/`. Expected: FAIL, `undefined: BuildScanJob`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same and `go vet ./...`. Expected: PASS. Then run `scan.sh` for real in the pergent image: `docker run --rm --read-only --user 65532:65532 --tmpfs /work:mode=1777 -e REPO_URL=https://github.com/gopaytech/pergent.git -e SCAN_BRANCH=master -e SCAN_COMMIT=<a master SHA> -e GIT_USERNAME= -e GIT_TOKEN= ghcr.io/gopaytech/pergent:v0.1.2 sh -ec "$(cat internal/kube/scan.sh)"`. Expected: exit 0, no `/work/error`.
- [ ] **Step 5: Commit** `feat(kube): readiness scan Job and pergent-sh scan-repo`.

---

### Task 6: Settings checks read in the pod

**Files:**
- Modify: `internal/gitlab/client.go`, `internal/github/client.go` (+tests)
- Create: `internal/engine/readiness_settings.go`, `internal/engine/readiness_settings_test.go`

**Interfaces:**
- Consumes: `gitLabRequiredApprovals`, `gitHubRequiredApprovals`, `approverUnreachableNote`, `Box.Open`, `App.CloneToken` (as dispatch uses them); Task 3 `PodResult`.
- Produces:

```go
type ProtectedBranch struct{ PushAccessLevels []AccessLevel } // gitlab; AccessLevel{AccessLevel int `json:"access_level"`}
func (c *Client) ProtectedBranch(ctx context.Context, projectID int64, branch string) (ProtectedBranch, bool, error) // 404 -> false, nil
// gitlab.Project gains OnlyAllowMergeIfPipelineSucceeds *bool `json:"only_allow_merge_if_pipeline_succeeds"`
func (c *Client) BranchRuleTypes(ctx context.Context, owner, repo, branch string) ([]string, error) // github; shares paging with RulesetApprovals
type Branch struct{ Protected, ProtectionShown bool; RequiredChecks int } // github
func (c *Client) Branch(ctx context.Context, owner, repo, branch string) (Branch, error)
type scanTarget struct {
	Branch, Commit, RepoURL, GitUser string
	GitToken []byte
	Pod readiness.PodResult
}
// scanSettings resolves the default branch to a commit and reads the 3 settings items.
// Pod is filled as far as read, also with an error; the error is one of the reasons below.
func (e *Engine) scanSettings(ctx context.Context, instance, project string) (scanTarget, error)
```

Reasons the scan stops on: `the repository has no default branch yet`, `pergent.sh hasn't set this repository up yet`, `pergent.sh has no clone token for this repository yet`, `couldn't read the default branch`. The settings rules are in Builder decisions.

- [ ] **Step 1: Write the failing tests** (fake GitLab and GitHub servers, as the engine tests use):

```go
func TestProtectedBranch(t *testing.T) // gitlab: levels decoded; 404 -> found false
func TestProjectPipelineFlag(t *testing.T) // gitlab: absent -> nil; false -> &false
func TestBranchAndRuleTypes(t *testing.T) // github: protected; contexts+checks counted; no protection object -> ProtectionShown false
func TestSettingsGitLab(t *testing.T) // approvals 2 -> Passed Count 2; Developers can push -> Warning; pipelines true -> Passed
func TestSettingsGitHub(t *testing.T) // pull_request rule -> Passed; required_status_checks rule -> Passed; approvals 1 -> Warning
func TestSettingsRefusedIsUnknown(t *testing.T) // 403 on each read in turn, both platforms: that item Unknown with its reason, never Passed;
 // ApproveAvailable false -> approvals Unknown with approverUnreachableNote
func TestSettingsNoRawErrors(t *testing.T) // a 500 whose body holds a URL and a token: neither in any Reason nor the error
func TestSettingsEmptyRepo(t *testing.T) // no default branch -> "the repository has no default branch yet"
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Protected|Pipeline|BranchAnd|Settings' ./internal/gitlab/ ./internal/github/ ./internal/engine/` with the DB env. Expected: FAIL, `undefined: ProtectedBranch`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same. Expected: PASS.
- [ ] **Step 5: Commit** `feat(engine): read readiness settings checks with the review bot or App`.

---

### Task 7: Engine: the scan loop, completion, attention and hooks

**Files:**
- Create: `internal/engine/readiness.go`, `internal/engine/readiness_test.go`
- Modify: `internal/engine/engine.go` (fields), `attention.go` (3 kinds and notes), `reconcile.go` (`reconcileRepo` and `reconcileGitHubRepo` also return `created bool`; `reconcile` queues on created; `DeleteReadinessScans` next to `dropAttention`)

**Interfaces:**
- Consumes: Tasks 3 to 6; `Store.LibrarySkills`, `Store.ResolveLibrary` (content for scopes), `Store.RepoDefaults`, `inputs.SkillScope`, `skillfetch.JobText`, `e.settings()`, dispatch's `mintToken`.
- Produces:

```go
// Engine gains Scans bool (dashboard on) and ScanImage string (PERGENT_SH_IMAGE).
const (scanCap = 2; scanGap = 5 * time.Minute; scanJobTimeout = 5 * time.Minute; scanGrace = time.Minute; scanPodBudget = time.Minute)
const (attNoSkillCoverage, attInputsOverBudget, attNoProjectContext = "no_skill_coverage", "inputs_over_budget", "no_project_context")
var ErrScanBusy = errors.New("a scan is already queued or running")
type ScanTooSoonError struct{ Wait time.Duration }
func (e *Engine) RequestScan(ctx context.Context, instance, project, actor string) (store.ReadinessScan, error) // ErrScanBusy, *ScanTooSoonError come with the active or latest scan
func (e *Engine) RunScans(ctx context.Context) // own goroutine: claim and launch, sweep every minute, rechecks
func (e *Engine) RecheckReadiness(instance, project string) // never blocks; project "" = every repository with skills null
func (e *Engine) CompleteScan(ctx context.Context, id, token string, body []byte) (readiness.ScanOutcome, error)
func (e *Engine) ScanTooLarge(ctx context.Context, id, token string) error // fails it: "the scan Job sent a result over 1 MiB"
```

A claimed scan runs: `scanSettings` (budget `scanPodBudget`) → `SetReadinessPod` → on error, `FinishReadinessScan` (nil hash) with the reason → `JobInput` from every library skill's scope, `Assigned` from the repository's list or the defaults → refuse over `MaxInputBytes` → mint the token → `LaunchScan` (failure: `couldn't start the scan Job; see the pergent.sh log`) → `StartReadinessJob`. An empty `ScanImage` fails it with `readiness scans need PERGENT_SH_IMAGE (set by the chart)`. `CompleteScan` checks the token (`ReadinessTokenIs`) before reading the body, then requires version 1 and the scan's commit (else failed, `the scan Job sent a result pergent.sh couldn't use`), passes every string through `JobText`, drops paths over `MaxPathBytes` and caps lists. A done scan sets or clears the 3 items from `ComputeFlags` (budget only when `BudgetKnown`); a failed scan changes none. Notes (exact):
- `no_skill_coverage`: `No skill this repository uses matches its files, so its reviews check nothing useful. The Readiness tab suggests skills that fit.`
- `inputs_over_budget`: `Skills and instructions come to <N> KiB, over the 900 KiB a review can take, so every review fails. Use fewer skills or shorten the instructions.`
- `no_project_context`: `The repository has no AGENTS.md, CLAUDE.md or README, so reviews don't know what the project is. Add one.`

- [ ] **Step 1: Write the failing tests** (fake clientset and platforms, DB env):

```go
func TestSwitchOnQueuesOneScan(t *testing.T) // first reconcile -> one scan by "switch-on"; second reconcile -> none
func TestNoScansWithoutDashboard(t *testing.T) // Scans false -> nothing queued, upgrade scans included
func TestStartupScansUnscanned(t *testing.T) // 3 switched on, 1 scanned -> RunScans queues 2 by "upgrade", oldest first; a restart queues none
func TestScanCapIsTwo(t *testing.T) // 3 queued -> 2 Jobs; still 2 when jobs.max_running is full of reviews
func TestScanNeedsNoLLM(t *testing.T) // no LLM set -> the Job starts; no container env names an LLM key
func TestScanPodPartStoredFirst(t *testing.T) // pod stored before LaunchScan; launch fails -> failed, pod kept
func TestScanEvictedKeepsSettings(t *testing.T) // pod Evicted -> sweep: failed with the too-large reason, pod kept
func TestCompleteScanOneTime(t *testing.T) // right token ScanStored; again ScanConflict; wrong ScanUnauthorized; other commit -> failed unusable
func TestScanAttention(t *testing.T) // no context, no match, over budget -> the 3 items; a clean scan clears them
func TestRecheckAfterSkillsChange(t *testing.T) // skills that match -> RecheckReadiness clears no_skill_coverage, no platform call
func TestRecheckDefaults(t *testing.T) // project "" rechecks only repositories with skills null
func TestSwitchOffDropsScans(t *testing.T) // switch off -> scans and the 3 items gone; a late result -> ScanUnauthorized
func TestFailedScanKeepsItems(t *testing.T) // an earlier no_project_context stays after a failed scan
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Scan|Recheck|SwitchOn|SwitchOff|FailedScan' ./internal/engine/` with the DB env. Expected: FAIL, `undefined: RequestScan`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite with the DB env. Expected: PASS.
- [ ] **Step 5: Commit** `feat(engine): readiness scan loop, results and Needs attention items`.

---

### Task 8: The result callback and wiring

**Files:**
- Create: `internal/callback/readiness.go`, `internal/callback/readiness_test.go`
- Modify: `internal/server/server.go` (Deps, route, doc comment), `cmd/pergent-sh/main.go` (Engine `Scans`, `ScanImage`; `RunScans` in `loops`; Deps; dashboard Options)

**Interfaces:**
- Consumes: Task 7 `CompleteScan`, `ScanTooLarge`; `readiness.ScanOutcome`.
- Produces:

```go
type ScanResults interface {
	CompleteScan(ctx context.Context, id, token string, body []byte) (readiness.ScanOutcome, error)
	ScanTooLarge(ctx context.Context, id, token string) error
}
type ScanHandler struct{ Scans ScanResults; Log *slog.Logger } // POST /readiness-scans/{id}/result, as FetchHandler
// server.Deps gains ReadinessScans callback.ScanResults; nil: no route.
```

- [ ] **Step 1: Write the failing tests:**

```go
func TestScanCallbackOneTime(t *testing.T) // no bearer 401; stored 204; again 409; wrong token 401; GET 405
func TestScanCallbackTooLarge(t *testing.T) // over 1 MiB -> 413, ScanTooLarge called with the token
func TestRoutesScanCallbackOnlyWithDashboard(t *testing.T) // nil ReadinessScans -> 404
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/callback/ ./internal/server/`. Expected: FAIL, `undefined: ScanHandler`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite with the DB env. Expected: PASS.
- [ ] **Step 5: Commit** `feat: readiness scan result callback and wiring`.

---

### Task 9: Dashboard API: readiness view, Scan now, rechecks

**Files:**
- Create: `internal/dashboard/api_readiness.go`, `internal/dashboard/api_readiness_test.go`
- Modify: `dashboard.go` (`Options.Readiness`), `api_read.go` (`repository`: a `/readiness` suffix next to `/inputs`), `api_write.go` (`POST /api/repositories/{instance}/{rest...}` becomes `repositoryPost`, dispatching `/settings/reset` and `/readiness/scan`; `updateSettings` rechecks when `skills` or `repoSkills` changed), `api_operator.go:845`, `api_skills.go:324`, `api_skill_fetch.go:486` (recheck all after a default-list change), `view.go`

**Interfaces:**
- Consumes: Task 3 `BuildChecklist`; Task 4 `ReadinessScans`; Task 7; `activeRepos`, `targetRepo`, `d.attention`, `d.audit`.
- Produces:

```go
type Readiness interface {
	RequestScan(ctx context.Context, instance, project, actor string) (store.ReadinessScan, error)
	RecheckReadiness(instance, project string)
}
type ScanView struct {
	ID, State, StartedBy, Branch, Commit, Error string
	QueuedAt time.Time
	FinishedAt *time.Time
}
type ReadinessView struct {
	Latest, Active *ScanView
	Checklist *readiness.Checklist // nil until a scan has finished
	CanScan, ConfigRepo bool // CanScan: maintainer or admin
	FollowsDefaults bool
	ApplyBase []string // today's list: the repository's, or the defaults
}
```

`POST .../readiness/scan` uses `targetRepo(w, r, "/readiness/scan", RoleMaintainer, true, msgNeedsRoleScan)`, so config repositories are allowed. Answers: 202 `{scan}` plus the audit entry; 409 `{error, scan}`; 429 `{error, waitSeconds}` with `Retry-After`.

- [ ] **Step 1: Write the failing tests:**

```go
func TestReadinessNotScanned(t *testing.T) // never scanned -> Latest and Checklist nil; CanScan per role
func TestReadinessChecklist(t *testing.T) // a finished scan -> 4 sections; live single_approval -> approvals Warning
func TestReadinessHiddenRepo404(t *testing.T) // a user who can't see it: GET and POST -> the generic 404 body only; nothing queued
func TestScanNowRoles(t *testing.T) // viewer 403 with msgNeedsRoleScan; maintainer 202; config repository maintainer 202
func TestScanNowTwice(t *testing.T) // active -> 409 with scan; latest queued 1 min ago and finished -> 429, waitSeconds 240,
 // Retry-After 240, "try again in 4 minutes"
func TestScanNowAudited(t *testing.T) // 202 -> one readiness_scan_requested; 409 and 429 -> none
func TestResetStillWorks(t *testing.T) // POST .../settings/reset unchanged
func TestRecheckOnSettingsSave(t *testing.T) // PUT skills -> RecheckReadiness(instance, project); PUT approve only -> none; defaults save -> ("", "")
func TestApplyBase(t *testing.T) // skills null -> FollowsDefaults, ApplyBase == defaults; a list -> ApplyBase == it
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Readiness|ScanNow|ResetStill|Recheck|ApplyBase' ./internal/dashboard/` with the DB env. Expected: FAIL, `undefined: ReadinessView`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Go suite with the DB env. Expected: PASS.
- [ ] **Step 5: Commit** `feat(dashboard): readiness view and Scan now`.

---

### Task 10: Web: the Readiness tab

**Files:**
- Create: `web/src/pages/Readiness.tsx`, `web/src/pages/Readiness.test.tsx`
- Modify: `web/src/api.ts` (types for Task 9 and Task 3, `paths.apiRepoReadiness`, `paths.apiRepoReadinessScan`), `web/src/pages/RepositoryDetail.tsx` (`Tab` gains `"readiness"`, pushed right after `"inputs"`), `web/src/pages/Attention.tsx` (the 3 kinds; their rows link to `?tab=readiness`)

**Interfaces:**
- Consumes: Task 9 JSON; `api.put` to `paths.apiRepoSettings` with `{skills}`; the existing `Pill`, `Loading`, `ErrorBox`, card and row classes.

Top line: `Scanned <N days> ago, by <who>, at commit <7 chars>` (switch-on: `Scanned <N days> ago, when the repository was switched on, at commit <7 chars>`; upgrade: `Scanned <N days> ago, after the upgrade, at commit <7 chars>`); `Scanning…` while `active` is set, polling every 5 s; `Not scanned yet` with no scan. Each item shows a state pill (`Passed`, `Warning`, `Missing`, `Couldn't check`), its reason, `why`, and `fix` in a `<pre>`. Suggestions are checkboxes. **Use these skills** saves `applyBase` plus the ticked ones, after `window.confirm("This repository will stop following the default skill list.")` when `followsDefaults`. A config repository shows `skills: [<applyBase + ticked>]` in a `<pre>` instead of the button.

- [ ] **Step 1: Write the failing tests** in `Readiness.test.tsx`:

```ts
it("shows Not scanned yet and Scan now for a maintainer")
it("hides Scan now from a viewer")
it("shows Couldn't check with the reason, never Passed, for an unknown item")
it("says a scan is already running on 409 and the wait on 429") // "Try again in 4 minutes."
it("confirms before leaving the default list, then PUTs applyBase plus the ticked skills")
it("shows the skills line to paste for a config repository, with no button")
it("lists at most 50 noise paths and the count of the rest")
it("places the Readiness tab right after Skills & instructions")
```

- [ ] **Step 2: Run** `cd web && npx vitest run src/pages/Readiness.test.tsx`. Expected: FAIL, module not found.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the web checks. Expected: PASS.
- [ ] **Step 5: Commit** `feat(web): Readiness tab`.

---

### Task 11: README and the end-to-end test

**Files:**
- Create: `internal/engine/e2e_readiness_test.go`
- Modify: `README.md`

README, from Global Constraints and Builder decisions: a `### Readiness scan` section under Dashboard (checks, what each settings read needs per platform, with **Administration: Read** on the GitHub review App listed as optional: it lets the 2 classic-branch-protection rows read instead of showing couldn't check (agreed in grilling); the GitHub App setup section says the same, couldn't-check reasons, when scans run, limits); 3 bullets under "Logs and Needs attention"; `/readiness-scans/` in "Exposing pergent.sh" (table and "Expose only" sentence); a callback row in HTTP routes (401, 409, 413); the scan Job in "What a Job can and cannot see"; `readiness_scan_requested` in Audit log; "Upgrading to 0.5.0" (migration `0015` at startup, back up Postgres; `/readiness-scans/` reachable by the Jobs like `/runs/`; every switched-on repository is scanned once after the upgrade, 2 at a time).

- [ ] **Step 1: Write** `TestReadinessScanEndToEnd` with `newDashboardE2E` and the fake GitLab: switch a repository on → reconcile → a `switch-on` scan → its Job in the fake clientset (deploy token in the clone container only) → post a result with the token from the Job's Secret through the real route → `GET .../readiness` has the checklist and Needs attention has `no_project_context` → Scan now at once is 409, and 429 after the scan ends.
- [ ] **Step 2: Run** `go test -count=1 -run TestReadinessScanEndToEnd ./internal/engine/` with the DB env. Expected: PASS; a failure is a bug in Tasks 4 to 9, fixed there with its own test.
- [ ] **Step 3: Write** the README changes.
- [ ] **Step 4: Run** the private-name scan. Expected: no output.
- [ ] **Step 5: Commit** `docs: readiness scan in the README; end-to-end test`.

---

### Task 12: Full verify gate

- [ ] **Step 1:** Go suite with the DB env, then `go vet ./...`. Expected: PASS, with store, dashboard and engine tests run.
- [ ] **Step 2:** Web checks, with no Go test running. Expected: PASS.
- [ ] **Step 3:** `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml`. Expected: PASS (chart unchanged).
- [ ] **Step 4:** Private-name scan in both repositories. Expected: no output.
- [ ] **Step 5:** `superpowers:requesting-code-review` (opus) on the whole branch, against the spec's anti-goals and this Review Focus. Fix each finding with its failing test first.
