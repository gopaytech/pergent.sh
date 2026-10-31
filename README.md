# pergent.sh

pergent.sh is a service that runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests. It runs inside your Kubernetes cluster and starts one Kubernetes Job per review.

When someone opens or pushes to a merge request (or pull request) in a repository you listed in the config:

1. GitLab sends a merge request webhook, or GitHub a `pull_request` webhook, to pergent.sh.
2. pergent.sh starts a Kubernetes Job. The Job checks out the change, runs `pergent agent` over it and posts the result back.
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub.
4. If the review found no blocking issue, actually reviewed something and skipped no changed file except under the rules that leave it reviewed (outside a skill's scope, excluded by a skill, no content changes), a separate approver approves it: a second GitLab user on GitLab, a second GitHub App (or a bot user) on GitHub. A draft is reviewed the same way but is approved only once it is marked ready.

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the LLM key and a one-time token to hand in its result.

This release covers GitLab (gitlab.com or any self-managed instance) and GitHub.com; see [GitHub](#github) for the GitHub setup. An optional web [dashboard](#dashboard) shows every review, lets admins manage teams, switch repositories on and change their settings, and lets the operator add GitLab and GitHub instances, their secrets and the LLM without editing the config file. An instance added in the dashboard never blocks startup: when pergent.sh can't reach it, it starts anyway and the dashboard shows what to fix. There are no merge request commands; see [Limits](#limits).

In this README a "merge request" is also a GitHub pull request, and the "review bot" and "approve bot" are the GitHub review App and approver, unless a section says otherwise.

## Requirements

- Kubernetes, and permission to create a Role in the namespace where the Jobs run.
- PostgreSQL 13 or newer, reachable from the pergent.sh pod. pergent.sh creates and migrates its own tables at startup.
- Nodes that can pull `ghcr.io/gopaytech/pergent.sh` (the service) and `ghcr.io/gopaytech/pergent` (the Jobs).
- On each GitLab instance you use, two users:
  - A **review bot** with the **Maintainer** role on every reviewed project, and an access token with the `api` scope. It creates the project webhook and the read-only deploy token, posts the comment and sets the commit status.
  - An **approve bot** that is an eligible approver of every reviewed project, with an access token with the `api` scope. It must be a different user from the review bot: pergent.sh doesn't start when both tokens belong to the same user. It only approves, and removes its approval when the merge request gets a new commit or a new target branch. If the project requires specific approvers or forbids approval by certain users, make sure this bot qualifies.

    > **Warning:** if a project needs only one approval and the approve bot is an eligible approver, the bot's approval alone unlocks the merge: a merge request can then be merged on a model's review without any human approving it. Give every reviewed project an approval rule that requires a human approval (for example, one approval from a group the approve bot is not in), so the bot's approval is never enough on its own.

    pergent.sh checks this at startup and every hour: for each reviewed project it reads the approval rules that apply to the default branch (`GET /projects/:id/approval_rules`; with none, the project's `approvals_before_merge`) and, when the most any of them requires is one or none, logs a warning naming the project: "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval". It doesn't know whether the approve bot is in a rule's approver list, so it warns whenever the count allows it. If the rules can't be read, that is logged once at info level; the check never stops a review.
- For GitHub, two GitHub Apps (or one App and a bot user); see [GitHub](#github).
- An API key for an LLM provider that opencode supports. It can be entered in the [dashboard](#dashboard) or kept in the Secret.
- A way for GitLab or GitHub to reach pergent.sh over HTTPS (an Ingress, for example).
- For the optional [dashboard](#dashboard): an HTTPS host for it. To let people sign in with GitLab or GitHub, also a GitLab OAuth application per GitLab instance or user authorization on the GitHub review App. The operator signs in with a password and needs neither.

## Install with Helm

The chart is in `charts/pergent-sh`. Postgres is not part of it. Every value is listed in the [chart reference](charts/pergent-sh/README.md).

1. Create the namespace the Jobs run in and the one pergent.sh runs in:

   ```bash
   kubectl create namespace pergent
   kubectl create namespace pergent-sh
   ```

2. Create a Secret in the namespace where pergent.sh will run. Its keys become environment variables. It needs only two:

   ```bash
   kubectl -n pergent-sh create secret generic pergent-sh-env \
     --from-literal=DATABASE_URL='postgres://pergent:CHANGE_ME@postgres.example.com:5432/pergent?sslmode=require' \
     --from-literal=PERGENT_SH_SECRET_KEY="$(openssl rand -base64 32)"
   ```

   Keep `PERGENT_SH_SECRET_KEY`: it encrypts every secret you enter in the dashboard and the stored deploy tokens. Changing it isn't supported; see [Environment variables](#environment-variables).

   Instances, their tokens and keys, and the LLM key are entered in the dashboard (step 5), so they need no keys in the Secret. If you keep them in the config file instead, put their keys in this Secret too; see [Instances and the LLM in the config file](#instances-and-the-llm-in-the-config-file).

3. Write your values:

   ```yaml
   # values.yaml
   existingSecret: pergent-sh-env
   jobsNamespace: pergent

   # Where Jobs may connect on TCP 443 and 80: GitLab or GitHub, and the LLM provider.
   jobEgress:
     cidrs:
       - 192.0.2.0/24

   # GitLab and GitHub send webhooks here; Jobs post their result here.
   ingress:
     enabled: true
     className: nginx
     host: pergent.example.com
     tls:
       - secretName: pergent-example-com-tls
         hosts: [pergent.example.com]

   # The dashboard, on its own host; see "Dashboard".
   dashboardIngress:
     enabled: true
     className: nginx
     hosts: [pergent-dashboard.example.com]
     tls:
       - secretName: pergent-dashboard-example-com-tls
         hosts: [pergent-dashboard.example.com]

   config:
     listen: ":8080"
     skills_dir: /etc/pergent-sh/skills
     public_url: https://pergent.example.com
     dashboard:
       url: https://pergent-dashboard.example.com
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

5. Set up in the browser. With `dashboard.url` set and no instance in the config, pergent.sh starts and waits for one. Take the generated admin password from the log ([First sign-in](#first-sign-in)), sign in as `admin` and change it, then add a GitLab or GitHub instance under **Integrations** ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)), set the LLM under **Settings**, and switch repositories on. People who sign in with GitLab or GitHub wait until an admin puts them on a [team](#teams). To set the first password from a Secret instead of the log, see [First sign-in](#first-sign-in).

On startup pergent.sh creates the merge request webhook and a read-only deploy token on every GitLab project that is switched on. You do not create either by hand. On GitHub there is nothing per repository to create: the review App's webhook covers every repository it is installed on.

What the chart creates:

- A Deployment with **one replica** and the `Recreate` strategy. Do not scale it up: at startup pergent.sh requeues runs it had claimed but not launched, which is only safe when no other pod is dispatching. The pod runs as non-root with a read-only root filesystem, drops all capabilities, and has startup, readiness and liveness probes on `/healthz` (the startup probe allows 150 s, as pergent.sh starts listening only after the database and migrations and, for each instance in the config file, its GitLab tokens or GitHub Apps check out; instances added in the dashboard never delay it). The pod restarts on a config or skills change. When `adminPassword.existingSecret` is set, the pod also gets that Secret's `adminPassword.key` as the environment variable named by `config.dashboard.admin_password_env` (see [First sign-in](#first-sign-in)).
- A Service on port 8080 and, when `ingress.enabled`, an Ingress that by default exposes only `/webhooks/` and `/runs/` (`ingress.paths`). The chart refuses to render it with `/`, `/api/` or `/auth/` in `ingress.paths`: those are the dashboard's.
- When `dashboardIngress.enabled`, a second Ingress for the [dashboard](#dashboard) on its own host(s), exposing `/`, `/api/` and `/auth/` of the same Service.
- A ServiceAccount, and in `jobsNamespace` a Role and RoleBinding for it: `batch/jobs` (create, get, list, watch, patch, delete), `secrets` and `configmaps` (create, get, delete), `pods` (get, list) and `pods/log` (get, to keep each review's log). There are no wildcard rules and nothing outside `jobsNamespace`.
- A NetworkPolicy in `jobsNamespace` for the Job pods (those labelled `app.kubernetes.io/managed-by: pergent-sh`): no ingress; egress to DNS (UDP and TCP 53), to TCP 443 and 80 in `jobEgress.cidrs`, and to TCP 8080 of the pergent.sh pods for the result callback. A NetworkPolicy only takes effect when your CNI enforces them.
- ConfigMaps for `config.yaml` and the `skills`.

**Narrow `jobEgress.cidrs`.** The default `0.0.0.0/0` lets a Job reach any address on ports 80 and 443, including in-cluster services and the cloud metadata endpoint. Set it to the addresses of GitLab (or GitHub) and the LLM provider. An empty list (`cidrs: []`) allows no external egress at all: Jobs then reach only DNS and pergent.sh's port 8080. The optional `jobEgress.except` list is added to every `ipBlock` as NetworkPolicy `except` (for example `169.254.169.254/32`); it applies to every cidr, and Kubernetes rejects an entry outside its cidr, so use it with a broad cidr such as the default `0.0.0.0/0`.

`config.jobs.namespace` must equal `jobsNamespace`, `config.skills_dir` must be `/etc/pergent-sh/skills` and `config.listen` must end in `:8080`; the chart refuses to render otherwise.

If `public_url` points at a hostname that the Job pods reach through the Ingress, they need egress to it on 443 (covered by the default `jobEgress.cidrs`). To keep Jobs off the internet path, set `callback_url` to the in-cluster Service, for example `http://pergent-sh.pergent-sh.svc:8080`.

### Instances and the LLM in the config file

An install managed from Git can keep its instances, the LLM and its repositories in the config file, with their secrets in the Secret. The config file wins over the dashboard: its instances, its `llm:` block and its `repositories` are read-only in the dashboard, and an instance added in the dashboard with the same name as one in the config stops pergent.sh at startup (the error names it: `instance "main" is in both the config file and the dashboard`). Add the keys to the Secret:

```bash
kubectl -n pergent-sh create secret generic pergent-sh-env \
  --from-literal=DATABASE_URL='postgres://pergent:CHANGE_ME@postgres.example.com:5432/pergent?sslmode=require' \
  --from-literal=PERGENT_SH_SECRET_KEY="$(openssl rand -base64 32)" \
  --from-literal=OPENCODE_API_KEY='CHANGE_ME' \
  --from-literal=GITLAB_REVIEW_TOKEN='CHANGE_ME' \
  --from-literal=GITLAB_APPROVE_TOKEN='CHANGE_ME' \
  --from-literal=GITLAB_WEBHOOK_SECRET="$(openssl rand -hex 32)"
```

and the instance, its repositories and the LLM to the values:

```yaml
config:
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
```

For GitHub, put the Apps' private keys (the whole `.pem` file of each) and the review App's webhook secret in the Secret and use `github:` instead of `gitlab:`; see [GitHub](#github). Without `dashboard.url`, pergent.sh needs at least one instance and the `llm:` block in the config file, and instances can only come from there.

### Environment variables

| Variable | Used for |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL. Required. |
| `PERGENT_SH_SECRET_KEY` | Encrypts the secrets you enter in the dashboard, the stored deploy tokens and the sign-in tokens (AES-256-GCM). 32 random bytes, standard base64. Required. **Changing it isn't supported** and there is no rotation: a stored secret that can't be opened with the new key shows as "not set" in the dashboard and must be entered again (the startup reconcile replaces the deploy tokens it can't open with new ones and revokes the old). |
| the variable named by `dashboard.admin_password_env` | The password of the first password admin, `admin`, at least 12 characters. Optional: without it pergent.sh generates one and logs it once. Read only on the start that creates the account. See [First sign-in](#first-sign-in). |
| the variable named by `llm.api_key_env` (default `OPENCODE_API_KEY`) | When the LLM is in the config file: the LLM key handed to each Job. Required then, and checked at startup. With the LLM set in the dashboard, the key is entered there. |
| each `gitlab[].review_token_env`, `approve_token_env`, `webhook_secret_env` | When the instance is in the config file: the review bot token, the approve bot token and the webhook secret of that instance. Required, and checked at startup. |
| each `github[].review_app.private_key_env` | When the instance is in the config file: the review App's private key, the whole PEM file GitHub gave you (PKCS#1 `RSA PRIVATE KEY` or PKCS#8 `PRIVATE KEY`). Required, and checked at startup. Never logged. |
| each `github[].approve_app.private_key_env` or `github[].approve_token_env` | When the instance is in the config file: the approve App's private key, or the approve bot user's fine-grained personal access token. Required, and checked at startup. |
| each `github[].webhook_secret_env` | When the instance is in the config file: the review App's webhook secret. Required. |
| each `gitlab[].oauth_client_id_env`, `oauth_client_secret_env` | When the instance is in the config file: its OAuth application, for signing in to the [dashboard](#dashboard). Both or neither; checked at startup when `dashboard.url` is set. |
| each `github[].oauth_client_id_env`, `oauth_client_secret_env` | When the instance is in the config file: the review App's client ID and a client secret, for signing in to the [dashboard](#dashboard). Both or neither; checked at startup when `dashboard.url` is set. |

pergent.sh also reads `KUBECONFIG` when it runs outside a cluster, which is handy for local development.

## GitLab project settings

- **Don't treat the `pergent` commit status as a gate.** GitLab lets anyone with the Developer role set a commit status of any name on a commit of the project, including the head of their own merge request, so a `pergent` status of `success` proves nothing about who set it. Don't rely on "Pipelines must succeed" (or any other check of commit statuses) to enforce the review. The approval is the gate: it comes from the approve bot, a separate user whose token only pergent.sh holds.
- **Require approval through approval rules.** Give the default branch an approval rule that needs a human approval as well as the bot's (for example two approvals, or one from a group the approve bot is not in); see the warning under [Requirements](#requirements). pergent.sh logs a warning every hour for a project one approval can merge.
- **Turn on "Remove all approvals when commits are added to the source branch"** (Settings > Merge requests > Approval settings). It is GitLab's counterpart of GitHub's "Dismiss stale pull request approvals": pergent.sh removes its approval on every push, but this makes GitLab do it too, as a second layer for a webhook pergent.sh missed.
- **Turn on "Prevent editing approval rules in merge requests"** (same place). Otherwise the author of a merge request can lower its required approvals to one, and the approve bot's approval alone unlocks the merge.

Approval rules and approval settings are GitLab Premium and Ultimate features. On GitLab Free, approvals are optional and don't block a merge, so pergent.sh's approval is advice only.

## GitHub

pergent.sh reaches GitHub.com through two GitHub Apps that you create (pergent.sh is self-hosted, so there is no shared App):

- the **review App** receives the `pull_request` webhook, reads the pull request and the repository, posts the comment, sets the `pergent` Check Run and mints each Job's clone token;
- the **approve App** only approves, and dismisses its own approvals when they go stale.

They are two Apps so that the identity that writes the review is never the one that approves it. Where GitHub doesn't count an App's approval, the approve App is replaced by a bot user; see [Approving as a bot user](#approving-as-a-bot-user).

### 1. Create the review App

In the organization's settings, **Developer settings → GitHub Apps → New GitHub App**:

- **Webhook:** active. URL `<public_url>/webhooks/github/<instance name>`, for example `https://pergent.example.com/webhooks/github/gh`. Secret: a random string (`openssl rand -hex 32`); you enter it as the instance's webhook secret in the dashboard, or, for an instance in the config file, put it in the Secret as the instance's `webhook_secret_env`.
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

Install both Apps on the organization, on all repositories or on the ones you will review. Installing an App switches nothing on: only the repositories listed in the config or switched on in the [dashboard](#dashboard) are reviewed. One `github:` instance covers every organization the two Apps are installed on; pergent.sh looks up the installation per repository.

### 4. Add the instance

In the dashboard, a password admin adds the instance under **Integrations → Add GitHub instance**: the API URL, both App IDs, both private keys (pasted whole), the webhook secret, and the review App's client ID and client secret if people sign in with it ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)).

To keep the instance in the config file instead, put the keys in the Secret and name them in the config:

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

At startup pergent.sh reads each App's slug (`GET /app`) and its bot user (`GET /users/<slug>[bot]`), and, for an instance in the config file, stops with an error naming the instance if a key doesn't work (an instance added in the dashboard is logged and recorded as "Needs attention" instead). The bot user lookup is unauthenticated (GitHub allows 60 an hour per address), which is plenty unless the pod restarts in a loop. Then, at startup and every hour, it looks each listed repository up: the review App's installation, the repository's ID and clone URL, and whether the approve side can reach it. A listed repository the review App isn't installed on is logged as an error on every pass and can't be reviewed.

### The `pergent` Check Run

The status of a review is a Check Run named `pergent` on the reviewed head commit, created by the review App:

- `in_progress` ("pergent review running") when the Job starts;
- then `completed` with the conclusion `success`, `failure`, `neutral` (a failed review with `failure_status: neutral`) or `cancelled` (the run was superseded: a newer push or a new target branch replaced it, or the pull request was closed). A review that couldn't be posted after 10 attempts ends as `failure`, titled "pergent couldn't post its review".

Its title is the status description from [How approval works](#how-approval-works) and its summary points to the pull request comment; findings are only in the comment. Every change is a new Check Run with the same name, and GitHub shows the latest. To make merging wait for the review, add `pergent` as a required status check in the branch's protection rule or ruleset, and **pin it to the review App**: otherwise GitHub accepts a check named `pergent` from any source, and a workflow or another App could satisfy it. In a branch protection rule, set the check's source to the review App instead of "Any source"; in a ruleset, choose the review App as the check's integration. See [Branch protection](#branch-protection).

### Branch protection

- **Turn on "Dismiss stale pull request approvals when new commits are pushed".** pergent.sh dismisses its approval on every push, but this makes GitHub do it too, as a second layer for a webhook pergent.sh missed.
- **Make a human approval necessary.** If a branch needs one approval and the approver's approval counts, the bot alone unlocks the merge. Require two approvals, or a code owner review where the approver is not a code owner. At startup and every hour, for each repository the approve side can reach, pergent.sh reads the required approving review count of the default branch from its rulesets (`GET /repos/{owner}/{repo}/rules/branches/{branch}`, which the review App's metadata permission allows) and from its classic branch protection (`.../branches/{branch}/protection/required_pull_request_reviews`, which needs the Administration read permission the review App doesn't ask for), takes the higher, and logs a warning naming the repository when it is one or none: "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval". When no ruleset requires reviews and the classic rule can't be read (a 403 or 404 without the Administration permission, or any other error), it logs once at info level that it couldn't check; the check never stops a review. It doesn't check code owner reviews.
- **Require the `pergent` check from the review App only.** Add `pergent` as a required status check and set its source to the review App: in a branch protection rule, pick the review App as the check's expected source instead of "Any source"; in a ruleset, choose the review App as the integration of the `pergent` check. With any source allowed, any Actions job or App that posts a check named `pergent` can satisfy the requirement.
- If you use **"Restrict who can dismiss pull request reviews"**, allow the approve App (or bot user). Otherwise GitHub refuses its dismissals. pergent.sh keeps the approval on record, logs the failure and tries again after a backoff: 30 seconds after the first failure, doubling after each one, at most 5 minutes. The 5-minute approval sweep tries it too. The stale approval stays until a dismissal works or GitHub dismisses it itself.
- **Check that the approval counts.** GitHub may not count a GitHub App's approving review toward a rule's required approvals; this release has not verified it. Use a test branch whose protection requires exactly **one** approval (with more, the result is ambiguous). Open a test pull request into it, let pergent.sh approve it, and look at the merge box: if the App's approval is listed but the required approvals are still missing, switch to a bot user.

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
2. For an instance added in the dashboard, paste the new `.pem` file into its private key field under **Integrations**: it is checked before it is saved and used at once, with no restart. For an instance in the config file, update the Secret with the new `.pem` file and restart pergent.sh (`kubectl -n pergent-sh rollout restart deployment/pergent-sh`), then check that it started.
3. Delete the old key in the App's settings.

Installation tokens minted with the old key expire within an hour.

## Dashboard

The dashboard is a web app built into pergent.sh. It is off until `dashboard.url` is set. It has two kinds of account:

- **Password accounts** are the operators. The first is `admin`. They see every repository, and alone manage password accounts, instances and their secrets, OAuth clients and the LLM.
- **Platform accounts** are made the first time someone signs in with their GitLab or GitHub account. They see no repository until an admin puts them on a [team](#teams), and then only the repositories that team covers and that they can see on that platform.

With the dashboard on, every review run, its findings, its files and its log are there, and admins switch repositories on and off and change their settings.

### Turning it on

1. Pick the dashboard's address, for example `https://pergent-dashboard.example.com`. It must be `https` and a bare host, without a path: the dashboard, its API (`/api/`) and its sign-in routes (`/auth/`) are served at `/` of that host, and its cookies only work over https. It can be an internal-only host. Webhooks and Job results stay on `public_url`, which GitHub.com must still reach from the internet.
2. Set it in the config and turn on the chart's dashboard Ingress, which is separate from the webhook Ingress so the two can be on different hosts:

   ```yaml
   config:
     dashboard:
       url: https://pergent-dashboard.example.com

   dashboardIngress:
     enabled: true
     className: nginx
     hosts: [pergent-dashboard.example.com]
     tls:
       - secretName: pergent-dashboard-example-com-tls
         hosts: [pergent-dashboard.example.com]
   ```

   It exposes `/`, `/api/` and `/auth/` of the pergent.sh Service; the webhook Ingress never does. Its `/` is a prefix, so the dashboard host also routes `/webhooks/`, `/runs/` and `/healthz` to pergent.sh; webhooks and results there still need a valid signature or result token, and `/healthz` only answers `ok`. The chart refuses to render it without `config.dashboard.url`, or when none of `hosts` is that URL's host. Without `dashboard.url` pergent.sh doesn't serve those routes at all.
3. [Sign in](#first-sign-in) as `admin`, add your instances under **Integrations** ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)), and set up sign-in with GitLab or GitHub on each instance people should use: [a GitLab OAuth application](#sign-in-with-gitlab) or [user authorization on the GitHub review App](#sign-in-with-github).

With `dashboard.url` set, pergent.sh starts with no instance at all and waits for one to be added in the dashboard. For an instance in the config file, `oauth_client_id_env` and `oauth_client_secret_env` (see [Config reference](#config-reference)) turn on its sign-in; pergent.sh doesn't start when an instance names one of them and that variable is unset or empty. The error names the instance and the variable.

### First sign-in

The first start with the dashboard on creates the password account `admin`. Its password is one of:

- **From a Secret.** Set `config.dashboard.admin_password_env` to an environment variable name and give the chart the Secret that holds the password:

  ```bash
  kubectl -n pergent-sh create secret generic pergent-sh-admin \
    --from-literal=password="$(openssl rand -base64 24)"
  ```

  ```yaml
  config:
    dashboard:
      admin_password_env: PERGENT_SH_ADMIN_PASSWORD
  adminPassword:
    existingSecret: pergent-sh-admin
    key: password        # the default
  ```

  The chart passes that key to the pod as `PERGENT_SH_ADMIN_PASSWORD`, and refuses to render `adminPassword.existingSecret` without `config.dashboard.admin_password_env`. The password must have at least 12 characters; with fewer, pergent.sh doesn't start and the error names the variable (`dashboard.admin_password_env: the password in PERGENT_SH_ADMIN_PASSWORD must have at least 12 characters`).
- **Generated.** When `admin_password_env` is unset, or the variable is unset or empty, pergent.sh generates a random password and logs it once, at warning level, on the start that creates the account: look for `"msg":"initial admin password"` in the log (`kubectl -n pergent-sh logs deploy/pergent-sh`). It is the only secret pergent.sh ever logs.

Either way `admin` must change the password at its first sign-in: until then the only page is the change form, and the API answers everything else with `change your password first`. A password has at least 12 characters and must differ from the old one; a change ends the account's other sessions. The variable is read only on the start that creates `admin`; changing it later does nothing, use the reset below.

More password admins are added by a password admin under **Users → Add password admin**: pergent.sh makes a random password, shows it once, and the new admin must change it at its first sign-in. The last password account can't be removed (`the last password account can't be removed`).

### Lost password

A password admin who lost the password asks pergent.sh for a new one from inside the pod:

```bash
kubectl -n pergent-sh exec deploy/pergent-sh -- /pergent-sh admin reset-password admin
```

(`deploy/pergent-sh` is the Deployment of a release called `pergent-sh`, as in [Install with Helm](#install-with-helm). The chart names the Deployment `<release>-pergent-sh`, or just `<release>` when the release name contains `pergent-sh`; `kubectl -n pergent-sh get deploy` shows it.) The command needs only `DATABASE_URL`, which the pod's environment has. It prints `new password for admin: ...` on stdout (never in the JSON log), clears the account's lock and ends its sessions, and the account must change that password at its next sign-in. The reset is recorded in the audit log with the actor `cli`.

### Password sign-in limits

- A wrong username or password always gets the same answer, `wrong username or password` (401), after the same work, so a guess doesn't tell whether the username exists.
- Five wrong passwords in a row lock that username for 15 minutes. A wrong current password at a password change counts the same.
- Each client IP may make 20 sign-in or password-change attempts in 5 minutes; further ones get 429 (`too many sign-in attempts, try again later`). The client IP is the **last** `X-Forwarded-For` entry, and the connection's own address when the header is absent. The proxy or Ingress in front of pergent.sh **must set `X-Forwarded-For`** to the address it saw: when nothing does, a client sends its own header and can change address on every attempt, so the per-IP limit can be bypassed. The per-username lock holds either way. An L7 load balancer in front of the Ingress that doesn't forward the client address makes every client share one address, and so one limit of 20 attempts in 5 minutes.
- Anyone who knows a username can keep it locked, with 5 wrong guesses every 15 minutes. `admin` is the one everybody knows. Add a second password admin with an unguessable username and remove `admin`, or restrict `/auth/password` at the Ingress to the addresses your operators sign in from.
- Passwords are checked, argon2id, at most 4 at a time. A sign-in that gets no slot within 5 seconds is answered 503 with the same `too many sign-in attempts, try again later`.

### Sign in with GitLab

Create one OAuth application per GitLab instance: as an instance administrator under **Admin area → Applications**, or under a group's **Settings → Applications**.

- **Redirect URI:** `<dashboard.url>/auth/gitlab/<instance name>/callback`, for example `https://pergent-dashboard.example.com/auth/gitlab/main/callback`.
- **Confidential:** yes.
- **Scopes:** `read_user` and `read_api`.

Enter its Application ID and Secret in the instance's form under **Integrations** (**OAuth client ID** and **OAuth client secret**), or, for an instance in the config file, put them in the Secret (for example `GITLAB_OAUTH_CLIENT_ID` and `GITLAB_OAUTH_CLIENT_SECRET`) and name them in the instance's `oauth_client_id_env` and `oauth_client_secret_env`. An instance without them has no sign-in button.

### Sign in with GitHub

GitHub sign-in uses the review App's own user authorization, so there is no second App to create. In the review App's settings:

- Under **Identifying and authorizing users**, set the **Callback URL** to `<dashboard.url>/auth/github/<instance name>/callback`, for example `https://pergent-dashboard.example.com/auth/github/gh/callback`. Keep **Expire user authorization tokens** on (pergent.sh renews them) and leave **Request user authorization (OAuth) during installation** off.
- Generate a **client secret**, and note the **Client ID** on the same page.

Enter the Client ID and the client secret in the instance's form under **Integrations**, or, for an instance in the config file, put them in the Secret and name them in `oauth_client_id_env` and `oauth_client_secret_env`. A user signed in through the App only reaches repositories the App is installed on.

### Teams

Everything a platform account may see or do comes from the teams it is on, under **Admin → Teams**, and never beyond what its platform lets it see.

- **Waiting.** A platform account on no team sees only a page that says `You're signed in as <username> on <instance>. An admin needs to add you to a team.`, with a sign-out button; the API answers everything else with 403 `an admin needs to add you to a team`. Admins see how many accounts wait as a badge on **Users**, and waiting accounts are listed first. An admin adds them to a team from **Teams**; the account sees the change on its next request.
- **A team** has a name, one role, members, and the repositories it covers. Members are platform accounts, of any instance; password accounts are never members. A maintainer or viewer team covers chosen repositories that are switched on, or all repositories of an instance. An admin team has no repository list. A user on several teams has, on each repository, the highest role of the teams that cover it.
- **Roles.**

  | Role | Can |
  | --- | --- |
  | `viewer` | Read the covered repositories: reviews, runs, findings, files, logs and settings. |
  | `maintainer` | Also change the settings of the covered repositories (approve clean reviews, review drafts, failure status, skills, Job timeout and memory), and reset them to the defaults. Not switch repositories on or off, and not change teams. |
  | `admin` | Also switch repositories on and off (**Add repository**), change the defaults on **Settings**, manage **Users** and **Teams**, and read the **Audit log**. An admin team covers every repository switched on for the instance its members signed in with that they can see on their platform. |

  Only admins manage teams: creating them, their members, roles and repositories. Platform admins can add people to admin teams and remove them.
- **Platform admins handle their own instance.** A platform admin lists and removes the accounts of the instance it signed in with, and adds and removes team members of that instance only. Members of other instances stay on a team as they are, are never named to it, and are counted as `+N members from other instances`; adding one is refused with 409 `you can only change members of your own instance` and removing one with 403 `you can only remove accounts of your own instance`. Password admins do all of it across instances.
- **The platform is always the ceiling.** A platform account sees a repository only when a team covers it **and** the platform still lets it see it ("can see", below), whatever its teams say. That holds for an admin team too: its members are admins on the repositories their platform shows them, not on the rest. The check is repeated every 5 minutes. When checks have failed for 10 minutes, the account sees nothing and has no role, an admin team's members included, until one succeeds.
- **Team changes apply on the next request.** Only the platform check keeps its 5-minute interval; a repository a team has just come to cover is checked with the platform before the request is served.
- **Password admins** see every repository and have no platform to check against. Only they manage password accounts (add them, remove them), instances and their secrets, OAuth clients, and the LLM. A platform admin never touches these. Where an admin page names repositories, a platform admin sees only those it can see: **Teams**, the **Audit log** and "Needs attention" leave out the rest, and **Integrations** shows its own instance, read-only. Editing a team that covers repositories it can't see keeps them as they are and shows `+N repositories you can't see`. It can name repositories of its own instance only (`you can add repositories of your own instance only`), and can't change the role of a team that covers repositories it can't see, or delete it: both are answered 409 `this team covers repositories you can't see; a password admin can change its role or remove it`.
- **Users.** **Admin → Users** lists every account, its platform and instance, its last sign-in and its teams. Removing a platform account ends its sessions and takes it off its teams; it is made again, on no team, at its next sign-in.

### Who sees what

- **Signing in.** One account per sign-in: someone who uses GitLab and GitHub signs in with one of them at a time and is two accounts, on two instances. A new sign-in ends the session the browser had. The user's OAuth token is stored encrypted with `PERGENT_SH_SECRET_KEY` and never sent to the browser. Password accounts have no platform token.
- **Sessions** are a cookie (`HttpOnly`, `Secure`, `SameSite=Lax`) holding a random ID. A session ends after 8 hours without use, and 7 days after sign-in whether used or not. pergent.sh deletes ended sessions every hour. Every write needs the session's CSRF token (`X-CSRF-Token`); the password form has its own, from `GET /auth/csrf`.
- **"Can see"** means the user can read the repository's code and merge requests on the platform, because findings and logs contain code:
  - GitLab: the Reporter role or above, through the project, its group, or a group the project or its group is shared with, or a public project whose repository and merge requests are open to everyone. A share is checked with one more call to GitLab, made only when the project's own permissions show less than Reporter.
  - GitHub: read access to the repository, or a public repository.
- **Checked on the server.** At sign-in pergent.sh asks GitLab or GitHub, with the user's token, about every repository the user's teams cover. It asks again when the answer is older than 5 minutes and the user makes a request, so lost access stops showing within 5 minutes. If asking fails, the last answer is used for 5 more minutes, then the user sees nothing until it works again. Every API answer is filtered by that list, which holds each repository's path and its platform ID: a run is shown only when both match, so a project created at the path of a deleted, renamed or transferred one never shows the earlier project's reviews. A repository, review or run the user can't see is answered exactly like one that doesn't exist.

**Everyone with a team** sees, for the repositories they can see:

- **Overview:** the number of reviews, approvals, blocking reviews and failed reviews over the last 7, 30 or 90 days, the most recent reviews, and "Needs attention".
- **Reviews:** filtered by repository, result and time. Each merge request has a page with every run (commit, what started it, result, duration) and, for a run, the status set on the platform, whether it was approved and why not, the pergent version, model, tokens, each skill's verdict, findings, checks, files (reviewed, skipped with the rule, not reviewed) and the log.
- **Repositories:** each with its last 7 days and health; its reviews, its setup checks and its skills and instructions. Skills & instructions shows what a review of the default branch would get, read with the review bot or App and kept for 5 minutes.
- **Skills:** the skill library, read-only.

**Admins** also get:

- the on/off switch of each repository and **Add repository**, which lists the repositories that both the admin (GitLab: Reporter or above as a member, a shared group's member included; GitHub: read access) and the review bot (GitLab: Maintainer) or review App (GitHub: installed) can reach, at most 500, with a search. A password admin has no platform account, so it sees what the review bot or App can reach;
- each repository's **Settings**: approve clean reviews, the failure status, review drafts, the library skills to run, and the Job's timeout (60 to 7200 seconds) and memory limit (from 256Mi, and not below `jobs.resources.requests.memory`, to 32Gi). Each shows "overridden here" or "default", and "Reset to defaults" clears every override;
- **Users**, **Teams** and the **Audit log**;
- **Integrations** (their own instance: connection, approver, each repository's setup and whether the approver reaches it, an approve token about to expire), read-only for platform admins, and **Settings**: the defaults a repository switched on in the dashboard gets (approve clean reviews, review drafts, failure status, Job timeout and memory; the config's repositories keep the config's own), and the LLM, which only password admins change.

Every write is checked on the server: it needs a session, the CSRF token, a role that allows it and a repository the user can see. A viewer calling the API directly gets 403 and nothing changes.

### Instances and secrets in the dashboard

Password admins add GitLab and GitHub instances, with their secrets, under **Admin → Integrations**, and set the LLM under **Admin → Settings**. Nobody else can: a platform admin sees its own instance there, read-only.

- **Adding a GitLab instance:** a name (`[a-z0-9-]`, at most 63 characters, part of the webhook URL), the instance's `https` URL without a trailing slash, the review token and the approve token, and optionally an OAuth application ID and secret for [sign-in](#sign-in-with-gitlab) (both or neither). The webhook secret is generated by pergent.sh, since pergent.sh creates the hooks itself.
- **Adding a GitHub instance:** a name, the API URL, the review App's ID and private key, its webhook secret (the one you set in the App), the approve App's ID and private key or, with the approve App ID left as 0, the approve bot user's token, and optionally the review App's client ID and a client secret for [sign-in](#sign-in-with-github). A private key is pasted as it is in the `.pem` file; the browser's one-line field loses its line breaks, and the dashboard puts them back.
- **Secrets are write-only.** A value is sent once and never comes back: not in a page, an API answer, a log, an audit entry or the browser's storage. The page shows only "set" or "not set" and when the secret last changed. Replacing a secret doesn't need the old value.
- **Checked before saving.** A save runs the checks startup runs for an instance in the config (the two bot tokens belong to different users, the App key signs a request GitHub accepts, ...) and is refused with the reason when one fails; nothing is stored then. A saved instance is used at once, without a restart.
- **Address and App IDs can't change.** The URL, API URL and App IDs are fixed once the instance is added (`an instance's address and App IDs can't change: remove it and add it again`); secrets and the OAuth client ID can be replaced at any time.
- **Removing an instance** asks for confirmation and answers `removing`. Its repositories are switched off at once (queued runs are superseded), its users' sessions end and their accounts and team memberships are deleted. In the background pergent.sh then takes its webhooks and deploy tokens off the platform and deletes the instance and its secrets, within a minute or so; the page shows "removing" until then. Its runs, findings and logs stay in the database: adding an instance of the same name again attaches them to it, and password admins see them. When the platform keeps refusing that cleanup, a "Needs attention" item says so (`pergent.sh could not take N project(s) of the instance "name", which is being removed, off its platform; it tries again at the next reconcile`) and the removal waits for the next reconcile.
- **An instance that can't connect** (at startup, or right after it was saved) doesn't stop pergent.sh, so the dashboard where it is fixed stays up. pergent.sh logs the error, and an instance-wide "Needs attention" item (kind `instance_unreachable`) shows it to the operator and to that instance's admins; the item goes when the instance connects. Its webhooks are still accepted and the events queued, and reviews of its repositories wait queued (the log says `queued reviews wait: pergent.sh isn't connected to their instance; an operator fixes it on Admin → Integrations`) until it is saved again in **Integrations**. An instance in the config file that can't connect still stops startup.
- **The config file wins.** Instances in the config are listed as "config" and are read-only (409 `this instance is managed by the config file; change it there`), and so are the secrets they name by environment variable. An instance in the dashboard whose name is also in the config stops startup, naming both. Repositories in the config name instances in the config only; repositories of an instance added in the dashboard are switched on in the dashboard.
- **The LLM** (provider, model, base URL and API key) is set in the dashboard by password admins, unless the config has an `llm:` block, which wins and makes it read-only (409 `the LLM is set in the config file; change it there`). The key is needed only when none is stored. With no LLM set, reviews wait queued, and the log says `no LLM is set: an operator sets it on Admin → Settings`.
- **Defaults.** The defaults on **Admin → Settings** (approve clean reviews, review drafts, failure status, Job timeout and Job memory) apply to repositories switched on in the dashboard only; repositories in the config keep the config's own. The Job image, concurrency, `max_running` and resources stay in the config and the chart.
- **`PERGENT_SH_SECRET_KEY`.** Secrets are stored encrypted with it. Changing it isn't supported and there is no rotation. A stored secret that can't be opened with the key shows as "not set" (startup logs which instance and fields, `secrets of an instance couldn't be opened with PERGENT_SH_SECRET_KEY; they count as not set`), and has to be entered again.

### Repositories in the config and in the dashboard

- A repository listed in `repositories` is shown as **managed by config** and is read-only in the dashboard; a write to it gets 409. Every dashboard setting has a config key (`approve`, `failure_status`, `review_drafts`, `skills`, `job_timeout`, `job_memory`; see [Config reference](#config-reference)).
- A repository switched on in the dashboard is stored in Postgres. pergent.sh sets it up (webhook and deploy token on GitLab) the same way as a config repository, in a reconcile of that repository alone it starts at once (switches made within 10 seconds of each other are handled together, at the end of those 10 seconds); switching it off removes them the same way. The hourly reconcile still covers every repository. Users whose teams cover it (an admin team covers every repository of its instance) see a newly switched-on repository from their next request, once the platform has confirmed they can see it.
- A repository can't be in both: when it is added to the config, the next reconcile deletes its dashboard record and the config's settings apply. Removing a repository from the config switches it off; it doesn't fall back to the dashboard. To hand a repository to the dashboard, remove it from the config, redeploy, then switch it on in the dashboard.

### Audit log

Admins read the audit log under **Admin → Audit log**: who did what and when, for every change made in the dashboard. Each entry names its actor as `alice (gitlab main)` (username, platform and instance), `root (password)` for a password admin or `cli` for a reset from the command line. It records sign-ins (GitLab, GitHub and password), failed password sign-ins and lockouts, password changes and resets, accounts, teams (the members added and removed by username, and the role before and after), instances and which of their fields changed, the LLM and the defaults (which fields), and repositories switched on and off with their settings changed or reset. It is kept for 90 days and never holds a secret value: an entry for a secret names the field, not its content. A platform admin doesn't see the entries about repositories it can't see, about other instances, or the password admins' own sign-ins and passwords.

### Logs and "Needs attention"

- **Logs.** When a review Job ends, or before the Job of a superseded run is deleted, pergent.sh keeps the last 200 KB of the pergent container's log, with the Job's clone token, LLM key and result token replaced by `[REDACTED]`. Logs are deleted after 30 days. A Job that is gone before its log is read (Jobs are removed an hour after they finish) has no log.
- **Needs attention** lists setup problems that pergent.sh detects, kept up to date by the hourly reconcile and the Job watcher (not computed when the page loads):
  - one approval is enough to merge, so pergent's approval alone can unlock it;
  - the GitHub approve App isn't installed on the repository, or the approve bot user can't reach it;
  - reviews running out of memory (two or more in the last 7 days);
  - the GitHub approve bot's token expires within 14 days. This one concerns the whole instance and is shown to its admins and the password admins;
  - an instance added in the dashboard couldn't be connected (`instance_unreachable`; it goes when the instance connects), or its removal is stuck because the platform refuses to give up its webhooks ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)). These concern the whole instance too: they are shown to the password admins and to that instance's admins.

### Limits of the access check

Each signed-in platform user costs one platform API call per repository their teams cover (an admin team covers every repository switched on for their instance), at most once every 5 minutes while they use the dashboard, plus one to read the user. A waiting user, with no team, costs that one call at sign-in and none after. Password admins cost none. On GitLab a repository whose own permissions show less than Reporter (an internal project, or one reached through a share) costs a second call. GitHub allows 5000 requests an hour per user, so a very large installation (several hundred repositories on one GitHub instance) can run a user into GitHub's rate limit; their check then fails as described above.

A sign-in, and each later access check, must finish within 30 seconds in all: the code exchange and reading the user, then the covered repositories, 8 at a time, each call with its own 10 second timeout. The repositories take about (number of repositories ÷ 8) × the platform's response time, so a user whose teams cover more than roughly 8 × 30 s ÷ that response time repositories can't be checked in time: about 900 at 250 ms a call, 2,400 at 100 ms, and half that on GitLab where most repositories need the second call. Past that, every sign-in of such a user fails with "Could not check which repositories you can see", and sessions already signed in see nothing once their last good check is 10 minutes old. The 30 seconds can't be configured: keep the repositories a team covers below that number (use several teams for different people), or split them across instances, and run pergent.sh close to GitLab or GitHub so each call is quick.

## Config reference

The config file is YAML (`pergent-sh serve --config <path>`, default `/etc/pergent-sh/config.yaml`). Unknown keys are an error. It holds no secrets: tokens and keys are named by environment variable. Every problem found is reported at startup, and pergent.sh does not start with an invalid config, a missing skill, a missing secret, a database it cannot reach, or, for an instance in the config file, a GitLab token that does not work or a GitHub App key or approve token that does not work (instances added in the dashboard never stop startup). The error names the instance and what failed, never the key or token. Without `dashboard.url`, at least one `gitlab` or `github` instance and an `llm:` block are required; with it, both can be added in the [dashboard](#dashboard) instead. `dashboard.admins`, the old admin groups and teams, was removed: a config that still has it is refused at startup (`dashboard.admins was removed: admins are now teams in the dashboard (see the README, "Teams")`).

| Field | Default | Description |
| --- | --- | --- |
| `listen` | `:8080` | Address of the HTTP server. |
| `public_url` | required | HTTPS base URL where GitLab and GitHub reach pergent.sh, without a trailing slash. The GitLab webhook is created as `<public_url>/webhooks/gitlab/<instance name>`; the GitHub review App's webhook URL is `<public_url>/webhooks/github/<instance name>`. |
| `callback_url` | `public_url` | Base URL where Jobs post their result (`<callback_url>/runs/<id>/result`). `http` or `https`, no trailing slash. Set it to the in-cluster Service to keep that traffic inside the cluster. |
| `skills_dir` | required | Directory with the skill library: top-level `*.md` files and `<name>/SKILL.md` folders. The chart sets it to `/etc/pergent-sh/skills`. |
| `gitlab[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. |
| `gitlab[].url` | required | Base URL of the GitLab instance. It must be `https` (the bot tokens travel in a header), without a trailing slash. A redirect to another host is never followed. |
| `gitlab[].review_token_env` | required | Name of the env var with the review bot token. |
| `gitlab[].approve_token_env` | required | Name of the env var with the approve bot token. It must belong to a different user than the review bot token. |
| `gitlab[].webhook_secret_env` | required | Name of the env var with the webhook secret. GitLab sends it as `X-Gitlab-Token`. |
| `github[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. One instance is one pair of Apps and covers every organization they are installed on. |
| `github[].api_url` | `https://api.github.com` | GitHub's REST API. Only GitHub.com is supported: another `https` URL (GitHub Enterprise Server) is accepted but untested. |
| `github[].review_app.app_id` | required | The review App's numeric App ID. |
| `github[].review_app.private_key_env` | required | Name of the env var with the review App's private key. |
| `github[].approve_app.app_id`, `.private_key_env` | one of `approve_app` and `approve_token_env` | The approve App: its App ID and the env var with its private key. The default approver. Both must differ from `review_app`'s: one App, or one key, can't both review and approve. |
| `github[].approve_token_env` | one of `approve_app` and `approve_token_env` | Name of the env var with the approve bot user's fine-grained personal access token, instead of an approve App. See [Approving as a bot user](#approving-as-a-bot-user). |
| `github[].webhook_secret_env` | required | Name of the env var with the review App's webhook secret. GitHub signs each delivery with it (`X-Hub-Signature-256`). |
| `gitlab[].oauth_client_id_env`, `gitlab[].oauth_client_secret_env` | none | Names of the env vars with the instance's OAuth application ID and secret, for [dashboard](#dashboard) sign-in. Both or neither; with neither, nobody signs in with this instance. |
| `github[].oauth_client_id_env`, `github[].oauth_client_secret_env` | none | Names of the env vars with the review App's client ID and client secret, for [dashboard](#dashboard) sign-in. Both or neither. |
| `dashboard.url` | none (dashboard off) | The dashboard's `https` address, a bare host without a port, path or trailing slash. Setting it turns the dashboard on, with password sign-in and the first password admin; instances and the LLM can then be added there. To let people sign in with GitLab or GitHub, an instance needs an OAuth client (`oauth_client_*_env` in the config, or entered in the dashboard). The sign-in callbacks are `<dashboard.url>/auth/<gitlab or github>/<instance name>/callback`. |
| `dashboard.admin_password_env` | none | Name of the env var with the first password admin's password (at least 12 characters), for the account `admin` made on the first start with the dashboard on. Empty or unset: pergent.sh generates a password and logs it once. The chart fills the variable from `adminPassword.existingSecret`. Needs `dashboard.url` to have an effect. See [First sign-in](#first-sign-in). |
| `repositories[].gitlab` | one of `gitlab` and `github` | Name of one of the `gitlab` instances. |
| `repositories[].github` | one of `gitlab` and `github` | Name of one of the `github` instances. |
| `repositories[].project` | required | Project path, such as `acme/widgets`; on GitHub exactly `owner/name`, matched case-insensitively. Each project appears once per instance. |
| `repositories[].skills` | all library skills | Names of library skills to run. Empty or omitted means all of them. A name that is not in the library stops pergent.sh at startup. |
| `repositories[].approve` | `true` | Approve clean reviews. With `false`, pergent.sh still reviews, comments and sets the status, but never approves, and withdraws any approval the approve bot already made in the repository. The dashboard's "Approve clean reviews" switch is the same setting for a repository the dashboard manages. See [Turning approval off](#turning-approval-off). |
| `repositories[].review_drafts` | `true` | Review drafts. With `false`, a draft merge request (GitLab `draft`, or `work_in_progress` on older versions) or draft pull request is not reviewed: no run, comment, status or approval, until it is marked ready, which starts its review. Closing or merging is handled as before. The dashboard's "Review drafts" switch is the same setting for a repository the dashboard manages. With `true`, a draft is reviewed but approved only once it is marked ready. See [Pushes, edits and failures](#pushes-edits-and-failures). |
| `repositories[].failure_status` | `failed` | Status used when a review could not be done: `failed` or `neutral`. GitLab has no neutral state, so `neutral` is set as `skipped`; a project can count skipped pipelines as successful through a GitLab setting. On GitHub it is the Check Run conclusion `failure` or `neutral`, and GitHub counts a `neutral` conclusion as passing for a required status check: with `neutral`, a failed review does not block the merge. `failed` is never counted as successful. Blocking findings are always `failed`. |
| `repositories[].job_timeout` | `jobs.timeout` | The review time budget of this repository's Jobs, such as `20m`. Must be positive. |
| `repositories[].job_memory` | `jobs.resources.limits.memory` | The memory limit of this repository's Jobs, such as `6Gi`. Only the memory limit changes; it must not be below `jobs.resources.requests.memory`. |
| `llm.provider` | required with an `llm:` block | opencode provider ID, such as `anthropic`. The whole `llm:` block is optional when `dashboard.url` is set (a password admin sets the LLM in the dashboard); a block that is present wins and makes the LLM read-only there, and any field set makes it present: it must then be complete (`provider`, `model` and the key variable). Without `dashboard.url` it is required. |
| `llm.model` | required with an `llm:` block | Model name, such as `claude-sonnet-4`. |
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
| Every skill is clean and at least one file was reviewed, but some changed file was skipped as `deleted`, `binary`, `vendored`, `lockfile`, `generated` or under a rule pergent.sh doesn't know | The review plus a note listing those files and the rules that skipped them | `success` ("No blocking issues; not approved: some changed files weren't reviewed") | **No**, a human needs to look |
| Every skill is clean, at least one file was reviewed and no file was skipped under those rules, but approval is turned off for the repository (`approve: false`, or the dashboard's switch) | The review plus `**Not approved:** approval is turned off for this repository.` (this note replaces the draft and approve App notes below) | `success` ("No blocking issues; not approved: approval is turned off") | **No**, see [Turning approval off](#turning-approval-off) |
| Every skill is clean, at least one file was reviewed and no file was skipped under those rules, but the merge request is a draft | The review plus a note that it will be approved when it's marked ready | `success` ("No blocking issues; not approved: draft") | **No**, until it is marked ready |
| As above and not a draft, but on GitHub the approve App isn't installed on the repository (or the approve bot user can't reach it) | The review plus `**Not approved:** the pergent approve App isn't installed on this repository.` (or `... the pergent approve bot can't reach this repository.`) | `success` ("No blocking issues; not approved: ...") | **No**, see [GitHub](#github) |
| Every skill is clean, at least one file was reviewed, no file was skipped under those rules, approval is on, and the merge request isn't a draft | The review | `success` ("No blocking issues") | Yes, see below |

Those five are pergent's default skip rules, and a merge request can trigger them on its own: for example, by putting code under a `vendor/` directory, it would keep that code from the reviewer. A file skipped because it is outside a skill's `paths`, matches a skill's `exclude`, or has no content changes does not stop the approval; a file skipped under any other rule does, including a rule a newer pergent adds that pergent.sh doesn't know yet. The note lists at most 20 paths, each cut to 120 characters, in at most 4000 bytes; a path with a backtick, a line break or another control character is shown quoted and escaped, so a file name can't change the comment's markdown.

The "Commit status" column is the GitLab commit status. On GitHub the same decision is the conclusion of the `pergent` Check Run:

| Status in the table | GitLab commit status | GitHub Check Run conclusion |
| --- | --- | --- |
| `success` | `success` | `success` |
| `failed` | `failed` | `failure` |
| `failure_status` | `failed` or `skipped` (`failure_status: neutral`) | `failure` or `neutral` (`failure_status: neutral`) |

A review that is running is `running` on GitLab and an `in_progress` Check Run on GitHub; a superseded run ends as `canceled` on GitLab and `cancelled` on GitHub. See [The `pergent` Check Run](#the-pergent-check-run).

Before approving, pergent.sh asks GitLab (or GitHub) for the merge request. It approves only when the merge request is still **open** and not a draft, its head is still the commit that was reviewed and its target branch is still the one it was reviewed against. If the head or target moved or the MR was closed, there is no approval; the review and its status still belong to the reviewed commit, and a newer push gets its own run. The approval is recorded before it is made, and a refusal when the bot already approves this head is treated as done. On GitHub the approval is a review with `event: APPROVE` on the reviewed commit; GitHub has no commit guard on it, so the head and target check right before is the guard.

When a merge request gets a new head or a new target branch after the approve bot approved it, the approve bot removes its approval (on GitHub it dismisses its approving reviews), whether or not the platform resets approvals itself. This happens as soon as the webhook arrives. A webhook can be lost (GitLab doesn't resend a failed delivery; pergent.sh was restarting, the hook was disabled), so pergent.sh also checks every approval it holds against the platform at startup and every 5 minutes: an approval of a commit that is no longer the head, or of a merge request that was retargeted or closed, is removed then; the approval of a merged one is forgotten without writing anything. An approval can therefore outlive a missed webhook by up to 5 minutes. For an open merge request, the current head also gets a review if the repository is still configured.

A failed, partial or missing review is never `success` and is never approved.

### Turning approval off

A repository's `approve: false` (or, for a repository the dashboard manages, its "Approve clean reviews" switch turned off) stops approvals there: reviews still run and are posted with their comment and status, and a clean review gets `**Not approved:** approval is turned off for this repository.` Only a review that would otherwise be approved changes; a blocking, failed, partial or draft review is decided as before.

Turning it off also withdraws every approval the approve bot already holds in the repository, on the next cleanup pass (every 2 seconds): GitLab's approve bot unapproves, and on GitHub the approve App or bot dismisses its approving reviews. An approval the bot can't withdraw is tried again with the same backoff as a stale approval (30 seconds, doubling up to 5 minutes). Turning approval back on approves nothing by itself: the next clean review of a merge request is approved.

### Pushes, edits and failures

- Only pushes, opening or reopening, and changing the target branch start a run. Editing the title, labels, description or assignees does not. Closing or merging supersedes the MR's runs. On GitHub these are the `pull_request` actions `opened`, `reopened`, `synchronize`, `edited` (only when `changes.base` is present: a retarget) and `closed`; every other action and event (including `ping`) is answered 200 and ignored.
- A repository with `review_drafts: false` doesn't review drafts. The webhook of a draft that would start a run (an open, reopen, push or retarget; GitLab `draft` or `work_in_progress`, GitHub `pull_request.draft`) is answered 200 and logged as `draft_skipped`, and nothing is queued; the merge request's runs still queued, running or not yet posted (of an older head or target) are superseded, and the dashboard shows them as "Not reviewed: the change is a draft and drafts are not reviewed here." If a run was queued before the merge request became a draft, or the setting changed, the dispatcher finds the draft and supersedes the run, with the same note in the dashboard, without starting a Job or posting a status or comment; a newer head of such a draft isn't queued either. Closing or merging still supersedes the runs, and an approval or review already made stays. Marking the draft ready starts its review: the head's review is decided again if there is one to decide (see the next item), otherwise the head is queued like a push.
- With `review_drafts` on (the default), marking a draft ready starts no run when the head already has a review (GitLab: an `update` whose `changes.draft` goes to false; GitHub: `ready_for_review`). When the head's review is already posted, or is being posted, pergent.sh decides it again: the comment loses its draft note, and a clean review is approved. When the review is still running, it is published as for any other merge request that isn't a draft. When the head has no review at all, it is queued like a push. GitLab versions that report `work_in_progress` instead of `draft` are handled the same way. Marking a merge request as a draft again doesn't withdraw an approval it already has.
- A run is one head reviewed against one target branch. Changing the target branch reviews the same head again against the new branch, supersedes the review against the old one and withdraws the bot's approval.
- A head and target branch that come back (retargeted main → develop → main, or pushed A → B → A) are reviewed again, even when they were reviewed before: since then the comment, the commit status (which GitLab keeps per commit, not per target branch) and the approval show the other review. A repeated webhook for the newest run's head and target starts nothing.
- Two pushes in quick succession: only the newest head is reviewed. The older Job is deleted, and a late result for it is accepted and discarded. Out-of-order push webhooks are handled: the dispatcher reviews the MR's current head.
- If GitLab has not computed the merge request's diff after 10 minutes, the run fails with a comment. On GitHub the merge base comes from comparing the pull request's `base.sha` with its head; if GitHub can't answer that within 10 minutes, the run fails with "GitHub hasn't computed this pull request's merge base yet".
- A Job that dies without a result (out of memory, a failed checkout, the deadline, a failed delivery, a deleted Job) produces an error comment and a non-success status within about 2 minutes.
- A Job whose pod can't start because its image can't be pulled (or its container config is invalid) is deleted and reported after about 5 minutes.
- A Job whose pod the cluster refuses to create (for example, one rejected by an admission policy) is only reported when the Job reaches its deadline, as `DeadlineExceeded`: `jobs.timeout` plus at least 4 minutes, and about a minute more for the report.
- Runs that were claimed but not launched (for example because pergent.sh crashed) are requeued at startup and after 5 minutes.
- Publishing is retried with backoff. After 10 failed attempts the run becomes `publish_failed`: pergent.sh makes one last attempt to set the head's status to `failed` with the description "pergent couldn't post its review" (GitHub: the Check Run completed with `failure` and that title), so a required check isn't left running, and posts nothing more to the merge request; it is never approved. The failure itself is only in the logs.
- The comment pergent renders is held to 990,000 bytes on GitLab and 60,000 on GitHub, which refuses a comment over 65,536 characters; the rest of the limit is room for the note pergent.sh adds (the list of skipped files is at most 20 paths and 4000 bytes).

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

A repository is on when it is listed in the config or switched on in the [dashboard](#dashboard). To switch a config repository off, remove it from `repositories` and redeploy; a dashboard repository is switched off with its switch. On the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token. Runs still queued for it are dropped when they come up for dispatch; a review that was already running is still posted, but never approved, as with approval turned off.

A GitHub repository has nothing to delete: removing it from the config deletes only pergent.sh's own record of it. Installing an App on a repository never switches it on; only the config list or the dashboard does.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days. An instance added in the dashboard is removed there instead, which takes its webhooks and deploy tokens off the platform first ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)).

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.

## HTTP routes

| Route | Used by |
| --- | --- |
| `POST /webhooks/gitlab/{instance}` | GitLab merge request webhooks. The `X-Gitlab-Token` header is compared in constant time with the instance's secret; a mismatch gets 401 and is otherwise ignored. An authenticated webhook whose payload can't be used (not JSON, no project id or merge request iid, a malformed head SHA) gets 200 and a warning in the log, because GitLab counts failed deliveries toward disabling the hook. |
| `POST /webhooks/github/{instance}` | The GitHub review App's `pull_request` webhook. `X-Hub-Signature-256` (HMAC-SHA256 of the body with the instance's webhook secret) is compared in constant time; a missing or wrong signature gets 401 and is otherwise ignored. A repository that isn't in the config gets 200 and is ignored. |
| `POST /runs/{id}/result` | A review Job delivering its result, with its one-time bearer token. |
| `GET /healthz` | Health check. It answers as soon as the server starts: pergent.sh serves right away and runs the startup reconcile in the background. |
| `GET /auth/{gitlab or github}/{instance}/login`, `GET .../callback`, `POST /auth/logout` | [Dashboard](#dashboard) sign-in with GitLab or GitHub, and sign-out. Only with `dashboard.url` set. |
| `GET /auth/csrf`, `POST /auth/password`, `POST /auth/password/change` | Password sign-in. `GET /auth/csrf` hands out the sign-in form's CSRF token (a cookie and `{"csrfToken": ...}`); `POST /auth/password` takes `{"username", "password"}` with that token in `X-CSRF-Token` and answers 204 and the session cookie; `POST /auth/password/change` takes `{"current", "new"}` for a signed-in password account. See [Password sign-in limits](#password-sign-in-limits). Only with `dashboard.url` set. |
| `/api/...` | The dashboard's JSON API. Every route but `GET /api/instances` needs a session; writes need the `X-CSRF-Token` header. Only with `dashboard.url` set. |
| `GET /` and every other path | The dashboard's pages. Only with `dashboard.url` set; without it they are 404. |

Logs are JSON on stdout. Secrets are never logged.

## Limits

This release is deliberately small:

- No merge request commands (such as asking for a re-review in a comment), and no re-run or dismiss buttons in the dashboard.
- Dashboard sign-in is a password (operators) or GitLab or GitHub. No OIDC, SAML or LDAP, no two-factor authentication, and no API tokens for scripts.
- Teams are kept in pergent.sh: they are not synced from GitLab groups or GitHub teams.
- A GitLab and a GitHub account of one person are two accounts, with no linking.
- `PERGENT_SH_SECRET_KEY` can't be rotated: see [Environment variables](#environment-variables).
- No advice on whether a human approval is still needed beyond the hourly warning for a repository that one approval can merge.
- No repository cache: every checkout is a fresh `git init` plus a shallow fetch.
- One replica, and PostgreSQL is external.
- GitHub.com only. `github[].api_url` accepts another URL, but GitHub Enterprise Server is untested.
- On GitHub, approving after the approve App is installed starts with the next push or ready-for-review; there is no catch-up pass over open pull requests.

## Publishing the chart (at release time)

The chart is not published yet. To publish a version, bump `version` (and `appVersion`) in `charts/pergent-sh/Chart.yaml` and set `image.tag` in `charts/pergent-sh/values.yaml` to the release version (for example `v0.1.0`, with `image.pullPolicy: IfNotPresent`): the default `main` is a moving development build and must not ship in a released chart. Then run:

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
