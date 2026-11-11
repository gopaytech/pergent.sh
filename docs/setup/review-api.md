# 🔗 Review API

The review API lets another system ask pergent.sh for reviews. This page covers **API clients and their keys**, and **asking for a review and reading it**.

**Not built yet:** callbacks, dismissals, a review knowing its subject's earlier findings, and the hourly, in-flight and token limits come in a later part. You can set their fields now, but nothing sends or enforces them yet. Don't rely on them.

It exists only when the dashboard is on (`dashboard.url` is set). It adds no config key.

## What a client is

A client is one system that calls the API: a script, a bot, another service. An admin makes it under **API clients** in the menu (`/api-clients`). Only admins see that page.

| Part | What it is |
| --- | --- |
| Name | 1 to 63 characters: `a-z`, `0-9` and `-`. It never changes. |
| Skills | The library skills its reviews use. |
| Model | A model from the [list of models](../concepts/models.md), or the default. |
| Callback URL | Where pergent.sh will send results. Empty means none. Only `http` and `https`, with no user name or password. **Stored only for now: nothing is sent to it yet.** |
| Limits | The caps on its requests (below). |
| Keys | The secrets its callers send. A client can have several. |
| Callback secret | Will sign the callbacks pergent.sh sends to the client. **Not used yet**, as no callback is sent. |

New clients get these limits. An admin changes them on the client's **Settings** tab.

| Limit | Default | Range | Enforced |
| --- | --- | --- | --- |
| Max request bytes | 524288 | 1024 to 819200 | Now |
| Max files | 200 | 1 to 1000 | Now |
| Max in flight | 5 | 1 to 50 | Not yet: a later part |
| Requests per hour | 600 | 1 to 100000 | Not yet: a later part |
| Monthly token budget | 10000000 | more than 0 | Not yet: a later part |

A value you set for a limit that is not enforced yet is kept and shown by `GET /v1/client`, and takes effect when that part ships.

## Make a client

1. Open **API clients** and press **New client**.
2. Type a name. Pick skills, a model and a callback URL if you know them; all can be changed later.
3. Copy the **callback secret** from the box that opens. It is shown once. Close the box only after you saved it.
4. Open the client and make its first key.

## Make a key

On the client's **Keys** tab, type a name for the key (one per caller, such as `ci` or `nightly`) and press **Create key**. A date is optional: the key stops working at the end of that day, UTC.

The key looks like `pgk_1a2b3c4d_...`. The 8 characters after `pgk_` are its **prefix**: the dashboard lists keys by name and prefix, with the dates they were made, expire and were last used. **The key is shown once.** pergent.sh keeps only a hash of it, so nobody can read it again, not even an admin. If it is lost, revoke it and make another.

A caller sends the key as a bearer token:

```
curl -H "Authorization: Bearer pgk_1a2b3c4d_..." https://<dashboard host>/v1/client
```

## What a key is and is not

| A key is | A key is not |
| --- | --- |
| A bearer token for one client | A dashboard sign-in: it never opens `/api/` |
| Able to call the `/v1/` routes of its own client | Able to see another client |
| Hashed (SHA-256) at rest and shown once | Readable later |
| Optionally time-limited | Scoped: every key of a client can do the same |

## Keep keys secret

- Give each caller its own key, so one can be revoked alone.
- Keep a key in the caller's secret store, never in a repository, a log or a chat.
- Set an expiry on a key that a person types or a short-lived job uses.
- pergent.sh never logs a key or a callback secret, and the audit log records a key by its client, name and prefix only.

## Revoke a key

On the **Keys** tab press **Revoke** on the key. The next call with it is refused. A revoked key gets the same `401 unauthorized` as one that never existed, so a caller can't tell the two apart. An expired key does too.

To replace a key without a gap: make the new one, switch the caller, then revoke the old one.

## Rotate the callback secret

On the **Settings** tab press **Rotate secret**. The new secret is shown once. The old one **keeps signing for 24 hours**, so the caller can switch over without dropping a callback; the tab says until when. Rotating again replaces the old secret that was still signing. No callback is sent yet, so for now the secret signs nothing.

