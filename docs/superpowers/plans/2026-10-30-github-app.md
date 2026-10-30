# pergent.sh 2b: GitHub App Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every pull request in a configured GitHub.com repository is reviewed exactly as GitLab MRs are in 2a. Each review gets one comment, a `pergent` Check Run, and an approving review by a second App only when the review is clean and still matches the current head and base.

**Architecture:**
- **Platform split.** The engine stops calling `*gitlab.Client` directly. It talks to two small interfaces per instance, `Forge` (the reviewing side) and `Approver`, with one implementation for GitLab (wrapping today's client) and one for GitHub.
- **GitHub auth.** Two GitHub Apps that each team creates itself. pergent.sh signs a JWT with each App's private key and exchanges it for one-hour installation tokens, kept in memory.
- **Clone token.** Each Job gets its own token scoped to one repository with `contents: read`, so there is no stored deploy token on GitHub.
- **Webhook.** One App-level webhook per instance.

**Tech Stack:** Go 1.24, stdlib only for GitHub. JWT RS256 is signed with `crypto/rsa` and `crypto/sha256`, without a JWT library.

**Spec:** no design doc; the user chose to skip it again. The plan argues from:
- the 2a plan, `docs/superpowers/plans/2026-10-30-gitlab-core.md` (Decisions and Global Constraints; all still hold unless changed below);
- the roadmap, `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md`;
- the decisions below.

## Decisions (agreed 2026-10-30)

- **Two GitHub Apps per installation, created by each team** (self-hosted, so pergent.sh hosts nothing):
  - `review`: Pull requests read, Contents read, Checks write, Issues write (PR comments are issue comments), Metadata read. It subscribes to the `pull_request` event and holds the webhook URL and secret.
  - `approve`: Pull requests write and Metadata read, with no webhook.
- **Status is a Check Run** named `pergent`. The conclusion is `success`, `failure`, `neutral` (for `failure_status: neutral`) or `cancelled` (superseded). Start sets `in_progress`.
- **Events.** Webhook `pull_request` with these actions:
  - `opened` and `reopened` enqueue;
  - `synchronize` enqueues (a push);
  - `edited` enqueues only when `changes.base` is present (a retarget), and other edits are ignored;
  - `closed` supersedes.
  - The signature is `X-Hub-Signature-256` (HMAC-SHA256 with the review App's webhook secret), compared in constant time.
- **Clone token.** `POST /app/installations/{id}/access_tokens` with `repository_ids: [repo id]` and `permissions: {contents: read}`, minted per Job and never stored. The git username is `x-access-token`. The fork fallback ref is `refs/pull/<n>/head`.
- **Scope.** GitHub.com only (`api_url` defaults to `https://api.github.com`; a different URL is accepted but untested). Repositories are switched on by listing them in config, as in 2a.
- **Process.** Same as 2a: this plan, then subagent-driven execution.
- **The bot's approval is meant to count toward required reviews.** Task 1 checks whether an App's approval does. If it doesn't, the approve side becomes a bot user with a fine-grained personal access token ("Pull requests: write"). That token is rotated by hand, and pergent.sh logs a Warn when the `GitHub-Authentication-Token-Expiration` response header says it expires within 14 days.
- **Approve App not installed on a repository.** Reconcile records it, and reviews still run. The approval is skipped without retries, and the comment says `**Not approved:** the pergent approve App isn't installed on this repository.` Once the App is installed, approvals start with the next push or ready-for-review; there is no catch-up pass.
- **Repositories are switched on by the config list only.** Installing the App on a repository does not switch it on.
- **One `github:` instance per App pair** covers every org the Apps are installed on. Installation IDs are looked up per repository.
- **Check Run output.** The title is the decision's description and the summary is one line pointing to the PR comment. Findings are not duplicated.
- **The README requires** the branch protection setting "Dismiss stale pull request approvals when new commits are pushed" as a second layer.
- **Drafts, on both platforms.** Drafts are reviewed and get a status, but are never approved. The description reads `No blocking issues; not approved: draft`, with a note in the comment. Marking a draft ready (GitHub `ready_for_review`; GitLab `update` whose `changes.draft` goes to false) re-runs the publish decision for the same head when that head's review is already published. Otherwise the normal flow covers it. This changes 2a's GitLab behavior.
- **Inputs are read at `base.sha`** on GitHub, the same rule as GitLab's `start_sha`.
- **The approval sweep** (the missed-webhook catch) runs every **5 minutes** on both platforms, at startup and then on its own ticker. It no longer runs on the 15 s watch tick, so about 20 approved PRs can't exhaust GitHub's rate limit.
- **The partial-review rule stays on both platforms.** There is no approval when a changed file was skipped as vendored, lockfile, binary, generated or deleted.
- **Builder calls made in this plan (the user can overturn any):**
  - **Merge base.** The PR's merge base comes from `GET /repos/{o}/{r}/compare/{base.sha}...{head.sha}` → `merge_base_commit.sha`. Inputs are read at `base.sha`, the target tip that GitHub recorded for the PR. If compare fails or the PR is `closed`, the run is handled like GitLab's not-ready or not-opened cases.
  - **Approval.** The approve App posts `POST /pulls/{n}/reviews` with `event: APPROVE` and `commit_id: head`. GitHub has no SHA guard like GitLab's, so the existing head and target re-check right before approving is the guard.
  - **Unapprove.** pergent.sh dismisses every non-dismissed `APPROVED` review by the approve App's bot user through `PUT .../reviews/{id}/dismissals` with a fixed message. A 404 or an already dismissed review counts as success.
  - **ApprovedBy.** pergent.sh lists the PR's reviews; the approve App counts as having approved if its latest review state is `APPROVED`.
  - **Bot user IDs.** The bot users' IDs come from `GET /users/{app-slug}%5Bbot%5D`, using the slug from `GET /app`. Comment search filters on the review App bot's user ID.
  - **Instance names** are unique across `gitlab:` and `github:`. A repository names exactly one of `gitlab:` or `github:`.
  - **What reconcile stores.** On GitHub it resolves and stores, per repository, the review App's installation ID, the repository ID, the clone URL, and whether the approve side can reach the repository (`approve_available`).
  - **Nothing to delete on removal.** Removing a GitHub repository from config deletes only its row; there is no per-repository webhook and no stored token.

## Open risk (Task 1 checks it before anything is built on it)

GitHub may not count an App's approving review toward a branch's **required approving reviews**, and an App may not be allowed to **dismiss** reviews unless branch protection allows it. Task 1 checks both on a real test repository with the user's help.

If App approvals don't count, the fallback is an approve **bot user with a fine-grained personal access token**: config `approve_token_env` instead of `approve_app`. The `Approver` interface makes the two interchangeable. Task 1's outcome is recorded in the ledger, and Task 6 builds whichever approver Task 1 chose. Both are specified there.

## Global Constraints

- All of the 2a plan's Global Constraints still hold. In particular:
  - tokens that can comment, approve or write never enter a Job, a log line or a commit;
  - a failed, partial or missing review is never `success` and never approved;
  - one pergent comment per PR, always containing `<!-- pergent -->`;
  - quick-action neutralising applies to every posted comment;
  - public repo, example names only (`github.example.com` is not GitHub.com; use `acme/widgets` on github.com in examples).
- **GitLab behavior must not change.** Every existing test passes unchanged except where a test's construction of `engine.Bots` has to move to the new interfaces.
- **App private keys are read from env vars named in config.** They are PEM text (PKCS#1 or PKCS#8 RSA) and are never logged, not even their length. The JWT has `iat = now − 60s`, `exp = now + 9m`, `iss = app_id`.
- **Installation tokens are cached in memory** per (app, installation) and refreshed when fewer than 5 minutes remain. A per-Job clone token is always freshly minted and is only ever placed in the Job's Secret.
- **GitHub API calls** send these headers:
  - `Accept: application/vnd.github+json`;
  - `X-GitHub-Api-Version: 2022-11-28`;
  - `User-Agent: pergent-sh`;
  - `Authorization: Bearer <token>`.

  Errors are retried as in the GitLab client (429, 5xx and connection errors, 3 times, honoring `Retry-After` and `X-RateLimit-Reset` up to 30 s). Cross-host redirects are refused, and the token is masked before truncation.
- **Store.** Migration `0004_github.sql` adds `projects.installation_id bigint NOT NULL DEFAULT 0` and `projects.approve_available boolean NOT NULL DEFAULT true`. `MRKey.ProjectID` is the GitHub repository ID and `MRKey.IID` is the PR number.

## Review Focus

1. **A `pull_request` `edited` event that changes only the title or body** must not start a run; one with `changes.base` must. The webhook test covers this.
2. **A webhook signed with the wrong secret, or not signed at all** gets 401 and the queue is never touched. A missing `X-Hub-Signature-256` header is a mismatch.
3. **The approve App not installed, or lacking permission.** Approval fails and is retried; it is never recorded as made. The status and comment are still posted. The publish test covers this.
4. **The clone token** is scoped to the one repository with `contents: read`, and never appears in logs or in the Job spec outside its Secret. The dispatch test asserts the token request body.
5. **A closed PR that GitHub reports as `merged: true`.** Its approval record is cleared without any GitHub write, the same rule as GitLab's `merged`. The sweep test covers this.

---

## File Structure

```
internal/forge/forge.go             Forge and Approver interfaces + platform-neutral types (Change, ChangeState, StatusState)
internal/forge/gitlabforge/         adapter over internal/gitlab (behavior identical to today)
internal/github/                    REST client: app JWT, installation tokens, PRs, compare, contents, tree, comments, check runs, reviews, repos
internal/forge/githubforge/         adapter over internal/github
internal/webhook/github.go          GitHub pull_request webhook handler
internal/config/config.go           github instances, repository.github, validation
internal/engine/*                   use forge interfaces; per-platform reconcile and clone credentials
internal/kube/{job.go,checkout.sh}  MR_REF instead of MR_IID; GIT_USERNAME stays
internal/store/migrations/0004_github.sql
cmd/pergent-sh/main.go, internal/server/server.go, chart, README
```

---

### Task 1: Verify GitHub App approvals and dismissals (spike, with the user)

**Files:**
- Create `docs/github-app-approval-check.md`, a short record of what was tested and observed. It names no real organization; it says "a test org".

The implementer does not need GitHub credentials. This task produces a checklist and a tiny throwaway Go program in `/tmp`, outside the repo, that the **user** runs against a test repository they own. The program:
- signs an App JWT;
- gets an installation token;
- submits an APPROVE review on a test PR;
- reads the PR's `reviewDecision` via GraphQL (`pullRequest { reviewDecision }`);
- then dismisses the review.

- [ ] **Step 1:** Write the checklist. It covers:
  - creating two test Apps with the permissions above;
  - a branch protection rule with 1 required approval and "dismiss stale approvals";
  - a PR opened by a human.
- [ ] **Step 2:** Write the throwaway program. It prints:
  - the approve and dismiss HTTP status codes;
  - `reviewDecision` before and after approving;
  - whether the PR shows as mergeable by branch protection.
- [ ] **Step 3:** Report BLOCKED with the checklist path and the program path, so the controller asks the user to run them. When the user returns the output, record it in the doc and commit:

  ```
  docs: record how GitHub treats App approvals and dismissals
  ```

  Then state the outcome, either **App approver** or **token approver**.

**Outcome gates Task 6** (which approver to build) and the README's approval section.

---

### Task 2: Platform interfaces and the GitLab adapter (pure refactor)

**Files:**
- Create: `internal/forge/forge.go`, `internal/forge/gitlabforge/gitlabforge.go`, `internal/forge/gitlabforge/gitlabforge_test.go`
- Modify: `internal/engine/{engine,dispatch,publish,approvals,reconcile,watch}.go`, `internal/engine/fakes_test.go`, `cmd/pergent-sh/main.go`

**Interfaces (produced):**

```go
package forge
type ChangeState string // "open" | "closed" | "merged" | "locked"
type Change struct { Number int64; State ChangeState; Draft bool; HeadSHA, TargetBranch, BaseSHA, StartSHA string }
// BaseSHA is the merge base; StartSHA is where repo inputs are read. Empty BaseSHA = not ready yet.
type StatusState string // "running" | "success" | "failed" | "neutral" | "canceled"
type Forge interface {
    inputs.Source                                       // GetFile, ListTree
    GetChange(ctx context.Context, projectID, number int64) (Change, error) // not found → ErrNotFound
    SetStatus(ctx context.Context, projectID int64, sha string, s StatusState, description string) error
    FindComment(ctx context.Context, projectID, number int64, marker string) (int64, bool, error) // own comments only
    CreateComment(ctx context.Context, projectID, number int64, body string) (int64, error)
    UpdateComment(ctx context.Context, projectID, number, commentID int64, body string) error // gone → ErrNotFound
    ChangeRef(number int64) string                      // "refs/merge-requests/N/head" | "refs/pull/N/head"
}
type Approver interface {
    Approve(ctx context.Context, projectID, number int64, sha string) error // head moved → ErrHeadMoved
    Unapprove(ctx context.Context, projectID, number int64) error           // nothing to undo → nil
    HasApproved(ctx context.Context, projectID, number int64) (bool, error)
}
var ErrNotFound, ErrHeadMoved error
```

- `inputs.Source` changes to return `forge.TreeEntry`, or keeps `gitlab.TreeEntry`; the implementer picks one neutral type and moves it, without duplicating it.
- The GitLab adapter maps the client's existing calls one to one:
  - GitLab `opened` → `open`;
  - neutral → `skipped`;
  - `HasApproved` = the bot user's ID is in `ApprovedBy`.
- `engine.Bots` becomes `Platform{Forge forge.Forge; Approver forge.Approver}`, keyed by instance.
- Reconcile and clone credentials stay GitLab-specific for now, behind `e.Config` platform checks. Task 6 adds GitHub's.

- [ ] **Step 1:** Write the adapter tests with `httptest` (one per method, asserting the mapping above). They should fail first.
- [ ] **Step 2:** Implement the interfaces and the adapter, and switch the engine and main over.
- [ ] **Step 3:** `go vet ./... && go test -count=1 ./...` with the DB set must pass. No GitLab behavior may change: the existing engine and e2e tests pass. A test may change only where it built `Bots` directly.
- [ ] **Step 4: Commit:** `refactor: engine talks to platforms through Forge and Approver`

---

### Task 2b: Drafts, re-publish on ready, and the 5-minute approval sweep (both platforms)

**Files:**
- Modify: `internal/policy/policy.go` and its test; `internal/store/runs.go` and its test; `internal/webhook/gitlab.go` and its test; `internal/engine/{publish,approvals,engine}.go` and their tests; `internal/forge/gitlabforge` (Draft from GitLab's `draft`); README.

**Produces:**
- `policy.Outcome` gains `Draft bool` and `ApproveUnavailable bool`. `Decide` keeps every existing row. When it would otherwise approve:
  - `Draft` → no approval, description `No blocking issues; not approved: draft`, and the note `**Not approved:** this is a draft. It will be approved when it's marked ready.`;
  - otherwise `ApproveUnavailable` → no approval, the same description pattern with the reason `the pergent approve App isn't installed on this repository`, and the note from the Decisions section.

  The status is `success` in both cases. Notes go before the findings block, as with the other notes.
- `func (s *Store) Republish(ctx, mr MRKey, headSHA string) (bool, error)`. If the MR's newest non-superseded run is `published` with this head, it moves back to `reported` with attempts 0, so the publisher decides again. Otherwise it returns false.
- The GitLab webhook calls `Republish` on `update` when `changes.draft.previous == true && current == false`, then Wakes.
- Publish passes `Draft` from `GetChange` (already read before approving) and `ApproveUnavailable` from the project row into `Decide`. On GitLab, `ApproveUnavailable` is always false.
- The approval sweep moves off the watch tick onto its own 5-minute ticker, plus one run at startup.

- [ ] **Step 1:** Write the failing tests:
  - policy rows for a draft, approve-unavailable, and a draft whose review has blocking findings (failed as before);
  - store `Republish` (published newest → reported; not newest or another head → false);
  - webhook: a draft→ready update calls `Republish`, and a ready→draft update doesn't;
  - an engine test: draft published without approval, then `Republish`, then publish → approved;
  - the sweep runs on its own interval, not every watch tick.
- [ ] **Step 2:** Implement it. **Step 3:** `go test -count=1 ./...` with the DB set passes.
- [ ] **Step 4: Commit:** `feat: drafts are reviewed but approved only when ready; sweep every 5 minutes`

---

### Task 3: Config for GitHub instances

**Files:** Modify `internal/config/config.go` and `internal/config/config_test.go`; add a testdata file.

**Produces:**

```go
type GitHubInstance struct {
    Name, APIURL, WebhookSecretEnv string     // APIURL default "https://api.github.com"
    ReviewApp  App                            // yaml review_app
    ApproveApp *App                           // yaml approve_app; exactly one of ApproveApp / ApproveTokenEnv
    ApproveTokenEnv string                    // yaml approve_token_env (token-approver fallback)
}
type App struct { AppID int64; PrivateKeyEnv string } // yaml app_id, private_key_env
// Config gains GitHub []GitHubInstance `yaml:"github"`; Repository gains GitHub string `yaml:"github"`.
func (c *Config) Platform(instance string) (string, bool) // "gitlab" | "github"
func (g GitHubInstance) Secrets() (reviewKey, approveKeyOrToken, webhook string, err error)
```

Validation:
- instance names are unique across both lists;
- a repository has exactly one of `gitlab` and `github`, and it names an existing instance of that platform;
- a GitHub project is `owner/name`;
- `app_id` > 0;
- exactly one of `approve_app` and `approve_token_env`;
- `api_url` must be https.

- [ ] **Step 1:** Write the failing tests: a valid mixed GitLab+GitHub file, and one rejection per rule above, each naming the field.
- [ ] **Step 2:** Implement it. **Step 3:** `go test ./internal/config/` passes.
- [ ] **Step 4: Commit:** `feat: config for GitHub App instances`

---

### Task 4: GitHub REST client

**Files:** Create `internal/github/{client.go,app.go,types.go,client_test.go,app_test.go}`

**Produces:**

```go
func NewApp(apiURL string, appID int64, privateKeyPEM string, hc *http.Client) (*App, error) // bad PEM → error without key text
func (a *App) JWT(now time.Time) (string, error)
func (a *App) Slug(ctx context.Context) (string, error)                                    // GET /app
func (a *App) BotUserID(ctx context.Context) (int64, error)                                // GET /users/{slug}[bot]
func (a *App) Installation(ctx context.Context, owner, repo string) (int64, error)        // GET /repos/{o}/{r}/installation; 404 → ErrNotFound
func (a *App) Client(installationID int64) *Client                                         // cached installation token, refreshed <5 min before expiry
func (a *App) CloneToken(ctx context.Context, installationID, repoID int64) (string, time.Time, error) // repository_ids + contents:read
func NewTokenClient(apiURL, token string, hc *http.Client) *Client                         // token-approver fallback
// Client methods (owner/repo addressed): Repo, PullRequest, Compare (merge_base_commit.sha),
// File(path, ref) ([]byte, bool, error) via /contents with Accept raw, Tree(path, ref) via /contents (dir listing),
// IssueComments (paginated, Link header), CreateIssueComment, UpdateIssueComment,
// CreateCheckRun(name, headSHA, status, conclusion, title, summary),
// CreateReview(number, commitID, "APPROVE"), ListReviews (paginated), DismissReview(number, reviewID, message).
// Errors: *APIError{Status, Body}; ErrNotFound on 404; same retry/redirect/masking rules as internal/gitlab.
```

- [ ] **Step 1:** Write the failing tests:
  - a JWT test: decode the header and claims, and verify the signature against the test key generated in the test;
  - `TestBadPEMErrorHasNoKey`;
  - a token-cache test: one token request for two calls, and a new request after the clock passes expiry − 5 min;
  - `TestCloneTokenScoped`: asserts the request body;
  - `httptest` tests per method: path, headers and body;
  - pagination via `Link: <…>; rel="next"`;
  - retry on 503 and on 403 with `X-RateLimit-Remaining: 0`;
  - cross-host redirect refused;
  - the token never appears in errors.
- [ ] **Step 2:** Implement it. **Step 3:** `go test ./internal/github/` passes.
- [ ] **Step 4: Commit:** `feat: GitHub App client`

---

### Task 5: Checkout ref and GitHub clone parameters

**Files:** Modify `internal/kube/{job.go,checkout.sh,job_test.go}`

- `JobParams.MRIID` is replaced by `ChangeRef string`, and the env var `MR_IID` by `MR_REF`.
- `checkout.sh` replaces `"refs/merge-requests/$MR_IID/head"` with `"$MR_REF"` and changes its fallback message to `falling back to $MR_REF and $TARGET_BRANCH`. No other byte of the script changes.
- Add the golden test that 2a deferred: the embedded script equals the exact expected text.

- [ ] **Step 1:** Write the failing tests: the golden script, and `MR_REF` present while `MR_IID` is absent.
- [ ] **Step 2:** Implement it. Engine dispatch passes `Forge.ChangeRef(run.MR.IID)`.
- [ ] **Step 3:** `go test ./internal/kube/ ./internal/engine/` passes.
- [ ] **Step 4: Commit:** `refactor: the checkout's fallback ref comes from the platform`

---

### Task 6: GitHub adapter, clone credentials and reconcile

**Files:**
- Create: `internal/forge/githubforge/{githubforge.go,approver.go,githubforge_test.go}`, `internal/store/migrations/0004_github.sql`
- Modify: `internal/store/projects.go` (`InstallationID`), `internal/engine/{dispatch,reconcile}.go`

**GitHub Forge:**
- `GetChange`:
  - from `PullRequest`: `state` + `merged` → `open`/`closed`/`merged`, and `Draft = draft`;
  - `HeadSHA = head.sha`, `TargetBranch = base.ref`, `StartSHA = base.sha`;
  - `BaseSHA` comes from `Compare(base.sha, head.sha)`. A compare 404 or 422 gives empty `BaseSHA`, which is "not ready".
- `SetStatus` creates a Check Run on `sha`:
  - `running` → `status: in_progress`, title "pergent review running";
  - everything else → `status: completed` with conclusion `success` / `failure` / `neutral` / `cancelled`, title = description.
- Comments are issue comments filtered by the review bot's user ID.
- `ChangeRef` → `refs/pull/N/head`.

**Approver (App):**
- `Approve` re-reads the PR. If `head.sha` ≠ sha it returns `ErrHeadMoved`; otherwise it posts `CreateReview(APPROVE, commit_id: sha)`.
- `Unapprove` dismisses every `APPROVED`, non-dismissed review by the approve bot user, with the message `pergent: the reviewed commit or target changed`.
- `HasApproved` checks whether the approve bot's latest review is `APPROVED`.

**Approver (token fallback):** the same three methods through `NewTokenClient`. The bot user ID comes from `GET /user`. When a response's `GitHub-Authentication-Token-Expiration` header is within 14 days, it logs a Warn, at most once per hour. Build this one only if Task 1 says App approvals don't count; otherwise skip it and note that in the README.

**Engine:**
- Dispatch gets clone credentials per platform:
  - GitLab: today's sealed deploy token;
  - GitHub: `CloneToken(installationID, repoID)`, username `x-access-token`.

  A missing installation gives `ReportFailure("the pergent review App isn't installed on this repository")`.
- Reconcile for GitHub repositories:
  - `Installation(owner, repo)` with the review App;
  - `Repo()` for the ID and clone URL;
  - upsert the row;
  - check the approve side: `Installation` with the approve App, or `GET /repos` with the token. Store the answer as `approve_available`; when it's false, log a Warn (publish then skips approval with the note from Task 2b).
  - Removed GitHub repositories: delete the row.

- [ ] **Step 1:** Write the failing tests:
  - adapter tests per method with `httptest`, including `merged: true` → `merged` and the compare 404 → not ready;
  - dispatch: a GitHub run creates a Job whose `GIT_USERNAME` is `x-access-token`, whose `MR_REF` is `refs/pull/N/head`, and whose Secret holds the minted token, after a token request scoped to the one repo;
  - reconcile: the installation is stored, and an approve App that isn't installed sets `approve_available = false`;
  - publish for such a repository: no approve call, the note posted, no retry.
- [ ] **Step 2:** Implement it. **Step 3:** `go test -count=1 ./...` with the DB set passes.
- [ ] **Step 4: Commit:** `feat: GitHub forge, clone tokens and reconcile`

---

### Task 7: GitHub webhook handler

**Files:** Create `internal/webhook/github.go`, `internal/webhook/github_test.go` and testdata fixtures; modify `internal/server/server.go`.

- Route: `POST /webhooks/github/{instance}`.
- Order:
  1. unknown instance → 404;
  2. read the body with `MaxBytesReader` at 5 MiB (413 when over);
  3. HMAC-SHA256 of the body against `X-Hub-Signature-256` (`sha256=<hex>`), in constant time; a missing or mismatched signature → 401;
  4. `X-GitHub-Event` other than `pull_request` → 200 (`ping` → 200);
  5. a repository (`repository.full_name`, compared case-insensitively) not in config → 200.
- Actions, using `pull_request.head.sha` and `pull_request.base.ref`:
  - `opened`, `reopened`, `synchronize` → `EnqueueRun`;
  - `edited` with `changes.base` → `EnqueueRun`;
  - `closed` → `SupersedeMR`;
  - `ready_for_review` → `Republish(mr, head)`, then Wake;
  - anything else → 200.
- Unusable but authenticated payloads → 200 with a Warn (2a rule). Storage error → 500.

- [ ] **Step 1:** Write the failing tests: one per rule above, including Review Focus #1 and #2, and an `edited` event without `changes.base` that is ignored.
- [ ] **Step 2:** Implement it. **Step 3:** `go test ./internal/webhook/ ./internal/server/` passes.
- [ ] **Step 4: Commit:** `feat: GitHub pull_request webhook`

---

### Task 8: Wiring, end-to-end test and docs

**Files:** Modify `cmd/pergent-sh/main.go`, `internal/engine/e2e_test.go` (or a new `e2e_github_test.go`), `deploy/helm/pergent-sh/*`, and `README.md`.

- main builds:
  - the review App and the approve App (or the token client) per GitHub instance;
  - the bot user IDs, failing fast and naming the instance, never the key;
  - each instance's webhook secret, for the route.
- **End-to-end test:** a fake GitHub (`httptest`) plus the fake clientset and the real store. It follows the 2a e2e steps through the real routes:
  1. a signed `opened` event;
  2. dispatch: the Job and the scoped token request;
  3. `synchronize` to a new head: the old Job is deleted and the old Check Run is `cancelled`;
  4. the late result for the old head is discarded;
  5. the clean result for the new head: one comment, a `success` Check Run, an APPROVE review on the new head;
  6. `synchronize` again: the review is dismissed;
  7. `closed` with `merged: true`: the approval record is cleared with no GitHub write.
- **Chart:** the existing-Secret docs gain the GitHub env vars. Nothing else changes, because the GitHub webhook comes in through the same service.
- **README:**
  - a "GitHub" section: creating the two Apps (permissions list, webhook URL `https://<public_url>/webhooks/github/<instance>`, secret, private keys into the Secret), installing both on the org, and the config example;
  - the Check Run;
  - Task 1's outcome on required reviews and dismissals;
  - key rotation (add a new key, update the Secret, restart, delete the old key).

- [ ] **Step 1:** Write the failing e2e test.
- [ ] **Step 2:** Wire main.
- [ ] **Step 3:** These must all pass:

  ```bash
  go vet ./... && go test -count=1 ./... && make build
  helm lint deploy/helm/pergent-sh -f deploy/helm/pergent-sh/ci-values.yaml
  ```

- [ ] **Step 4:** The private-name scan from 2a must be clean, apart from the known hits in go.sum and the plans.
- [ ] **Step 5: Commit:** `feat: serve GitHub App reviews; docs`

## Manual smoke test (after Task 8, by the user)

On a test org:
1. Open a PR. It gets a comment, a `pergent` Check Run and an approval.
2. Push. The approval is dismissed and the PR is reviewed again.
3. Change the base branch. It is reviewed again and the approval is withdrawn.
4. A fork PR is reviewed.
5. A PR changing only `package-lock.json` gets no approval and the note.
