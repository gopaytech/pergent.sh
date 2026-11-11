# 🔗 Review API

The review API lets another system ask pergent.sh for reviews. This page covers **API clients and their keys**, **asking for a review and reading it**, **getting the result by a signed callback**, **dismissing a finding**, **the limits** and **versioning**.

It exists only when the dashboard is on (`dashboard.url` is set). It adds no config key.

## What a client is

A client is one system that calls the API: a script, a bot, another service. An admin makes it under **API clients** in the menu (`/api-clients`). Only admins see that page.

| Part | What it is |
| --- | --- |
| Name | 1 to 63 characters: `a-z`, `0-9` and `-`. It never changes. |
| Skills | The library skills its reviews use. |
| Model | A model from the [list of models](../concepts/models.md), or the default. |
| Callback URL | Where pergent.sh sends each result: [Get the result by callback](#get-the-result-by-callback). Empty means none. Only `http` and `https`, with no user name or password. Use `https`. |
| Limits | The caps on its requests (below). |
| Keys | The secrets its callers send. A client can have several. |
| Callback secret | Signs the callbacks pergent.sh sends to the client. |

New clients get these limits. An admin changes them on the client's **Settings** tab. What each one does is in [Limits](#limits).

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

On the **Settings** tab press **Rotate secret**. The new secret is shown once. The old one **keeps signing for 24 hours**, so the caller can switch over without dropping a callback; the tab says until when. During those 24 hours each callback carries both signatures. Rotating again replaces the old secret that was still signing.

## Remove a client

On the **Settings** tab press **Remove client**. Its keys stop at once, the Jobs of its running reviews are deleted, and its data is deleted, its queued reviews with it. No callback is sent for them, and callbacks not yet delivered are dropped. It cannot be undone.

## Check a key

`GET /v1/client` says which client a key belongs to, its limits and the tokens its reviews used this UTC month. It is a good first call.

```json
{
  "name": "ci",
  "limits": {
    "max_request_bytes": 524288,
    "max_files": 200,
    "max_in_flight": 5,
    "requests_per_hour": 600,
    "monthly_token_budget": 10000000
  },
  "usage": {"month": "2026-11", "tokens_used": 120000, "tokens_left": 9880000}
}
```

Every `/v1/` answer is sent `Cache-Control: no-store`. Field names are snake_case. An error is `{"code": "...", "message": "..."}`:

| Status | `code` | When |
| --- | --- | --- |
| 401 | `unauthorized` | The key is missing, unknown, revoked or expired. The body is the same for each. |
| 404 | `not_found` | A path under `/v1/` that is not a route (the key is checked first). |
| 429 | `rate_limited` | The key is past its client's **Requests per hour**. Wait the `Retry-After` seconds. |
| 500 | `internal` | The key could not be checked. Try again. |

There are ten codes in all: `unauthorized`, `not_found`, `invalid`, `too_large`, `rate_limited`, `budget_used_up`, `cannot_review`, `not_finished`, `idempotency_key_reused` and `internal` (every 5xx). The tables below say when each comes.

v1 only gains optional fields and keeps its codes: ignore a field you don't know ([Versioning](#versioning)). The full description is in [`openapi.yaml`](../../openapi.yaml) and the routes are in [HTTP routes](../reference/http-routes.md).

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

1. The key is past its **Requests per hour**: `429 rate_limited`.
2. The body is over **Max request bytes** (never more than 819200): `413`. A body that could not be read at all is `422 invalid`, `the body could not be read`.
3. The `Idempotency-Key` header is not valid: `422`.
4. The key was used before: the first review is returned, or `422 idempotency_key_reused` for another body. Nothing else is checked.
5. The client's **Monthly token budget** is used up: `429 budget_used_up`.
6. The body is not UTF-8 text, not valid JSON, or breaks a field or file rule: `422 invalid`.
7. More files than **Max files**, or files over the packed limit: `413 too_large`.
8. The client can't review: `409 cannot_review`.
9. Otherwise the review is queued: `202`.

The answer is `202` with the review (`"status": "queued"`) and a `Location: /v1/reviews/<id>` header. A new request for a subject (client, tenant and subject together) that has a review queued or running supersedes it; a review already being finished is not superseded.

Send an `Idempotency-Key` header (1 to 200 printable ASCII characters) to retry safely. The same key with the **same body** (byte for byte) returns the first review, as it is now, with `202`, and queues nothing. The same key with another body is `422 idempotency_key_reused`.

| Status | `code` | When |
| --- | --- | --- |
| 413 | `too_large` | The body is over the client's **Max request bytes**, there are more files than **Max files**, or the files are too large once packed for the review. |
| 422 | `invalid` | The body could not be read, is not valid JSON or not UTF-8 text, a field or a file breaks its rule, or the `Idempotency-Key` is not valid. The message names it, a file as `files[<index>]`. |
| 422 | `idempotency_key_reused` | The `Idempotency-Key` was used before for another body. |
| 409 | `cannot_review` | The client has no skills, a skill is not in the library, or its model is no longer in the catalog. |
| 429 | `rate_limited`, `budget_used_up` | Past the key's **Requests per hour**, or the client's **Monthly token budget**. See [Limits](#limits). |
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

Poll until the status is `completed`, `failed` or `superseded`; a review takes minutes. What a finished body holds depends on how it ended:

| `status` | `finished_at` | `verdict` | `skills` | `failure` | `usage` |
| --- | --- | --- | --- | --- | --- |
| `completed` | yes | yes | yes | no | yes |
| `failed` | yes | `failed` | left out | yes | only if the Job left a result |
| `superseded` | left out | left out | left out | no | left out |

`usage` is `tokens_in` and `tokens_out`. A `completed` body looks like this:

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
| `starting` | Before the Job, or while it builds the commits | `this client's monthly token budget is used up`, `this client has no skills: an admin chooses them in its settings`, `the model chosen for this API client can't be used: an admin can change it in the client's settings`, `skill "x" is not in the skill library`, `the review's files are no longer stored` |
| `review` | While pergent reviews | `the review job ran out of memory (OOMKilled)`, `pergent couldn't review the change` |
| `delivery` | When the result comes back | `pergent sent a result pergent.sh couldn't use` |
| `unknown` | The Job vanished, or hit its deadline before it started | `the review job disappeared before sending a result` |

## Get the result by callback

When a review ends (`completed`, `failed` or `superseded`), pergent.sh `POST`s it to the client's **Callback URL**. That is the only callback a review gets: none for `queued` or `running`. The body is the review's `GET /v1/reviews/{id}` body at the time it is sent, byte for byte.

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` |
| `X-Pergent-Delivery` | A UUID, the same on every retry of this callback. Drop a delivery you already handled. |
| `X-Pergent-Signature` | `t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">`, with the callback secret as the key. |

- **Delivered** is any `2xx` answer. Anything else, a redirect (never followed) or no answer within 10 seconds is a failed attempt.
- **Retries** wait 10 s, then twice as long each time (10 s, 20 s, 40 s, ... at most an hour), for 24 hours after the review ended. Then pergent.sh gives up and the result stays readable with `GET`. A callback is sent **at least once**: a retry can follow a try your receiver handled, so use `X-Pergent-Delivery` to drop the repeat.
- **A rotation** puts two `v1=` values in the header for 24 hours, the new secret's first. Accept the callback if **any** `v1=` matches.
- **The URL and secret** are read at each attempt, so a fix to either applies to the next retry.
- **Failing callbacks** raise a **Needs attention** item for the client, seen by admins, once 3 of its callbacks from the last 24 hours are undelivered. It clears when fewer are.
- **Emptying the Callback URL** turns callbacks off: those not yet delivered are given up at their next try (`the client has no callback URL`), and they no longer count as failing, so the item clears.

### Handle a delivery

1. **Verify it** (below) before you read the body. Refuse a delivery that does not verify, with `401`.
2. **Drop a duplicate.** `X-Pergent-Delivery` is the same on every retry of a callback. A retry can arrive after you handled the first try but failed to answer in time, so keep the IDs you handled and answer `2xx` again for one you have.
3. **Answer fast.** Reply `2xx` within 10 seconds and do the work after. Anything slower counts as a failed attempt and comes again.
4. **Gate on `verdict`**, as for `GET`. A body with `"test": true` is a [test callback](#try-the-callback): drop it.

### Verify a callback

The header is `X-Pergent-Signature: t=<unix seconds>,v1=<hex>`, with one `v1=` per secret (two during a rotation). Each `v1` is the hex HMAC-SHA256, keyed with the callback secret, of the text `<t>.<body>`: the `t` value, a dot, then the **raw bytes** of the body. Check it like this:

1. Read the raw body before you parse it. A body parsed and written again is not the same bytes.
2. Refuse a `t` more than 5 minutes from your clock, so an old capture can't be sent again. Every retry is signed afresh, so a good retry always has a recent `t`.
3. Compute the HMAC and compare it with **each** `v1` in constant time. Accept if any matches.

A function in Go:

```go
package callbacks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// verify checks the X-Pergent-Signature header against the raw request body.
func verify(header string, body []byte, secret string, now time.Time) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("no timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("timestamp out of range")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("no signature matches")
}
```

Call it with `r.Header.Get("X-Pergent-Signature")`, the bytes from `io.ReadAll(r.Body)`, your secret and `time.Now()`.

In a shell, with the raw body saved as `body.json` (don't read it into a variable: that drops a trailing newline), print the HMAC and compare it with each `v1`:

```
t=1700000000   # from X-Pergent-Signature
{ printf '%s.' "$t"; cat body.json; } | openssl dgst -sha256 -hmac "$CALLBACK_SECRET" | awk '{print $NF}'
```

For the body `{}` (two bytes, no newline), `t=1700000000` and the secret `pgs_x`, it prints `209391bb8c46dd6f45407256a2fa898b73eb7f9ad511a1b206a745eb936f3c6e`, so the header is `t=1700000000,v1=209391bb8c46dd6f45407256a2fa898b73eb7f9ad511a1b206a745eb936f3c6e`.

### Try the callback

On the client's **Settings** tab press **Send test callback**. pergent.sh sends your Callback URL a test review the way it sends a real one: the same headers, the same secrets (the old one too, during a rotation), a new `X-Pergent-Delivery` each time, and no redirect followed. The tab then reads `Answered 204 in 42 ms`, or the reason it got no answer. It is a failure unless the URL answered `2xx`.

The body is a review with `"test": true`, the ID `00000000-0000-0000-0000-000000000000`, the subject `pergent.sh test callback` and the verdict `nothing_reviewed`. Never treat it as a pass: a receiver that gates on `no_blocking_issues` rejects it, and one that sees `test: true` should drop it. It is not stored as a callback of any review and does not count as failing. The button needs a Callback URL; without one it answers `this client has no callback URL`.

## Dismiss a finding

A finding that is wrong can be set aside for its **subject** (the client, `tenant` and `subject` together), so later reviews of the subject honour it: see [Later reviews of a subject](#later-reviews-of-a-subject). `{finding}` is the finding's `id` in that review's result.

```
curl -X POST -H "Authorization: Bearer pgk_1a2b3c4d_..." -H "Content-Type: application/json" \
  -d '{"reason": "checked upstream"}' \
  https://<dashboard host>/v1/reviews/<id>/findings/<finding id>/dismiss
```

To take it back (undo), `DELETE` the same path. It has no body:

```
curl -X DELETE -H "Authorization: Bearer pgk_1a2b3c4d_..." \
  https://<dashboard host>/v1/reviews/<id>/findings/<finding id>/dismiss
```

- `200 {"status": "dismissed"}`, or `{"status": "already_dismissed"}` (nothing changes, the first reason stays). `DELETE` answers `{"status": "restored"}` or `{"status": "not_dismissed"}`.
- `reason` is required, 1 to 500 characters, with no control character other than a line break or a tab (`422 invalid` otherwise, as for a body that is not valid JSON or UTF-8 text).
- The body is at most 16384 bytes (`413 too_large`).
- The review must be `completed` (`409 not_finished`), and the finding must be in its result with a `fingerprint` (`404 not_found`). A review of another client is `404 not_found` first, before the body is looked at.
- The review it was made on does not change, and no callback is sent. The actor is recorded as `key <key name>` and is in no response body.

An admin does the same on the review's page: **Dismiss** on a finding (a reason is required) and **Stop** on one that is dismissed. The page lists each dismissal and restore under **History**, with who did it. These are not in the audit log.

## Send the change again, don't re-run

There is no re-run. To review again, send the change again with the same `subject`: a review of that subject still queued or running is superseded, and the new one reviews what you send now. Use a new `Idempotency-Key` for each new attempt; reuse one only to retry a request whose answer you lost.

## Later reviews of a subject

A review of a subject that was reviewed before is a re-review, as a merge request's next push is:

- It is given the findings of the subject's newest `completed` review, as data to compare against.
- It is told what people dismissed on the subject (at most 20 a skill, blocking first), with the reasons, so it can leave them out. If they can't be read, or don't fit, it runs without them.
- A finding a dismissal covers stays in the result with `dismissed` (`reason`, `at`) and does not block: a skill whose blocking findings are all dismissed is `no_blocking_issues`. A `blocking` dismissal covers both severities, a `suggestion` one only suggestions. The dismissals are read when the review is finished, so one made while it ran counts.

## Versioning

The API is `/v1/`. Inside v1 the shape only grows:

- **Ignore a field you do not know.** v1 may add optional fields to any body, including a callback's.
- **The enums are fixed in v1.** `status`, `verdict`, `step`, the skipped `rule` and the error `code` never gain a value. A new value is a change of meaning, so it comes as `/v2/`, never inside `/v1/`. A caller that switches on one of them need not handle an unknown value, but should treat anything it does not expect as not a pass.
- **Routes, fields and codes are not removed or renamed** in v1.
- Field names are snake_case. Times are RFC 3339.

[`openapi.yaml`](../../openapi.yaml) describes every route, body, code and enum, and the callback as the webhook `reviewFinished`.

## Strip secrets before you send

The files and the description go to the model, so they leave your system. Remove credentials, tokens and private data from them first: pergent.sh can't tell which part of a file is secret. It masks only the secrets it holds itself, such as the model's key, in what it stores and returns.

## See a client's reviews in the dashboard

A client's page opens on its **Reviews** tab: its reviews, newest first, with the subject, tenant, title, status and verdict. Filter by tenant, subject or status, and press **Load more** for older ones. A review's subject opens its page: who asked (subject, tenant, the key's name), the model, the tokens, each skill's findings (with the fix and the replacement text) and its files (reviewed, skipped with the rule, and not reviewed with the reason). A failed review reads `Failed at <step>: <reason>`, with the admin-only detail below. A review with a callback lists its **Callback**: delivered, pending or given up, and each attempt with its time, the status the URL answered (or the error) and how long it took. The **Logs** tab shows the Job's log, the clone log and the pod's state. The page has no re-run button: the caller sends the change again.

The list of clients shows, for each, the reviews of the UTC month so far, how many failed and the tokens they used.

## Limits

An admin sets the limits of each client (the defaults are [above](#what-a-client-is)). Four things are held, and each refuses in its own way:

| Limit | Past it |
| --- | --- |
| Max request bytes, Max files | The request is `413 too_large`. Packed for the Job, the files may not pass 1000000 bytes either. |
| Requests per hour | Each key holds this many requests, on any route, and gets them back evenly, not all at the top of the hour (600 an hour is one more every 6 seconds). A new key can use all of them at once. Past it the request is `429 rate_limited`, `too many requests for this key`, with `Retry-After`: the seconds until one more is allowed. Every request with a good key takes one, whatever its route or answer; a request refused with `429` takes none. The count is kept in memory: a restart gives every key a full allowance. |
| Max in flight | At most this many of the client's reviews run at once. More wait in the queue, oldest first, and hold up no other client and no merge request. |
| Monthly token budget | Once the tokens of the reviews asked for this UTC month reach it, `POST /v1/reviews` is `429 budget_used_up`, `this client's monthly token budget is used up`, with `Retry-After` until the next month. A review already queued fails at `starting` with the same reason. A retry with an `Idempotency-Key` used before is still answered. |

Wait the `Retry-After` seconds after a `429`, then try again; do not retry at once. The tokens are counted as on the **API clients** list: tokens in and out, by the month each review was asked for. A review that is running counts once its result is in, so the budget can be passed by the reviews running when it is reached. `GET /v1/client` shows what is used and left.

## How a review runs

- It is a Kubernetes Job, like a merge request's review. Both share `jobs.max_running`, first in, first out. A client's **Max in flight** holds its own reviews back too.
- It uses the client's skills and model, read when the Job starts. The Job's timeout, memory and Max steps are the defaults on **Admin → Settings**.
- The files reach the Job in a Secret of their own, which only its checkout container reads. The watcher tries to delete it the first time it sees the checkout done, and again before it reports a stuck, vanished or finished Job. A delete that fails is logged and not tried again at that step: the Secret then goes with the failure report, or with the Job, an hour after it ends. If the result arrives before a watcher pass sees the checkout done, the Secret stays until the Job is deleted, an hour after it ends. The stored copy is deleted once the Job has started.
- Nothing is posted to GitLab or GitHub.

A failed review has a `failure` with the step it failed at: see [A failed review](#a-failed-review).

## Audit log

Making, changing and removing a client, making and revoking a key, rotating a callback secret and sending a test callback (`api_callback_tested`, naming the client) are in **Admin → Audit log**.