## Remove a client

On the **Settings** tab press **Remove client**. Its keys stop at once, the Jobs of its running reviews are deleted, and its data is deleted, its queued reviews with it. It cannot be undone.

## Check a key

`GET /v1/client` says which client a key belongs to and its limits. It is a good first call.

```json
{
  "name": "ci",
  "limits": {
    "max_request_bytes": 524288,
    "max_files": 200,
    "max_in_flight": 5,
    "requests_per_hour": 600,
    "monthly_token_budget": 10000000
  }
}
```

Every `/v1/` answer is sent `Cache-Control: no-store`. Field names are snake_case. An error is `{"code": "...", "message": "..."}`:

| Status | `code` | When |
| --- | --- | --- |
| 401 | `unauthorized` | The key is missing, unknown, revoked or expired. The body is the same for each. |
| 404 | `not_found` | A path under `/v1/` that is not a route (the key is checked first). |
| 500 | `internal` | The key could not be checked. Try again. |

v1 only gains optional fields and keeps its codes: ignore a field you don't know. The full description is in [`openapi.yaml`](../../openapi.yaml) and the routes are in [HTTP routes](../reference/http-routes.md).

## Ask for a review

`POST /v1/reviews` queues a review of a change. The change is the content of each file before (`old`) and after (`new`), not a diff. Save the body as `request.json` and send it:

```json
{
  "subject": "route:123",
  "tenant": "acme",
  "title": "Rate limit the route",
  "description": "Why the change is made. The model reads it as data.",
  "files": [
    {"path": "routes/api.yaml", "old": "limit: 10\n", "new": "limit: 5\n"},
    {"path": "routes/new.yaml", "new": "limit: 1\n"}
  ]
}
```

```
curl -i -X POST https://<dashboard host>/v1/reviews \
  -H "Authorization: Bearer pgk_1a2b3c4d_..." \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: route-123-attempt-1" \
  --data-binary @request.json
```

| Field | Rule |
| --- | --- |
| `subject` | Required. 1 to 200 characters. Your stable ID for what changed. |
| `tenant` | At most 100 characters. Your customer or workspace. |
| `title` | At most 200 characters. Shown on the dashboard. |
| `description` | At most 32 KiB of UTF-8. Dropped with the files once the review has them. |
| `files` | Not empty, at most the client's **Max files**. See the file rules below. |

`subject`, `tenant` and `title` hold no control character. Unknown fields are ignored. The whole body must be UTF-8 text. A byte that is not UTF-8 (a latin-1 file), or a `\uD800` to `\uDFFF` escape that is not half of a pair, is refused: pergent.sh won't review text you did not send.

### The file rules

