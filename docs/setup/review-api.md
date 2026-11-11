# 🔗 Review API

The review API lets another system ask pergent.sh for reviews. This page covers **API clients and their keys**, and **asking for a review and reading it**. Callbacks come later and are not described here yet.

It exists only when the dashboard is on (`dashboard.url` is set). It adds no config key.

## What a client is

A client is one system that calls the API: a script, a bot, another service. An admin makes it under **API clients** in the menu (`/api-clients`). Only admins see that page.

| Part | What it is |
| --- | --- |
| Name | 1 to 63 characters: `a-z`, `0-9` and `-`. It never changes. |
| Skills | The library skills its reviews use. |
| Model | A model from the [list of models](../concepts/models.md), or the default. |
| Callback URL | Where pergent.sh will send results. Empty means none. Only `http` and `https`, with no user name or password. |
| Limits | The caps on its requests (below). |
| Keys | The secrets its callers send. A client can have several. |
| Callback secret | Signs the callbacks pergent.sh sends to the client. |

New clients get these limits. An admin changes them on the client's **Settings** tab.

| Limit | Default | Range |
| --- | --- | --- |
| Max request bytes | 524288 | 1024 to 819200 |
| Max files | 200 | 1 to 1000 |
| Max in flight | 5 | 1 to 50 |
| Requests per hour | 600 | 1 to 100000 |
| Monthly token budget | 10000000 | more than 0 |

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

On the **Settings** tab press **Rotate secret**. The new secret is shown once. The old one **keeps signing for 24 hours**, so the caller can switch over without dropping a callback; the tab says until when. Rotating again replaces the old secret that was still signing.

## Remove a client

On the **Settings** tab press **Remove client**. Its keys stop at once and its data is deleted. It cannot be undone.

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

`POST /v1/reviews` queues a review of a change. The change is the content of each file before (`old`) and after (`new`), not a diff.

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

| Field | Rule |
| --- | --- |
| `subject` | Required. 1 to 200 characters. Your stable ID for what changed. |
| `tenant` | At most 100 characters. Your customer or workspace. |
| `title` | At most 200 characters. Shown on the dashboard. |
| `description` | At most 32 KiB of UTF-8. Dropped with the files once the review has them. |
| `files` | Not empty, at most the client's **Max files**. Each has a relative `path` (once in the list, no `..`) and `old`, `new` or both. A file with no `old` is new, one with no `new` is deleted. UTF-8 text. |

`subject`, `tenant` and `title` hold no control character. Unknown fields are ignored. A new request for a subject (client, tenant and subject together) that has a review queued or running supersedes it.

The answer is `202` with the review (`"status": "queued"`) and `Location: /v1/reviews/<id>`. The files are sealed, kept only until the review's Job has them, and then deleted.

Send an `Idempotency-Key` header (1 to 200 printable ASCII characters) to retry safely: the same key with the same body returns the first review and queues nothing. The same key with another body is `422 idempotency_key_reused`.

| Status | `code` | When |
| --- | --- | --- |
| 413 | `too_large` | The body is over the client's **Max request bytes** (never more than 819200), there are more files than **Max files**, or the files are too large once packed for the review. |
| 422 | `invalid` | The body is not valid JSON, or a field or a file breaks its rule. The message names it, a file as `files[<index>]`. |
| 422 | `idempotency_key_reused` | The `Idempotency-Key` was used before for another body. |
| 409 | `cannot_review` | The client has no skills, a skill is not in the library, or its model is no longer in the catalog. |

## Read a review

`GET /v1/reviews/{id}` answers `200` with the review. While it waits or runs the body holds its `status` (`queued`, `running`, `failed`, `superseded`) and the request's `subject`, `tenant` and `title`. When it has finished, the body also holds the `verdict`, each skill's findings and files, and the tokens used. Only the verdict `no_blocking_issues` is a pass: a review that failed, or that reviewed no file, is never one.

A review of another client, an ID that does not exist and an ID that is not a review ID all get the same `404 not_found`. The fields are in [`openapi.yaml`](../../openapi.yaml) (`ReviewApiReview`).

## Audit log

Making, changing and removing a client, making and revoking a key, and rotating a callback secret are in **Admin → Audit log**.
