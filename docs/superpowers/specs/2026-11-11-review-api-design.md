# A review API for systems other than GitHub and GitLab

Status: decided with the owner on 2026-11-11, awaiting approval.

## Why

pergent.sh reviews only GitHub pull requests and GitLab merge requests. Other systems have changes worth reviewing too. FastGateway, the first caller, runs its own internal AI review of gateway configs and API specs. That reviewer has no skills, no dismissals and no history. With this API, FastGateway, and any other system, can send a change to pergent.sh. pergent.sh reviews it with its skills and sends the findings back.

The API is a public, versioned standard, "the pergent review API, v1". Anyone can build against it from the docs alone.

## Goals

- An outside system sends the files of a change and gets findings back, from skills an admin chose.
- An admin manages these systems ("API clients") and their keys in a new dashboard section.
- A change to the same thing, reviewed again, knows its earlier findings and dismissals.
- Each client has limits, so one caller can't starve the repositories or spend the model budget.
- The standard is documented well enough that a caller needs nothing else: an OpenAPI file, a guide, curl examples, and how to verify a callback.

## Non-goals

- A git-based input. The caller sends file contents. pergent.sh never clones for an API review.
- A review that answers within seconds. A review takes minutes, like an MR review, and the caller is told when it ends.
- Access for non-admins. Only admins see API clients and their reviews.
- Skills or settings per tenant. A tenant is a label.
- A client library. The spec and examples are the kit.
- Review instructions, repository skills (`.pergent/skills`), linked repositories, inline threads, commit statuses or approvals for API reviews. They belong to a forge.
- Incremental reviews (carried files). Every API review covers every file sent.
- API keys for the dashboard API. These keys reach only the review API.

## Anti-goals

