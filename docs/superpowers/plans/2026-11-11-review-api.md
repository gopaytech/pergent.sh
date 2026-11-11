# Review API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an outside system send the files of a change to `POST /v1/reviews` with a client key, have pergent.sh review them with the skills an admin chose for that client, and get the findings back by a signed callback and by `GET /v1/reviews/{id}`.

**Architecture:** An API review is a row of `runs` with `kind = 'api'` (instance `''`, project ID `0`, `mr_iid` from its own sequence), plus a row of `api_reviews` that holds the client, tenant, subject and sealed inputs. Sharing `runs` gives API reviews the queue, `jobs.max_running`, the watcher, job tracing, logs, the result callback and usage for free. The engine branches on `run.Kind` in dispatch, watch, publish and cleanup, so the forge path stays as it is. The Job has no clone: its checkout runs `git fast-import` on a stream pergent.sh built from the sent files, and pergent.sh computes both commit SHAs up front. The new package `internal/reviewapi` holds the key format, the `/v1/` handlers, the file rules, the stream, the result mapping and the signature.

**Tech Stack:** Go, Postgres (pgx), client-go (fake clientset in tests), real `git` in two tests, React + TanStack Query + Vitest.

**Spec:** `docs/superpowers/specs/2026-11-11-review-api-design.md`

## Global Constraints

- **Never start a container:** no `docker`, `orbctl`, nothing that wakes OrbStack. Postgres-backed tests (`internal/store`, `internal/engine`, DB-backed `internal/dashboard`) skip without `PERGENT_SH_TEST_DATABASE_URL`, and run only in CI. Locally they print a fast `ok`. Write them carefully: CI runs them for the first time.
- **No external platforms:** no `gh`, `glab`, `curl`.
- **No release:** no version bump, tag, chart version, release notes or version number anywhere. Upgrade notes go in `docs/upgrading.md` under a top heading `## Unreleased`. Leave `openapi.yaml`'s `info.version` alone.
- **Public repo.** Before every commit run, and expect empty output: `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`. `git grep` misses untracked files: `git add` first, then scan.
- Commit with `git -c user.email=zufardhiyaulhaq@gmail.com commit`. Messages end with exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never merge, push, tag or open a PR unless the owner asks.
- **Migrations only add:** `0038_api_clients.sql` (Part 1), `0039_api_reviews.sql` (Part 2), `0040_api_callbacks.sql` (Part 3). New tables, plus new columns with a default or NULL. Nothing renamed or dropped. The previous release must keep running MR reviews on the migrated database.
- **The forge path does not change.** `internal/kube/testdata/job-no-links.golden.json` stays byte for byte. `TestJobUnchangedWithoutLinks` and every existing engine, store and dashboard test pass unchanged.
- Admin dashboard routes register only through `Dashboard.adminRoute` and carry `x-pergent-access: admin`. The `/v1/` routes are gated by `reviewapi.Handler.RequireKey` and carry `x-pergent-access: key`. `internal/dashboard/openapi_test.go` and `routes_test.go` check both.
- Failure texts and error messages start lowercase. A failure text that reaches a caller names no file of the checkout, no secret and no other client.
- **Secrets:** a key is shown once and stored as SHA-256 only. The callback secret is sealed with `secretbox`. Neither reaches a log, the audit log, a metric label or an error body. Callback attempt errors are stored redacted with the client's secrets.
- **No new config key.** The `/v1/` routes exist only when the dashboard is on (`dashboard.url` set), as the skill fetch route does.
- **Docs are short and scannable.** User-facing behaviour goes in `docs/` in the same commit. A new page gets a line in `docs/README.md`.
- The API's JSON uses snake_case (`end_line`, `tokens_in`). The dashboard API keeps camelCase.
- Web gate in a worktree with no `web/node_modules`: `ln -s ~/Documents/personal/github/pergent.sh/web/node_modules web/node_modules`, run the gate, then remove the link, `web/tsconfig.tsbuildinfo` and `internal/web/dist/{assets,index.html}`. Never run vitest while `go test` runs.

## Review Focus

1. **A caller sends `.gitattributes` with `* text eol=crlf` or `filter=lfs`, or a file with CRLF endings.** The SHAs pergent.sh computed must still equal what the Job built, or every such review fails "for a different commit". `git fast-import` applies no filters. (Task 8: `TestImportMatchesGit` includes such files. Task 9: the script test does too.)
2. **A retried `POST` with the same `Idempotency-Key`, while the first is queued and after it finished.** Expect the same review ID, no superseding, no second Job. The same key with another body is `422 idempotency_key_reused`. (Task 6: `TestEnqueueAPIReviewIdempotent`. Task 11: `TestCreateReviewIdempotent`.)
3. **A key of client A uses client B's review ID** on GET, dismiss or undismiss. Expect a `404` whose body is byte for byte the body for an unknown ID. (Task 11: `TestOtherClientsReviewIsNotFound`. Task 18: the dismiss paths.)
4. **A callback receiver that answers `301`, hangs past 10 s, or checks with the old secret after a rotation.** The redirect is not followed and counts as a failed attempt. The timeout is a failed attempt. The same `X-Pergent-Delivery` goes on every retry. Within 24 h of a rotation both `v1=` values are sent. (Task 16.)
5. **An admin removes a client while one of its reviews runs.** Expect the Job deleted, no callback, the Job's late result answered `401` with nothing re-created, and the client's usage gone. (Task 7: `TestDeleteAPIClientReturnsLiveJobs`. Task 12: `TestRemoveClientDeletesLiveJobs`.)

---

# Part 1: Clients and keys

Ends with: admins create clients, keys and secrets in the new API clients section. A key authenticates `GET /v1/client`. No reviews yet.

### Task 1: Store: clients and keys

**Files:**
- Create: `internal/store/migrations/0038_api_clients.sql`, `internal/store/apiclients.go`, `internal/store/apiclients_test.go`

