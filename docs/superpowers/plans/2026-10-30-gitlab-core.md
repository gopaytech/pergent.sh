# pergent.sh 2a: GitLab End to End Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every MR in a configured GitLab project gets a pergent review run as a Kubernetes Job, with the comment, commit status and bot approval posted by pergent.sh, and no UI.

**Architecture:**
- A single Go service, `pergent-sh serve`, receives GitLab MR webhooks and queues runs in PostgreSQL.
- Background loops dispatch each run as a Kubernetes Job running `pergent agent`, receive the Job's result on a one-time-token callback, and publish it to GitLab with the bot tokens, which never enter a Job.
- Configuration is one YAML file plus env vars for secrets, shipped by a Helm chart.

**Tech Stack:**
- Go 1.24, `net/http` (1.22 routing patterns) and `log/slog`.
- PostgreSQL 13+ through `github.com/jackc/pgx/v5`.
- `k8s.io/client-go` / `k8s.io/api` / `k8s.io/apimachinery` v0.33.x, and `gopkg.in/yaml.v3`.
- Helm 3.

**Spec:** there's no design doc for this slice; the user chose to skip it. The plan argues from:
- `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md` (this repo), piece 2 and its agent-mode contract list;
- the pergent repo's `docs/superpowers/specs/2026-10-29-pergent-agent-mode-design.md` §1–§8, especially §8, "What pergent.sh does with it";
- the pergent repo's `docs/agent-result-schema.md` and README "🤖 Agent mode";
- the decisions below.

## Decisions (agreed 2026-10-30)

- **Scope of 2a.** GitLab only, no UI, no MR commands, no approval advice. GitHub is 2b, the dashboard 2c, commands 2d, advice 2e.
- **Repository on/off.** A repository is on when it is listed in the config file. Removing it switches it off: its webhook and deploy token are deleted on the next reconcile.
- **Nothing reviewed (Q1).** When every skill is `no_blocking_issues` but no skill reviewed any file (every changed file was default-skipped):
  - the bot does **not** approve;
  - the commit status is `success` with "Nothing to review: every changed file is skipped";
  - the comment gets a note listing the skipped files and their rules.
- **Platforms (Q2).** Any GitLab instance by URL, gitlab.com included.
- **Builder calls made in this plan (the user can overturn any):**
  - **Team instructions.** The root `AGENTS.md`, or `CLAUDE.md` when there is no `AGENTS.md`, plus `.pergent/instructions.md`, read at the MR's `diff_refs.start_sha`. Files are read at that SHA, the target branch's tip when the MR was computed, rather than at the branch name, so the inputs are fixed for a run.
  - **Repository skills.** These live in `.pergent/skills/` (`*.md` and `<name>/SKILL.md`) at the same SHA. A repository skill wins over a library skill of the same name.
  - **Library skills.** A directory mounted into pergent.sh, in the same layout. A repository's `skills:` list picks from it; an empty list means all of them.
  - **Neutral on GitLab.** GitLab has no neutral commit status, so `failure_status: neutral` maps to GitLab's `skipped` state. GitLab can count skipped pipelines as successful through a project setting; `failed` cannot be.
  - **Clone token.** A GitLab deploy token with `read_repository`, created per project by the review bot. It is stored AES-256-GCM encrypted, expires after 30 days, is replaced when fewer than 7 days remain, and is revoked on switch-off. The review bot therefore needs Maintainer.
  - **No repository cache in 2a.** Every checkout is `git init` plus fetches, so the cache-mount rules of the contract do not apply yet.
  - **Unapprove on push.** When an MR gets a new head after the bot approved an older one, the approve bot unapproves at once, whether or not GitLab resets approvals itself.
  - **Init container.** It uses the pinned pergent image, which has git and a shell. Both containers run as the same user, so no `safe.directory` is needed.
  - **Postgres is external.** The chart takes `DATABASE_URL` and doesn't bundle a database. The Deployment runs 1 replica.

## Global Constraints

- **Public repository.**
  - No employer, internal hostnames, internal repositories or people in code, docs, tests or commits.
  - Examples use `gitlab.example.com`, `pergent.example.com` and `acme/widgets`.
- **Image pinning.** The pergent image is pinned by tag or digest: `jobs.image` without a tag, or with tag `latest`, is a config error. The default is `ghcr.io/gopaytech/pergent:v0.1.0`.
- **Tokens that can comment, approve or write never enter a Job, a Job's Secret, a log line or a commit message.**
- **Per-Job secrets.**
  - One Secret per Job, owned by the Job.
  - The init container gets only the git token.
  - The pergent container gets only `OPENCODE_API_KEY` and `PERGENT_RESULT_TOKEN`.