- **A false pass.** A review that fails, in whole or for one skill, never reads as a pass in the result or the callback. Neither does a review in which no skill reviewed any file.
- **A leaked key or secret.** A key is shown once and stored as a hash. Keys and the callback secret never reach a log, the audit log or a metric label. The result and logs are redacted like an MR review's: the values pergent.sh knows, plus pergent's own token rules. pergent.sh has no detector for credentials inside the sent files (my call). The guide tells callers to strip secrets before sending, since the files go to the model provider too.
- **Keeping the caller's files.** Gateway configs can hold upstream credentials. pergent.sh holds the sent files sealed until the Job is created, then deletes them. In the cluster they live in their own Secret, deleted once the checkout step ends, and in the pod's work volume while the pod runs. After that only the result (kept like an MR run's) and redacted logs (30 days) remain.
- **One client reaching another's data.** A key reads and acts on its own client's reviews only. Another client's review ID answers as not found.
- **pergent.sh calling an address a caller chose.** The callback URL is set by an admin. Nothing in a request can change where pergent.sh sends anything.
- **Breaking MR reviews or the previous release.** Migrations only add, and the GitHub and GitLab paths behave as before.

## Decisions

These are the owner's.

| Question | Decision |
|---|---|
| Which way work flows | The caller asks, and pergent.sh reviews and answers. |
| What is reviewed | Gateway configs and API specs to start with, but any text files. |
| How the content arrives | The caller sends the files in the request. |
| How the result gets back | A callback to the client's URL, and a status call by review ID. |
| Who picks skills | An admin, per client, from the skill library. The caller sends only content. |
| How long a review may take | Minutes, in the background. |
| Who the standard is for | Any outside system. Public and versioned. |
| Keys | Several named keys per client, each shown once and revocable at once, with a last-used time. |
| The caller's own customers | A tenant label on each request, for filters and counts. |
| Who sees API reviews | Admins, in the new section. |
| Re-reviews | A stable subject ID per request. Earlier findings and dismissals carry over by subject. |
| The callback URL | Set per client by an admin. Each callback is signed with a client secret. |
| Dismissals | A dismiss call per finding, with a reason, stored by subject. |
| Limits | Max request size (bytes and files), reviews in flight, a monthly token budget, and requests per hour per key. |
| The sent files | Discarded once the Job has them. |
| Where it lives in the dashboard | A new "API clients" section in the sidebar beside Repositories, for admins. |
| Model | The default model, overridable per client. |
| What ships beside the API | OpenAPI, a guide, curl examples and callback verification. No library. |
| Delivery | Several PRs, then one release. |
| Skipped files | The result lists reviewed and skipped files with the reason. A review in which no skill reviewed a file is `nothing_reviewed`, not a pass. |
| What a dismissal does | It counts on later reviews of the same subject: a repeated dismissed finding is marked dismissed and doesn't block. The review it was dismissed on stays as it was, with no new callback. |
| What identifies a subject | Client, tenant and subject together. |
| How long the files stay in the cluster | Until the checkout step ends, in a Secret of their own. |
| Re-runs | None for API reviews, since the files are gone. The caller sends the request again. |
| Sharing Job slots with MR reviews | First in, first out under `jobs.max_running`. Each client's in-flight cap bounds its share. |
| A client that can't review | `POST` is refused with `409` when it has no skills or its model is gone. |
| What a file may be | UTF-8 text. A relative path, unique, with no `..`. Anything else is `422`. |
| A description | Optional, passed to the model as data, at most 32 KiB. |
| Undoing a dismissal | `DELETE` on the dismiss path, or "stop" on the dashboard. |
| Admins dismissing | Yes, on an API review's page, stored under the same subject. |
| Where API dismissals are recorded | The review's history, with the key's name. Not the audit log. |
| Rendered markdown | Not in the result. Callers build their own view. |
| Versioning | v1 only gains optional fields, and callers ignore fields they don't know. A new enum value, or a removed or renamed field, is `/v2/`. |
| Removing a client | Keys stop at once. Live reviews end with no callback. Reviews, dismissals and settings are deleted. The audit log keeps the record. |
| A caller's own limits and usage | `GET /v1/client`. |
| Key scopes | None in v1. |
| Key expiry | Optional, set when the key is made. An expired key is `401`. |
| Test callback | A button in the client's settings. |
| Callbacks | One per review, when it ends. None for "running". |
| Usage and metrics | The Usage page shows clients and tenants. Metrics get a `client` label, never `tenant`. |
| Failing callbacks | A "Needs attention" item. |
| How `/v1/` is reached | The same port as everything else. The docs list it as a prefix to expose. |
| Release | Not part of this work. The owner decides when to release and which version. |
| The calls marked "my call" in the first draft | Confirmed by the owner. |

## How it works

### API clients and keys

An admin creates a client with a name (`^[a-z0-9-]+$`, like instance names). The client holds:

- **Skills**, from the library, as a repository's library skills are chosen.
- **Model**: the default, or one picked from the model catalog.
- **Callback URL** and its **signing secret**. pergent.sh generates the secret, shows it once, and the admin can rotate it.
- **Limits**: max request bytes, max files, reviews in flight, monthly token budget, requests per hour per key. Each has a default (left to the builder) and the admin can change it.
- **Keys**: any number, each with a name and an optional expiry date. A key is shown once when created. The list shows its name, a short prefix that identifies it, when it was created, when it expires and when it was last used. Revoking a key stops it on the next request, and so does expiry. Every key can do everything in v1: there are no scopes.

Creating, changing and removing a client, creating and revoking keys and rotating the secret go to the audit log, without any secret.

**Removing a client** stops its keys at once and ends its queued and running reviews with no callback. It then deletes the client's reviews, dismissals and settings. The audit log keeps the record of the removal.

Keys look like `pgk_<prefix>_<secret>` (my call). pergent.sh stores the SHA-256 of the key, compared in constant time, as Job result tokens are today. The prefix makes a key easy to recognise in a leak scan and in the key list.

### The API

The review API is separate from the dashboard API. It takes `Authorization: Bearer <key>`, with no cookie and no CSRF token, under its own prefix, `/v1/` (my call). Every call works on the key's own client.

| Call | What it does |
|---|---|
| `POST /v1/reviews` | Asks for a review. Answers `202` with the review ID and status `queued`. |
| `GET /v1/reviews/{id}` | The review's status, and its result once done. |
| `POST /v1/reviews/{id}/findings/{finding}/dismiss` | Rejects a finding, with a reason. |
| `DELETE /v1/reviews/{id}/findings/{finding}/dismiss` | Undoes that dismissal. |
| `GET /v1/client` | The client's limits, its tokens used this month and the budget left. |

A request carries:

- `subject`: the caller's stable ID for what changed, such as `route:123` (at most 200 characters).
- `tenant`: the caller's customer or workspace (optional, at most 100 characters).
- `title`: a line shown on the dashboard (optional).
- `description`: what the change is for (optional, at most 32 KiB). It reaches the model as data, as an MR description does (`PERGENT_CHANGE_DESCRIPTION`).
- `files`: each with a `path`, its `old` content and its `new` content. A missing `old` means the file is new. A missing `new` means it was deleted. Contents are UTF-8 text. A path is relative, with no leading `/` and no `..`, and appears once. Paths git can't store, such as a `.git` segment, a control character, or `a` beside `a/b`, are refused too. Anything else is refused with `422`.

A request is refused with `409` and a reason when the client has no skills or its model is no longer in the catalog.

There is no re-run of an API review, from the dashboard or the API. The files are gone once the Job has them, so the caller sends the request again.

Old and new content, not a diff, is my call: the Job needs whole files to build a base and a head, and a diff without the base can't be applied.

An `Idempotency-Key` header (my call) makes a retried `POST` return the first review instead of starting a second one.

A subject is identified by client, tenant and subject together (the owner's). A request without a tenant has the empty tenant. Supersede, previous findings and dismissals all use that key, so one customer's history never reaches another customer's review.

A new request for a subject that has a review queued or running supersedes the older one, as a new push does on an MR (my call). The older review ends as `superseded`, and its callback says so.

A dismissal (the owner's):

- is stored under the finding's subject and fingerprint, with the reason and the key's name as the actor
- may be for a blocking finding or a suggestion
- doesn't change the review it was made on, and sends no callback
- applies to later reviews of the same subject: a finding with that fingerprint comes back marked `dismissed` with its reason, and doesn't count toward a blocking verdict, as `render.ApplyDismissals` does for MRs
- reaches the model on later reviews as rejected findings (`PERGENT_DISMISSALS`)
- is recorded in the review's history with the key's name, not in the audit log
- can be undone with `DELETE` through the API, or stopped by an admin on the dashboard

Admins can also dismiss findings on an API review's page, stored under the same subject. Dismissing the same finding twice is a no-op (my call).

### Versioning

v1 only gains optional fields. Callers must ignore fields they don't know. A new verdict or status value, or a removed or renamed field, makes a new `/v2/`, so a caller that gates on the verdict never meets a value it doesn't know.

Statuses: `queued`, `running`, `completed`, `failed`, `superseded`.

Refusals: `401` for a missing, unknown or revoked key; `413` over the size limits; `429` over the rate limit (with `Retry-After`) or with the month's token budget used up; `422` for a bad body.

### The result

The result is the standard's own shape, versioned on its own, and not pergent's internal agent result. pergent.sh maps one into the other. It holds:

- `id`, `subject`, `tenant`, `status`
- `verdict`, the first that applies (the owner's):
  - `failed` when any skill failed, so a review with a failed skill can never pass
  - `nothing_reviewed` when no skill reviewed any file
  - `has_blocking_issues` when an undismissed blocking finding remains
  - `no_blocking_issues` otherwise, which is the only pass
- `skills`: each with its name, verdict, findings, and its files: `reviewed`, `skipped` with the rule for each skip, and `not_reviewed` with a reason. The v1 skip rules are fixed: deleted, lockfile, generated, vendored, binary, no content changes, excluded by the skill, and `other` for any rule pergent adds later. A file too large for a batch is not a skip: pergent lists it as not reviewed, and that skill fails.
- each finding: `id`, `fingerprint`, `file`, `line`, `end_line`, `severity` (`blocking` or `suggestion`), `title`, `explanation`, `fix`, `suggestion`, and `dismissed` with its reason when that finding was dismissed earlier on the same subject
- `failure`: the step and reason, when the review failed (the same steps as job tracing: starting, review, delivery, unknown)
- `usage`: tokens in and out

The result carries no rendered markdown. pergent's comment is shaped for GitHub and GitLab, and callers build their own view from the findings.

### The callback

When a review ends, completed, failed or superseded, pergent.sh `POST`s the result to the client's callback URL. That is the only callback a review gets; there is none for "running". The body is the same as `GET /v1/reviews/{id}`.

- Signature header: `X-Pergent-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">`, using the client's secret. The guide shows how to verify it and to reject an old `t`.
- `X-Pergent-Delivery: <uuid>`, the same on every retry, so the caller can drop duplicates.
- Any `2xx` is delivered. Anything else, or no answer within 10 seconds, is retried with backoff for up to 24 hours (my call). The review page shows each attempt and its error.
- A callback that never succeeds leaves the result readable by `GET`.
- **Rotating the secret** keeps the old one valid for 24 hours (my call). During that time each callback carries two `v1=` values, one per secret, so a caller still checking with the old secret accepts it. The guide says to accept a match on any `v1=`.
- **The URL** may be `http` or `https`, and a private or in-cluster address (my call). An admin sets it, as an admin already sets the GitLab and GitHub addresses pergent.sh calls. The guide recommends `https`.
- **Send test callback**, in the client's settings, posts a signed sample body and shows the response code or error.
- **Failing callbacks** raise a "Needs attention" item for the client, like `clone_failing`. The threshold, for example 3 reviews in 24 hours never delivered, is left to the builder.

### Running the review

An API review runs as a Kubernetes Job with `pergent agent`, like an MR review, and goes through the same queue, the same `jobs.max_running` cap and the same watcher and job tracing.

What changes:

- **Input.** There is no clone. The checkout step builds a local git repository from the sent files: one commit of the old files (the base) and one of the new (the head). `pergent agent` then reviews `--base..--head` as it does today. The engine must know both SHAs to check the result. Building the commits deterministically (fixed author, committer and date) lets it compute them up front. That method is my call, and the builder can choose another way to check the result.
- **Where the files go.** From the request until dispatch, the files are stored sealed with `secretbox`. The stored copy is deleted when the Job is created, or when the review ends without one.
  - In the cluster, the files go in a Secret of their own (the owner's), not the ConfigMap or the Job's Secret. They may hold credentials, and the Job's Secret must stay for the `pergent` container.
  - Only the checkout step mounts the files' Secret. The watcher deletes it once the checkout has ended, and the Job's cleanup deletes it in any case.
  - The Secret's 1 MiB cap bounds the request size. The hard maximum is left to the builder, below that cap.
- **Skills and model** come from the client, not from repository settings.
- **Previous findings** are the last completed review of the same client and subject, passed as `--previous`.
- **Dismissals** of that subject are passed as `PERGENT_DISMISSALS`.
- **Retention.** Results are kept like MR runs' results, and logs for 30 days (my call). Dismissals stay until the client is removed.
- **Publishing.** There is no comment, status or approval. Publishing an API review means building the result and sending the callback.

### Limits

- **Request size**: checked before anything is stored. Over the limit is `413`.
- **Requests per hour per key**: over the limit is `429` with `Retry-After`.
- **Reviews in flight**: a client's queued reviews beyond its cap wait in the queue. `jobs.max_running` still caps everything, and API and MR reviews share it first in, first out. MR reviews get no priority.
- **Monthly token budget**: counted from the client's run usage, by calendar month in UTC, as in the token-quota work. A request when the budget is used up is refused with `429` and a reason. A queued review that reaches dispatch after the budget ran out fails with that reason, and the callback says so (my call: an API caller needs an answer, not a review held until next month).

### The dashboard

The new **API clients** section appears in the sidebar beside Repositories, for admins only.

- **List**: each client with reviews this month, failures and tokens used.
- **A client's page**:
  - Reviews: filter by tenant, subject and status.
  - Settings: skills, model, callback URL and secret, limits.
  - Keys.
- **A review's page**: the findings, logs and failure step, as a review page today, plus the subject, the tenant, the dismissal history and the callback attempts. Admins can dismiss a finding here. There is no re-run button.
- **Usage**: the Usage page shows tokens per client and per tenant.
- **Metrics**: API reviews and callbacks carry a `client` label, never `tenant`, because tenants come from the caller and could grow without bound.

### Docs

- A guide in `docs/setup/` for callers: create a client, make a key, send a review, read the result, verify a callback, dismiss a finding. It includes curl examples, and the signature check in a shell one-liner and in Go.
- The `/v1/` routes in `openapi.yaml`, with a bearer-key security scheme.
- `docs/reference/limits.md` changes from "no API tokens for scripts" to say what these keys can and can't do.
- `/v1/` is served on the same port as the rest. The chart has no Ingress, so the docs list `/v1/` beside `/webhooks/` as the prefixes an operator exposes to callers.
- `docs/upgrading.md` for the release.

## Constraints

- Migrations only add. MR runs keep working on a migrated database with the previous release.
- `runs` and the code around it assume a forge: `(instance, project_id, mr_iid)`, `Forge.GetChange`, a clone URL, comment publishing. API reviews need their own path through dispatch and publish. They must not reach the forge path, and the forge path must not change.
- Admin routes in the dashboard use `adminRoute` and carry `x-pergent-access: admin`. The `/v1/` routes get their own access value. `openapi_test.go` checks both.
- The repo is public: no private host or company names in code or docs.
- No new config key unless needed. A new one is noted in `docs/upgrading.md` because it breaks rollback.

## Delivery

Several PRs (the owner's). No release is part of this work: the owner decides when to release and which version. The order is my call:

1. **Clients and keys.** Tables, the API clients section (list, settings, keys), the bearer-key check, the audit entries. No reviews yet.
2. **Reviews.** `POST` and `GET /v1/reviews`, sealed storage of the files, the checkout path that builds the two commits, the result mapping, and the review pages. The request size and file-count limits come here too, so the files always fit the 1 MiB Secret.
3. **Callbacks, re-reviews and limits.** Signed callbacks with retries, the dismiss call, previous findings and dismissals by subject, the other three limits, the guide and the OpenAPI file in full.

## Left to the builder

- Table and column names, and whether API reviews share `runs` (with a kind column) or get their own table. Either way, the dashboard's usage and failure counts include them.
- The default for each limit, and the hard maximum request size.
- The exact error bodies, and the JSON field names not fixed above.
- The rate limiter's storage. One replica is supported, so memory may do.
- Metrics for API reviews and callback failures.
- Whether job tracing's clone log has an equivalent for the checkout step that builds the commits.
