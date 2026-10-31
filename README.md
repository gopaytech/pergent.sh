# pergent.sh

pergent.sh is a service that runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests. It runs inside your Kubernetes cluster and starts one Kubernetes Job per review.

When someone opens or pushes to a merge request (or pull request) in a repository you listed in the config:

1. GitLab sends a merge request webhook, or GitHub a `pull_request` webhook, to pergent.sh.
2. pergent.sh starts a Kubernetes Job. The Job checks out the change, runs `pergent agent` over it and posts the result back.
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub.
4. If the review found no blocking issue, actually reviewed something and skipped no changed file except under the rules that leave it reviewed (outside a skill's scope, excluded by a skill, no content changes), a separate approver approves it: a second GitLab user on GitLab, a second GitHub App (or a bot user) on GitHub. A draft is reviewed the same way but is approved only once it is marked ready.

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the LLM key and a one-time token to hand in its result.

This release covers GitLab (gitlab.com or any self-managed instance) and GitHub.com; see [GitHub](#github) for the GitHub setup. An optional web [dashboard](#dashboard) shows every review and lets admins switch repositories on and change their settings. There are no merge request commands; see [Limits](#limits).

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
- An API key for an LLM provider that opencode supports.
- A way for GitLab or GitHub to reach pergent.sh over HTTPS (an Ingress, for example).
- For the optional [dashboard](#dashboard): an HTTPS host for it, and a GitLab OAuth application per GitLab instance or user authorization on the GitHub review App.

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
- A Service on port 8080 and, when `ingress.enabled`, an Ingress that by default exposes only `/webhooks/` and `/runs/` (`ingress.paths`). The chart refuses to render it with `/`, `/api/` or `/auth/` in `ingress.paths`: those are the dashboard's.
- When `dashboardIngress.enabled`, a second Ingress for the [dashboard](#dashboard) on its own host(s), exposing `/`, `/api/` and `/auth/` of the same Service.
- A ServiceAccount, and in `jobsNamespace` a Role and RoleBinding for it: `batch/jobs` (create, get, list, watch, patch, delete), `secrets` and `configmaps` (create, get, delete), `pods` (get, list) and `pods/log` (get, to keep each review's log). There are no wildcard rules and nothing outside `jobsNamespace`.
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
| each `gitlab[].oauth_client_id_env`, `oauth_client_secret_env` | The instance's OAuth application, for signing in to the [dashboard](#dashboard). Required when `dashboard.url` is set, and checked at startup. |
| each `github[].oauth_client_id_env`, `oauth_client_secret_env` | The review App's client ID and a client secret, for signing in to the [dashboard](#dashboard). Required when `dashboard.url` is set, and checked at startup. |

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

- **Webhook:** active. URL `<public_url>/webhooks/github/<instance name>`, for example `https://pergent.example.com/webhooks/github/gh`. Secret: a random string (`openssl rand -hex 32`); it goes into the Secret as the instance's `webhook_secret_env`.
- **Repository permissions:**
  - Pull requests: **Read-only**
  - Contents: **Read-only**
  - Checks: **Read and write**
  - Issues: **Read and write** (pull request comments are issue comments)
  - Metadata: **Read-only** (always on)
- **Organization permissions:**
  - Members: **Read-only**. The [dashboard](#dashboard) checks with it whether a user is in the admin team. Ask for it even if you don't use the dashboard yet: adding a permission later makes every organization approve the App again.
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
2. Update the Secret with the new `.pem` file.
3. Restart pergent.sh (`kubectl -n pergent-sh rollout restart deployment/pergent-sh`) and check that it started.
4. Delete the old key in the App's settings.

Installation tokens minted with the old key expire within an hour.

## Dashboard

The dashboard is a web app built into pergent.sh. It is off until `dashboard.url` is set. People sign in with their GitLab or GitHub account and see the repositories pergent.sh reviews that they can see on that platform, with every review run, its findings, its files and its log. Admins also switch repositories on and off and change their settings.

### Turning it on

1. Pick the dashboard's address, for example `https://pergent-dashboard.example.com`. It must be `https` and a bare host, without a path: the dashboard, its API (`/api/`) and its sign-in routes (`/auth/`) are served at `/` of that host, and its cookies only work over https. It can be an internal-only host. Webhooks and Job results stay on `public_url`, which GitHub.com must still reach from the internet.
2. Set up sign-in on every instance people should sign in with: [a GitLab OAuth application](#sign-in-with-gitlab) or [user authorization on the GitHub review App](#sign-in-with-github). Put the client ID and secret in the Secret.
3. Add to the config:

   ```yaml
   config:
     gitlab:
       - name: main
         # ...the tokens as before...
         oauth_client_id_env: GITLAB_OAUTH_CLIENT_ID
         oauth_client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
     dashboard:
       url: https://pergent-dashboard.example.com
       admins:
         - instance: main
           group: acme/platform     # GitLab: a group path
         # - instance: gh
         #   team: acme/platform    # GitHub: org/team-slug
   ```

4. Turn on the chart's dashboard Ingress, which is separate from the webhook Ingress so the two can be on different hosts:

   ```yaml
   dashboardIngress:
     enabled: true
     className: nginx
     hosts: [pergent-dashboard.example.com]
     tls:
       - secretName: pergent-dashboard-example-com-tls
         hosts: [pergent-dashboard.example.com]
   ```

   It exposes `/`, `/api/` and `/auth/` of the pergent.sh Service; the webhook Ingress never does. Its `/` is a prefix, so the dashboard host also routes `/webhooks/`, `/runs/` and `/healthz` to pergent.sh; webhooks and results there still need a valid signature or result token, and `/healthz` only answers `ok`. The chart refuses to render it without `config.dashboard.url`, or when none of `hosts` is that URL's host. Without `dashboard.url` pergent.sh doesn't serve those routes at all.

pergent.sh doesn't start when `dashboard.url` is set and an instance's `oauth_client_id_env` or `oauth_client_secret_env` variable is unset or empty; the error names the instance and the variable. With no `dashboard.admins`, it starts and logs a warning: everyone can sign in, but nobody is an admin.

### Sign in with GitLab

Create one OAuth application per GitLab instance: as an instance administrator under **Admin area → Applications**, or under a group's **Settings → Applications**.

- **Redirect URI:** `<dashboard.url>/auth/gitlab/<instance name>/callback`, for example `https://pergent-dashboard.example.com/auth/gitlab/main/callback`.
- **Confidential:** yes.
- **Scopes:** `read_user` and `read_api`.

Put its Application ID and Secret in the Secret (for example `GITLAB_OAUTH_CLIENT_ID` and `GITLAB_OAUTH_CLIENT_SECRET`) and name them in the instance's `oauth_client_id_env` and `oauth_client_secret_env`.

### Sign in with GitHub

GitHub sign-in uses the review App's own user authorization, so there is no second App to create. In the review App's settings:

- Under **Identifying and authorizing users**, set the **Callback URL** to `<dashboard.url>/auth/github/<instance name>/callback`, for example `https://pergent-dashboard.example.com/auth/github/gh/callback`. Keep **Expire user authorization tokens** on (pergent.sh renews them) and leave **Request user authorization (OAuth) during installation** off.
- Generate a **client secret**, and note the **Client ID** on the same page.
- Check that the App has **Organization permissions → Members: Read-only** (see [Create the review App](#1-create-the-review-app)). An App created before it was on that list needs it added, and each organization then has to approve the new permission. Without it, or when the App isn't installed in the team's organization, GitHub refuses to show the team (403): users still sign in and see their repositories, but nobody is an admin through that team. pergent.sh logs a warning naming the instance and team and records a "Needs attention" item for the instance, which goes away at the first access check that can read the team. Other errors of that check (5xx, rate limits, the network) still fail the sign-in.

Put the Client ID and the client secret in the Secret and name them in the GitHub instance's `oauth_client_id_env` and `oauth_client_secret_env`. A user signed in through the App only reaches repositories the App is installed on.

### Who sees what

- **Signing in.** One account per sign-in: someone who uses GitLab and GitHub signs in with one of them at a time and sees that instance's repositories. A new sign-in ends the session the browser had. The user's OAuth token is stored encrypted with `PERGENT_SH_SECRET_KEY` and never sent to the browser.
- **Sessions** are a cookie (`HttpOnly`, `Secure`, `SameSite=Lax`) holding a random ID. A session ends after 8 hours without use, and 7 days after sign-in whether used or not. pergent.sh deletes ended sessions every hour.
- **"Can see"** means the user can read the repository's code and merge requests on the platform, because findings and logs contain code:
  - GitLab: the Reporter role or above, through the project, its group, or a group the project or its group is shared with, or a public project whose repository and merge requests are open to everyone. A share is checked with one more call to GitLab, made only when the project's own permissions show less than Reporter.
  - GitHub: read access to the repository, or a public repository.
- **Checked on the server.** At sign-in pergent.sh asks GitLab or GitHub, with the user's token, about every repository switched on for that instance, and asks whether the user is in the admin group or team. It asks again when the answer is older than 5 minutes and the user makes a request, so lost access stops showing within 5 minutes. If asking fails, the last answer is used for 5 more minutes, then the user sees nothing until it works again. Every API answer is filtered by that list, which holds each repository's path and its platform ID: a run is shown only when both match, so a project created at the path of a deleted, renamed or transferred one never shows the earlier project's reviews. A repository, review or run the user can't see is answered exactly like one that doesn't exist.
- **Admins** are the members of the `dashboard.admins` group or team on the instance they signed in with. GitLab: direct members of the group only, active, with the Guest role or above (Minimal Access doesn't count, nor does a membership awaiting approval). Members of a parent group, or of a group the admin group is shared with, are not admins: add admins to the group directly. GitHub: active members of the team. Being an admin on one instance gives nothing on another.

**Everyone signed in** sees, for the repositories they can see:

- **Overview:** the number of reviews, approvals, blocking reviews and failed reviews over the last 7, 30 or 90 days, the most recent reviews, and "Needs attention".
- **Reviews:** filtered by repository, result and time. Each merge request has a page with every run (commit, what started it, result, duration) and, for a run, the status set on the platform, whether it was approved and why not, the pergent version, model, tokens, each skill's verdict, findings, checks, files (reviewed, skipped with the rule, not reviewed) and the log.
- **Repositories:** each with its last 7 days and health; its reviews, its setup checks and its skills and instructions. Skills & instructions shows what a review of the default branch would get, read with the review bot or App and kept for 5 minutes.
- **Skills:** the skill library, read-only.

**Admins** also get:

- the on/off switch of each repository and **Add repository**, which lists the repositories that both the admin (GitLab: Reporter or above as a member, a shared group's member included; GitHub: read access) and the review bot (GitLab: Maintainer) or review App (GitHub: installed) can reach, at most 500, with a search;
- each repository's **Settings**: approve clean reviews, the failure status, review drafts, the library skills to run, and the Job's timeout (60 to 7200 seconds) and memory limit (from 256Mi, and not below `jobs.resources.requests.memory`, to 32Gi). Each shows "overridden here" or "default", and "Reset to defaults" clears every override;
- **Integrations** (their own instance: connection, approver, each repository's setup and whether the approver reaches it, an approve token about to expire) and **Settings** (the defaults and the admin groups or teams), both read-only.

Every write is checked on the server: it needs an admin, a repository the admin can see, and the session's CSRF token. A developer calling the API directly gets 403 and nothing changes.

### Repositories in the config and in the dashboard

- A repository listed in `repositories` is shown as **managed by config** and is read-only in the dashboard; a write to it gets 409. Every dashboard setting has a config key (`approve`, `failure_status`, `review_drafts`, `skills`, `job_timeout`, `job_memory`; see [Config reference](#config-reference)).
- A repository switched on in the dashboard is stored in Postgres. pergent.sh sets it up (webhook and deploy token on GitLab) the same way as a config repository, in a reconcile of that repository alone it starts at once (switches made within 10 seconds of each other are handled together, at the end of those 10 seconds); switching it off removes them the same way. The hourly reconcile still covers every repository. Other signed-in users see a newly switched-on repository at their next access check, within 5 minutes.
- A repository can't be in both: when it is added to the config, the next reconcile deletes its dashboard record and the config's settings apply. Removing a repository from the config switches it off; it doesn't fall back to the dashboard. To hand a repository to the dashboard, remove it from the config, redeploy, then switch it on in the dashboard.

### Logs and "Needs attention"

- **Logs.** When a review Job ends, pergent.sh keeps the last 200 KB of the pergent container's log, with the Job's clone token, LLM key and result token replaced by `[REDACTED]`. Logs are deleted after 30 days. A Job that is gone before its log is read (Jobs are removed an hour after they finish) has no log.
- **Needs attention** lists setup problems that pergent.sh detects, kept up to date by the hourly reconcile and the Job watcher (not computed when the page loads):
  - one approval is enough to merge, so pergent's approval alone can unlock it;
  - the GitHub approve App isn't installed on the repository, or the approve bot user can't reach it;
  - reviews running out of memory (two or more in the last 7 days);
  - the GitHub approve bot's token expires within 14 days. This one concerns the whole instance and is shown to its admins only;
  - the review App can't read a GitHub admin team (see [Sign in with GitHub](#sign-in-with-github)), recorded by the access check of a user who signs in. It concerns the whole instance too and is shown to its admins only, so with a single admin team nobody sees it until it is fixed: the warning in the log is the way to find it.

### Limits of the access check

Each signed-in user costs one platform API call per repository switched on for their instance, at most once every 5 minutes while they use the dashboard, plus one to read the user and one per admin group or team of the instance. On GitLab a repository whose own permissions show less than Reporter (an internal project, or one reached through a share) costs a second call. GitHub allows 5000 requests an hour per user, so a very large installation (several hundred repositories on one GitHub instance) can run a user into GitHub's rate limit; their check then fails as described above.

A sign-in, and each later access check, must finish within 30 seconds in all: the code exchange and reading the user, then the repositories, 8 at a time, then the admin groups or teams one after another, each call with its own 10 second timeout. The repositories take about (number of repositories ÷ 8) × the platform's response time, so an instance with more than roughly 8 × 30 s ÷ that response time repositories switched on can't be checked in time: about 900 at 250 ms a call, 2,400 at 100 ms, and half that on GitLab where most repositories need the second call. Past that, every sign-in on the instance fails with "Could not check which repositories you can see", and sessions already signed in see nothing once their last good check is 10 minutes old. The 30 seconds can't be configured: keep the repositories switched on for an instance below that number, or split them across instances, and run pergent.sh close to GitLab or GitHub so each call is quick.

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
| `dashboard.url` | none (dashboard off) | The dashboard's `https` address, a bare host without a port, path or trailing slash. Setting it turns the dashboard on; at least one instance must then have its `oauth_client_*_env` set. The sign-in callbacks are `<dashboard.url>/auth/<gitlab or github>/<instance name>/callback`. |
| `dashboard.admins[].instance` | none | An instance with sign-in. Its admins are the members of `group` or `team`. Needs `dashboard.url`. |
| `dashboard.admins[].group` | required on a GitLab instance | A GitLab group path, such as `acme/platform`. Only its direct members (Guest or above) are admins; members of a parent group or of a shared group are not. |
| `dashboard.admins[].team` | required on a GitHub instance | A GitHub team as `org/team-slug`, such as `acme/platform`. |
| `repositories[].gitlab` | one of `gitlab` and `github` | Name of one of the `gitlab` instances. |
| `repositories[].github` | one of `gitlab` and `github` | Name of one of the `github` instances. |
| `repositories[].project` | required | Project path, such as `acme/widgets`; on GitHub exactly `owner/name`, matched case-insensitively. Each project appears once per instance. |
| `repositories[].skills` | all library skills | Names of library skills to run. Empty or omitted means all of them. A name that is not in the library stops pergent.sh at startup. |
| `repositories[].approve` | `true` | Approve clean reviews. With `false`, pergent.sh still reviews, comments and sets the status, but never approves, and withdraws any approval the approve bot already made in the repository. The dashboard's "Approve clean reviews" switch is the same setting for a repository the dashboard manages. See [Turning approval off](#turning-approval-off). |
| `repositories[].review_drafts` | `true` | Review drafts. With `false`, a draft merge request (GitLab `draft`, or `work_in_progress` on older versions) or draft pull request is not reviewed: no run, comment, status or approval, until it is marked ready, which starts its review. Closing or merging is handled as before. The dashboard's "Review drafts" switch is the same setting for a repository the dashboard manages. With `true`, a draft is reviewed but approved only once it is marked ready. See [Pushes, edits and failures](#pushes-edits-and-failures). |
| `repositories[].failure_status` | `failed` | Status used when a review could not be done: `failed` or `neutral`. GitLab has no neutral state, so `neutral` is set as `skipped`; a project can count skipped pipelines as successful through a GitLab setting. On GitHub it is the Check Run conclusion `failure` or `neutral`, and GitHub counts a `neutral` conclusion as passing for a required status check: with `neutral`, a failed review does not block the merge. `failed` is never counted as successful. Blocking findings are always `failed`. |
| `repositories[].job_timeout` | `jobs.timeout` | The review time budget of this repository's Jobs, such as `20m`. Must be positive. |
| `repositories[].job_memory` | `jobs.resources.limits.memory` | The memory limit of this repository's Jobs, such as `6Gi`. Only the memory limit changes; it must not be below `jobs.resources.requests.memory`. |
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

A repository is on when it is listed in the config or switched on in the [dashboard](#dashboard). To switch a config repository off, remove it from `repositories` and redeploy; a dashboard repository is switched off with its switch. On the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token. Runs still queued for it are dropped when they come up for dispatch.

A GitHub repository has nothing to delete: removing it from the config deletes only pergent.sh's own record of it. Installing an App on a repository never switches it on; only the config list or the dashboard does.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days.

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.

## HTTP routes

| Route | Used by |
| --- | --- |
| `POST /webhooks/gitlab/{instance}` | GitLab merge request webhooks. The `X-Gitlab-Token` header is compared in constant time with the instance's secret; a mismatch gets 401 and is otherwise ignored. An authenticated webhook whose payload can't be used (not JSON, no project id or merge request iid, a malformed head SHA) gets 200 and a warning in the log, because GitLab counts failed deliveries toward disabling the hook. |
| `POST /webhooks/github/{instance}` | The GitHub review App's `pull_request` webhook. `X-Hub-Signature-256` (HMAC-SHA256 of the body with the instance's webhook secret) is compared in constant time; a missing or wrong signature gets 401 and is otherwise ignored. A repository that isn't in the config gets 200 and is ignored. |
| `POST /runs/{id}/result` | A review Job delivering its result, with its one-time bearer token. |
| `GET /healthz` | Health check. It answers as soon as the server starts: pergent.sh serves right away and runs the startup reconcile in the background. |
| `GET /auth/{gitlab or github}/{instance}/login`, `GET .../callback`, `POST /auth/logout` | [Dashboard](#dashboard) sign-in and sign-out. Only with `dashboard.url` set. |
| `/api/...` | The dashboard's JSON API. Every route but `GET /api/instances` needs a session; writes need the `X-CSRF-Token` header. Only with `dashboard.url` set. |
| `GET /` and every other path | The dashboard's pages. Only with `dashboard.url` set; without it they are 404. |

Logs are JSON on stdout. Secrets are never logged.

## Limits

This release is deliberately small:

- No merge request commands (such as asking for a re-review in a comment), and no re-run or dismiss buttons in the dashboard.
- The dashboard has two roles, admin and everyone else, and no audit log. Its sign-in is GitLab or GitHub only.
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