- **The git token never lands on disk.** It goes only through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` (`http.extraHeader`), never in a URL or `.git/config`.
- **Inert checkout.** `core.hooksPath=/dev/null` and an invalid push URL.
- **A failed, partial or missing review is never `success` and never approved.**
  - Unknown verdicts and unknown statuses are treated as failures.
- **Result tokens.** Random 32 bytes, stored as SHA-256, single-use, expiring at the Job deadline + 60 s.
- **Job shape.** `backoffLimit: 0`, `restartPolicy: Never`, `terminationGracePeriodSeconds: 30` and `ttlSecondsAfterFinished: 3600`.
  - `automountServiceAccountToken: false` and `enableServiceLinks: false`.
  - Containers: `allowPrivilegeEscalation: false`, capabilities `drop: [ALL]`.
- **Deadline.** `activeDeadlineSeconds` = `jobs.timeout` + 120 + clone allowance, where the clone allowance is `max(120 s, 2 × last measured checkout time for the project)`.
- **One pergent comment per MR.** Updated in place, always containing `<!-- pergent -->`. The commit status name is `pergent`.
- **pergent agent flags used:** `--repo-path=/work/repo --base --head --skills-dir=/in/skills [--instructions=/in/instructions.md] [--previous=/in/previous.json] --scrub-instructions --timeout --concurrency --max-comment-size=990000 --result-url`.
- **Every webhook must be authenticated.** It is compared in constant time against the instance's secret; a mismatch gets 401 and is otherwise ignored.
- **Tests.**
  - Store and end-to-end tests need `PERGENT_SH_TEST_DATABASE_URL` and skip without it. CI provides `postgres:16`.
  - All other tests are hermetic: `httptest` for GitLab, the client-go fake clientset for Kubernetes.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **An MR edit that isn't a push** (title, label, description, assignee) must not start a run. The webhook test covers it.
2. **Two pushes in quick succession.** Only the newest head is reviewed and can be approved. The older Job is deleted, and its late result is accepted and discarded, never posted. The engine test covers it.
3. **The head moves between the result and the approval.** No approval; the status is still set on the result's own SHA. The publisher test covers it.
4. **A Job that dies without a result** (init fetch failure, OOMKilled, image pull failure, exit 4, a deleted Job). An error comment and a non-success status follow within about 2 minutes, never silence. The watcher test covers it.
5. **Inputs over the ConfigMap limit.** `previous.json` is dropped first; if the inputs are still over, the run fails with a comment saying so and no Job starts. The inputs test covers it.

---

## File Structure

```
cmd/pergent-sh/main.go           serve: load config, migrate, reconcile, start loops + HTTP
internal/config/                 YAML config, env-var secrets, validation
internal/store/                  pgx store, embedded SQL migrations, run lifecycle
internal/secretbox/              AES-256-GCM seal/open for stored deploy tokens
internal/gitlab/                 REST client: MRs, files, tree, notes, statuses, approvals, hooks, deploy tokens
internal/result/                 pergent agent result v1 (the fields pergent.sh reads), Parse, Trim
internal/policy/                 Decide: outcome → comment, status, approve (pure)
internal/inputs/                 library skills, repo skills, instructions, previous; size fitting
internal/kube/                   BuildJob/ConfigMap/Secret (pure), Launcher (create/delete/inspect)
internal/webhook/                GitLab MR webhook handler
internal/callback/               POST /runs/{id}/result
internal/engine/                 Dispatch/Publish/Cleanup/Watch loops, Reconcile, e2e test
deploy/helm/pergent-sh/          chart
Dockerfile, Makefile, .github/workflows/{ci,release}.yml, README.md
```

---

### Task 1: Module, config and build scaffolding

**Files:**
- Create: `go.mod` (module `github.com/gopaytech/pergent.sh`, go 1.24), `Makefile` (`build` → `bin/pergent-sh`, `test`, `vet`, `clean`), `.gitignore` (`bin/`), `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/testdata/valid.yaml`

**Interfaces:**
- Produces:
  ```go
  type FailureStatus string // "failed" | "neutral"
  type Config struct {
      Listen      string        `yaml:"listen"`       // default ":8080"
      PublicURL   string        `yaml:"public_url"`   // required, https URL, no trailing slash
      CallbackURL string        `yaml:"callback_url"` // default PublicURL
      SkillsDir   string        `yaml:"skills_dir"`   // required
      GitLab      []Instance    `yaml:"gitlab"`
      Repositories []Repository `yaml:"repositories"`
      LLM         LLM           `yaml:"llm"`
      Jobs        Jobs          `yaml:"jobs"`
  }
  type Instance struct { Name, URL, ReviewTokenEnv, ApproveTokenEnv, WebhookSecretEnv string } // yaml snake_case
  type Repository struct { GitLab, Project string; Skills []string; FailureStatus FailureStatus } // FailureStatus default "failed"
  type LLM struct { Provider, Model, BaseURL, APIKeyEnv string } // APIKeyEnv default "OPENCODE_API_KEY"
  type Jobs struct {
      Namespace   string                      // required
      Image       string                      // default "ghcr.io/gopaytech/pergent:v0.1.0"
      MaxRunning  int                         // default 5, ≥1
      Timeout     time.Duration               // default 10m, yaml "10m"
      Concurrency int                         // default 4, ≥1
      Resources   corev1.ResourceRequirements // default requests cpu 500m mem 1Gi, limits cpu 2 mem 4Gi
  }
  func Load(path string) (*Config, error)            // parse, defaults, Validate
  func (c *Config) Validate() error
  func (c *Config) Instance(name string) (Instance, bool)
  func (c *Config) Repository(instance, project string) (Repository, bool)
  func (i Instance) Secrets() (review, approve, webhook string, err error) // reads the named env vars; empty → error naming the var
  ```

- [ ] **Step 1: Write failing tests** in `config_test.go`:
  - `TestLoadValid`: loads `testdata/valid.yaml` (one instance `main`, `https://gitlab.example.com`, one repo `acme/widgets`) and asserts the defaults: `Listen==":8080"`, `CallbackURL==PublicURL`, `Jobs.Image=="ghcr.io/gopaytech/pergent:v0.1.0"`, `Jobs.MaxRunning==5`, `Jobs.Timeout==10*time.Minute`, the repo's `FailureStatus=="failed"`, `LLM.APIKeyEnv=="OPENCODE_API_KEY"`.
  - `TestValidateRejects`: a table with one mutation each, each expecting an error that contains the quoted text:
    - image `ghcr.io/gopaytech/pergent` gives "pinned";
    - image `…:latest` gives "pinned";
    - `failure_status: maybe` gives "failure_status";
    - a repo naming an unknown instance gives "unknown gitlab instance";
    - a duplicate repo gives "duplicate repository";
    - a duplicate instance name gives "duplicate gitlab instance";
    - an empty `public_url` gives "public_url";
    - `public_url: http://…` gives "https";
    - a missing `jobs.namespace` gives "namespace";
    - an instance name outside `^[a-z0-9-]+$` gives "instance name";
    - `max_running: 0` gives "max_running".
  - `TestImageDigestAllowed`: `ghcr.io/gopaytech/pergent@sha256:<64 hex>` validates.
  - `TestSecretsMissingEnv`: `Secrets()` with an unset `ReviewTokenEnv` returns an error naming that env var.
- [ ] **Step 2:** `go test ./internal/config/` must FAIL (package missing).
- [ ] **Step 3: Implement** `config.go`. `time.Duration` is decoded from strings through a small `duration` type with `UnmarshalYAML`. Use `yaml.v3` `KnownFields(true)` so typos are errors.
- [ ] **Step 4:** `go test ./internal/config/ && go vet ./...` must PASS.
- [ ] **Step 5: Commit:** `feat: config file with pinned pergent image and env-var secrets`.

---

### Task 2: Store and migrations

**Files:**
- Create: `internal/store/store.go`, `internal/store/runs.go`, `internal/store/projects.go`, `internal/store/migrations/0001_init.sql`, `internal/store/store_test.go`

**Interfaces:**
- Produces:
  ```go
  type MRKey struct { Instance string; ProjectID, IID int64 }
  type RunState string
  const (Queued, Running, Reported, Published, PublishFailed, Superseded RunState = "queued","running","reported","published","publish_failed","superseded")
  type Run struct {
      ID string; MR MRKey; ProjectPath, HeadSHA, BaseSHA, StartSHA, TargetBranch string
      State RunState; JobName string; TokenExpiresAt time.Time
      Result []byte; FailureReason string; Attempts int
      CreatedAt, DispatchedAt, ReportedAt, NotBefore time.Time; CleanedUp bool
  }
  type ResultOutcome int
  const (ResultStored ResultOutcome = iota; ResultDiscarded; ResultConflict; ResultUnauthorized)
  type Project struct { Instance string; ProjectID int64; Path, HTTPURLToRepo string; HookID, DeployTokenID int64
                        DeployTokenUsername string; DeployTokenSealed []byte; DeployTokenExpiresAt time.Time; CloneSeconds int }
  type StaleApproval struct { MR MRKey; ApprovedSHA string }

  func Open(ctx context.Context, url string) (*Store, error)
  func (s *Store) Close()
  func (s *Store) Migrate(ctx context.Context) error
  func (s *Store) EnqueueRun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch string) (id string, created bool, err error)
  func (s *Store) SupersedeMR(ctx context.Context, mr MRKey) (int, error)
  func (s *Store) CountRunning(ctx context.Context) (int, error)
  func (s *Store) ClaimQueued(ctx context.Context, limit int, now time.Time) ([]Run, error)
  func (s *Store) Requeue(ctx context.Context, id string, notBefore time.Time) error // Attempts++
  func (s *Store) MarkDispatched(ctx context.Context, id, jobName, baseSHA, startSHA string, tokenHash []byte, expires, now time.Time) error
  func (s *Store) StoreResult(ctx context.Context, id string, token string, body []byte, now time.Time) (ResultOutcome, error)
  func (s *Store) ReportFailure(ctx context.Context, id, reason string, now time.Time) (bool, error)
  func (s *Store) RunsToPublish(ctx context.Context, now time.Time, limit int) ([]Run, error)
  func (s *Store) MarkPublished(ctx context.Context, id string) error
  func (s *Store) PublishRetry(ctx context.Context, id, errText string, notBefore time.Time) error // Attempts++; at 10 → publish_failed
  func (s *Store) RunsToCleanUp(ctx context.Context) ([]Run, error)
  func (s *Store) MarkCleanedUp(ctx context.Context, id string) error
  func (s *Store) RunningRuns(ctx context.Context) ([]Run, error)
  func (s *Store) PreviousResult(ctx context.Context, mr MRKey, beforeRunID string) ([]byte, bool, error)
  func (s *Store) NoteID(ctx context.Context, mr MRKey) (int64, error)            // 0 when none
  func (s *Store) SetNoteID(ctx context.Context, mr MRKey, noteID int64) error
  func (s *Store) SetApprovedSHA(ctx context.Context, mr MRKey, sha string) error  // "" clears
  func (s *Store) StaleApprovals(ctx context.Context) ([]StaleApproval, error)
  func (s *Store) Project(ctx context.Context, instance string, projectID int64) (Project, bool, error)
  func (s *Store) Projects(ctx context.Context) ([]Project, error)
  func (s *Store) UpsertProject(ctx context.Context, p Project) error
  func (s *Store) DeleteProject(ctx context.Context, instance string, projectID int64) error
  func (s *Store) SetCloneSeconds(ctx context.Context, instance string, projectID int64, seconds int) error
  ```

