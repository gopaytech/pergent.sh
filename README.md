# pergent.sh

pergent.sh is a service that runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests. It runs inside your Kubernetes cluster and starts one Kubernetes Job per review.

When someone opens or pushes to a merge request in a repository you listed in the config:

1. GitLab sends a merge request webhook to pergent.sh.
2. pergent.sh starts a Kubernetes Job. The Job checks out the merge request, runs `pergent agent` over the changes and posts the result back.
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a commit status named `pergent`.
4. If the review found no blocking issue, actually reviewed something and skipped no changed file under pergent's default rules, a second GitLab user (the approve bot) approves the merge request.

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the LLM key and a one-time token to hand in its result.

This release covers GitLab only (gitlab.com or any self-managed instance). It has no web UI and no merge request commands; see [Limits](#limits).

## Requirements

- Kubernetes, and permission to create a Role in the namespace where the Jobs run.
- PostgreSQL 13 or newer, reachable from the pergent.sh pod. pergent.sh creates and migrates its own tables at startup.
- Nodes that can pull `ghcr.io/gopaytech/pergent.sh` (the service) and `ghcr.io/gopaytech/pergent` (the Jobs).
- On each GitLab instance, two users:
  - A **review bot** with the **Maintainer** role on every reviewed project, and an access token with the `api` scope. It creates the project webhook and the read-only deploy token, posts the comment and sets the commit status.
  - An **approve bot** that is an eligible approver of every reviewed project, with an access token with the `api` scope. It only approves, and removes its approval when the merge request gets a new commit or a new target branch. If the project requires specific approvers or forbids approval by certain users, make sure this bot qualifies.

    > **Warning:** if a project needs only one approval and the approve bot is an eligible approver, the bot's approval alone unlocks the merge: a merge request can then be merged on a model's review without any human approving it. Give every reviewed project an approval rule that requires a human approval (for example, one approval from a group the approve bot is not in), so the bot's approval is never enough on its own.
- An API key for an LLM provider that opencode supports.
- A way for GitLab to reach pergent.sh over HTTPS (an Ingress, for example).

## Install with Helm

The chart is in `deploy/helm/pergent-sh`. Postgres is not part of it.

1. Create the namespace the Jobs run in and the one pergent.sh runs in:

   ```bash
   kubectl create namespace pergent
   kubectl create namespace pergent-sh
   ```

2. Create a Secret in the namespace where pergent.sh will run. Its keys become environment variables:

   ```bash
   kubectl -n pergent-sh create secret generic pergent-sh-env \
     --from-literal=DATABASE_URL='postgres://pergent:CHANGE_ME@postgres.example.com:5432/pergent?sslmode=require' \
     --from-literal=PERGENT_SH_SECRET_KEY="$(openssl rand -base64 32)" \
     --from-literal=OPENCODE_API_KEY='CHANGE_ME' \
     --from-literal=GITLAB_REVIEW_TOKEN='CHANGE_ME' \
     --from-literal=GITLAB_APPROVE_TOKEN='CHANGE_ME' \
     --from-literal=GITLAB_WEBHOOK_SECRET="$(openssl rand -hex 32)"
   ```

   Keep `PERGENT_SH_SECRET_KEY`: it encrypts the stored deploy tokens, and a different key makes them unreadable.

3. Write your values:

   ```yaml
   # values.yaml
   existingSecret: pergent-sh-env
   jobsNamespace: pergent

   # Where Jobs may connect on TCP 443 and 80: GitLab and the LLM provider.
   jobEgress:
     cidrs:
       - 192.0.2.0/24

   ingress:
     enabled: true
     className: nginx
     host: pergent.example.com
     tls:
       - secretName: pergent-example-com-tls
         hosts: [pergent.example.com]

   config:
     listen: ":8080"
     skills_dir: /etc/pergent-sh/skills
     public_url: https://pergent.example.com
     gitlab:
       - name: main
         url: https://gitlab.example.com
         review_token_env: GITLAB_REVIEW_TOKEN
         approve_token_env: GITLAB_APPROVE_TOKEN
         webhook_secret_env: GITLAB_WEBHOOK_SECRET
     repositories:
       - gitlab: main
         project: acme/widgets
     llm:
       provider: anthropic
       model: claude-sonnet-4
       api_key_env: OPENCODE_API_KEY
     jobs:
       namespace: pergent

   # The skill library: file name -> content.
   skills:
     code-review.md: |
       ---
       name: code-review
       ---
       Review the change for correctness bugs and unsafe error handling.
   ```

4. Install:

   ```bash
   helm install pergent-sh deploy/helm/pergent-sh -n pergent-sh -f values.yaml
   ```

On startup pergent.sh creates the merge request webhook and a read-only deploy token on every listed project. You do not create either by hand.

What the chart creates:

- A Deployment with **one replica** and the `Recreate` strategy. Do not scale it up: at startup pergent.sh requeues runs it had claimed but not launched, which is only safe when no other pod is dispatching. The pod runs as non-root with a read-only root filesystem, drops all capabilities, and has startup, readiness and liveness probes on `/healthz` (the startup probe allows 150 s, as pergent.sh starts listening only after the database, migrations and GitLab tokens check out). The pod restarts on a config or skills change.
- A Service on port 8080 and, when `ingress.enabled`, an Ingress that by default exposes only `/webhooks/` and `/runs/` (`ingress.paths`).
- A ServiceAccount, and in `jobsNamespace` a Role and RoleBinding for it: `batch/jobs` (create, get, list, watch, patch, delete), `secrets` and `configmaps` (create, get, delete) and `pods` (get, list). There are no wildcard rules and nothing outside `jobsNamespace`.
- A NetworkPolicy in `jobsNamespace` for the Job pods (those labelled `app.kubernetes.io/managed-by: pergent-sh`): no ingress; egress to DNS (UDP and TCP 53), to TCP 443 and 80 in `jobEgress.cidrs`, and to TCP 8080 of the pergent.sh pods for the result callback. A NetworkPolicy only takes effect when your CNI enforces them.
- ConfigMaps for `config.yaml` and the `skills`.

**Narrow `jobEgress.cidrs`.** The default `0.0.0.0/0` lets a Job reach any address on ports 80 and 443, including in-cluster services and the cloud metadata endpoint. Set it to the addresses of GitLab and the LLM provider. An empty list (`cidrs: []`) allows no external egress at all: Jobs then reach only DNS and pergent.sh's port 8080. The optional `jobEgress.except` list is added to every `ipBlock` as NetworkPolicy `except` (for example `169.254.169.254/32`); it applies to every cidr, and Kubernetes rejects an entry outside its cidr, so use it with a broad cidr such as the default `0.0.0.0/0`.

`config.jobs.namespace` must equal `jobsNamespace`, `config.skills_dir` must be `/etc/pergent-sh/skills` and `config.listen` must end in `:8080`; the chart refuses to render otherwise.

If `public_url` points at a hostname that the Job pods reach through the Ingress, they need egress to it on 443 (covered by the default `jobEgress.cidrs`). To keep Jobs off the internet path, set `callback_url` to the in-cluster Service, for example `http://pergent-sh.pergent-sh.svc:8080`.

### Environment variables

| Variable | Used for |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL. Required. |
| `PERGENT_SH_SECRET_KEY` | Encrypts stored deploy tokens (AES-256-GCM). 32 random bytes, standard base64. Required. To rotate it, change it and restart: the startup reconcile replaces every deploy token it can no longer decrypt with a new one and revokes the old. |
| the variable named by `llm.api_key_env` (default `OPENCODE_API_KEY`) | The LLM key handed to each Job. Required. |
| each `gitlab[].review_token_env`, `approve_token_env`, `webhook_secret_env` | The review bot token, the approve bot token and the webhook secret of that instance. Required, and checked at startup. |

pergent.sh also reads `KUBECONFIG` when it runs outside a cluster, which is handy for local development.

## Config reference

The config file is YAML (`pergent-sh serve --config <path>`, default `/etc/pergent-sh/config.yaml`). Unknown keys are an error. It holds no secrets: tokens and keys are named by environment variable. Every problem found is reported at startup, and pergent.sh does not start with an invalid config, a missing skill, a missing secret, a database it cannot reach or a GitLab token that does not work.

| Field | Default | Description |
| --- | --- | --- |
| `listen` | `:8080` | Address of the HTTP server. |
| `public_url` | required | HTTPS base URL where GitLab reaches pergent.sh, without a trailing slash. The webhook is created as `<public_url>/webhooks/gitlab/<instance name>`. |
| `callback_url` | `public_url` | Base URL where Jobs post their result (`<callback_url>/runs/<id>/result`). `http` or `https`, no trailing slash. Set it to the in-cluster Service to keep that traffic inside the cluster. |
| `skills_dir` | required | Directory with the skill library: top-level `*.md` files and `<name>/SKILL.md` folders. The chart sets it to `/etc/pergent-sh/skills`. |
| `gitlab[].name` | required | Instance name, `[a-z0-9-]+`, unique. Part of the webhook URL. |
| `gitlab[].url` | required | Base URL of the GitLab instance. It must be `https` (the bot tokens travel in a header), without a trailing slash. A redirect to another host is never followed. |
| `gitlab[].review_token_env` | required | Name of the env var with the review bot token. |
| `gitlab[].approve_token_env` | required | Name of the env var with the approve bot token. |
| `gitlab[].webhook_secret_env` | required | Name of the env var with the webhook secret. GitLab sends it as `X-Gitlab-Token`. |
| `repositories[].gitlab` | required | Name of one of the `gitlab` instances. |
| `repositories[].project` | required | Project path, such as `acme/widgets`. Each project appears once per instance. |
| `repositories[].skills` | all library skills | Names of library skills to run. Empty or omitted means all of them. A name that is not in the library stops pergent.sh at startup. |
| `repositories[].failure_status` | `failed` | Commit status used when a review could not be done: `failed` or `neutral`. GitLab has no neutral state, so `neutral` is set as `skipped`. A project can count skipped pipelines as successful through a GitLab setting; `failed` is never counted as successful. Blocking findings are always `failed`. |
| `llm.provider` | required | opencode provider ID, such as `anthropic`. |
| `llm.model` | required | Model name, such as `claude-sonnet-4`. |
| `llm.base_url` | none | Custom API base URL for the provider. |
| `llm.api_key_env` | `OPENCODE_API_KEY` | Name of the env var with the LLM key. |
| `jobs.namespace` | required | Namespace the Jobs run in. |
| `jobs.image` | `ghcr.io/gopaytech/pergent:v0.1.0` | The pergent image the Jobs run. It must be pinned by a tag other than `latest`, or by digest. |
| `jobs.max_running` | `5` | Most Jobs running at once. Further runs wait in the queue. At least 1. |
| `jobs.timeout` | `10m` | pergent's own review budget per Job (`--timeout`). The Job's deadline adds 2 minutes to deliver and a clone allowance of at least 2 minutes. |
| `jobs.concurrency` | `4` | pergent's `--concurrency`: opencode runs in flight inside one Job. At least 1. |
| `jobs.resources` | requests 500m CPU, 1Gi memory; limits 2 CPU, 4Gi memory | Kubernetes `requests` and `limits` for both containers of a Job. If you set either `requests` or `limits`, the default is not used at all. |

## Skills and instructions

- **Library skills** are the files in `skills_dir`. A repository's `skills:` list picks from them.
- **Repository skills** live in the reviewed repository, in `.pergent/skills/` (`*.md` files and `<name>/SKILL.md` folders). A repository skill replaces a library skill of the same name.
- **Instructions** are the root `AGENTS.md` (or `CLAUDE.md` when there is no `AGENTS.md`) plus `.pergent/instructions.md`.

Repository skills and instructions are read from the target branch at the merge request's `diff_refs.start_sha`, not from the merge request's own branch, so a merge request cannot change the rules it is reviewed by. Together with the previous review they must fit in 900 KiB; the previous review is dropped first, and if the skills and instructions alone are too large the run fails with a comment and no Job starts.

## How approval works

pergent.sh decides from what `pergent agent` reports. The result is checked again before anything is published: it must parse and be for the same base and head commits as the run, or it is posted as a failure.

| What happened | Comment | Commit status | Approved |
| --- | --- | --- | --- |
| Any skill has blocking issues | The review | `failed` ("Blocking issues found") | No |
| Any skill failed, has an unknown verdict, or is "clean" yet lists files it did not review | The review | `failure_status` ("Review failed for N of M skills") | No |
| The Job died or sent nothing usable, the result is an error, has no skills, or has an unknown status | An error comment with the reason | `failure_status` | No |
| Every skill is clean, but no file was reviewed because every changed file was skipped | The review plus a note listing the skipped files and the rules that skipped them | `success` ("Nothing to review: every changed file is skipped") | **No**, a human needs to look |
| Every skill is clean and at least one file was reviewed, but some changed file was skipped as `deleted`, `binary`, `vendored`, `lockfile` or `generated` | The review plus a note listing those files and the rules that skipped them | `success` ("No blocking issues; not approved: some changed files weren't reviewed") | **No**, a human needs to look |
| Every skill is clean, at least one file was reviewed, and no file was skipped under those rules | The review | `success` ("No blocking issues") | Yes, see below |

Those five are pergent's default skip rules, and the change itself decides whether they apply: a merge request can, for example, put code under a `vendor/` directory to keep it from the reviewer. A file skipped only because it is outside a skill's `paths`, matches a skill's `exclude`, or has no content changes does not stop the approval. The note lists at most 20 paths.

Before approving, pergent.sh asks GitLab for the merge request. It approves only when the merge request is still **open**, its head is still the commit that was reviewed and its target branch is still the one it was reviewed against. If the head or target moved or the MR was closed, there is no approval; the review and its status still belong to the reviewed commit, and a newer push gets its own run. The approval is recorded before it is made, and a GitLab refusal because the bot already approved is treated as done.

When a merge request gets a new head or a new target branch after the approve bot approved it, the approve bot removes its approval, whether or not GitLab resets approvals itself. This happens as soon as the webhook arrives. GitLab doesn't resend a webhook it failed to deliver (pergent.sh was restarting, the hook was disabled), so pergent.sh also checks every approval it holds against GitLab at startup and every 15 seconds: an approval of a commit that is no longer the head, or of a merge request that was retargeted or closed, is removed then, and the current head gets a review if the repository is still configured.

A failed, partial or missing review is never `success` and is never approved.

### Pushes, edits and failures

- Only pushes, opening or reopening, and changing the target branch start a run. Editing the title, labels, description or assignees does not. Closing or merging supersedes the MR's runs.
- A run is one head reviewed against one target branch. Changing the target branch reviews the same head again against the new branch, supersedes the review against the old one and withdraws the bot's approval.
- Two pushes in quick succession: only the newest head is reviewed. The older Job is deleted, and a late result for it is accepted and discarded. Out-of-order push webhooks are handled: the dispatcher reviews the MR's current head.
- If GitLab has not computed the merge request's diff after 10 minutes, the run fails with a comment.
- A Job that dies without a result (out of memory, a failed checkout, the deadline, a failed delivery, a deleted Job) produces an error comment and a non-success status within about 2 minutes.
- A Job whose pod can't start because its image can't be pulled (or its container config is invalid) is deleted and reported after about 5 minutes.
- A Job whose pod the cluster refuses to create (for example, one rejected by an admission policy) is only reported when the Job reaches its deadline, as `DeadlineExceeded`: `jobs.timeout` plus at least 4 minutes, and about a minute more for the report.
- Runs that were claimed but not launched (for example because pergent.sh crashed) are requeued at startup and after 5 minutes.
- Publishing to GitLab is retried with backoff. After 10 failed attempts the run becomes `publish_failed` and nothing more is posted to the merge request; the failure is only in the logs.

## What a Job can and cannot see

A Job has two containers, started in order. Both run the pinned pergent image and have `allowPrivilegeEscalation: false` and all capabilities dropped. The pod never mounts a Kubernetes service account token.

**The checkout container** gets only a read-only deploy token (`read_repository`), through git's `http.extraHeader` setting passed in environment variables. The token is never written to disk, never in a URL and never in `.git/config`. It fetches only the base and head commits (not the whole repository history), disables hooks and gives the remote an invalid push URL.

**The pergent container** runs `pergent agent` and gets:

- the checkout, which is untrusted code under review, read-only to the agent;
- the skills, the instructions and the previous review, mounted read-only at `/in`;
- `OPENCODE_API_KEY` (the LLM key) and a one-time result token. The result token is random, stored only as a hash, valid for one result and expires shortly after the Job's deadline;
- the provider, model and, if set, base URL.

The agent in the pergent container can only read files, grep and list. It cannot edit files or run commands, and `*.env`, `*.pem`, `*.key` and `*.tfvars` files are denied. pergent deletes instruction files found in the checkout, so the reviewed repository cannot change the instructions.

A Job **cannot** see: the review bot token, the approve bot token, the webhook secret, the database URL or the secret key, other Jobs' Secrets, the Kubernetes API (no service account token, and the Role is for pergent.sh's account only), or anything but DNS, GitLab and LLM addresses in `jobEgress.cidrs` and pergent.sh's own port 8080, when your CNI enforces the NetworkPolicy. Be aware that the LLM key is in the pergent container's environment, so use a key meant for this purpose, with a spending limit.