| Rule | Detail |
| --- | --- |
| `path` | Relative, at most 1024 bytes, no `.` or `..` part, no empty part (`a//b`, a trailing `/`), each part at most 255 bytes. No control character, no `\`, valid UTF-8. |
| One entry per path | A path appears once in the list. |
| `old` and `new` | At least one. No `old` is a new file, no `new` is a deleted one, an empty string `""` is an empty file. Both are UTF-8 text. |
| No `.git` | No part may be `.git`, or look like it to git on some file system: `.GIT`, `.git ` or `.git.` with spaces or dots after, `git~1`, `.git::$INDEX_ALLOCATION`. `.gitignore` and `.github` are fine. |
| Files vs directories | A path can't also be a directory of another path on the same side (`a` and `a/b` both in `new`). |

A broken rule is `422 invalid`, and the message says which file: `files[1]: the path appears twice`.

Together the files are packed for the review. If they are over 1000000 bytes once packed, the request is `413 too_large`.

### What happens to a request

The checks run in this order, and the first that fails answers:

1. The body is over **Max request bytes** (never more than 819200): `413`.
2. The `Idempotency-Key` header is not valid: `422`.
3. The key was used before: the first review is returned, or `422 idempotency_key_reused` for another body. Nothing else is checked.
4. The body is not UTF-8 text, not valid JSON, or breaks a field or file rule: `422 invalid`.
5. More files than **Max files**, or files over the packed limit: `413 too_large`.
6. The client can't review: `409 cannot_review`.
7. Otherwise the review is queued: `202`.

The answer is `202` with the review (`"status": "queued"`) and a `Location: /v1/reviews/<id>` header. A new request for a subject (client, tenant and subject together) that has a review queued or running supersedes it; a review already being finished is not superseded.

Send an `Idempotency-Key` header (1 to 200 printable ASCII characters) to retry safely. The same key with the **same body** (byte for byte) returns the first review, as it is now, with `202`, and queues nothing. The same key with another body is `422 idempotency_key_reused`.

| Status | `code` | When |
| --- | --- | --- |
| 413 | `too_large` | The body is over the client's **Max request bytes**, there are more files than **Max files**, or the files are too large once packed for the review. |
| 422 | `invalid` | The body is not valid JSON or not UTF-8 text, a field or a file breaks its rule, or the `Idempotency-Key` is not valid. The message names it, a file as `files[<index>]`. |
| 422 | `idempotency_key_reused` | The `Idempotency-Key` was used before for another body. |
| 409 | `cannot_review` | The client has no skills, a skill is not in the library, or its model is no longer in the catalog. |
| 500 | `internal` | pergent.sh couldn't queue the review. Retry with the same `Idempotency-Key`. |

## Read a review

`GET /v1/reviews/{id}` answers `200` with the review. A review of another client, an ID that does not exist and an ID that is not a review ID all get the same `404 not_found`.

```
curl -H "Authorization: Bearer pgk_1a2b3c4d_..." https://<dashboard host>/v1/reviews/<id>
```

While it waits or runs the body holds the `id`, the request's `subject`, `tenant` and `title`, the `status` and `created_at`:

| `status` | Meaning |
| --- | --- |
| `queued` | Waiting for a slot, or its Job has not started. |
| `running` | The Job is running, or its result is being finished. |
| `completed` | pergent returned a result. **This is not a pass:** read the `verdict`. |
| `failed` | The review could not be done. The body holds the `failure` (below). |
| `superseded` | A newer request for the same subject replaced it. No result. |

Poll until the status is `completed`, `failed` or `superseded`; a review takes minutes. A finished body also holds `finished_at`, the `verdict`, `skills` and `usage` (`tokens_in`, `tokens_out`, set when the Job left a result):

```json
{
  "id": "6f1c0a52-...",
  "subject": "route:123",
  "tenant": "acme",
  "title": "Rate limit the route",
  "status": "completed",
  "created_at": "2026-11-11T09:00:00Z",
  "finished_at": "2026-11-11T09:03:10Z",
  "verdict": "has_blocking_issues",
  "skills": [
    {
      "name": "routes",
      "verdict": "has_blocking_issues",
      "findings": [
        {
          "id": "a1b2c3",
          "fingerprint": "a1b2c3d4e5f6...",
          "file": "routes/api.yaml",
          "line": 1,
          "severity": "blocking",
          "title": "The limit drops traffic",
          "explanation": "...",
          "fix": "...",
          "suggestion": "limit: 50\n"
        }
      ],
      "files": {
        "reviewed": ["routes/api.yaml"],
        "skipped": [],
        "not_reviewed": []
      }
    }
  ],
  "usage": {"tokens_in": 12000, "tokens_out": 800}
}
```

A finding has `severity` `blocking` or `suggestion`; its `id` is the first characters of its `fingerprint`. `line`, `end_line`, `fix` and `suggestion` (the exact replacement text) appear when there is one. The fields are in [`openapi.yaml`](../../openapi.yaml) (`ReviewApiReview`).

### The verdict

Gate on `verdict`, never on `status`. Only `no_blocking_issues` is a pass.

| `verdict` | Meaning |
| --- | --- |
| `no_blocking_issues` | Something was reviewed and nothing blocks. |
| `has_blocking_issues` | A skill found a blocking problem. |
| `nothing_reviewed` | No skill reviewed a file (all were skipped). Not a pass. |
| `failed` | The review failed, or a skill failed or could not review a file. Not a pass. |

A skill has the same four verdicts. The review's verdict is the first that applies: `failed` if any skill failed, else `nothing_reviewed` if no skill reviewed a file, else `has_blocking_issues` if any skill has one, else `no_blocking_issues`. A skill's verdict covers only the files in its `files.reviewed`.

### Files a skill did not review

`files.skipped` lists a file left out on purpose, with the `rule`:

| `rule` | Why |
| --- | --- |
| `deleted` | The change removes the file. |
| `binary` | The file is binary. |
| `no_content_changes` | Nothing in the file changed. |
| `lockfile`, `vendored`, `generated` | A lockfile, a vendored file or a generated file, by its path or header. |
| `out_of_scope` | Outside the skill's `paths`. |
| `excluded_by_skill` | Matches the skill's `exclude`. |
| `other` | Any other rule. Treat it as skipped. |

`files.not_reviewed` lists a file that should have been reviewed and was not, with a `reason`. The skill is then `failed`. The set of rules is fixed for v1.

### A failed review

A review with `status: failed` has `verdict: failed` and a `failure` with the `step` it failed at and a `reason`:

| `step` | When | Example `reason` |
| --- | --- | --- |
| `starting` | Before the Job, or while it builds the commits | `this client has no skills: an admin chooses them in its settings`, `the model chosen for this API client can't be used: an admin can change it in the client's settings`, `skill "x" is not in the skill library`, `the review's files are no longer stored` |
| `review` | While pergent reviews | `the review job ran out of memory (OOMKilled)`, `pergent couldn't review the change` |
| `delivery` | When the result comes back | `pergent sent a result pergent.sh couldn't use` |
| `unknown` | The Job vanished, or hit its deadline before it started | `the review job disappeared before sending a result` |