Schema (`0001_init.sql`):
- Three tables: `runs`, `merge_requests` (PK `(instance, project_id, mr_iid)`; `note_id`, `approved_sha`) and `projects`, with columns matching the structs.
- `runs.id uuid DEFAULT gen_random_uuid()`; `runs.result jsonb`; `runs.token_hash bytea`.
- A partial unique index on `runs(instance, project_id, mr_iid, head_sha) WHERE state <> 'superseded'`.
- `schema_migrations(version int)`. `Migrate` applies embedded files in name order, each in a transaction, with `pg_advisory_xact_lock`.

Semantics the tests pin:
- `EnqueueRun`, in one transaction:
  - marks every `queued`/`running` run of the same MR with a different head `superseded`;
  - inserts a new `queued` run unless a non-superseded run for this head exists, in which case `created=false` and that run's ID is returned.
- `ClaimQueued` takes `state='queued' AND not_before<=now` rows, oldest first, `FOR UPDATE SKIP LOCKED`, sets them `running`.
- `StoreResult`:
  - an unknown ID, a hash mismatch (constant-time) or `now > token_expires_at` gives `ResultUnauthorized`;
  - a superseded run gives `ResultDiscarded`, and the token is cleared;
  - a run already holding a result or failure reason gives `ResultConflict`;
  - otherwise the result is stored, the state becomes `reported`, the token is cleared, and the outcome is `ResultStored`.
- `ReportFailure` only moves a `queued`/`running` run with no result to `reported`; the bool reports whether it did.
- `PreviousResult` returns the result of the latest `published` run of the MR whose result has `"status":"completed"`, other than `beforeRunID`.
- `StaleApprovals` returns MRs whose `approved_sha` is non-empty and differs from the head of their newest non-superseded run.

- [ ] **Step 1: Write failing tests**, all skipping without `PERGENT_SH_TEST_DATABASE_URL`. Each test creates a fresh schema with `CREATE SCHEMA test_<random>` and `search_path`.
  - `TestEnqueueSupersedesOlderHead`: enqueue head A, then head B, on the same MR. A is `superseded`, B is `queued`.
  - `TestEnqueueSameHeadIsIdempotent`: enqueuing head A twice gives the same ID, with `created=false` the second time.
  - `TestClaimQueuedRespectsNotBeforeAndLimit`.
  - `TestStoreResultOutcomes`: one row per outcome:
    - Stored;
    - a second post gives Conflict;
    - the wrong token gives Unauthorized;
    - an expired token gives Unauthorized;
    - a superseded run gives Discarded;
    - a run after `ReportFailure` gives Conflict.
  - `TestPublishRetryGivesUpAtTen`: after 10 calls the state is `publish_failed`.
  - `TestPreviousResultSkipsErrorsAndCurrent`.
  - `TestStaleApprovals`: approved A, new run for B gives one stale approval; approved B gives none.
  - `TestMigrateTwice`: the second `Migrate` is a no-op.
- [ ] **Step 2:** `PERGENT_SH_TEST_DATABASE_URL=postgres://… go test ./internal/store/` must FAIL.
- [ ] **Step 3: Implement.** `StoreResult` hashes the presented token with SHA-256 and compares with `subtle.ConstantTimeCompare`, inside `SELECT … FOR UPDATE`.
- [ ] **Step 4:** the tests must PASS, and also PASS without the env var (they skip).
- [ ] **Step 5: Commit:** `feat: postgres store for runs, merge requests and projects`.

---

### Task 3: Secretbox and GitLab client

**Files:**
- Create: `internal/secretbox/secretbox.go`, `internal/secretbox/secretbox_test.go`, `internal/gitlab/client.go`, `internal/gitlab/types.go`, `internal/gitlab/client_test.go`

**Interfaces:**
- Produces:
  ```go
  // secretbox
  func New(keyBase64 string) (*Box, error) // 32-byte key, std base64; else error
  func (b *Box) Seal(plain []byte) []byte  // nonce || ciphertext
  func (b *Box) Open(sealed []byte) ([]byte, error)

  // gitlab
  type StatusState string // "running","success","failed","canceled","skipped"
  type DiffRefs struct { BaseSHA, HeadSHA, StartSHA string }
  type MergeRequest struct { IID int64; State, SHA, TargetBranch string; DiffRefs DiffRefs }
  type Project struct { ID int64; PathWithNamespace, HTTPURLToRepo string }
  type TreeEntry struct { Name, Path, Type string } // Type "blob" | "tree"
  type Hook struct { ID int64; URL string }
  type DeployToken struct { ID int64; Username, Token string; ExpiresAt time.Time }
  type User struct { ID int64; Username string }
  type APIError struct { Status int; Body string } // Error() truncates Body to 200 bytes
  var ErrNotFound, ErrHeadMoved error

  func New(baseURL, token string, hc *http.Client) *Client
  func (c *Client) CurrentUser(ctx) (User, error)
  func (c *Client) GetProject(ctx, path string) (Project, error)
  func (c *Client) GetMergeRequest(ctx, projectID, iid int64) (MergeRequest, error)
  func (c *Client) GetFile(ctx, projectID int64, path, ref string) ([]byte, bool, error) // 404 → nil,false,nil
  func (c *Client) ListTree(ctx, projectID int64, path, ref string) ([]TreeEntry, error) // 404 → nil,nil; follows X-Next-Page
  func (c *Client) FindNote(ctx, projectID, iid int64, authorID int64, marker string) (int64, bool, error)
  func (c *Client) CreateNote(ctx, projectID, iid int64, body string) (int64, error)
  func (c *Client) UpdateNote(ctx, projectID, iid, noteID int64, body string) error // 404 → ErrNotFound
  func (c *Client) SetCommitStatus(ctx, projectID int64, sha string, state StatusState, description string) error // name "pergent"
  func (c *Client) Approve(ctx, projectID, iid int64, sha string) error // 409 → ErrHeadMoved
  func (c *Client) Unapprove(ctx, projectID, iid int64) error           // 404 → nil
  func (c *Client) EnsureHook(ctx, projectID int64, url, secret string) (int64, error)
  func (c *Client) DeleteHook(ctx, projectID, hookID int64) error         // 404 → nil
  func (c *Client) CreateDeployToken(ctx, projectID int64, name string, expires time.Time) (DeployToken, error) // scopes [read_repository]
  func (c *Client) DeleteDeployToken(ctx, projectID, tokenID int64) error // 404 → nil
  ```