**Interfaces:**
- Produces:
  - `type APILimits struct{ MaxRequestBytes, MaxFiles, MaxInFlight, RequestsPerHour int; MonthlyTokenBudget int64 }`
  - `var DefaultAPILimits = APILimits{524288, 200, 5, 600, 10_000_000}` and `const HardMaxRequestBytes = 819200`, `HardMaxFiles = 1000` (Builder call: the defaults and hard maximums; the hard maximum keeps the files' Secret under 1 MiB)
  - `type APIClient struct{ ID int64; Name string; Skills []string; ModelID string /* "" = default */; CallbackURL string; CallbackSecretSealed, PreviousSecretSealed []byte; PreviousSecretUntil time.Time; Limits APILimits; CreatedAt, UpdatedAt time.Time }`
  - `type APIKey struct{ ID, ClientID int64; Name, Prefix string; Hash []byte; CreatedAt time.Time; ExpiresAt, LastUsedAt *time.Time }`
  - `var ErrAPIClientExists, ErrAPIKeyExists, ErrAPIPrefixTaken = errors.New(...)`
  - `func (s *Store) CreateAPIClient(ctx context.Context, c APIClient) (APIClient, error)`
  - `func (s *Store) APIClients(ctx context.Context) ([]APIClient, error)` (by name)
  - `func (s *Store) APIClientByName(ctx context.Context, name string) (APIClient, bool, error)` and `APIClientByID(ctx, id int64) (APIClient, bool, error)`
  - `func (s *Store) UpdateAPIClient(ctx context.Context, name string, apply func(*APIClient)) (APIClient, bool, error)` (row locked; `Name`, `ID`, `CreatedAt` not writable)
  - `func (s *Store) DeleteAPIClient(ctx context.Context, name string) (jobs []string, found bool, err error)` (Part 1: `jobs` is always nil; Task 7 fills it)
  - `func (s *Store) CreateAPIKey(ctx context.Context, k APIKey) (APIKey, error)`, `APIKeys(ctx, clientID int64) ([]APIKey, error)` (newest first), `DeleteAPIKey(ctx, clientID, id int64) (APIKey, bool, error)`
  - `func (s *Store) APIKeyByPrefix(ctx context.Context, prefix string) (APIKey, APIClient, bool, error)`
  - `func (s *Store) TouchAPIKey(ctx context.Context, id int64, now time.Time) error` (writes only when `last_used_at` is NULL or older than one minute)

- [ ] **Step 1: Write the migration**

```sql
-- API clients: outside systems that ask for reviews through the review API
-- (/v1/), and their keys. Only adds: the previous release ignores both.
CREATE TABLE api_clients (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                   text        NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9-]+$' AND length(name) <= 63),
    skills                 text[]      NOT NULL DEFAULT '{}',
    model_id               text,
    callback_url           text        NOT NULL DEFAULT '',
    callback_secret_sealed bytea       NOT NULL,
    previous_secret_sealed bytea,
    previous_secret_until  timestamptz,
    max_request_bytes      integer     NOT NULL DEFAULT 524288 CHECK (max_request_bytes BETWEEN 1024 AND 819200),
    max_files              integer     NOT NULL DEFAULT 200 CHECK (max_files BETWEEN 1 AND 1000),
    max_in_flight          integer     NOT NULL DEFAULT 5 CHECK (max_in_flight BETWEEN 1 AND 50),
    requests_per_hour      integer     NOT NULL DEFAULT 600 CHECK (requests_per_hour BETWEEN 1 AND 100000),
    monthly_token_budget   bigint      NOT NULL DEFAULT 10000000 CHECK (monthly_token_budget > 0),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- A key is stored as the SHA-256 of the whole key; prefix finds the row.
CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    client_id    bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix       text        NOT NULL UNIQUE CHECK (prefix ~ '^[0-9a-f]{8}$'),
    key_hash     bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz
);
CREATE UNIQUE INDEX api_keys_name_idx ON api_keys (client_id, lower(name));
```

- [ ] **Step 2: Write the failing tests** in `apiclients_test.go` (Postgres, skip without the env var):
  - `TestCreateAPIClientRoundTrip`: create `fastgateway` with skills `["gateway"]`, limits zero → stored limits equal `DefaultAPILimits`. `APIClientByName` returns it with the sealed secret. A second create of the same name gives `ErrAPIClientExists`.
  - `TestUpdateAPIClientKeepsName`: `apply` sets `Name = "x"` and `Skills = ["a","b"]` → the name is unchanged, the skills are saved, `UpdatedAt` moves.
  - `TestAPIKeyLookup`: create a key with prefix `0a1b2c3d` → `APIKeyByPrefix("0a1b2c3d")` returns the key and its client. Prefix `ffffffff` → not found. A second key with the same prefix → `ErrAPIPrefixTaken`. The same name in any case (`CI` vs `ci`) for one client → `ErrAPIKeyExists`.
  - `TestDeleteAPIKeyStopsLookup`: after `DeleteAPIKey`, `APIKeyByPrefix` → not found. Another client's key ID → `found == false`.
  - `TestTouchAPIKeyThrottles`: touch at `t`, then at `t+30s` → `LastUsedAt == t`. At `t+61s` → it moves.
  - `TestDeleteAPIClientCascadesKeys`: delete → the key is gone and `found == true`. Delete again → `found == false`.

- [ ] **Step 3: Run, expect FAIL.** `go vet ./internal/store` fails with `undefined: APIClient`.

- [ ] **Step 4: Implement.** A zero `Limits` field on create takes the default. Map unique violations by constraint name (`api_clients_name_key`, `api_keys_name_idx`, `api_keys_prefix_key`) to the three errors.

- [ ] **Step 5: Verify.** `go build ./... && go vet ./... && go test ./internal/store` passes (the DB tests skip).

- [ ] **Step 6: Commit.** `feat(store): API clients and their keys`

### Task 2: Key format and the bearer-key gate

**Files:**
- Create: `internal/reviewapi/key.go`, `internal/reviewapi/key_test.go`, `internal/reviewapi/handler.go`, `internal/reviewapi/handler_test.go`, `internal/reviewapi/errors.go`
- Modify: `internal/server/server.go` (`Deps.ReviewAPI *reviewapi.Handler`; `if d.ReviewAPI != nil && d.Config.DashboardEnabled() { d.ReviewAPI.Routes(mux) }`), `cmd/pergent-sh/main.go` (build it when the dashboard is on), `internal/dashboard/openapi_test.go` (`accessRank["key"] = 1`; `gateOf` maps a selector named `RequireKey` to `"key"`; `undocumentedCatchAlls["/v1/"] = "catch-all: 404 JSON for an unknown review API path"`), `openapi.yaml` (the "Who calls what" row, `securitySchemes.reviewApiKey: {type: http, scheme: bearer}`, `GET /v1/client`)

**Interfaces:**
- Produces:
  - `func NewKey() (key, prefix string, hash []byte, err error)`: `key = "pgk_" + prefix + "_" + base64.RawURLEncoding(32 random bytes)`, `prefix` = 8 lowercase hex characters (4 random bytes), `hash = HashKey(key)`
  - `func ParseKey(key string) (prefix string, ok bool)`, `func HashKey(key string) []byte` (SHA-256 of the whole key)
  - `func NewCallbackSecret() (string, error)`: `"pgs_" + base64.RawURLEncoding(32 random bytes)`
  - `type Store interface{ APIKeyByPrefix(...); TouchAPIKey(...) }` (later tasks add methods; `*store.Store` satisfies it)
  - `type Handler struct{ Store Store; Box *secretbox.Box; Models ModelSource; Now func() time.Time; Wake func(); Log *slog.Logger }` with `type ModelSource interface{ ModelByID(id string) (instances.Model, bool); DefaultModel() (instances.Model, bool) }`
  - `func (h *Handler) RequireKey(next http.Handler) http.Handler`: always sets `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. Puts the key and client in the request context: `func keyOf(r *http.Request) store.APIKey`, `func clientOf(r *http.Request) store.APIClient`.
  - `func (h *Handler) Routes(mux *http.ServeMux)` registers `GET /v1/client` and the catch-all `/v1/` (404). Later tasks add routes here, each as `mux.Handle("<METHOD> /v1/...", h.RequireKey(http.HandlerFunc(h.<fn>)))`.
  - Error body `type ErrorBody struct{ Code string "code"; Message string "message" }`. The v1 codes are fixed now (a new code would be `/v2/`): `unauthorized`, `not_found`, `invalid`, `too_large`, `rate_limited`, `budget_used_up`, `cannot_review`, `not_finished`, `idempotency_key_reused`.
  - `GET /v1/client` → `ClientBody{ Name string "name"; Limits LimitsBody "limits"; Usage *UsageBody "usage,omitempty" }`. `LimitsBody` has `max_request_bytes`, `max_files`, `max_in_flight`, `requests_per_hour`, `monthly_token_budget`. `Usage` arrives in Task 19.

- [ ] **Step 1: Write the failing tests.** `handler_test.go` uses a `fakeStore` (map by prefix), so it runs locally.
  - `TestNewKeyParses`: `NewKey` → `ParseKey(key)` gives the prefix. `HashKey(key)` equals `hash`. 100 keys are all distinct.
  - `TestParseKeyRefuses`: `""`, `pgk_`, `pgk_0a1b2c3d`, `pgk_0A1B2C3D_x`, `pgx_0a1b2c3d_x`, `pgk_0a1b2c3_xx` → `ok == false`.
  - `TestRequireKey401`: no header, `Basic x`, an unknown prefix, the right prefix with a wrong secret, an expired key (`ExpiresAt` = now) → each `401` with body `{"code":"unauthorized","message":"missing, unknown, revoked or expired key"}`, and the bodies are identical. `Cache-Control: no-store` is set on every response.
  - `TestRequireKeyTouches`: a good key → `200` from `GET /v1/client`, and `TouchAPIKey` is called with the key's ID.
  - `TestClientBodyShowsLimits`: the body is exactly `{"name":"fastgateway","limits":{"max_request_bytes":524288,"max_files":200,"max_in_flight":5,"requests_per_hour":600,"monthly_token_budget":10000000}}`.
  - `TestUnknownV1PathIs404JSON`: `GET /v1/nope` with a good key → `404` `{"code":"not_found","message":"not found"}`.
  - Extend `TestOpenAPIRouteScanner` with a source snippet that registers through `h.RequireKey(...)` → access `key`.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi ./internal/dashboard -run 'TestNewKey|TestParseKey|TestRequireKey|TestClientBody|TestUnknownV1|TestOpenAPIRouteScanner'`

- [ ] **Step 3: Implement.** The hash is compared with `subtle.ConstantTimeCompare`. An unknown prefix still hashes the presented key. Document `GET /v1/client` (tag `Review API`, `x-pergent-access: key`, `security: [reviewApiKey: []]`, schemas `ReviewApiClient`, `ReviewApiError`) and add `reviewapi.ClientBody` and `reviewapi.ErrorBody` to `openAPISchemas`.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi ./internal/dashboard ./internal/server ./cmd/pergent-sh` passes.

- [ ] **Step 5: Commit.** `feat(reviewapi): client keys and the bearer-key gate`

### Task 3: Dashboard API for clients, keys and the secret

**Files:**
- Create: `internal/dashboard/api_clients.go`, `internal/dashboard/api_clients_test.go`
- Modify: `internal/dashboard/dashboard.go` (`d.apiClientRoutes(mux)`; `Options.DeleteJob func(ctx context.Context, name string) error`, nil-safe), `internal/dashboard/view.go` (views; the audit action list comment), `openapi.yaml`, `internal/dashboard/openapi_test.go` (schemas)

**Interfaces:**
- Consumes: Task 1 store, Task 2 `reviewapi.NewKey`, `reviewapi.NewCallbackSecret`.
- Produces (all through `adminRoute`):
  - `GET /api/admin/api-clients` → `{items: []APIClientRowView}`. `APIClientRowView{Name "name"; Keys int "keys"}` (Task 13 adds the month's counts).
  - `POST /api/admin/api-clients` with `APIClientInput{Name "name"; Skills []string "skills"; ModelID string "modelId"; CallbackURL string "callbackUrl"; Limits *APILimitsView "limits"}` → `201 {client: APIClientView, callbackSecret: string}`
  - `GET /api/admin/api-clients/{name}` → `APIClientView{Name; Skills; ModelID "modelId"; CallbackURL "callbackUrl"; PreviousSecretUntil *time.Time "previousSecretUntil,omitempty"; Limits APILimitsView; Keys []APIKeyView; CreatedAt "createdAt"}`
  - `PUT /api/admin/api-clients/{name}` with the input minus `name` (a full replace; `limits` required) → `200 APIClientView`
  - `DELETE /api/admin/api-clients/{name}` → `204`. It calls `DeleteJob` for each returned job name, best effort and logged.
  - `POST /api/admin/api-clients/{name}/secret` → `200 {callbackSecret}`. The old secret moves to `previous_secret_*` until now + 24 h.
  - `POST /api/admin/api-clients/{name}/keys` with `{name, expiresAt?}` → `201 {key: APIKeyView, secret: string}`
  - `DELETE /api/admin/api-clients/{name}/keys/{id}` → `204`
  - `APILimitsView` uses camelCase (`maxRequestBytes`, `maxFiles`, `maxInFlight`, `requestsPerHour`, `monthlyTokenBudget`). `APIKeyView{ID int64 "id"; Name; Prefix; CreatedAt; ExpiresAt *; LastUsedAt *}`.
  - Audit actions, with instance and project empty. The detail is the client name, plus the key's name and prefix where there is a key, and never a secret: `api_client_created`, `api_client_updated`, `api_client_removed`, `api_key_created`, `api_key_revoked`, `api_callback_secret_rotated`.
- Rules (`400`, lowercase messages):
  - name `^[a-z0-9-]+$`, at most 63.
  - each skill passes `inputs.ValidName`; a skill missing from the library is allowed, since the 409 at review time covers it.
  - `modelId` "" or an existing catalog ID.
  - `callbackUrl` "" or an absolute `http`/`https` URL with a host, no userinfo, no fragment, at most 2000 bytes. Builder call: an empty URL means no callbacks; private and in-cluster addresses are allowed, as the spec says.
  - limits within the CHECK ranges.
  - key name 1 to 100 characters; `expiresAt` in the future.
  - Unknown client: `404`. Name taken: `409`.

- [ ] **Step 1: Write the failing tests** (Postgres, `e.signInOperator()` for an admin):
  - `TestCreateAPIClientShowsSecretOnce`: `201`. `callbackSecret` starts `pgs_`. `GET` of the client has no `callbackSecret` key, and its raw JSON does not contain the secret.
  - `TestAPIClientRoutesAre404ForUsers`: every route above, as an SSO user (`e.aliceSession()`) → `404` with the unknown-path body.
  - `TestCreateAPIKeyShownOnce`: `secret` starts `pgk_<prefix>_`. The list shows the prefix and no secret. The audit entry `api_key_created` has detail `fastgateway: ci (0a1b2c3d)` and no `pgk_`.
  - `TestRotateSecretKeepsOld24h`: rotate → `previousSecretUntil` is now + 24 h. The audit detail holds no `pgs_`.
  - `TestAPIClientInputRules`: a table of bad inputs → `400` with these messages: `the name may hold only a-z, 0-9 and -`, `the callback URL must be an http or https URL with no user name or password`, `max files must be from 1 to 1000`, `no model has this ID`.
  - `TestDeleteAPIClientAudits`: `204`, then `GET` → `404`, and `api_client_removed` is logged.
  - Add the routes to `openapi.yaml` (`x-pergent-access: admin`) and the views to `openAPISchemas`. Check the request bodies by hand.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/dashboard -run 'TestOpenAPI|TestAdminRoutes'` fails on the undocumented routes.

- [ ] **Step 3: Implement.** The secret is sealed with `d.Auth`'s box (the same `secretbox` key as other dashboard secrets). The key's random prefix is retried up to 3 times on `ErrAPIPrefixTaken`.

- [ ] **Step 4: Verify.** `go test ./internal/dashboard` passes.

- [ ] **Step 5: Commit.** `feat(dashboard): manage API clients, their keys and callback secret`

### Task 4: Web: the API clients section

**Files:**
- Create: `web/src/pages/ApiClients.tsx`, `web/src/pages/ApiClients.test.tsx`, `web/src/pages/ApiClient.tsx`, `web/src/pages/ApiClient.test.tsx`, `web/src/OnceSecret.tsx`
- Modify: `web/src/layout/Layout.tsx` (for admins, `{ to: "/api-clients", label: "API clients", icon: "🔗" }` goes right after Repositories in the user menu), `web/src/App.tsx` (`api-clients`, `api-clients/:name`, each in `<AdminOnly>`), `web/src/api.ts` (types matching Task 3), `web/src/test/fixtures.ts`

- [ ] **Step 1: Write the failing tests**
  - `Layout.test.tsx`: an admin's user menu lists `API clients` right after `Repositories`. A user's menu doesn't.
  - `ApiClients.test.tsx`: the list shows each name and key count. **New client** posts exactly `{name, skills, modelId: "", callbackUrl}` (no `limits`), then shows the secret once in `OnceSecret`, with a copy button and the text `Copy this secret now: it is not shown again.` A user gets the not-found page.
  - `ApiClient.test.tsx`: the Settings tab PUTs the exact body `{skills, modelId, callbackUrl, limits}`. The Keys tab creates a key and shows it once, then lists name, prefix, created, expires (`never` when null) and last used (`never` when null). **Revoke** asks to confirm, then sends `DELETE`. **Rotate secret** shows the new one once and the line `The old secret also works until <time>.` **Remove client** asks to confirm with the text `Its keys stop at once, its running reviews end with no callback, and its reviews and dismissals are deleted.`

- [ ] **Step 2: Run, expect FAIL.** `cd web && npx vitest run src/pages/ApiClients.test.tsx src/pages/ApiClient.test.tsx src/layout/Layout.test.tsx`

- [ ] **Step 3: Implement.** Reuse `Tabs`, `CopyButton`, `ErrorBox`, `secretFill` from `ui.tsx`, the skill picker of the repository settings, and the model choice list (`GET /api/admin/models`). Leave the Reviews tab out until Task 14.

- [ ] **Step 4: Verify.** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build` passes.

- [ ] **Step 5: Commit.** `feat(web): API clients section with settings and keys`

### Task 5: Part 1 docs and verification

**Files:**
- Create: `docs/setup/review-api.md` (the caller guide: what a client is, create one, make a key, keep it secret, revoke, rotate the callback secret)
- Modify: `docs/README.md` (a Setup row: `[Review API](setup/review-api.md) | Let another system ask for reviews`), `docs/concepts/dashboard.md` (one paragraph on API clients, admins only), `docs/reference/limits.md` (replace "and no API tokens for scripts" with: API keys reach only the review API, never the dashboard API; they have no scopes), `docs/reference/http-routes.md` (`/v1/client`, the admin client routes), `docs/upgrading.md` (`## Unreleased` above the newest section: migration `0038` adds two tables; the previous release ignores them; no new config key)

- [ ] **Step 1: Write the docs.** Short sections, a table of what a key is and isn't.
- [ ] **Step 2: Full Go gate.** `go build ./... && go vet ./... && go test ./...` passes. Postgres tests skip locally because `PERGENT_SH_TEST_DATABASE_URL` is unset, and they first run in CI. Do not start a container.
- [ ] **Step 3: Web gate.** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build` passes.
- [ ] **Step 4: Scan.** `git add -A` then the name scan prints nothing.
- [ ] **Step 5: Commit.** `docs: the review API's clients and keys`. Part 1 is one PR.

---

# Part 2: Reviews

Ends with: `POST /v1/reviews` queues a review, a Job builds the two commits from the sent files and reviews them, `GET /v1/reviews/{id}` returns the result, and admins see the reviews. No callbacks, dismissals, previous findings, budget, rate limit or in-flight cap yet. The size and file limits do hold here, because the files' Secret needs them.

### Task 6: Store: API reviews in `runs`

**Files:**
- Create: `internal/store/migrations/0039_api_reviews.sql`, `internal/store/apireviews.go`, `internal/store/apireviews_test.go`
- Modify: `internal/store/runs.go` (`Run.Kind string`, `Run.APIClientID *int64`, `runColumns`, `scanRun`; consts `KindMR = "mr"`, `KindAPI = "api"`, `OutcomeNothingReviewed = "nothing_reviewed"`)

**Interfaces:**
- Produces:
  - `type NewAPIReview struct{ ClientID int64; Tenant, Subject, Title, KeyName, IdempotencyKey string; RequestHash []byte; BaseSHA, HeadSHA string; InputsSealed []byte; FileCount, FileBytes int }`
  - `type APIReview struct{ Run Run; ClientID int64; ClientName, Tenant, Subject, Title, KeyName string; Response []byte; FinishedAt time.Time; HasInputs bool; CreatedAt time.Time }`
  - `var ErrIdempotencyMismatch = errors.New("store: idempotency key reused with another request")`
  - `func (s *Store) EnqueueAPIReview(ctx context.Context, n NewAPIReview) (id string, created bool, err error)`
  - `func (s *Store) APIReview(ctx context.Context, clientID int64, id string) (APIReview, bool, error)` (another client's ID → not found)
  - `func (s *Store) APIReviewByID(ctx context.Context, id string) (APIReview, bool, error)` (for admins)
  - `type APIReviewFilter struct{ ClientID int64; Tenant, Subject, Status string; After *APIReviewCursor; Limit int }` with `type APIReviewCursor struct{ CreatedAt time.Time; ID string }`, `func (s *Store) APIReviews(ctx context.Context, f APIReviewFilter) ([]APIReview, error)` (newest first; `Status` is a v1 status, Task 10's `reviewapi.StatusOf` in SQL form)
  - `func (s *Store) APIReviewInputs(ctx context.Context, runID string) ([]byte, bool, error)`, `DropAPIInputs(ctx, runID string) error`, `DropSpentAPIInputs(ctx context.Context) (int, error)`
  - `func (s *Store) FinishAPIReview(ctx context.Context, runID string, response []byte, dec RunDecision) error` (one transaction: `MarkPublished`'s update, `api_reviews.response`, `finished_at = now()`)

- [ ] **Step 1: Write the migration**

```sql
-- API reviews. An API review is a run of kind 'api' with no merge request:
-- instance '', project_id 0, and mr_iid a number of its own, so the live-run
-- index never joins two of them. api_reviews holds what the caller sent;
-- inputs_sealed (the fast-import stream and the description) is cleared
-- once the Job has it. Only adds: the previous release reads neither column
-- and, finding no repository '', supersedes a queued API review.
ALTER TABLE runs ADD COLUMN kind text NOT NULL DEFAULT 'mr' CHECK (kind IN ('mr', 'api'));
ALTER TABLE runs ADD COLUMN api_client_id bigint REFERENCES api_clients (id) ON DELETE CASCADE;
ALTER TABLE runs ADD CONSTRAINT runs_api_shape
    CHECK (kind = 'mr' OR (instance = '' AND project_id = 0 AND api_client_id IS NOT NULL));
CREATE INDEX runs_api_client_idx ON runs (api_client_id, state) WHERE api_client_id IS NOT NULL;
CREATE SEQUENCE api_review_numbers;

CREATE TABLE api_reviews (
    run_id          uuid        PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    client_id       bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    tenant          text        NOT NULL DEFAULT '',
    subject         text        NOT NULL,
    title           text        NOT NULL DEFAULT '',
    key_name        text        NOT NULL DEFAULT '',
    idempotency_key text,
    request_hash    bytea,
    inputs_sealed   bytea,
    file_count      integer     NOT NULL,
    file_bytes      integer     NOT NULL,
    response        jsonb,
    finished_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_reviews_subject_idx ON api_reviews (client_id, tenant, subject, created_at);
CREATE INDEX api_reviews_client_idx ON api_reviews (client_id, created_at);
CREATE UNIQUE INDEX api_reviews_idempotency_idx ON api_reviews (client_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
```

- [ ] **Step 2: Write the failing tests** (Postgres):
  - `TestEnqueueAPIReviewShape`: the run has `Kind == "api"`, `MR.Instance == ""`, `MR.ProjectID == 0`, `MR.IID > 0`, `State == Queued`, `QuotaChecked`, `BaseSHA` and `HeadSHA` as given, `Trigger == "push"`. `APIReview(clientID, id)` returns tenant, subject and `HasInputs`.
  - `TestEnqueueAPIReviewSupersedesSubject`: two enqueues for (client, `acme`, `route:1`) → the first is `superseded`. One for tenant `""` and the same subject leaves both alone. One for another client leaves both alone.
  - `TestEnqueueAPIReviewKeepsReported`: a reported (not yet published) review of the subject is not superseded.
  - `TestEnqueueAPIReviewSameContentTwoSubjects`: the same `HeadSHA` for two subjects → both stay queued (no unique violation).
  - `TestEnqueueAPIReviewIdempotent`: the same key and hash → the same ID with `created == false`, nothing superseded, also after the first was published. The same key with another hash → `ErrIdempotencyMismatch`. The same key under another client → a new review.
  - `TestAPIReviewOtherClientNotFound`: client B's ID with client A → `found == false`.
  - `TestDropAPIInputs`: after `DropAPIInputs`, `APIReviewInputs` → `found == false`. `DropSpentAPIInputs` clears a superseded review's inputs and one with a `job_name`, and keeps a queued one's.
  - `TestFinishAPIReview`: a reported review → `published`, `Outcome` recorded, `Response` and `FinishedAt` set. A queued review → error.
  - `TestAPIReviewsFilter`: filter by tenant, by subject and by status `queued`, `running`, `completed`, `failed`, `superseded`. The cursor pages.

- [ ] **Step 3: Run, expect FAIL.** `go vet ./internal/store` fails with `undefined: NewAPIReview`.

- [ ] **Step 4: Implement.** One transaction under `pg_advisory_xact_lock(hashtextextended('api/'||client||'/'||tenant||'/'||subject, 0))`:
  1. idempotency lookup;
  2. supersede the subject's `queued`/`running` API runs (Builder call: only those, as for an MR; a reported review finishes);
  3. insert the run with `mr_iid = nextval('api_review_numbers')`;
  4. insert `api_reviews`.

  The idempotency key is checked under a second lock on `'apikey/'||client||'/'||key`, so two concurrent retries make one review.

- [ ] **Step 5: Verify.** `go build ./... && go vet ./... && go test ./internal/store` passes.

- [ ] **Step 6: Commit.** `feat(store): API reviews as runs of their own kind`

### Task 7: Store: keep API reviews out of the MR views; removal ends live reviews

**Files:**
- Modify: `internal/store/dashboard_read.go` (`reviewDigestsSQL`: `AND r.kind = 'mr'`), `internal/store/usage.go` (`UsageByRepository`, `UsageRows` and every `MetricsTotals` query on `run_usage`/`runs` read only rows with `instance <> ''`), `internal/store/apiclients.go` (`DeleteAPIClient` returns the job names of the client's runs in `running` with a `job_name`, read in the same transaction as the delete), `internal/dashboard/api_read.go` (`visibleRun` answers not found for `run.Kind != store.KindMR`)
- Test: `internal/store/apireviews_test.go`, `internal/dashboard/api_read_test.go`

- [ ] **Step 1: Write the failing tests**
  - `TestReviewDigestsLeaveOutAPIReviews` and `TestUsageByRepositoryLeavesOutAPIReviews` (Postgres): an API review with usage rows plus an MR run → only the MR is listed.
  - `TestRunStatsCountAPIReviews`: `RunStats` with `Visibility{All: true}` counts the API review under key `{"", ""}`. Builder call: the admin overview's counts include API reviews, as the spec asks.
  - `TestMetricsTotalsLeaveOutAPIReviews`: no row of any `MetricsTotals` list has instance `""`.
  - `TestDeleteAPIClientReturnsLiveJobs`: a running review with job `pergent-review-abc` → `jobs == ["pergent-review-abc"]`. After the delete the run, its `api_reviews` row and its `run_usage` rows are gone.
  - `TestRunRoutesHideAPIReviews` (dashboard): an admin's `GET /api/runs/{id}`, `/log` and `/diagnostics` of an API review → `404`.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/store ./internal/dashboard -run 'LeaveOutAPI|CountAPI|ReturnsLiveJobs|HideAPIReviews'` (in CI; locally the vet must pass and the DB-free dashboard test fails)

- [ ] **Step 3: Implement** as listed. Builder call: the usage CSV leaves API reviews out; the Usage page gets them per client in Task 19.

- [ ] **Step 4: Verify.** `go test ./internal/store ./internal/dashboard` passes.

- [ ] **Step 5: Commit.** `feat(store): keep API reviews apart from merge request views`

### Task 8: The sent files: rules, the fast-import stream, the two SHAs

**Files:**
- Create: `internal/reviewapi/files.go`, `internal/reviewapi/files_test.go`

**Interfaces:**
- Produces:
  - `type File struct{ Path string "path"; Old *string "old"; New *string "new" }`
  - `type FileError struct{ Index int; Message string }` (`Error()` = `files[<i>]: <message>`)
  - `func CheckFiles(files []File) error` (a `*FileError`, or a plain error for the list as a whole)
  - `type Import struct{ Stream []byte; Base, Head string; Bytes int }` (`Bytes` = total old and new content)
  - `func BuildImport(files []File) (Import, error)`
  - `const Committer = "pergent <pergent@pergent.sh> 0 +0000"`
- Rules. Each failure is a `422 invalid`.
  - From the spec: UTF-8 content; a relative path, unique, with no `..` segment; a file has `old`, `new` or both.
  - Builder call, because git can't store these: a path is not empty and at most 1024 bytes, valid UTF-8, with no control character (below `0x20`, or `0x7f`) and no `\`. It has no leading or trailing `/`, no empty, `.` or `..` segment, and no segment equal to `.git` in any case.
  - Builder call: within the old set, and within the new set, no path is a directory of another (`a` and `a/b`).
  - At least one file, or `files must not be empty`.
  - Messages: `the path must be relative`, `the path must not hold .. or .`, `the path appears twice`, `the path holds a character git can't store`, `old is not UTF-8 text`, `new is not UTF-8 text`, `a file needs old, new or both`, `the path is also a directory in the same side`.
- Algorithm (pinned; the Job and the engine must agree byte for byte):

```
Stream: for each file in path order, one "blob\nmark :<n>\ndata <len>\n<bytes>\n" per old and per new content.
commit refs/heads/base / committer <Committer> / data 5 / "base\n" / one "M 100644 :<mark> <quoted path>" per old file
commit refs/heads/head / committer <Committer> / data 5 / "head\n" / from refs/heads/base / deleteall / one M line per new file
quoted path = '"' + path with \ → \\ and " → \" + '"'
SHAs: blob = sha1("blob <len>\0" + bytes). tree entries are "<mode> <name>\0<20 bytes>", mode 100644 or 40000,
sorted by name, with a tree's name compared as name + "/". commit = sha1("commit <len>\0" + body), body =
"tree <hex>\n" [+ "parent <base>\n"] + "author <Committer>\ncommitter <Committer>\n\n" + "base\n" | "head\n".
An empty side is the empty tree.
```

- [ ] **Step 1: Write the failing tests**
  - `TestCheckFiles`: a table with one case per rule and message above, plus good cases (`a/b.yaml` new only; `x` old only; a path with spaces and `é`).
  - `TestBuildImportShas`: one new file `a.txt` with `hi\n` → `Base` is the commit of the empty tree, and a fixed `Head` written into the test once it agrees with git.
  - `TestImportMatchesGit` (skips if `git` is not on `PATH`): `git init`, `git fast-import --quiet` with the stream, then `git rev-parse refs/heads/base refs/heads/head` equals `Base`/`Head`. Cases: nested directories; `a` and `a.b` and `a/c` ordering; a deleted file; an unchanged file; a file with CRLF; `.gitattributes` holding `* text eol=crlf` and `*.yaml filter=lfs`; a path with `"` and `\`-free UTF-8; empty base.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi -run 'TestCheckFiles|TestBuildImport|TestImportMatchesGit'`

- [ ] **Step 3: Implement** `CheckFiles` and `BuildImport` as pinned.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi` passes, with `TestImportMatchesGit` run, not skipped.

- [ ] **Step 5: Commit.** `feat(reviewapi): build a change's two commits from the sent files`

### Task 9: Kube: the API review Job

**Files:**
- Create: `internal/kube/apicheckout.sh`, `internal/kube/apijob_test.go`
- Modify: `internal/kube/job.go` (`JobParams.Import []byte`, `JobParams.APIClient string`, `LabelAPIClient = "pergent.sh/api-client"`, `func BuildFilesSecret(p JobParams, owner *batchv1.Job) *corev1.Secret`), `internal/kube/launcher.go` (`Launch` creates the files Secret when `p.Import != nil`; `func (l *Launcher) DeleteFiles(ctx context.Context, jobName string) error`, where NotFound is not an error)

**Interfaces:**
- Produces:
  - With `p.Import != nil`, the checkout container runs `apicheckout.sh` with env `BASE_SHA`, `HEAD_SHA` and no `GIT_TOKEN`. It mounts the Secret `<name>-files` (key `import`, `0400`) read-only at `/files`. The pergent container never mounts it.
  - Labels: `pergent.sh/api-client=<name>` (left out when invalid). No instance, project or MR labels, since they are zero.
  - `const FilesSecretMaxBytes = 1_000_000`: `Launch` refuses a larger stream before creating anything.
- Script (pinned):

```sh
set -eu
repo="${REPO_DIR:-/work/repo}"; import="${IMPORT_FILE:-/files/import}"
git init -q "$repo"; cd "$repo"
git config core.hooksPath /dev/null
git fast-import --quiet < "$import"
[ "$(git rev-parse refs/heads/base)" = "$BASE_SHA" ] || { echo "the built base commit is not $BASE_SHA" >&2; exit 1; }
[ "$(git rev-parse refs/heads/head)" = "$HEAD_SHA" ] || { echo "the built head commit is not $HEAD_SHA" >&2; exit 1; }
git checkout -q --detach "$HEAD_SHA"
```

- [ ] **Step 1: Write the failing tests**
  - `TestAPIJobShape`: `BuildJob` with `Import` set → the checkout command holds `apicheckout.sh`. The checkout env has no `GIT_TOKEN`, `REPO_URL` or `MR_REF`. Only the checkout container mounts `files`. The volume is a Secret volume of `<name>-files` with item `import`. The labels hold `pergent.sh/api-client` and no `pergent.sh/instance`.
  - `TestJobUnchangedWithoutLinks` still passes against the golden: the MR Job is unchanged.
  - `TestLaunchAPIJobCreatesFilesSecret` (fake clientset): the Job, `-secrets`, `-inputs` and `-files` exist, the last owned by the Job. `DeleteFiles` removes it, and a second call returns nil.
  - `TestLaunchRefusesLargeImport`: a 1,000,001-byte stream → error, and no Job is created.
  - `TestAPICheckoutScript` (skips without `git`): run the real script under `sh` with `REPO_DIR` and `IMPORT_FILE` in a temp dir and a stream from `reviewapi.BuildImport`, including Task 8's `.gitattributes` case → exit 0, and `git rev-parse HEAD` equals `Head`. A wrong `BASE_SHA` → exit 1 with `the built base commit is not`.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/kube -run 'TestAPIJob|TestLaunchAPIJob|TestLaunchRefusesLarge|TestAPICheckoutScript|TestJobUnchanged'`

- [ ] **Step 3: Implement.** `kube` must not import `reviewapi`, so the script test builds its stream with a copy of the format, or imports `reviewapi` only from the `_test` package.

- [ ] **Step 4: Verify.** `go test ./internal/kube` passes.

- [ ] **Step 5: Commit.** `feat(kube): an API review Job builds its commits from a Secret of its own`

### Task 10: The v1 result

**Files:**
- Create: `internal/reviewapi/body.go`, `internal/reviewapi/body_test.go`

**Interfaces:**
- Produces:
  - `type Review struct{ ID "id"; Subject "subject"; Tenant "tenant"; Title "title"; Status "status"; CreatedAt time.Time "created_at"; FinishedAt *time.Time "finished_at,omitempty"; Verdict string "verdict,omitempty"; Skills []Skill "skills,omitempty"; Failure *Failure "failure,omitempty"; Usage *Usage "usage,omitempty"; Test bool "test,omitempty" }`
  - `type Skill struct{ Name; Verdict; Findings []Finding "findings"; Files Files "files" }`, `type Files struct{ Reviewed []string "reviewed"; Skipped []Skipped "skipped"; NotReviewed []NotReviewed "not_reviewed" }`, `Skipped{Path, Rule}`, `NotReviewed{Path, Reason}`
  - `type Finding struct{ ID, Fingerprint, File string; Line, EndLine int "line,omitempty"/"end_line,omitempty"; Severity, Title, Explanation string; Fix string "fix,omitempty"; Suggestion string "suggestion,omitempty"; Dismissed *Dismissed "dismissed,omitempty" }`, `type Dismissed struct{ Reason string "reason"; At time.Time "at" }`
  - `type Failure struct{ Step, Reason string }`, `type Usage struct{ TokensIn, TokensOut int64 "tokens_in"/"tokens_out" }`
  - Enums, fixed for v1:
    - status: `queued running completed failed superseded`
    - verdict: `failed nothing_reviewed has_blocking_issues no_blocking_issues`
    - failure step: `starting review delivery unknown`
    - skip rule: `deleted binary no_content_changes lockfile vendored generated out_of_scope excluded_by_skill other`
  - `type DismissalRef struct{ Fingerprint, Severity, Reason string; At time.Time }`
  - `func Map(res *result.Result, usage []store.SkillUsage, dismissals []DismissalRef, redact func(string) string) (verdict string, skills []Skill, u Usage)`
  - `func StatusOf(r store.APIReview) string`, `func FailureOf(r store.APIReview) *Failure`, `func Pending(r store.APIReview) Review` (the body before a stored response: queued, running, superseded)
- Rules (pinned):
  - Verdict: the first that applies, as the spec says. A skill's own verdict is pergent's, with two changes. It is `nothing_reviewed` when it is not failed and `files.reviewed` is empty. It turns `has_blocking_issues` into `no_blocking_issues` when every blocking finding is dismissed. Builder call: the per-skill `nothing_reviewed`, so no skill reads as a pass over nothing.
  - Dismissal coverage is `render.ApplyDismissals`'s: a `blocking` dismissal covers both severities, a `suggestion` one only suggestions. A dismissed finding stays in `findings` with `dismissed`.
  - Finding IDs come from `result.ToRender`. `fix` is pergent's `suggested_fix`.
  - Skip rules map as `"no content changes"` → `no_content_changes`, `"out of scope"` → `out_of_scope`, `"excluded by skill"` → `excluded_by_skill`. An unknown rule becomes `other`.
  - `not_reviewed` entries carry pergent's `cause` as `reason`. Spec gap: a file too large for a batch is a `not_reviewed` entry and fails its skill; it is not a skip.
  - Every text a model or the checkout wrote goes through `redact`.
  - Status: `queued` while queued, or running with no `job_name`. `running` with a Job, or reported. `superseded`. Once published, `completed` when the result has status `completed`, else `failed`. `publish_failed` is `failed` with step `unknown` and reason `pergent.sh couldn't finish the review`. Builder call: `failed` means no usable result; a completed review with a failed skill is `completed` with verdict `failed`.
  - Step: `""` → `starting` (the review failed before its Job), `clone` → `starting` (the checkout that builds the commits), `review` and `delivery` as they are, anything else `unknown`.

- [ ] **Step 1: Write the failing tests** (pure, from `internal/result/testdata` and inline results):
  - `TestVerdictOrder`: one skill failed and one blocking → `failed`. All skills with `reviewed: []` → `nothing_reviewed`. A blocking finding → `has_blocking_issues`. Its blocking dismissal → `no_blocking_issues`, and the finding carries `dismissed.reason`. A suggestion dismissal on a blocking finding → still `has_blocking_issues`.
  - `TestSkillNothingReviewed`: a skill with `reviewed: []` and a skill with findings → the first is `nothing_reviewed`, the overall verdict follows the second.
  - `TestSkipRulesMapped`: each pergent rule maps as pinned, and `"new rule"` → `other`.
  - `TestNotReviewedCarriesCause`: an oversize file → `not_reviewed[0].reason` is pergent's cause, and the skill is `failed`.
  - `TestMapRedacts`: a finding explanation holding `sk-secret` with a redactor for it → `***`.
  - `TestStatusOf`: one case per state above. `TestFailureStep`: `""`, `clone`, `review`, `delivery`, `odd` → `starting starting review delivery unknown`.
  - `TestPendingBody`: a queued review marshals to exactly `{"id":"…","subject":"route:1","tenant":"acme","title":"","status":"queued","created_at":"…"}`.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi -run 'TestVerdict|TestSkill|TestSkipRules|TestNotReviewed|TestMapRedacts|TestStatusOf|TestFailureStep|TestPendingBody'`

- [ ] **Step 3: Implement.** `Map` and the helpers as pinned.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi` passes.

- [ ] **Step 5: Commit.** `feat(reviewapi): the v1 review result`

### Task 11: `POST` and `GET /v1/reviews`

**Files:**
- Create: `internal/reviewapi/reviews.go`, `internal/reviewapi/reviews_test.go`
- Modify: `internal/reviewapi/handler.go` (routes; `Store` gains `EnqueueAPIReview`, `APIReview`, `ResolveLibrary`), `cmd/pergent-sh/main.go`, `openapi.yaml` (`POST /v1/reviews`, `GET /v1/reviews/{id}`, schemas `ReviewApiRequest`, `ReviewApiReview` and its parts), `internal/dashboard/openapi_test.go` (schemas)

**Interfaces:**
- Consumes: Task 6 store, Task 8 `CheckFiles`/`BuildImport`, Task 10 `Pending`.
- Produces:
  - `type Request struct{ Subject "subject"; Tenant "tenant"; Title "title"; Description "description"; Files []File "files" }`
  - `type Sealed struct{ Import []byte "import"; Description string "description" }`: the JSON that `Box.Seal` turns into `inputs_sealed`. Builder call: the description travels sealed with the files and is dropped with them; only the title stays.
  - `POST /v1/reviews` → `202` with `Pending(...)` (status `queued`), and `Location: /v1/reviews/<id>`.
  - `GET /v1/reviews/{id}` → `200` with the stored response once published, else `Pending`.
- Order of checks (pinned):
  1. `RequireKey`.
  2. Body read through `http.MaxBytesReader(min(client.MaxRequestBytes, store.HardMaxRequestBytes))`, else `413 too_large` `the request is over <n> bytes`.
  3. An `Idempotency-Key` header (1 to 200 printable ASCII) whose review exists → `202` with that review's current body, or `422 idempotency_key_reused`.
  4. JSON decode, with unknown fields ignored. Malformed JSON → `422 invalid` `the body is not valid JSON`.
  5. Field rules → `422 invalid`. `subject`: 1 to 200 characters. `tenant`: at most 100. `title`: at most 200 (Builder call). None of the three holds a control character. `description`: at most 32 KiB, UTF-8. Then `CheckFiles`.
  6. More files than `MaxFiles`, or a stream over `kube.FilesSecretMaxBytes` → `413 too_large`.
  7. `409 cannot_review`: no skills (`this client has no skills: an admin chooses them in its settings`), a skill not in the library (`skill "<name>" is not in the skill library`), or the model unusable (`this client's model is no longer in the model catalog`).
  8. Enqueue, then `Wake()`.
  - `RequestHash` is SHA-256 of the raw body.

- [ ] **Step 1: Write the failing tests** (fake store and models, no database):
  - `TestCreateReviewQueues`: a good request → `202`, `status: queued`, `Location`. The fake store got the `Base`/`Head` of `BuildImport` and `KeyName == "ci"`. Opening `InputsSealed` gives the stream and the description.
  - `TestCreateReviewRules`: one case per rule above → its status, `code` and message.
  - `TestCreateReviewTooLarge`: a body one byte over `MaxRequestBytes` → `413`, and nothing is enqueued. `MaxFiles + 1` files → `413`.
  - `TestCreateReviewCannotReview`: no skills, a missing skill, the model gone → `409`, each with its message.
  - `TestCreateReviewIdempotent`: the same key twice → the same ID, one enqueue, the second answer `202`. The same key with another body → `422 idempotency_key_reused`.
  - `TestGetReview`: queued → the pending body. Published → the stored response byte for byte.
  - `TestOtherClientsReviewIsNotFound`: client B's ID → `404`, with exactly the body of an unknown ID, and of a malformed one.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi -run 'TestCreateReview|TestGetReview|TestOtherClients'`

- [ ] **Step 3: Implement.** Document both routes, with the field rules and enums, in `openapi.yaml`.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi ./internal/dashboard` passes.

- [ ] **Step 5: Commit.** `feat(reviewapi): ask for a review and read it`

### Task 12: Engine: dispatch, watch, publish and clean up API reviews

**Files:**
- Create: `internal/engine/apireview.go`, `internal/engine/e2e_api_test.go`
- Modify: `internal/engine/dispatch.go` (`DispatchOnce`: `if run.Kind == store.KindAPI { err = d.dispatchAPI(ctx) }`), `internal/engine/watch.go`, `internal/engine/publish.go` (`PublishOnce` branch; `cleanUp` skips the status block for API runs; `giveUp` returns early for API runs), `internal/engine/engine.go` (`pass` adds `DropSpentAPIInputs`, logged at info), `internal/dashboard/api_clients.go` (`DELETE` calls `DeleteJob`), `cmd/pergent-sh/main.go` (`DeleteJob: launcher.Delete`)

**Interfaces:**
- Produces:
  - `func (d *dispatcher) dispatchAPI(ctx context.Context) error`
  - `func (e *Engine) publishAPI(ctx context.Context, run store.Run, log *slog.Logger) error`
  - Failure reasons, which reach the caller: `reasonClientGone = "the API client was removed"`, `reasonClientModel = "the model chosen for this API client can't be used: an admin can change it in the client's settings"`, `reasonInputsGone = "the review's files are no longer stored"`, `reasonClientNoSkills = "this client has no skills: an admin chooses them in its settings"` (the same text as the `409`). A missing skill reuses `skill %q is not in the skill library`.
- `dispatchAPI` steps (pinned):
  1. Load the client by `run.APIClientID`. Missing → `d.fail(reasonClientGone)`.
  2. Resolve the skills with `ResolveLibrary(client.Skills)`. None, or one missing → `d.fail`.
  3. The model: `ModelByID(client.ModelID)`, or `DefaultModel()` when the ID is "". Unusable → `d.fail(reasonClientModel)`. A default with no key → `postpone(waitDelay)`, as for an MR.
  4. Timeout, step limit and memory come from `e.settings().BrowserDefaults(ctx, "", "")`. Builder call: the dashboard's defaults, as for a repository with no overrides.
  5. Open the sealed inputs. Missing → `d.fail(reasonInputsGone)`.
  6. `in := &inputs.Inputs{Skills, Sources: library, Versions}`, then `in.AddDescription(title, description)`, then `in.Fit(inputs.ConfigMapLimit)`.
  7. `SetRunModel`, mint the token, then `Launch` with `JobParams{Import, APIClient: client.Name, BaseSHA: run.BaseSHA, HeadSHA: run.HeadSHA, MaxCommentSize: kube.CommentLimitGitLab, ResultURL, LLM, Inputs, ...}` and `kube.Secrets{LLMKey, ResultToken}`.
  8. `MarkDispatched(run.BaseSHA, "")`, then `DropAPIInputs`, then `SetRunInputs` (sources `library`).
  - No forge call, no status and no attention item.
- `watch` changes, all for API runs only:
  - `SetCloneSeconds`, `refreshOutOfMemory` and `refreshCloneFailing` run only for `KindMR`.
  - `Launcher.DeleteFiles` is called when the checkout is first seen done (`run.CheckoutSeen < 2 && seen >= 2`), and before reporting a stuck, vanished or finished Job. A failure is logged; the Job's deletion is the backstop.
- `publishAPI`:
  - Parse `run.Result` when there is no failure reason.
  - Get usage from `usage.FromResult`.
  - Run `reviewapi.Map(res, usage, nil, e.redact)`.
  - Build the `Review` body with status, failure and finished time.
  - Call `FinishAPIReview` with the decision: `failed` → `OutcomeFailed`, `has_blocking_issues` → `OutcomeBlocking`, `nothing_reviewed` → `OutcomeNothingReviewed`, `no_blocking_issues` → `OutcomeNoBlocking`. A failed review is `OutcomeFailed`.
  - A result that can't be read is published as status `failed`, step `delivery`, reason `pergent sent a result pergent.sh couldn't use`. It is never a pass.

- [ ] **Step 1: Write the failing tests** (Postgres, fake clientset; in `e2e_api_test.go` with `newE2E`):
  - `TestAPIReviewEndToEnd`: enqueue a review of two files, then `DispatchOnce` → a Job with `-files`, the inputs dropped, `MarkDispatched`. Post a completed result for the run's `Base`/`Head` to the result handler, then `PublishOnce` → published. `GET` body status `completed`, verdict `has_blocking_issues`, `usage.tokens_in` from the result.
  - `TestAPIReviewSharesMaxRunning`: `jobs.max_running: 1`, an MR run queued first and an API review second → one pass dispatches only the MR.
  - `TestAPIReviewNoSkillsFails`: the client's skills emptied after the enqueue → published `failed`, step `starting`, reason `reasonClientNoSkills`. No Job.
  - `TestAPIReviewModelGoneFails`: `reasonClientModel`.
  - `TestAPIReviewWrongCommitFails`: a result for another head → status `failed`, step `delivery` (the existing check).
  - `TestWatchDeletesFilesAfterCheckout`: tick one, checkout running → the files Secret stays. Tick two, checkout done → the Secret is gone. No `projects` row and no attention row is written.
  - `TestSupersededAPIReviewCleanedUp`: a second enqueue of the subject while the first runs → `CleanupOnce` deletes the first Job, sets no status and marks it cleaned up.
  - `TestRemoveClientDeletesLiveJobs` (dashboard): `DELETE` of a client whose review runs → `DeleteJob` is called with its job name. A later `POST /runs/{id}/result` for that run → `401`, and no row exists.
  - `TestMRDispatchUnchanged`: the existing `dispatch_test.go` suite passes as is.

- [ ] **Step 2: Run, expect FAIL.** `go vet ./internal/engine` fails with `undefined: dispatchAPI`. In CI the e2e tests fail.

- [ ] **Step 3: Implement** as pinned.

- [ ] **Step 4: Verify.** `go test ./internal/engine ./internal/dashboard ./internal/callback` passes.

- [ ] **Step 5: Commit.** `feat(engine): run API reviews through the queue, the watcher and the publisher`

### Task 13: Dashboard: a client's reviews and an API review's page

**Files:**
- Create: `internal/dashboard/api_client_reviews.go`, `internal/dashboard/api_client_reviews_test.go`
- Modify: `internal/store/apireviews.go` (`func (s *Store) APIClientStats(ctx context.Context, since time.Time) (map[int64]APIClientStats, error)` with `APIClientStats{Reviews, Failed, Tokens int64}`), `internal/dashboard/view.go`, `internal/dashboard/api_clients.go` (list rows gain `reviewsThisMonth`, `failedThisMonth`, `tokensThisMonth`, from the 1st of the UTC month), `openapi.yaml`, `openapi_test.go`

**Interfaces:**
- Produces (admin routes):
  - `GET /api/admin/api-clients/{name}/reviews?tenant=&subject=&status=&cursor=` → `{items: []APIReviewRowView, nextCursor}`. A row has `id`, `tenant`, `subject`, `title`, `status`, `verdict`, `createdAt`, `finishedAt`.
  - `GET /api/admin/api-reviews/{id}` → `APIReviewDetailView`. It has `id`, `client`, `tenant`, `subject`, `title`, `keyName`, `status`, `model`, `createdAt`, `finishedAt`, `failure: {step, reason, detail}` (admins see the detail), and `review: json.RawMessage`, which is the v1 body (`$ref` `ReviewApiReview`).
  - `GET /api/admin/api-reviews/{id}/log` → `LogView`. `GET /api/admin/api-reviews/{id}/diagnostics` → `DiagnosticsView` with `podState`.
  - A bad `status` is `400 status must be one of queued, running, completed, failed, superseded`. An unknown or MR run ID is `404`.

- [ ] **Step 1: Write the failing tests** (Postgres): `TestClientReviewsFilter` (tenant, subject, status, cursor), `TestAPIReviewDetail` (the `review` key equals the stored response; `failure.step` is `starting` for a dispatch failure), `TestAPIReviewDetailRefusesMRRun`, `TestClientListMonthCounts` (two reviews, one failed, the token sum, and last month's review left out). Add the routes and views to `openapi.yaml` and `openAPISchemas`.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/dashboard -run 'TestOpenAPI|TestAdminRoutes'`

- [ ] **Step 3: Implement** the routes and `APIClientStats`.

- [ ] **Step 4: Verify.** `go test ./internal/dashboard ./internal/store` passes.

- [ ] **Step 5: Commit.** `feat(dashboard): list a client's reviews and show an API review`

### Task 14: Web: the reviews tab and the API review page

**Files:**
- Create: `web/src/pages/ApiReview.tsx`, `web/src/pages/ApiReview.test.tsx`
- Modify: `web/src/pages/ApiClient.tsx` (a Reviews tab first, with filters), `web/src/pages/ApiClients.tsx` (the month columns), `web/src/App.tsx` (`api-clients/:name/reviews/:id` in `<AdminOnly>`), `web/src/api.ts`, `web/src/test/fixtures.ts`

- [ ] **Step 1: Write the failing tests**
  - The Reviews tab lists rows and filters by tenant, subject and status (the query string is exact).
  - The review page shows subject, tenant, key name, status, the verdict pill and, per skill, findings (file:line, severity, title, explanation, fix, suggestion as text), reviewed files and skipped files with their rule. A failed review shows `Failed at <step>: <reason>`.
  - The Logs tab reads `/log` and `/diagnostics`.
  - There is no re-run button: `queryByRole("button", {name: /re-run/i})` is null.
  - The list shows `reviewsThisMonth`, `failedThisMonth` and `tokensThisMonth`.

- [ ] **Step 2: Run, expect FAIL.** `cd web && npx vitest run src/pages/ApiReview.test.tsx src/pages/ApiClient.test.tsx src/pages/ApiClients.test.tsx`

- [ ] **Step 3: Implement.** Model text renders as text, never HTML. Reuse `OutcomePill`'s tones for verdicts.

- [ ] **Step 4: Verify.** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build` passes.

- [ ] **Step 5: Commit.** `feat(web): an API client's reviews and the API review page`

### Task 15: Part 2 docs and verification

**Files:**
- Modify:
  - `docs/setup/review-api.md`: send a review (curl), the file rules, statuses, verdicts, read the result, re-send instead of re-run, strip secrets before sending.
  - `docs/setup/install.md`: the "Exposing" table gains a `/v1/` row: outside systems call it; expose it where they reach it; only with the dashboard on.
  - `docs/reference/http-routes.md`: the `/v1/reviews` routes.
  - `docs/concepts/how-it-works.md`: one line.
  - `docs/upgrading.md`, under `## Unreleased`: migration `0039`. On rollback the previous release supersedes queued API reviews, can't publish reported ones, and shows them under an empty repository in Usage.

- [ ] **Step 1: Write the docs.**
- [ ] **Step 2: Full Go gate.** `go build ./... && go vet ./... && go test ./...` passes. The Postgres tests skip locally and run in CI. No container.
- [ ] **Step 3: Web gate.** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build` passes.
- [ ] **Step 4: Scan.** `git add -A`, then the name scan prints nothing.
- [ ] **Step 5: Commit.** `docs: send a change to the review API and read the result`. Part 2 is one PR.

---

# Part 3: Callbacks, re-reviews and limits

Ends with: every finished review is sent, signed, to the client's URL with retries. Findings can be dismissed by key or by an admin. A later review of the subject knows its previous findings and dismissals. All four limits hold. The guide and `openapi.yaml` are complete.

### Task 16: Signed callbacks with retries

**Files:**
- Create: `internal/store/migrations/0040_api_callbacks.sql`, `internal/store/apicallbacks.go`, `internal/store/apicallbacks_test.go`, `internal/reviewapi/sign.go`, `internal/reviewapi/sign_test.go`, `internal/engine/apicallbacks.go`, `internal/engine/apicallbacks_test.go`
- Modify:
  - `internal/store/apireviews.go`: `EnqueueAPIReview` inserts a callback row for each review it supersedes; `FinishAPIReview` inserts one for the finished review; both only when the client's `callback_url` is not empty.
  - `internal/engine/publish.go`: `giveUp` for an API run calls `Store.EnqueueAPICallback`, best effort.
  - `internal/engine/attention.go`: `attCallbackFailing = "callback_failing"`.
  - `internal/dashboard/api_read.go`: `attention()` shows `callback_failing` items, which have instance `""` and the client name as project, to admins only, with `AttentionView.Client string "client,omitempty"`.
  - `cmd/pergent-sh/main.go`: the `eng.RunCallbacks` loop.

**Interfaces:**
- Produces:
  - `func Sign(body []byte, t time.Time, secrets ...string) string` → `t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>` with one `v1=` per secret, in order.
  - `type APICallback struct{ ID, RunID string; ClientID int64; ClientName, URL string; SecretSealed, PreviousSealed []byte; PreviousUntil, CreatedAt time.Time; Attempts int }`
  - `func (s *Store) DueAPICallbacks(ctx context.Context, now time.Time, limit int) ([]APICallback, error)`
  - `type CallbackAttempt struct{ At time.Time; StatusCode int; Error string; Duration time.Duration }`
  - `func (s *Store) RecordAPICallbackAttempt(ctx context.Context, id string, a CallbackAttempt, delivered bool, next time.Time, gaveUp bool) error`
  - `func (s *Store) EnqueueAPICallback(ctx context.Context, runID string) error` (a no-op when one exists)
  - `func (s *Store) APICallbackAttempts(ctx context.Context, runID string) (state string, attempts []CallbackAttempt, err error)` (state `none`, `pending`, `delivered`, `gave_up`)
  - `func (s *Store) CountUndeliveredAPICallbacks(ctx context.Context, clientID int64, since time.Time) (int, error)`
  - `func (e *Engine) DeliverCallbacksOnce(ctx context.Context) error` and `func (e *Engine) RunCallbacks(ctx context.Context)` (every 2 s, at most 4 sends at once). `Engine.CallbackClient *http.Client`, where nil means a client with a 10 s timeout that follows no redirect.
- Rules (pinned):
  - The body is the review's `GET` body at send time. Headers: `Content-Type: application/json`, `X-Pergent-Delivery: <callback id>`, and `X-Pergent-Signature` from `Sign` with the current secret, plus the previous one while `previous_secret_until > now`.
  - Any 2xx is delivered. Anything else, a redirect or no answer in 10 s is a failed attempt.
  - The next try is `now + min(10 s × 2^attempts, 1 h)`. Once `now ≥ created_at + 24 h`, a failed attempt gives up.
  - The URL and secrets are read at send time. An empty URL gives up with the error `the client has no callback URL`.
  - Errors are stored with the URL's query and the secrets replaced by `***`, and are cut to 500 bytes.
  - `callback_failing` is set when `CountUndeliveredAPICallbacks(client, now-24h) >= 3` (callbacks made in the window, not delivered, with at least one failed attempt). Builder call: the threshold. The message is `Callbacks to this client failed for <n> reviews in the last 24 hours. Check its callback URL; the results stay readable with GET /v1/reviews/{id}.` It is cleared when the count drops below 3, and is checked after every attempt.

```sql
-- One callback per finished or superseded API review, and its attempts.
CREATE TABLE api_callbacks (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid        NOT NULL UNIQUE REFERENCES runs (id) ON DELETE CASCADE,
    client_id    bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    next_at      timestamptz NOT NULL DEFAULT now(),
    attempts     integer     NOT NULL DEFAULT 0,
    delivered_at timestamptz,
    gave_up_at   timestamptz
);
CREATE INDEX api_callbacks_due_idx ON api_callbacks (next_at) WHERE delivered_at IS NULL AND gave_up_at IS NULL;
CREATE TABLE api_callback_attempts (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    callback_id uuid        NOT NULL REFERENCES api_callbacks (id) ON DELETE CASCADE,
    at          timestamptz NOT NULL,
    status_code integer     NOT NULL DEFAULT 0,
    error       text        NOT NULL DEFAULT '',
    duration_ms integer     NOT NULL
);
-- (Task 18 appends api_dismissals and api_review_events to this file.)
```

- [ ] **Step 1: Write the failing tests**
  - `TestSign`: secret `pgs_x`, `t=1700000000`, body `{}` → the HMAC written into the test. With two secrets → two `v1=` values, the current one first.
  - Engine (`httptest` receiver, Postgres):
    - `TestCallbackDelivered`: a published review → one POST whose body equals `GET`. The signature verifies. The delivery header is a UUID. The state is `delivered`.
    - `TestCallbackRetriesSameDelivery`: `500`, then `200` → two attempts, the same `X-Pergent-Delivery`, and the second at least 10 s after the first (fake clock).
    - `TestCallbackRedirectIsFailure`: `301` to another server → that server gets nothing, and the attempt is stored with status `301`.
    - `TestCallbackTimeout`: a handler that sleeps past the client timeout (shortened in the test) → a failed attempt with a timeout error.
    - `TestCallbackGivesUpAfter24h`: the fake clock past `created_at + 24h` → `gave_up`.
    - `TestCallbackRotationSendsBoth`: after a rotation, the header has two `v1=`, and the old secret verifies.
    - `TestSupersededReviewGetsCallback`: the body status is `superseded`.
    - `TestRemovedClientNoCallback`: delete the client before the send → nothing is sent and no error is logged.
    - `TestCallbackFailingAttention`: three undelivered callbacks in 24 h → the item with the exact message; a delivery that brings the count to 2 clears it.
    - `TestCallbackErrorRedacted`: a URL with `?token=abc` that fails → the stored error has no `abc`.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi -run TestSign`, and `go vet ./internal/engine`.

- [ ] **Step 3: Implement** as pinned.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi ./internal/engine ./internal/store ./internal/dashboard` passes.

- [ ] **Step 5: Commit.** `feat(engine): send each finished API review to the client, signed, with retries`

### Task 17: Test callback button and the callback attempts on the review page

**Files:**
- Modify: `internal/dashboard/api_clients.go` (`POST /api/admin/api-clients/{name}/test-callback`), `internal/dashboard/api_client_reviews.go` (the detail gains `callback: {state, attempts: [{at, statusCode, error, durationMs}]}`), `internal/engine/apicallbacks.go` (export `func SendCallback(ctx context.Context, hc *http.Client, url string, body []byte, delivery string, secrets ...string) CallbackAttempt` for both), `web/src/pages/ApiClient.tsx`, `web/src/pages/ApiReview.tsx`, tests, `openapi.yaml`

**Interfaces:**
- Produces: `POST .../test-callback` → `200 {statusCode, error, durationMs}`. The body is `Review{ID: "00000000-0000-0000-0000-000000000000", Subject: "pergent.sh test callback", Status: "completed", Verdict: "nothing_reviewed", Test: true}` (Builder call: `nothing_reviewed`, so a careless caller can't take it for a pass). It is signed as a real callback, with a new delivery UUID. A client with no URL → `409 this client has no callback URL`.

- [ ] **Step 1: Write the failing tests**
  - `TestTestCallbackSigned`: the `httptest` receiver gets the exact body and a signature that verifies. The answer has `statusCode: 204`.
  - `TestTestCallbackNoURL`: `409`.
  - Web: **Send test callback** shows `Answered 204 in <n> ms` or the error text. The review page lists each attempt.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/dashboard -run TestTestCallback`, and `cd web && npx vitest run src/pages/ApiClient.test.tsx src/pages/ApiReview.test.tsx`

- [ ] **Step 3: Implement.** The dashboard reaches the sender through `Options.SendCallback`, wired in `main` to `engine.SendCallback` with the engine's client.

- [ ] **Step 4: Verify.** `go test ./internal/dashboard && cd web && npx vitest run` passes.

- [ ] **Step 5: Commit.** `feat: test a client's callback, and show each review's attempts`

### Task 18: Dismissals by subject

**Files:**
- Create: `internal/store/apidismissals.go`, `internal/store/apidismissals_test.go`, `internal/reviewapi/dismiss.go`, `internal/reviewapi/dismiss_test.go`
- Modify: `internal/store/migrations/0040_api_callbacks.sql` (append the two tables below; the migration is not on `main` yet), `internal/reviewapi/handler.go` (routes), `internal/dashboard/api_client_reviews.go` (admin routes; the detail gains `history`), `openapi.yaml`, `openapi_test.go`, web `ApiReview.tsx` and its test

**Interfaces:**
- Produces:
  - `type APIDismissal struct{ ClientID int64; Tenant, Subject, Fingerprint, Severity, Skill, File, Title, Reason, Actor string; CreatedAt time.Time }`
  - `func (s *Store) DismissAPIFinding(ctx context.Context, d APIDismissal, runID string) (created bool, err error)` (`ON CONFLICT DO NOTHING`; when created, a `dismissed` event on `runID` in the same transaction)
  - `func (s *Store) UndismissAPIFinding(ctx context.Context, clientID int64, tenant, subject, fingerprint, runID, actor string) (bool, error)` (an `undismissed` event when there was one)
  - `func (s *Store) APIDismissals(ctx context.Context, clientID int64, tenant, subject string) ([]APIDismissal, error)`
  - `type APIReviewEvent struct{ At time.Time; Kind, Fingerprint, Title, Actor, Reason string }`, `func (s *Store) APIReviewEvents(ctx context.Context, runID string) ([]APIReviewEvent, error)`
  - `POST /v1/reviews/{id}/findings/{finding}/dismiss` with `{"reason": "..."}` → `200 {"status":"dismissed"}` or `{"status":"already_dismissed"}`. `DELETE` on the same path → `{"status":"restored"}` or `{"status":"not_dismissed"}`.
  - Admin: `POST|DELETE /api/admin/api-reviews/{id}/findings/{finding}/dismiss` with the same bodies (`{status}`). The detail's `history []{at, kind, fingerprint, title, actor, reason}`.
- Rules:
  - `{finding}` is the finding's `id` in that review's stored result. Builder call: stored by its fingerprint, skill, file, title and severity.
  - `reason` is required, 1 to `actions.MaxReasonRunes`, or `422 invalid` `a dismissal needs a reason of at most 500 characters`.
  - A review not yet completed → `409 not_finished` `findings can be dismissed once the review is completed`. An unknown finding → `404`.
  - The actor is `key <key name>` or the admin's username. It is never in the audit log and never in the v1 body.
  - The review it was made on is unchanged, and no callback is sent.

```sql
CREATE TABLE api_dismissals (
    client_id   bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    tenant      text        NOT NULL,
    subject     text        NOT NULL,
    fingerprint text        NOT NULL,
    severity    text        NOT NULL CHECK (severity IN ('blocking', 'suggestion')),
    skill       text        NOT NULL,
    file        text        NOT NULL,
    title       text        NOT NULL,
    reason      text        NOT NULL,
    actor       text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, tenant, subject, fingerprint)
);
CREATE TABLE api_review_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    at          timestamptz NOT NULL DEFAULT now(),
    kind        text        NOT NULL CHECK (kind IN ('dismissed', 'undismissed')),
    fingerprint text        NOT NULL,
    title       text        NOT NULL,
    actor       text        NOT NULL,
    reason      text        NOT NULL DEFAULT ''
);
CREATE INDEX api_review_events_run_idx ON api_review_events (run_id, at);
```

- [ ] **Step 1: Write the failing tests**
  - Store: `TestDismissAPIFindingTwiceIsNoop` (one row, one event), `TestUndismissAPIFinding`, and `TestAPIDismissalsBySubject` (another tenant's and another client's rows are left out).
  - reviewapi (fake store):
    - `TestDismissFinding`: `200 dismissed`, then `already_dismissed`.
    - `TestDismissNeedsReason`: `422`.
    - `TestDismissBeforeCompleted`: `409`.
    - `TestDismissUnknownFinding`: `404`.
    - `TestDismissOtherClientsReview`: `404`, byte for byte the unknown-ID body.
    - `TestDismissChangesNothing`: the review's `GET` body is unchanged afterwards.
  - Dashboard: `TestAdminDismissStoresUnderSubject` (actor = username, an event in `history`, no audit entry). Web: the **Dismiss** form (reason required) and **Stop** on a dismissed finding.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi -run TestDismiss`

- [ ] **Step 3: Implement** as pinned.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi ./internal/dashboard ./internal/store && cd web && npx vitest run` passes.

- [ ] **Step 5: Commit.** `feat: dismiss an API review's findings by subject`

### Task 19: Re-reviews and the four limits

**Files:**
- Create: `internal/reviewapi/limiter.go`, `internal/reviewapi/limiter_test.go`
- Modify:
  - `internal/store/apireviews.go`:
    - `func (s *Store) LastAPIReview(ctx context.Context, clientID int64, tenant, subject, beforeRunID string) (Run, bool, error)` (the newest published review whose result status is `completed`).
    - `func (s *Store) APITokensSince(ctx context.Context, clientID int64, since time.Time) (int64, error)` (`run_usage` joined to `api_reviews`).
  - `internal/store/runs.go`: `ClaimQueued` gains the in-flight cap.
  - `internal/engine/apireview.go`: previous result, dismissals and budget.
  - `internal/engine/publish.go`: `publishAPI` passes the subject's dismissals to `Map`.
  - `internal/reviewapi/handler.go`: the rate limit in `RequireKey`.
  - `internal/reviewapi/reviews.go`: the budget at `POST`.
  - `internal/reviewapi/handler.go`: `GET /v1/client` gains `usage`.
  - `cmd/pergent-sh/main.go`: one `Limiter`.

**Interfaces:**
- Produces:
  - `type Limiter struct{ ... }`, `func (l *Limiter) Allow(keyID int64, perHour int, now time.Time) (ok bool, retryAfter time.Duration)` (a token bucket in memory, one per key: capacity `perHour`, refilled at `perHour/3600` per second). Builder call: memory, and every authenticated `/v1/` request counts.
  - `UsageBody{ Month string "month" /* "2026-11" form, UTC */; TokensUsed int64 "tokens_used"; TokensLeft int64 "tokens_left" }`
  - `reasonBudget = "this client's monthly token budget is used up"`
- Rules (pinned):
  - Rate: over the limit → `429 rate_limited` `too many requests for this key`, with `Retry-After` (whole seconds, at least 1).
  - Budget at `POST`: `APITokensSince(client, start of the UTC month) >= MonthlyTokenBudget` → `429 budget_used_up` with `reasonBudget` and `Retry-After` until the next month. An idempotent replay is answered before this check.
  - Budget at dispatch: the same check fails the review with `reasonBudget`, step `starting`.
  - In flight: a claim skips an API run when its client already has `max_in_flight` runs in `running`, counting those claimed in the same statement. MR runs are claimed exactly as before.
  - `--previous`: `result.Trim(LastAPIReview(...).Result)`. Unreadable → none, logged.
  - `PERGENT_DISMISSALS`: `actions.PastDismissals(rows, skills, 0, redact)` over the subject's `APIDismissals`, turned into `store.Dismissal` with `Teaches: true`. The review runs without them when they can't be read, logged.
  - Publish: `Map` gets the subject's dismissals as of publish.

```sql
-- ClaimQueued, sketch: number each client's queued API runs oldest first and keep
-- those that fit beside its running ones. MR rows pass the cap test untouched.
SELECT id FROM (
  SELECT r.id, r.created_at, r.kind, r.api_client_id,
         row_number() OVER (PARTITION BY r.api_client_id ORDER BY r.created_at, r.id) AS n
  FROM runs r WHERE r.state = 'queued' AND (r.not_before IS NULL OR r.not_before <= $2)
    AND r.instance <> ALL($3::text[])) q
WHERE q.kind = 'mr' OR q.n + (SELECT count(*) FROM runs x WHERE x.api_client_id = q.api_client_id AND x.state = 'running')
      <= (SELECT c.max_in_flight FROM api_clients c WHERE c.id = q.api_client_id)
ORDER BY q.created_at, q.id LIMIT $1  -- then the existing FOR UPDATE SKIP LOCKED on runs
```

- [ ] **Step 1: Write the failing tests**
  - `TestLimiter`: 3 per hour → 3 allowed, the 4th refused with `retryAfter` about 20 min. 20 min later one more is allowed. Keys are apart.
  - `TestRateLimit429`: the response, `Retry-After` and code.
  - `TestBudget429`: the used-up budget → `429 budget_used_up`, and nothing is enqueued.
  - `TestClientUsage`: `usage` is `{"month":"2026-11","tokens_used":120,"tokens_left":9999880}` with the fake store and clock.
  - Store:
    - `TestClaimQueuedCapsAPIClient`: cap 2, one running, three queued → one claimed. Another client is not held up.
    - `TestClaimQueuedIgnoresCapForMR`: MR runs are claimed in order as before, mixed with API runs first in, first out.
    - `TestLastAPIReview`: only the subject's newest completed published review. Another tenant's is never returned.
  - Engine:
    - `TestAPIReReviewGetsPreviousAndDismissals`: the second review's ConfigMap has `previous.json`, and `dismissals.json` names the dismissed finding. The env has `PERGENT_DISMISSALS`.
    - `TestAPIReReviewMarksDismissed`: the second result repeats the blocking finding → its body verdict is `no_blocking_issues`, and the finding has `dismissed.reason`.
    - `TestBudgetUsedUpAtDispatch`: published `failed` with `reasonBudget`, and its callback says so.

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/reviewapi -run 'TestLimiter|TestRateLimit|TestBudget|TestClientUsage'`

- [ ] **Step 3: Implement** as pinned.

- [ ] **Step 4: Verify.** `go test ./internal/reviewapi ./internal/engine ./internal/store` passes.

- [ ] **Step 5: Commit.** `feat: re-reviews by subject, and the client limits`

### Task 20: Usage per client and tenant, and metrics

**Files:**
- Modify:
  - `internal/store/usage.go`:
    - `func (s *Store) UsageByAPIClient(ctx context.Context, since time.Time) ([]ClientUsage, error)` with `ClientUsage{Name string; Tokens TokenSum; Reviews int64; Tenants []TenantUsage}` and `TenantUsage{Tenant string; Tokens TokenSum; Reviews int64}`.
    - `MetricsTotals` gains `APIReviews []APIReviewTotal{Client, Result string; Reviews int64}`, where `Result` is the stored verdict or `superseded`; `APITokens []APITokenTotal{Client string; Tokens TokenSum}`; and `APICallbacks []APICallbackTotal{Client, Outcome string; Count int64}`, where `Outcome` is `delivered`, `failed_attempt` or `gave_up`.
  - `internal/metrics/metrics.go`: `APIReviewsTotal = "pergent_api_reviews_total"`, `APITokensTotal = "pergent_api_tokens_total"`, `APICallbacksTotal = "pergent_api_callbacks_total"`, with help texts.
  - `cmd/pergent-sh/metrics.go`: the `client` label only, never a tenant; an unknown result is `labelOtherVerdict`.
  - `internal/dashboard/api_usage.go` and `view.go`: `UsageOverviewView.Clients []UsageClientView`; totals add them.
  - `web/src/pages/Usage.tsx` and its test, `openapi.yaml`.

- [ ] **Step 1: Write the failing tests**
  - `TestUsageByAPIClient` (Postgres): two tenants, sums and counts.
  - `TestAPIMetricsHaveClientLabel`: every API sample's label names are exactly `client` plus `result`, `kind` or `outcome`, with no `tenant`.
  - `TestUsageOverviewAddsClients`: the total equals repositories plus clients.
  - Web: an "API clients" table on Usage, with tenants under each client.

- [ ] **Step 2: Run, expect FAIL.** `go test ./cmd/pergent-sh -run TestAPIMetrics`

- [ ] **Step 3: Implement** the queries, metrics, views and table.

- [ ] **Step 4: Verify.** `go test ./cmd/pergent-sh ./internal/dashboard ./internal/store && cd web && npx vitest run` passes.

- [ ] **Step 5: Commit.** `feat: usage and metrics per API client`

### Task 21: The full guide, OpenAPI and verification

**Files:**
- Modify:
  - `docs/setup/review-api.md`:
    - verify a callback: the header format; reject `t` older than 5 minutes; accept a match on any `v1=`; a shell one-liner, `printf '%s.%s' "$t" "$body" | openssl dgst -sha256 -hmac "$secret"`; a Go function using `hmac.Equal`;
    - `X-Pergent-Delivery` for duplicates;
    - retries for 24 h;
    - `Idempotency-Key`;
    - dismiss and undo, with curl;
    - the four limits and their refusals;
    - versioning: ignore unknown fields; a new enum value is `/v2/`.
  - `openapi.yaml`: every `/v1/` route, request, response, code and enum, and the callback as a `webhooks:` entry `reviewFinished` with both headers.
  - `docs/concepts/metrics.md`: the three metrics.
  - `docs/concepts/usage.md`: clients and tenants.
  - `docs/reference/limits.md`: no client library, no scopes, no detector for secrets in sent files.
  - `docs/reference/http-routes.md`.
  - `docs/upgrading.md`, under `## Unreleased`:
    - migration `0040`;
    - the outbound callback needs egress from the pergent.sh pod to each client's URL;
    - `/v1/` must be exposed for callers;
    - "Needs attention" kind `callback_failing`;
    - rolling back stops callbacks, dismissals and limits, and API reviews are then superseded as in Part 2.

- [ ] **Step 1: Write the docs and the OpenAPI.** `go test ./internal/dashboard -run TestOpenAPI` passes, with every `/v1/` schema in `openAPISchemas`. Check the request bodies by hand.
- [ ] **Step 2: Full Go gate.** `go build ./... && go vet ./... && go test ./...` passes. The Postgres tests skip locally and run first in CI. No container.
- [ ] **Step 3: Web gate.** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build` passes.
- [ ] **Step 4: Scan.** `git add -A`, then the name scan prints nothing.
- [ ] **Step 5: Commit.** `docs: the review API guide, callbacks and limits`. Part 3 is one PR.