## Send the change again, don't re-run

There is no re-run. To review again, send the change again with the same `subject`: a review of that subject still queued or running is superseded, and the new one reviews what you send now. Use a new `Idempotency-Key` for each new attempt; reuse one only to retry a request whose answer you lost.

## Strip secrets before you send

The files and the description go to the model, so they leave your system. Remove credentials, tokens and private data from them first: pergent.sh can't tell which part of a file is secret. It masks only the secrets it holds itself, such as the model's key, in what it stores and returns.

## See a client's reviews in the dashboard

A client's page opens on its **Reviews** tab: its reviews, newest first, with the subject, tenant, title, status and verdict. Filter by tenant, subject or status, and press **Load more** for older ones. A review's subject opens its page: who asked (subject, tenant, the key's name), the model, the tokens, each skill's findings (with the fix and the replacement text) and its files (reviewed, skipped with the rule, and not reviewed with the reason). A failed review reads `Failed at <step>: <reason>`, with the admin-only detail below. The **Logs** tab shows the Job's log, the clone log and the pod's state. The page has no re-run button: the caller sends the change again.

The list of clients shows, for each, the reviews of the UTC month so far, how many failed and the tokens they used.

## How a review runs

- It is a Kubernetes Job, like a merge request's review. Both share `jobs.max_running`, first in, first out.
- It uses the client's skills and model, read when the Job starts. The Job's timeout, memory and Max steps are the defaults on **Admin → Settings**.
- The files reach the Job in a Secret of their own, which only its checkout container reads. The watcher deletes it the first time it sees the checkout done, and before it reports a stuck, vanished or finished Job. If the result arrives before a watcher pass sees the checkout done, the Secret stays until the Job is deleted, an hour after it ends. The stored copy is deleted once the Job has started.
- Nothing is posted to GitLab or GitHub.

A failed review has a `failure` with the step it failed at: see [A failed review](#a-failed-review).

## Audit log

Making, changing and removing a client, making and revoking a key, and rotating a callback secret are in **Admin → Audit log**.