- REST paths are under `/api/v4`. A file path is URL-escaped as one segment, with `/` as `%2F`. The token goes in the `PRIVATE-TOKEN` header.
- 429, 5xx and connection errors are retried 3 times (1 s, 2 s, 4 s; a `Retry-After` up to 30 s is honored), within the context.
- `SetCommitStatus` truncates the description to 255 bytes on a UTF-8 boundary.
- `EnsureHook` lists hooks, then edits the one with the same URL or creates one, with `merge_requests_events: true`, `push_events: false`, `enable_ssl_verification: true` and `token: secret`.

- [ ] **Step 1: Write failing tests:**
  - `TestBoxRoundTrip`.
  - `TestBoxRejectsShortKey`.
  - `TestBoxOpenTampered`, which gives an error.
  - One `httptest` test per client method, asserting the method, path, query, body and `PRIVATE-TOKEN` header:
    - `GetFile` for `.pergent/instructions.md` requests `/api/v4/projects/7/repository/files/.pergent%2Finstructions.md/raw?ref=<sha>`;
    - `Approve` sends `{"sha":"<sha>"}` and maps 409 to `ErrHeadMoved`;
    - `SetCommitStatus` sends `state`, `name=pergent` and `description`.
  - `TestRetryOn503ThenSuccess`.
  - `TestNoRetryOn400`.
  - `TestListTreePaginates`: two pages via `X-Next-Page`.
  - `TestFindNoteMatchesAuthorAndMarker`: notes by another author that contain the marker are ignored.
  - `TestAPIErrorNeverContainsToken`: an error from a 500 response doesn't include the token string.
- [ ] **Step 2:** `go test ./internal/secretbox/ ./internal/gitlab/` must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: gitlab client and secretbox`.

---

### Task 4: Result parsing and approval policy

**Files:**
- Create: `internal/result/result.go`, `internal/result/result_test.go`, `internal/policy/policy.go`, `internal/policy/policy_test.go`

**Interfaces:**
- Produces:
  ```go
  // result: only the fields pergent.sh reads; unknown fields are ignored
  type Result struct {
      SchemaVersion int; Status string; PergentVersion, Base, Head string
      Skills []Skill; Error *Error; Comment Comment
  }
  type Skill struct { Name, Verdict string; Findings []json.RawMessage; Files Files }
  type Files struct { Reviewed []string; Skipped []Skipped; NotReviewed []json.RawMessage }
  type Skipped struct { Path, Rule string }
  type Error struct { Code, Message string }
  type Comment struct { Markdown, FindingsBlock string } // json "markdown","findings_block"
  func Parse(data []byte) (*Result, error) // schema_version must be 1; status "completed"|"error"; comment.markdown non-empty; completed needs ≥1 skill
  func Trim(data []byte) ([]byte, error)   // {"schema_version":1,"status":"completed","skills":[{"name":…,"findings":[…]}]}

  // policy
  const Marker = "<!-- pergent -->"
  type Status string // "success" | "failed" | "neutral"
  type Outcome struct { Result *result.Result; FailureReason string }
  type Decision struct { Comment string; Status Status; Description string; Approve bool }
  func Decide(o Outcome, failure config.FailureStatus) Decision
  func ErrorComment(reason string) string // Marker + "\n⚠️ **pergent couldn't review this MR:** " + reason + "."
  ```

`Decide` rules, in order:

| Case | Status | Description | Approve | Comment |
|---|---|---|---|---|
| `FailureReason != ""` | failure setting | `pergent couldn't review this MR` | no | `ErrorComment(reason)` |
| `Result.Status == "error"` | failure setting | `pergent couldn't review this MR` | no | `Result.Comment.Markdown` |
| any skill `has_blocking_issues` | `failed` | `Blocking issues found` | no | markdown |
| any skill `failed` or an unknown verdict | failure setting | `Review failed for N of M skills` | no | markdown |
| all `no_blocking_issues`, no file reviewed by any skill | `success` | `Nothing to review: every changed file is skipped` | no | markdown + nothing-reviewed note |
| all `no_blocking_issues` | `success` | `No blocking issues` | **yes** | markdown |

- The failure setting is `failed` → `StatusFailed`, `neutral` → `StatusNeutral`.
- Every returned comment contains `Marker`; prepend `Marker + "\n"` when it is absent.
- The nothing-reviewed note:

  ```
  ---
  **Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look.

  - `go.sum` (lockfile)
  ```

  - It lists the distinct skipped paths across skills with their rules, sorted, at most 20, then `- …and N more`.
  - It is inserted before `FindingsBlock` when the markdown ends with it, and appended otherwise.

- [ ] **Step 1: Write failing tests:**
  - `TestParse`: the two examples from pergent's `docs/agent-result-schema.md`, completed and error, parse correctly. Schema version 2 is rejected. Missing `comment.markdown` is rejected. Unknown fields are accepted.
  - `TestTrimKeepsNamesAndFindingsOnly`.
  - `TestDecide`: one table row per rule above, plus:
    - mixed blocking and failed gives `failed` / `Blocking issues found`;
    - verdict `"weird"` is treated as failed;
    - a failure setting of `neutral` gives `StatusNeutral` for the failed rows;
    - the marker is prepended when missing;
    - the note goes before the findings block;
    - 25 skipped files give 20 listed plus `…and 5 more`.
- [ ] **Step 2:** `go test ./internal/result/ ./internal/policy/` must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: agent result parsing and the approval policy`.

---

### Task 5: Review inputs

**Files:**
- Create: `internal/inputs/inputs.go`, `internal/inputs/frontmatter.go`, `internal/inputs/inputs_test.go`, `internal/inputs/testdata/library/{code-review.md,security/SKILL.md}`

**Interfaces:**
- Consumes: `gitlab.TreeEntry` and `config.Repository`.
- Produces:
  ```go
  type Source interface {
      GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error)
      ListTree(ctx context.Context, projectID int64, path, ref string) ([]gitlab.TreeEntry, error)
  }
  type Library map[string][]byte
  func LoadLibrary(dir string) (Library, error) // same layout rules as pergent's LoadDir: *.md, <name>/SKILL.md (folder without it → error), dot-entries skipped, symlinks followed; duplicate names → error
  func SkillName(content []byte, fallback string) (string, error) // frontmatter `name` else fallback; must match ^[A-Za-z0-9][A-Za-z0-9._-]*$
  type Inputs struct {
      Skills       map[string][]byte // name → content
      Instructions []byte            // nil when none
      Previous     []byte            // nil when none
      Overridden   []string          // library skills replaced by repo skills, sorted
  }
  func Resolve(ctx context.Context, src Source, lib Library, repo config.Repository, projectID int64, ref string, previous []byte) (*Inputs, error)
  func (in *Inputs) Size() int
  func (in *Inputs) Fit(limit int) error // over limit → drop Previous; still over → ErrTooLarge
  var ErrTooLarge error
  const ConfigMapLimit = 900 * 1024
  ```
- Instructions are assembled from these sections, in order, each only when its file exists:
  - `# Project instructions (AGENTS.md)\n\n<content>\n`, or the same heading with `CLAUDE.md` when there is no `AGENTS.md`;
  - `\n# Team review instructions (.pergent/instructions.md)\n\n<content>\n`.