Each Job's Secret and inputs ConfigMap are owned by the Job, so deleting the Job removes them. Finished Jobs are removed after an hour.

## Fork merge requests

The checkout first tries to fetch the base and head commits by SHA. When the server refuses that (this is common for merge requests from forks), it fetches `refs/merge-requests/<iid>/head` instead, then deepens the target branch (starting at 50 commits and doubling, up to 3200) until the merge base is there. If the merge base is still missing at that depth, the checkout fails and the merge request gets an error comment.

## Switching a repository off

A repository is on when it is listed in the config. To switch it off, remove it from `repositories` and redeploy. On the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token. Runs still queued for it are dropped when they come up for dispatch.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days.

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.

## HTTP routes

| Route | Used by |
| --- | --- |
| `POST /webhooks/gitlab/{instance}` | GitLab merge request webhooks. The `X-Gitlab-Token` header is compared in constant time with the instance's secret; a mismatch gets 401 and is otherwise ignored. An authenticated webhook whose payload can't be used (not JSON, no project id or merge request iid, a malformed head SHA) gets 200 and a warning in the log, because GitLab counts failed deliveries toward disabling the hook. |
| `POST /runs/{id}/result` | A review Job delivering its result, with its one-time bearer token. |
| `GET /healthz` | Health check. It answers as soon as the server starts: pergent.sh serves right away and runs the startup reconcile in the background. |

Logs are JSON on stdout. Secrets are never logged.

## Limits

This release (2a) is GitLab only and deliberately small:

- No web UI or dashboard.
- No merge request commands (such as asking for a re-review in a comment).
- No advice on whether a human approval is still needed.
- No repository cache: every checkout is a fresh `git init` plus a shallow fetch.
- One replica, and PostgreSQL is external.
- A repository is switched off only by removing it from the config.
- GitHub is not supported yet.

## Development

```bash
make build   # builds bin/pergent-sh
make vet
make test
```

Tests for the store and the end-to-end flow need a PostgreSQL database; they skip when `PERGENT_SH_TEST_DATABASE_URL` is not set:

```bash
docker run -d --name pergent-sh-pg -e POSTGRES_PASSWORD=pergent -e POSTGRES_DB=pergent_test -p 55432:5432 postgres:16
export PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:pergent@localhost:55432/pergent_test?sslmode=disable'
go test -count=1 ./...
```

All other tests need no external service. Release images are built by the `release` workflow when a `v*.*.*` tag is pushed and published as `ghcr.io/gopaytech/pergent.sh:<tag>` (linux/amd64).
