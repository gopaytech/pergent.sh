# pergent.sh

pergent.sh is a service that runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests. It runs inside your Kubernetes cluster and starts one Kubernetes Job per review.

When someone opens or pushes to a merge request (or pull request) in a repository you listed in the config:

1. GitLab sends a merge request webhook, or GitHub a `pull_request` webhook, to pergent.sh.
2. pergent.sh starts a Kubernetes Job. The Job checks out the change, runs `pergent agent` over it and posts the result back.
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub.
4. If the review found no blocking issue, actually reviewed something and skipped no changed file under pergent's default rules, a separate approver approves it: a second GitLab user on GitLab, a second GitHub App (or a bot user) on GitHub. A draft is reviewed the same way but is approved only once it is marked ready.

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the LLM key and a one-time token to hand in its result.

This release covers GitLab (gitlab.com or any self-managed instance) and GitHub.com; see [GitHub](#github) for the GitHub setup. It has no web UI and no merge request commands; see [Limits](#limits).

In this README a "merge request" is also a GitHub pull request, and the "review bot" and "approve bot" are the GitHub review App and approver, unless a section says otherwise.

## Requirements

- Kubernetes, and permission to create a Role in the namespace where the Jobs run.
- PostgreSQL 13 or newer, reachable from the pergent.sh pod. pergent.sh creates and migrates its own tables at startup.
- Nodes that can pull `ghcr.io/gopaytech/pergent.sh` (the service) and `ghcr.io/gopaytech/pergent` (the Jobs).
- On each GitLab instance you use, two users:
  - A **review bot** with the **Maintainer** role on every reviewed project, and an access token with the `api` scope. It creates the project webhook and the read-only deploy token, posts the comment and sets the commit status.
  - An **approve bot** that is an eligible approver of every reviewed project, with an access token with the `api` scope. It only approves, and removes its approval when the merge request gets a new commit or a new target branch. If the project requires specific approvers or forbids approval by certain users, make sure this bot qualifies.

    > **Warning:** if a project needs only one approval and the approve bot is an eligible approver, the bot's approval alone unlocks the merge: a merge request can then be merged on a model's review without any human approving it. Give every reviewed project an approval rule that requires a human approval (for example, one approval from a group the approve bot is not in), so the bot's approval is never enough on its own.
- For GitHub, two GitHub Apps (or one App and a bot user); see [GitHub](#github).
- An API key for an LLM provider that opencode supports.
- A way for GitLab or GitHub to reach pergent.sh over HTTPS (an Ingress, for example).

## Install with Helm

The chart is in `charts/pergent-sh`. Postgres is not part of it. Every value is listed in the [chart reference](charts/pergent-sh/README.md).

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

   For GitHub, put the Apps' private keys (the whole `.pem` file of each) and the review App's webhook secret in the Secret instead of, or next to, the GitLab tokens; see [GitHub](#github).

3. Write your values:

   ```yaml
   # values.yaml
   existingSecret: pergent-sh-env
   jobsNamespace: pergent

   # Where Jobs may connect on TCP 443 and 80: GitLab or GitHub, and the LLM provider.
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
   helm install pergent-sh charts/pergent-sh -n pergent-sh -f values.yaml
   ```

   By default the chart runs `ghcr.io/gopaytech/pergent.sh:main`, the newest build of the `main` branch (every push to `main` publishes it, together with `sha-<short sha>`), with `image.pullPolicy: Always`. The tag moves, so a pod restart can pick up a newer build. For production pin an image: `--set image.tag=sha-<short sha>` or a release tag such as `v0.1.0` (then `--set image.pullPolicy=IfNotPresent`).

On startup pergent.sh creates the merge request webhook and a read-only deploy token on every listed GitLab project. You do not create either by hand. On GitHub there is nothing per repository to create: the review App's webhook covers every repository it is installed on.

For GitHub only, set `config.gitlab: []`: the chart's default values contain an example GitLab instance, and pergent.sh would refuse to start without its tokens.

What the chart creates:

- A Deployment with **one replica** and the `Recreate` strategy. Do not scale it up: at startup pergent.sh requeues runs it had claimed but not launched, which is only safe when no other pod is dispatching. The pod runs as non-root with a read-only root filesystem, drops all capabilities, and has startup, readiness and liveness probes on `/healthz` (the startup probe allows 150 s, as pergent.sh starts listening only after the database, migrations, GitLab tokens and GitHub Apps check out). The pod restarts on a config or skills change.
- A Service on port 8080 and, when `ingress.enabled`, an Ingress that by default exposes only `/webhooks/` and `/runs/` (`ingress.paths`).
- A ServiceAccount, and in `jobsNamespace` a Role and RoleBinding for it: `batch/jobs` (create, get, list, watch, patch, delete), `secrets` and `configmaps` (create, get, delete) and `pods` (get, list). There are no wildcard rules and nothing outside `jobsNamespace`.
- A NetworkPolicy in `jobsNamespace` for the Job pods (those labelled `app.kubernetes.io/managed-by: pergent-sh`): no ingress; egress to DNS (UDP and TCP 53), to TCP 443 and 80 in `jobEgress.cidrs`, and to TCP 8080 of the pergent.sh pods for the result callback. A NetworkPolicy only takes effect when your CNI enforces them.
- ConfigMaps for `config.yaml` and the `skills`.

**Narrow `jobEgress.cidrs`.** The default `0.0.0.0/0` lets a Job reach any address on ports 80 and 443, including in-cluster services and the cloud metadata endpoint. Set it to the addresses of GitLab (or GitHub) and the LLM provider. An empty list (`cidrs: []`) allows no external egress at all: Jobs then reach only DNS and pergent.sh's port 8080. The optional `jobEgress.except` list is added to every `ipBlock` as NetworkPolicy `except` (for example `169.254.169.254/32`); it applies to every cidr, and Kubernetes rejects an entry outside its cidr, so use it with a broad cidr such as the default `0.0.0.0/0`.

`config.jobs.namespace` must equal `jobsNamespace`, `config.skills_dir` must be `/etc/pergent-sh/skills` and `config.listen` must end in `:8080`; the chart refuses to render otherwise.

If `public_url` points at a hostname that the Job pods reach through the Ingress, they need egress to it on 443 (covered by the default `jobEgress.cidrs`). To keep Jobs off the internet path, set `callback_url` to the in-cluster Service, for example `http://pergent-sh.pergent-sh.svc:8080`.

### Environment variables

| Variable | Used for |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL. Required. |
| `PERGENT_SH_SECRET_KEY` | Encrypts stored deploy tokens (AES-256-GCM). 32 random bytes, standard base64. Required. To rotate it, change it and restart: the startup reconcile replaces every deploy token it can no longer decrypt with a new one and revokes the old. |
| the variable named by `llm.api_key_env` (default `OPENCODE_API_KEY`) | The LLM key handed to each Job. Required. |
| each `gitlab[].review_token_env`, `approve_token_env`, `webhook_secret_env` | The review bot token, the approve bot token and the webhook secret of that instance. Required, and checked at startup. |
| each `github[].review_app.private_key_env` | The review App's private key: the whole PEM file GitHub gave you (PKCS#1 `RSA PRIVATE KEY` or PKCS#8 `PRIVATE KEY`). Required, and checked at startup. Never logged. |
| each `github[].approve_app.private_key_env` or `github[].approve_token_env` | The approve App's private key, or the approve bot user's fine-grained personal access token. Required, and checked at startup. |
| each `github[].webhook_secret_env` | The review App's webhook secret. Required. |

pergent.sh also reads `KUBECONFIG` when it runs outside a cluster, which is handy for local development.

## GitHub

pergent.sh reaches GitHub.com through two GitHub Apps that you create (pergent.sh is self-hosted, so there is no shared App):

- the **review App** receives the `pull_request` webhook, reads the pull request and the repository, posts the comment, sets the `pergent` Check Run and mints each Job's clone token;
- the **approve App** only approves, and dismisses its own approvals when they go stale.

They are two Apps so that the identity that writes the review is never the one that approves it. Where GitHub doesn't count an App's approval, the approve App is replaced by a bot user; see [Approving as a bot user](#approving-as-a-bot-user).

### 1. Create the review App

In the organization's settings, **Developer settings → GitHub Apps → New GitHub App**:

- **Webhook:** active. URL `<public_url>/webhooks/github/<instance name>`, for example `https://pergent.example.com/webhooks/github/gh`. Secret: a random string (`openssl rand -hex 32`); it goes into the Secret as the instance's `webhook_secret_env`.
- **Repository permissions:**
  - Pull requests: **Read-only**
  - Contents: **Read-only**
  - Checks: **Read and write**
  - Issues: **Read and write** (pull request comments are issue comments)
  - Metadata: **Read-only** (always on)
- **Subscribe to events:** **Pull request**.
- **Where can this GitHub App be installed:** "Only on this account", or "Any account" to install it on several organizations.

Create it, note its **App ID** and generate a **private key** (a `.pem` file).

### 2. Create the approve App

The same way, with:

- **Webhook:** not active (uncheck "Active").
- **Repository permissions:** Pull requests: **Read and write**; Metadata: **Read-only**.
- No events.

Note its App ID and generate a private key.

### 3. Install both Apps

Install both Apps on the organization, on all repositories or on the ones you will review. Installing an App switches nothing on: only the repositories listed in the config are reviewed. One `github:` instance covers every organization the two Apps are installed on; pergent.sh looks up the installation per repository.

### 4. Secret and config

```bash
kubectl -n pergent-sh create secret generic pergent-sh-env \
  --from-literal=DATABASE_URL='postgres://pergent:CHANGE_ME@postgres.example.com:5432/pergent?sslmode=require' \
  --from-literal=PERGENT_SH_SECRET_KEY="$(openssl rand -base64 32)" \
  --from-literal=OPENCODE_API_KEY='CHANGE_ME' \
  --from-file=GITHUB_REVIEW_APP_KEY=review-app.private-key.pem \
  --from-file=GITHUB_APPROVE_APP_KEY=approve-app.private-key.pem \
  --from-literal=GITHUB_WEBHOOK_SECRET='the secret you gave the review App'
```

```yaml
config:
  public_url: https://pergent.example.com
  gitlab: []            # GitHub only: drop the chart's example GitLab instance
  github:
    - name: gh
      review_app:
        app_id: 123456
        private_key_env: GITHUB_REVIEW_APP_KEY
      approve_app:
        app_id: 123457
        private_key_env: GITHUB_APPROVE_APP_KEY
      webhook_secret_env: GITHUB_WEBHOOK_SECRET
  repositories:
    - github: gh
      project: acme/widgets
```

At startup pergent.sh reads each App's slug (`GET /app`) and its bot user (`GET /users/<slug>[bot]`), and stops with an error naming the instance if a key doesn't work. The bot user lookup is unauthenticated (GitHub allows 60 an hour per address), which is plenty unless the pod restarts in a loop. Then, at startup and every hour, it looks each listed repository up: the review App's installation, the repository's ID and clone URL, and whether the approve side can reach it. A listed repository the review App isn't installed on is logged as an error on every pass and can't be reviewed.

### The `pergent` Check Run

The status of a review is a Check Run named `pergent` on the reviewed head commit, created by the review App:

- `in_progress` ("pergent review running") when the Job starts;
- then `completed` with the conclusion `success`, `failure`, `neutral` (a failed review with `failure_status: neutral`) or `cancelled` (superseded by a newer push).

Its title is the status description from [How approval works](#how-approval-works) and its summary points to the pull request comment; findings are only in the comment. Every change is a new Check Run with the same name, and GitHub shows the latest. To make merging wait for the review, add `pergent` as a required status check in the branch's protection rule or ruleset.

### Branch protection

- **Turn on "Dismiss stale pull request approvals when new commits are pushed".** pergent.sh dismisses its approval on every push, but this makes GitHub do it too, as a second layer for a webhook pergent.sh missed.
- **Make a human approval necessary.** If a branch needs one approval and the approver's approval counts, the bot alone unlocks the merge. Require two approvals, or a code owner review where the approver is not a code owner.
- If you use **"Restrict who can dismiss pull request reviews"**, allow the approve App (or bot user). Otherwise GitHub refuses its dismissals: pergent.sh logs the failure and tries again on every cleanup pass, and the stale approval stays until a dismissal works or GitHub dismisses it itself.
- **Check that the approval counts.** GitHub may not count a GitHub App's approving review toward a rule's required approvals; this release has not verified it. Open a test pull request, let pergent.sh approve it, and look at the merge box: if the App's approval is listed but the required approvals are still missing, switch to a bot user.

### Approving as a bot user

Use this instead of the approve App when your branch protection doesn't count the App's approval.

1. Create a GitHub user for the bot and give it **Write** access to the reviewed repositories (GitHub counts approvals only from people with write access).
2. As that user, create a **fine-grained personal access token**: resource owner the organization, repository access the reviewed repositories, repository permission **Pull requests: Read and write** (Metadata read-only is included). If the organization requires approval of fine-grained tokens, approve it.
3. Put it in the Secret (for example `GITHUB_APPROVE_TOKEN`) and use `approve_token_env` instead of `approve_app`:

   ```yaml
   github:
     - name: gh
       review_app:
         app_id: 123456
         private_key_env: GITHUB_REVIEW_APP_KEY
       approve_token_env: GITHUB_APPROVE_TOKEN
       webhook_secret_env: GITHUB_WEBHOOK_SECRET
   ```

At startup pergent.sh checks the token by asking whose it is (`GET /user`). A repository the token can't see (404) gets the "approve bot can't reach this repository" note; any other refusal is logged as an error by the hourly pass, which keeps its earlier answer.

The token is rotated by hand: create a new token, update the Secret, restart pergent.sh, then delete the old token. When GitHub reports (in the `GitHub-Authentication-Token-Expiration` header) that the token expires within 14 days, pergent.sh logs the warning "the GitHub approve token expires soon; rotate it", at most once an hour.

### When the approve App isn't installed on a repository

Reviews still run and are posted with their comment and Check Run. A clean review is not approved: the comment says `**Not approved:** the pergent approve App isn't installed on this repository.` and the Check Run title says why. This is not retried. pergent.sh notices an installation at startup and in the hourly pass; from then on the next push or ready-for-review is approved. There is no catch-up pass over pull requests that were already reviewed.

### Rotating an App's private key

A GitHub App can have several private keys at once:

1. In the App's settings, generate a new private key.
2. Update the Secret with the new `.pem` file.
3. Restart pergent.sh (`kubectl -n pergent-sh rollout restart deployment/pergent-sh`) and check that it started.
4. Delete the old key in the App's settings.

Installation tokens minted with the old key expire within an hour.

## Config reference

The config file is YAML (`pergent-sh serve --config <path>`, default `/etc/pergent-sh/config.yaml`). Unknown keys are an error. It holds no secrets: tokens and keys are named by environment variable. Every problem found is reported at startup, and pergent.sh does not start with an invalid config, a missing skill, a missing secret, a database it cannot reach, a GitLab token that does not work, or a GitHub App key or approve token that does not work. The error names the instance and what failed, never the key or token.

| Field | Default | Description |
| --- | --- | --- |
| `listen` | `:8080` | Address of the HTTP server. |
| `public_url` | required | HTTPS base URL where GitLab and GitHub reach pergent.sh, without a trailing slash. The GitLab webhook is created as `<public_url>/webhooks/gitlab/<instance name>`; the GitHub review App's webhook URL is `<public_url>/webhooks/github/<instance name>`. |
| `callback_url` | `public_url` | Base URL where Jobs post their result (`<callback_url>/runs/<id>/result`). `http` or `https`, no trailing slash. Set it to the in-cluster Service to keep that traffic inside the cluster. |
| `skills_dir` | required | Directory with the skill library: top-level `*.md` files and `<name>/SKILL.md` folders. The chart sets it to `/etc/pergent-sh/skills`. |
| `gitlab[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. |
| `gitlab[].url` | required | Base URL of the GitLab instance. It must be `https` (the bot tokens travel in a header), without a trailing slash. A redirect to another host is never followed. |
| `gitlab[].review_token_env` | required | Name of the env var with the review bot token. |
| `gitlab[].approve_token_env` | required | Name of the env var with the approve bot token. |
| `gitlab[].webhook_secret_env` | required | Name of the env var with the webhook secret. GitLab sends it as `X-Gitlab-Token`. |
| `github[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. One instance is one pair of Apps and covers every organization they are installed on. |
| `github[].api_url` | `https://api.github.com` | GitHub's REST API. Only GitHub.com is supported: another `https` URL (GitHub Enterprise Server) is accepted but untested. |
| `github[].review_app.app_id` | required | The review App's numeric App ID. |
| `github[].review_app.private_key_env` | required | Name of the env var with the review App's private key. |
| `github[].approve_app.app_id`, `.private_key_env` | one of `approve_app` and `approve_token_env` | The approve App: its App ID and the env var with its private key. The default approver. |
| `github[].approve_token_env` | one of `approve_app` and `approve_token_env` | Name of the env var with the approve bot user's fine-grained personal access token, instead of an approve App. See [Approving as a bot user](#approving-as-a-bot-user). |
| `github[].webhook_secret_env` | required | Name of the env var with the review App's webhook secret. GitHub signs each delivery with it (`X-Hub-Signature-256`). |
| `repositories[].gitlab` | one of `gitlab` and `github` | Name of one of the `gitlab` instances. |
| `repositories[].github` | one of `gitlab` and `github` | Name of one of the `github` instances. |
| `repositories[].project` | required | Project path, such as `acme/widgets`; on GitHub exactly `owner/name`, matched case-insensitively. Each project appears once per instance. |
| `repositories[].skills` | all library skills | Names of library skills to run. Empty or omitted means all of them. A name that is not in the library stops pergent.sh at startup. |
| `repositories[].failure_status` | `failed` | Status used when a review could not be done: `failed` or `neutral`. GitLab has no neutral state, so `neutral` is set as `skipped`; a project can count skipped pipelines as successful through a GitLab setting. On GitHub it is the Check Run conclusion `failure` or `neutral`. `failed` is never counted as successful. Blocking findings are always `failed`. |
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

Repository skills and instructions are read from the target branch at the merge request's `diff_refs.start_sha` (on GitHub, the pull request's `base.sha`), not from the merge request's own branch, so a merge request cannot change the rules it is reviewed by. Together with the previous review they must fit in 900 KiB; the previous review is dropped first, and if the skills and instructions alone are too large the run fails with a comment and no Job starts.

## How approval works

pergent.sh decides from what `pergent agent` reports. The result is checked again before anything is published: it must parse and be for the same base and head commits as the run, or it is posted as a failure.

| What happened | Comment | Commit status | Approved |
| --- | --- | --- | --- |
| Any skill has blocking issues | The review | `failed` ("Blocking issues found") | No |
| Any skill failed, has an unknown verdict, or is "clean" yet lists files it did not review | The review | `failure_status` ("Review failed for N of M skills") | No |
| The Job died or sent nothing usable, the result is an error, has no skills, or has an unknown status | An error comment with the reason | `failure_status` | No |
| Every skill is clean, but no file was reviewed because every changed file was skipped | The review plus a note listing the skipped files and the rules that skipped them | `success` ("Nothing to review: every changed file is skipped") | **No**, a human needs to look |
| Every skill is clean and at least one file was reviewed, but some changed file was skipped as `deleted`, `binary`, `vendored`, `lockfile` or `generated` | The review plus a note listing those files and the rules that skipped them | `success` ("No blocking issues; not approved: some changed files weren't reviewed") | **No**, a human needs to look |
| Every skill is clean, at least one file was reviewed and no file was skipped under those rules, but the merge request is a draft | The review plus a note that it will be approved when it's marked ready | `success` ("No blocking issues; not approved: draft") | **No**, until it is marked ready |
| As above and not a draft, but on GitHub the approve App isn't installed on the repository (or the approve bot user can't reach it) | The review plus `**Not approved:** the pergent approve App isn't installed on this repository.` (or `... the pergent approve bot can't reach this repository.`) | `success` ("No blocking issues; not approved: ...") | **No**, see [GitHub](#github) |
| Every skill is clean, at least one file was reviewed, no file was skipped under those rules, and the merge request isn't a draft | The review | `success` ("No blocking issues") | Yes, see below |

Those five are pergent's default skip rules, and a merge request can trigger them on its own: for example, by putting code under a `vendor/` directory, it would keep that code from the reviewer. A file skipped only because it is outside a skill's `paths`, matches a skill's `exclude`, or has no content changes does not stop the approval. The note lists at most 20 paths.

Before approving, pergent.sh asks GitLab (or GitHub) for the merge request. It approves only when the merge request is still **open** and not a draft, its head is still the commit that was reviewed and its target branch is still the one it was reviewed against. If the head or target moved or the MR was closed, there is no approval; the review and its status still belong to the reviewed commit, and a newer push gets its own run. The approval is recorded before it is made, and a refusal when the bot already approves this head is treated as done. On GitHub the approval is a review with `event: APPROVE` on the reviewed commit; GitHub has no commit guard on it, so the head and target check right before is the guard.

When a merge request gets a new head or a new target branch after the approve bot approved it, the approve bot removes its approval (on GitHub it dismisses its approving reviews), whether or not the platform resets approvals itself. This happens as soon as the webhook arrives. A webhook can be lost (GitLab doesn't resend a failed delivery; pergent.sh was restarting, the hook was disabled), so pergent.sh also checks every approval it holds against the platform at startup and every 5 minutes: an approval of a commit that is no longer the head, or of a merge request that was retargeted or closed, is removed then; the approval of a merged one is forgotten without writing anything. An approval can therefore outlive a missed webhook by up to 5 minutes. For an open merge request, the current head also gets a review if the repository is still configured.

A failed, partial or missing review is never `success` and is never approved.

### Pushes, edits and failures

- Only pushes, opening or reopening, and changing the target branch start a run. Editing the title, labels, description or assignees does not. Closing or merging supersedes the MR's runs. On GitHub these are the `pull_request` actions `opened`, `reopened`, `synchronize`, `edited` (only when `changes.base` is present: a retarget) and `closed`; every other action and event (including `ping`) is answered 200 and ignored.
- Marking a draft ready starts no run (GitLab: an `update` whose `changes.draft` goes to false; GitHub: `ready_for_review`). When the head's review is already posted, or is being posted, pergent.sh decides it again: the comment loses its draft note, and a clean review is approved. When the review is still running, it is published as for any other merge request that isn't a draft. GitLab versions that report `work_in_progress` instead of `draft` are handled the same way. Marking a merge request as a draft again doesn't withdraw an approval it already has.
- A run is one head reviewed against one target branch. Changing the target branch reviews the same head again against the new branch, supersedes the review against the old one and withdraws the bot's approval.
- A head and target branch that come back (retargeted main → develop → main, or pushed A → B → A) are reviewed again, even when they were reviewed before: since then the comment, the commit status (which GitLab keeps per commit, not per target branch) and the approval show the other review. A repeated webhook for the newest run's head and target starts nothing.
- Two pushes in quick succession: only the newest head is reviewed. The older Job is deleted, and a late result for it is accepted and discarded. Out-of-order push webhooks are handled: the dispatcher reviews the MR's current head.
- If GitLab has not computed the merge request's diff after 10 minutes, the run fails with a comment. On GitHub the merge base comes from comparing the pull request's `base.sha` with its head; if GitHub can't answer that within 10 minutes, the run fails with "GitHub hasn't computed this pull request's merge base yet".
- A Job that dies without a result (out of memory, a failed checkout, the deadline, a failed delivery, a deleted Job) produces an error comment and a non-success status within about 2 minutes.
- A Job whose pod can't start because its image can't be pulled (or its container config is invalid) is deleted and reported after about 5 minutes.
- A Job whose pod the cluster refuses to create (for example, one rejected by an admission policy) is only reported when the Job reaches its deadline, as `DeadlineExceeded`: `jobs.timeout` plus at least 4 minutes, and about a minute more for the report.
- Runs that were claimed but not launched (for example because pergent.sh crashed) are requeued at startup and after 5 minutes.
- Publishing is retried with backoff. After 10 failed attempts the run becomes `publish_failed` and nothing more is posted to the merge request; the failure is only in the logs.

## What a Job can and cannot see

A Job has two containers, started in order. Both run the pinned pergent image and have `allowPrivilegeEscalation: false` and all capabilities dropped. The pod never mounts a Kubernetes service account token.

**The checkout container** gets only a read-only clone token, through git's `http.extraHeader` setting passed in environment variables. On GitLab it is the project's deploy token (`read_repository`). On GitHub it is an installation token of the review App minted for this one Job, scoped to the one repository with `contents: read`, used as `x-access-token`; GitHub expires it after an hour and pergent.sh never stores it. The token is never written to disk, never in a URL and never in `.git/config`. It fetches only the base and head commits (not the whole repository history), disables hooks and gives the remote an invalid push URL.

**The pergent container** runs `pergent agent` and gets:

- the checkout, which is untrusted code under review, read-only to the agent;
- the skills, the instructions and the previous review, mounted read-only at `/in`;
- `OPENCODE_API_KEY` (the LLM key) and a one-time result token. The result token is random, stored only as a hash, valid for one result and expires shortly after the Job's deadline;
- the provider, model and, if set, base URL.

The agent in the pergent container can only read files, grep and list. It cannot edit files or run commands, and `*.env`, `*.pem`, `*.key` and `*.tfvars` files are denied. pergent deletes instruction files found in the checkout, so the reviewed repository cannot change the instructions.

A Job **cannot** see: the review bot token, the approve bot token, the GitHub App private keys and their installation tokens (other than its own clone token), the webhook secret, the database URL or the secret key, other Jobs' Secrets, the Kubernetes API (no service account token, and the Role is for pergent.sh's account only), or anything but DNS, the GitLab, GitHub and LLM addresses in `jobEgress.cidrs` and pergent.sh's own port 8080, when your CNI enforces the NetworkPolicy. Be aware that the LLM key is in the pergent container's environment, so use a key meant for this purpose, with a spending limit.

Each Job's Secret and inputs ConfigMap are owned by the Job, so deleting the Job removes them. Finished Jobs are removed after an hour.

## Fork merge requests

The checkout first tries to fetch the base and head commits by SHA. When the server refuses that (this is common for merge requests from forks), it fetches `refs/merge-requests/<iid>/head` (GitHub: `refs/pull/<number>/head`) instead, then deepens the target branch (starting at 50 commits and doubling, up to 3200) until the merge base is there. If the merge base is still missing at that depth, the checkout fails and the merge request gets an error comment.

## Switching a repository off

A repository is on when it is listed in the config. To switch it off, remove it from `repositories` and redeploy. On the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token. Runs still queued for it are dropped when they come up for dispatch.

A GitHub repository has nothing to delete: removing it from the config deletes only pergent.sh's own record of it. Installing an App on a repository never switches it on; only the config list does.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days.

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.

## HTTP routes

| Route | Used by |
| --- | --- |
| `POST /webhooks/gitlab/{instance}` | GitLab merge request webhooks. The `X-Gitlab-Token` header is compared in constant time with the instance's secret; a mismatch gets 401 and is otherwise ignored. An authenticated webhook whose payload can't be used (not JSON, no project id or merge request iid, a malformed head SHA) gets 200 and a warning in the log, because GitLab counts failed deliveries toward disabling the hook. |
| `POST /webhooks/github/{instance}` | The GitHub review App's `pull_request` webhook. `X-Hub-Signature-256` (HMAC-SHA256 of the body with the instance's webhook secret) is compared in constant time; a missing or wrong signature gets 401 and is otherwise ignored. A repository that isn't in the config gets 200 and is ignored. |
| `POST /runs/{id}/result` | A review Job delivering its result, with its one-time bearer token. |
| `GET /healthz` | Health check. It answers as soon as the server starts: pergent.sh serves right away and runs the startup reconcile in the background. |

Logs are JSON on stdout. Secrets are never logged.

## Limits

This release is deliberately small:

- No web UI or dashboard.
- No merge request commands (such as asking for a re-review in a comment).
- No advice on whether a human approval is still needed.
- No repository cache: every checkout is a fresh `git init` plus a shallow fetch.
- One replica, and PostgreSQL is external.
- A repository is switched off only by removing it from the config.
- GitHub.com only. `github[].api_url` accepts another URL, but GitHub Enterprise Server is untested.
- On GitHub, approving after the approve App is installed starts with the next push or ready-for-review; there is no catch-up pass over open pull requests.

## Publishing the chart (at release time)

The chart is not published yet. To publish a version, bump `version` (and `appVersion`) in `charts/pergent-sh/Chart.yaml`, then run:

```bash
make helm.create.releases   # packages the chart into charts/releases and refreshes its index.yaml
```

and commit `charts/releases/`. The directory is served by GitHub Pages from this repository (now public); a repository admin has to enable Pages first. `make readme` regenerates the chart reference (`charts/pergent-sh/README.md`) from the comments in `charts/pergent-sh/values.yaml` with [helm-docs](https://github.com/norwoodj/helm-docs); run it after changing a value.

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

All other tests need no external service. Release images are built by the `release` workflow when a `v*.*.*` tag is pushed and published as `ghcr.io/gopaytech/pergent.sh:<tag>` (linux/amd64). The `ci` workflow also publishes `ghcr.io/gopaytech/pergent.sh:main` and `:sha-<short sha>` on every push to `main`, after the test and chart jobs pass; `latest` is only set by releases.