- A repository skill error, such as a bad name, a folder without `SKILL.md` or a duplicate, is returned as an error naming the repository path. The caller turns it into a failed run, so the review is never skipped silently.
- `Resolve` validates that every name in `repo.Skills` exists in `lib` and returns an error otherwise.

- [ ] **Step 1: Write failing tests** with a fake `Source` backed by a map:
  - `TestLoadLibrary`.
  - `TestResolveAllLibraryWhenListEmpty`.
  - `TestResolveSelectedLibrary`.
  - `TestRepoSkillOverridesLibrary`: `Overridden == ["code-review"]`, and the content is the repo's.
  - `TestRepoSkillFolderWithoutSkillMD`: an error naming `.pergent/skills/foo`.
  - `TestInstructionsCombined`.
  - `TestInstructionsClaudeFallback`.
  - `TestNoInstructionsIsNil`.
  - `TestSkillNameRejectsSlash`.
  - `TestFitDropsPreviousFirst`.
  - `TestFitTooLarge`.
  - `TestUnknownLibrarySkill`.
- [ ] **Step 2:** `go test ./internal/inputs/` must FAIL.
- [ ] **Step 3: Implement.** Frontmatter is a leading `---\n…\n---` block parsed with yaml.v3; only `name` is read.
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: resolve library skills, repo skills and team instructions`.

---

### Task 6: Kubernetes Job building, launching and inspecting

**Files:**
- Create: `internal/kube/job.go`, `internal/kube/launcher.go`, `internal/kube/inspect.go`, `internal/kube/checkout.sh` (embedded), `internal/kube/job_test.go`, `internal/kube/launcher_test.go`, `internal/kube/inspect_test.go`

**Interfaces:**
- Consumes: `inputs.Inputs` and `config.LLM`.
- Produces:
  ```go
  type JobParams struct {
      Name, Namespace, RunID, Image string
      Timeout, CloneAllowance time.Duration; Concurrency int
      RepoURL string; MRIID int64; TargetBranch, BaseSHA, HeadSHA, GitUsername string
      ResultURL string; LLM config.LLM; Resources corev1.ResourceRequirements
      Inputs *inputs.Inputs
  }
  type Secrets struct { GitToken, LLMKey, ResultToken string }
  func JobName(runID string) string                 // "pergent-review-" + first 12 hex chars of runID without dashes
  func Deadline(timeout, cloneAllowance time.Duration) time.Duration // timeout + 2m + cloneAllowance
  func CloneAllowance(lastCloneSeconds int) time.Duration            // max(2m, 2×last)
  func BuildJob(p JobParams) *batchv1.Job
  func BuildConfigMap(p JobParams, owner *batchv1.Job) *corev1.ConfigMap // name p.Name+"-inputs"
  func BuildSecret(p JobParams, owner *batchv1.Job, s Secrets) *corev1.Secret // name p.Name+"-secrets"; keys git-token, llm-api-key, result-token
  type Launcher struct { Client kubernetes.Interface; Namespace string }
  func (l *Launcher) Launch(ctx context.Context, p JobParams, s Secrets) error
  func (l *Launcher) Delete(ctx context.Context, name string) error // NotFound → nil; background propagation
  type Observation struct { Exists, Finished bool; FinishedAt time.Time; Failure, Stuck string; Clone time.Duration }
  func (l *Launcher) Inspect(ctx context.Context, name string, now time.Time) (Observation, error)
  ```

**Job shape.** Labels on the Job and pod template: `app.kubernetes.io/managed-by: pergent-sh` and `pergent.sh/run: <runID>`. The rest is in Global Constraints.

**Init container `checkout`.**
- It runs `sh -ec` with the embedded `checkout.sh`.
- Env: `REPO_URL`, `MR_IID`, `TARGET_BRANCH`, `BASE_SHA`, `HEAD_SHA`, `GIT_USERNAME`, and `GIT_TOKEN` from the Secret key `git-token`.
- It mounts `work` at `/work`.
- The script must be exactly this, because it is security-critical:

```sh
set -eu
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=http.extraHeader
export GIT_CONFIG_VALUE_0="Authorization: Basic $(printf '%s:%s' "$GIT_USERNAME" "$GIT_TOKEN" | base64 -w0)"
git init -q /work/repo
cd /work/repo
git config core.hooksPath /dev/null
git remote add origin "$REPO_URL"
git remote set-url --push origin pergent-push-disabled:
if ! git fetch -q --no-tags --depth=1 origin "$BASE_SHA" "$HEAD_SHA"; then
  echo "fetch by SHA failed; falling back to refs/merge-requests/$MR_IID/head and $TARGET_BRANCH" >&2
  git fetch -q --no-tags --depth=1 origin "refs/merge-requests/$MR_IID/head"
  depth=50
  until git cat-file -e "$BASE_SHA^{commit}" 2>/dev/null; do
    if [ "$depth" -gt 3200 ]; then echo "merge base $BASE_SHA not found within depth 3200" >&2; exit 1; fi
    git fetch -q --no-tags --depth="$depth" origin "$TARGET_BRANCH"
    depth=$((depth * 2))
  done
fi
git checkout -q --detach "$HEAD_SHA"
```

**Main container `pergent`.**
- Args: the flag list from Global Constraints. `--instructions` and `--previous` are present only when the inputs have them.
- Env: `OPENCODE_PROVIDER` and `OPENCODE_MODEL`; `OPENCODE_BASE_URL` only if set; `OPENCODE_API_KEY` from the Secret key `llm-api-key`; `PERGENT_RESULT_TOKEN` from `result-token`.
- It mounts `work` at `/work` and `inputs` at `/in`, read-only.

**ConfigMap.**
- Keys: `skill-<n>.md` (n = index in sorted name order), `instructions.md` and `previous.json`.
- The volume `items` map `skill-<n>.md` to `skills/<name>.md` and the others to their own names.

**`Launch`.**
1. Create the Job with `suspend: true`.
2. Create the Secret and ConfigMap with an `ownerReference` to the Job's UID.
3. Patch `spec.suspend=false`.
4. On any error, delete the Job and return the error.

**`Inspect`.** Reads the Job and its pods (label `job-name=<name>`).
- `Failure` texts:
  - init container exit ≠ 0: `couldn't fetch the merge request's commits (checkout exited with code N)`;
  - `OOMKilled`: `the review job ran out of memory (OOMKilled)`;
  - pergent exit 4: `the review job couldn't deliver its result (exit code 4)`;
  - Job condition `DeadlineExceeded`: `the review job hit its deadline (DeadlineExceeded)`;
  - other failure: `the review job failed (<reason>)`;
  - a Complete Job leaves `Failure` empty.
- `Stuck`: `couldn't start the review job (<reason>)` when a pod waits with `ErrImagePull`, `ImagePullBackOff`, `InvalidImageName` or `CreateContainerConfigError` and was created more than 5 minutes before `now`.
- `Clone`: the init container's `finishedAt − startedAt`.

- [ ] **Step 1: Write failing tests:**
  - `TestBuildJobShape`: asserts every Global-Constraint field, including:
    - `automountServiceAccountToken=false` and `enableServiceLinks=false`;
    - the security context;
    - `activeDeadlineSeconds == 600+120+120` for `Timeout=10m` and `CloneAllowance=2m`.
  - `TestSecretsSplitPerContainer`: the init container's env references only `git-token`; the pergent container's env references only `llm-api-key` and `result-token`; no literal token value appears anywhere in the Job object (marshal it and search for the strings).
  - `TestCheckoutScriptHasNoTokenInURL`: `REPO_URL` has no userinfo, and the script contains no `git config` of the token.
  - `TestArgsOptionalInputs`: no instructions and no previous means no flags for them.
  - `TestConfigMapItems`.
  - `TestCloneAllowance`: 0 gives 2m, 90 gives 3m.
  - `TestLaunchOrder`: with the fake clientset, after `Launch` the Job is unsuspended and the Secret and ConfigMap carry an ownerRef with the Job UID.
  - `TestLaunchCleansUpOnSecretError`: a reactor fails Secret creation, and the Job is gone afterwards.
  - `TestInspectReasons`: a table over pod and Job statuses, one row per text above, plus "Job not found" giving `Exists=false`.
- [ ] **Step 2:** `go test ./internal/kube/` must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: build, launch and inspect review jobs`.

---

### Task 7: GitLab webhook handler

**Files:**
- Create: `internal/webhook/gitlab.go`, `internal/webhook/gitlab_test.go`, `internal/webhook/testdata/{mr_open.json,mr_push.json,mr_title_edit.json,mr_close.json,mr_merge.json}`

**Interfaces:**
- Consumes: `store.MRKey`, `config.Config` and `config.Instance.Secrets`.
- Produces:
  ```go
  type Queue interface {
      EnqueueRun(ctx context.Context, mr store.MRKey, projectPath, headSHA, targetBranch string) (string, bool, error)
      SupersedeMR(ctx context.Context, mr store.MRKey) (int, error)
  }
  type Handler struct { Config *config.Config; Secrets map[string]string /* instance → webhook secret */; Queue Queue; Wake func(); Log *slog.Logger }
  func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) // mounted at POST /webhooks/gitlab/{instance}
  ```
- Rules:
  - An unknown instance gives 404.
  - An `X-Gitlab-Token` mismatch (constant-time) gives 401.
  - A body over 5 MiB gives 413.
  - An `X-Gitlab-Event` other than `Merge Request Hook` gives 200 and is ignored.
  - A project (`project.path_with_namespace`) not in config gives 200 and is ignored.
  - `object_attributes.action`:
    - `open` or `reopen` enqueues;
    - `update` enqueues only when `object_attributes.oldrev` is present (a push);
    - `close` or `merge` calls `SupersedeMR`;
    - anything else is ignored.
  - The head is `object_attributes.last_commit.id`.
  - After an enqueue with `created`, `Wake()` is called.
  - A storage error gives 500 so GitLab retries.

- [ ] **Step 1: Write failing tests**, with fixtures holding only the fields used and example names:
  - `TestOpenEnqueues`.
  - `TestPushEnqueues`.
  - `TestTitleEditIgnored`.
  - `TestCloseSupersedes`.
  - `TestMergeSupersedes`.
  - `TestBadTokenIs401`: the queue is not called.
  - `TestUnknownProjectIgnored`.
  - `TestOtherEventIgnored`.
  - `TestStoreErrorIs500`.
  - `TestWakeOnlyWhenCreated`.
- [ ] **Step 2:** `go test ./internal/webhook/` must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: gitlab merge request webhook`.

---

### Task 8: Result callback

**Files:**
- Create: `internal/callback/callback.go`, `internal/callback/callback_test.go`

**Interfaces:**
- Consumes: `store.StoreResult`, `store.ResultOutcome`, `store.ReportFailure` and `result.Parse`.
- Produces:
  ```go
  type Results interface {
      Run(ctx context.Context, id string) (store.Run, bool, error)
      StoreResult(ctx context.Context, id, token string, body []byte, now time.Time) (store.ResultOutcome, error)
      ReplaceWithFailure(ctx context.Context, id, reason string) error
  }
  type Handler struct { Results Results; Now func() time.Time; Wake func(); Log *slog.Logger }
  func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) // POST /runs/{id}/result
  ```
  Add `func (s *Store) Run(ctx, id) (Run, bool, error)` to the store in this task.
- Rules:
  1. A missing or non-`Bearer` Authorization header gives 401.
  2. A body over 11 MiB gives 413.
  3. Always call `StoreResult` with the raw body first, so the token is checked before anything else. A bad token must never be able to fail someone's run.
  4. `StoreResult` outcomes:
     - Unauthorized: 401;
     - Conflict: 409;
     - Discarded: 200;
     - Stored: go on to rule 5.
  5. After `Stored`:
     - If `result.Parse` fails, or its `Base`/`Head` differ from the run's, call `ReplaceWithFailure(ctx, id, "pergent sent a result pergent.sh couldn't use: <reason>")`. The reason for a SHA mismatch contains "different commit". Then `Wake()` and reply 400.
     - Otherwise `Wake()` and reply 200.
  6. Unknown IDs answer exactly like a bad token (401).
  7. The token is never logged.

  `ReplaceWithFailure` is a new store method: it clears `result`, sets `failure_reason`, and keeps the state `reported`. Add it to `Results`.

- [ ] **Step 1: Write failing tests**, with a fake `Results`:
  - `TestValidResultStored`: 200 and `Wake` called.
  - `TestMissingBearer401`.
  - `TestWrongToken401`: no failure is reported.
  - `TestDuplicate409`.
  - `TestSuperseded200`.
  - `TestTooLarge413`.
  - `TestHeadMismatchFailsRun`: 400, and the run is replaced with a failure whose reason contains "different commit".
  - `TestGarbageWithBadTokenIs401`.
  - `TestUnknownRun401`.
- [ ] **Step 2:** `go test ./internal/callback/` must FAIL.
- [ ] **Step 3: Implement**, plus the store method and its store test `TestReplaceWithFailure`.
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: one-time-token result callback`.

---

### Task 9: Engine — dispatch

**Files:**
- Create: `internal/engine/engine.go`, `internal/engine/dispatch.go`, `internal/engine/dispatch_test.go`, `internal/engine/fakes_test.go` (fake GitLab via `httptest`, fake clientset, real store)

**Interfaces:**
- Consumes: Tasks 1–6.
- Produces:
  ```go
  type Bots struct { Review, Approve *gitlab.Client; ReviewUser gitlab.User }
  type Engine struct {
      Store *store.Store; Config *config.Config; Bots map[string]Bots
      Launcher *kube.Launcher; Library inputs.Library; Box *secretbox.Box; LLMKey string
      Now func() time.Time; Log *slog.Logger; wake chan struct{}
  }
  func (e *Engine) Wake()
  func (e *Engine) DispatchOnce(ctx context.Context) error
  func (e *Engine) Supersede(ctx context.Context, id string) error // wraps store.Supersede(ctx, id), added in this task
  ```
- `DispatchOnce`:
  - Claim `MaxRunning − CountRunning` runs.
  - For each run:
    1. `GetMergeRequest`. If it is not `opened`, or its `SHA` ≠ the run's head, call `store.Supersede(ctx, id)` and move on. The newer push has its own run, and a closed MR needs none.
    2. Empty `DiffRefs.BaseSHA` (GitLab still computing): `Requeue` +10 s.
    3. Load the project row; `CloneAllowance(project.CloneSeconds)`.
    4. Read the deploy token: `Box.Open(project.DeployTokenSealed)`.
    5. Previous result: `PreviousResult(mr, run.ID)` → `result.Trim`.
    6. `inputs.Resolve` at `DiffRefs.StartSHA`, then `Fit(ConfigMapLimit)`.
    7. Mint the token: 32 random bytes, base64url without padding. Its expiry is `now + Deadline + 60s`.
    8. `Launcher.Launch` with `ResultURL = CallbackURL + "/runs/" + id + "/result"` and `RepoURL = project.HTTPURLToRepo`. Tests seed the project row with `UpsertProject`; Reconcile fills it in production (Task 11).
    9. `MarkDispatched`.
    10. `SetCommitStatus(head, running, "pergent review running")`.
  - Errors:
    - `inputs` errors and `ErrTooLarge` → `ReportFailure` with the error's text. `ErrTooLarge` uses `the skills and instructions for this repository are larger than 900 KiB`.
    - A missing deploy token → `ReportFailure("pergent.sh has no clone token for this project yet")`.
    - Other errors (GitLab or Kubernetes) → `Requeue` with backoff 10 s × 2^attempts. After 5 attempts, `ReportFailure("pergent.sh couldn't start the review: <err>")`.
  - A failure in one run never stops the others.

- [ ] **Step 1: Write failing tests** (real store; skip without the DB URL):
  - `TestDispatchLaunchesJob`: the fake clientset holds a Job named `JobName(id)` with the right `--base`, `--head` and `--result-url`; the run is `running` with a token hash; the fake GitLab received a `running` status.
  - `TestDispatchRespectsMaxRunning`.
  - `TestDispatchSupersedesMovedHead`.
  - `TestDispatchRequeuesWithoutBaseSHA`.
  - `TestDispatchRepoSkillErrorFailsRun`: the failure reason names the file.
  - `TestDispatchTooLargeFailsRun`.
  - `TestDispatchGitLabErrorRequeuesThenFails`.
- [ ] **Step 2:** the tests must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: dispatch queued runs as review jobs`.

---

### Task 10: Engine — publish, cleanup and stale approvals

**Files:**
- Create: `internal/engine/publish.go`, `internal/engine/publish_test.go`

**Interfaces:**
- Produces:
  ```go
  func (e *Engine) PublishOnce(ctx context.Context) error
  func (e *Engine) CleanupOnce(ctx context.Context) error
  func GitLabState(s policy.Status) gitlab.StatusState // success→success, failed→failed, neutral→skipped
  ```
- `PublishOnce`: for each run returned by `RunsToPublish(now, 20)`:
  1. `policy.Decide`, using `result.Parse(run.Result)` or `run.FailureReason`. A stored result that no longer parses is decided as a failure with `pergent sent a result pergent.sh couldn't use`.
  2. Upsert the comment:
     - with a stored `NoteID`, `UpdateNote`, which falls through to the next case on `ErrNotFound`;
     - otherwise `FindNote(ReviewUser.ID, policy.Marker)`, then update it or `CreateNote`;
     - then `SetNoteID`.
  3. `SetCommitStatus(run.HeadSHA, GitLabState(d.Status), d.Description)`.
  4. If `d.Approve`:
     - `GetMergeRequest`;
     - approve only if it is `opened` and `SHA == run.HeadSHA`, then `Approve(…, run.HeadSHA)`;
     - `ErrHeadMoved` and a moved head both skip approval without an error;
     - on success, `SetApprovedSHA(head)`.
  5. `MarkPublished`. Any error → `PublishRetry` with backoff 10 s × 2^attempts, capped at 10 min.
- `CleanupOnce`:
  - For `RunsToCleanUp`: `Launcher.Delete(JobName)` when a Job name is set; `SetCommitStatus(head, canceled, "Superseded by a newer push")` when the run was dispatched; then `MarkCleanedUp`.
  - Then, for each `StaleApprovals` entry: `Approve` bot `Unapprove`, then `SetApprovedSHA("")`.

- [ ] **Step 1: Write failing tests:**
  - `TestPublishApproves`: all clean → a note is created, status `success`, approve called with `sha`, `approved_sha` set.
  - `TestPublishUpdatesExistingNote`.
  - `TestPublishRecreatesDeletedNote`.
  - `TestPublishHeadMovedNoApprove`: the status is still set on the run's own head.
  - `TestPublishBlockingNoApprove`.
  - `TestPublishNothingReviewedNoApprove`.
  - `TestPublishNeutralMapsToSkipped`.
  - `TestPublishFailureReasonComment`.
  - `TestPublishRetriesOnGitLabError`.
  - `TestCleanupDeletesJobAndCancelsStatus`.
  - `TestStaleApprovalUnapproved`.
- [ ] **Step 2:** the tests must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the tests must PASS.
- [ ] **Step 5: Commit:** `feat: publish results, approve on clean reviews, clean up superseded runs`.

---

### Task 11: Engine — watch, reconcile, main and end-to-end test

**Files:**
- Create: `internal/engine/watch.go`, `internal/engine/reconcile.go`, `internal/engine/watch_test.go`, `internal/engine/reconcile_test.go`, `internal/engine/e2e_test.go`, `cmd/pergent-sh/main.go`

**Interfaces:**
- Produces:
  ```go
  func (e *Engine) WatchOnce(ctx context.Context) error
  func (e *Engine) Reconcile(ctx context.Context) error
  func (e *Engine) Run(ctx context.Context) // dispatch, publish and cleanup every 2s or on Wake; watch every 15s; reconcile every hour
  ```
- `WatchOnce`: for each `RunningRuns` entry with a `JobName`, call `Inspect`:
  - `Stuck` set → `Delete`, then `ReportFailure(Stuck)`;
  - `Exists == false` and `now − DispatchedAt ≥ 60s` → `ReportFailure("the review job disappeared before sending a result")`;
  - `Finished` and `now − FinishedAt ≥ 60s` → `ReportFailure(Failure)`, or `the review job finished without sending a result` when `Failure` is empty;
  - `Clone > 0` → `SetCloneSeconds`.
  - `ReportFailure` returning false (a result won the race) is fine.
  - Call `Wake()` after any report.
- `Reconcile`, for each configured repository:
  1. `GetProject(path)`, then upsert the project row with ID, path and clone URL.
  2. `EnsureHook(<PublicURL>/webhooks/gitlab/<instance>, webhook secret)`, then store `HookID`.
  3. If there is no deploy token, or it expires within 7 days:
     - `CreateDeployToken("pergent-sh", now+30d)`;
     - seal it and store it;
     - then `DeleteDeployToken(old)`.

  For project rows not in config: `DeleteHook`, `DeleteDeployToken`, `DeleteProject`. An error on one repository is logged and the rest continue; `Reconcile` returns a joined error.
- `main.go`: `pergent-sh serve --config <path>` (default `/etc/pergent-sh/config.yaml`).
  - Required env: `DATABASE_URL` and `PERGENT_SH_SECRET_KEY`, plus the LLM key and bot tokens named in config.
  - Startup: `Load` config → `LoadLibrary(SkillsDir)` → validate repo skill lists against the library → `store.Open` and `Migrate` → per instance, build both clients and `CurrentUser` (fail fast on bad tokens) → in-cluster kube config → `Reconcile` (errors logged) → HTTP server and `Engine.Run`.
  - Routes: `POST /webhooks/gitlab/{instance}`, `POST /runs/{id}/result` and `GET /healthz`.
  - HTTP server timeouts: `ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s, `WriteTimeout` 60 s.
  - SIGTERM: shut down HTTP with 20 s, then cancel the loops.
  - slog JSON to stdout.

- [ ] **Step 1: Write failing tests:**
  - `TestWatchReportsOOM`.
  - `TestWatchWaitsGraceBeforeReporting`: 30 s after finishing nothing is reported; at 61 s it is.
  - `TestWatchStuckImagePull`: the Job is deleted and the reason reported.
  - `TestWatchJobDisappeared`.
  - `TestWatchRecordsCloneSeconds`.
  - `TestReconcileCreatesHookAndToken`: the token is stored sealed, and opening it gives the token the fake returned.
  - `TestReconcileRotatesExpiringToken`: a new token is created, the old one deleted.
  - `TestReconcileRemovesUnconfigured`.
  - `TestEndToEnd`, with the real store, fake GitLab and fake clientset, driving the `*Once` methods:
    1. A webhook `open` for head A, then `DispatchOnce`, creates a Job.
    2. A webhook push for head B, then `CleanupOnce`, deletes Job A and cancels A's status.
    3. `DispatchOnce` creates Job B.
    4. A clean result posted for run A gets 200 and is discarded; nothing is posted.
    5. A clean result posted for run B gets 200.
    6. `PublishOnce`: exactly one note, status `success` on B, and an approve with `sha=B`.
    7. A webhook push for head C, then `CleanupOnce`, unapproves.
- [ ] **Step 2:** the tests must FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** `go test ./... && go vet ./... && make build` must PASS.
- [ ] **Step 5: Commit:** `feat: watch jobs, reconcile hooks and deploy tokens, serve`.

---

### Task 12: Image, Helm chart, CI and README

**Files:**
- Create:
  - `Dockerfile`: `golang:1.24-bookworm` builder with `CGO_ENABLED=0`, then `gcr.io/distroless/static-debian12:nonroot`, entrypoint `["/pergent-sh","serve"]`.
  - `deploy/helm/pergent-sh/{Chart.yaml,values.yaml,templates/_helpers.tpl,templates/deployment.yaml,templates/service.yaml,templates/configmap.yaml,templates/skills-configmap.yaml,templates/serviceaccount.yaml,templates/rbac.yaml,templates/networkpolicy.yaml,templates/ingress.yaml}`.
  - `.github/workflows/ci.yml`.
  - `.github/workflows/release.yml`.
  - `README.md`.

**Chart:**
- `values.yaml`:
  - `image.repository: ghcr.io/gopaytech/pergent.sh`, `image.tag` (defaults to the chart `appVersion`);
  - `existingSecret` (required; holds `DATABASE_URL`, `PERGENT_SH_SECRET_KEY`, `OPENCODE_API_KEY` and the bot/webhook env vars; mounted with `envFrom`);
  - `config` (rendered verbatim to `config.yaml`) and `skills` (a map of `name.md` → content, mounted at `/etc/pergent-sh/skills`);
  - `jobsNamespace` and `jobEgress.cidrs` (default `["0.0.0.0/0"]`);
  - `ingress.enabled: false`.
- `deployment.yaml`: 1 replica; `runAsNonRoot`, read-only root filesystem, drop ALL; liveness and readiness on `/healthz`.
- `rbac.yaml`: a Role and RoleBinding in `jobsNamespace`:
  - `batch/jobs`: create, get, list, watch, patch, delete;
  - `secrets` and `configmaps`: create, get, delete;
  - `pods`: get, list.
- `networkpolicy.yaml`, in `jobsNamespace`, selects `app.kubernetes.io/managed-by: pergent-sh`:
  - ingress: none;
  - egress: DNS (UDP/TCP 53), TCP 443 and 80 to `jobEgress.cidrs`, and TCP 8080 to the pergent-sh pods.

**CI and release:**
- `ci.yml` runs on push to `main` and on PRs:
  - a `postgres:16` service;
  - `go vet ./...`;
  - `go test -count=1 ./...` with `PERGENT_SH_TEST_DATABASE_URL`;
  - `make build`;
  - `helm lint deploy/helm/pergent-sh`;
  - `helm template` with `deploy/helm/pergent-sh/ci-values.yaml`, which you also create.
- `release.yml` is the same as pergent's: tag `v*.*.*` → `ghcr.io/gopaytech/pergent.sh:<tag>`, linux/amd64.

**README sections:**
- what it does;
- requirements: Kubernetes, PostgreSQL 13+, and a GitLab review bot with **Maintainer** (webhooks, deploy tokens, comments, statuses) plus an approve bot that is an eligible approver;
- install with Helm;
- the config reference, with every field from Task 1 and its default;
- how approval works: the Decide table, including "nothing to review" and the head re-check;
- what a Job can and cannot see;
- the fork fetch fallback;
- limits of 2a: no UI, no commands, no repository cache; a repository is switched off by removing it from config.

- [ ] **Step 1:** write the Dockerfile; `docker build .` must succeed.
- [ ] **Step 2:** write the chart. `helm lint deploy/helm/pergent-sh` and `helm template t deploy/helm/pergent-sh -f deploy/helm/pergent-sh/ci-values.yaml` must succeed.
- [ ] **Step 3:** confirm the rendered Role has no `*` verbs and the NetworkPolicy has empty ingress (inspect `helm template` output with `grep`).
- [ ] **Step 4:** write the workflows and README. Scan for private names:

  ```bash
  grep -rniE 'golabs|gojek|gopay[^t]|goto' --exclude-dir=.git .
  ```

  It must print nothing; `gopaytech` is the public org and is allowed.
- [ ] **Step 5: Commit:** `feat: image, helm chart, ci, release workflow and readme`.

---

## Manual smoke test (after Task 12, by the user or with their go-ahead)

On a test GitLab project and a test cluster:
1. Open an MR touching one Go file: there is a comment, status `success` and an approval.
2. Push a second commit while the Job runs: the first Job is deleted and only the second head is reviewed.
3. Open an MR changing only `go.sum`: no approval, and the "nothing to review" note appears.
4. Open an MR from a fork: the checkout falls back to `refs/merge-requests/<iid>/head` when needed.
5. Set `jobs.resources.limits.memory: 64Mi`: an OOMKilled error comment appears within about 2 minutes.
