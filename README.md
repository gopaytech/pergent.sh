<div align="center">
  <img src="docs/assets/logo.png" alt="pergent" width="110">

  <h1>pergent.sh</h1>

  <p><strong>Run pergent code reviews on GitLab merge requests and GitHub pull requests, one Kubernetes Job per review.</strong></p>

  <p>
    <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
    <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/gopaytech/pergent.sh"></a>
    <a href="https://github.com/gopaytech/pergent.sh/pkgs/container/pergent.sh"><img alt="Image" src="https://img.shields.io/badge/ghcr.io-pergent.sh-informational"></a>
    <a href="charts/pergent-sh/README.md"><img alt="Helm chart" src="https://img.shields.io/badge/helm-pergent--sh-informational"></a>
  </p>

  <p>
    <a href="#-install-with-helm">Install</a> ·
    <a href="#-github">GitHub setup</a> ·
    <a href="#-dashboard">Dashboard</a> ·
    <a href="#-skills-and-instructions">Skills</a> ·
    <a href="#-commands">Commands</a> ·
    <a href="#-config-reference">Config reference</a>
  </p>
</div>

---

pergent.sh is a service that runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests. It runs inside your Kubernetes cluster and starts one Kubernetes Job per review.

## ✨ Features

- **A Job per review**: each review runs as its own Kubernetes Job in your cluster, started by a merge request or pull request webhook, so reviews run in parallel up to `jobs.max_running`.
- **One comment, one status**: pergent.sh posts a single comment, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub.
- **Inline findings**: each finding is also posted as a thread on its lines, with a one-click fix when it is small, and resolved when a later push fixes it. See [Inline findings](#-inline-findings).
- **Incremental reviews**: a push reviews only the files it changed and carries the rest over from the last review. See [Incremental reviews](#-incremental-reviews).
- **Approval with a human guard**: a clean review that actually reviewed something and skipped no file outside the allowed rules is approved by the approver; on GitLab, pergent.sh logs a warning for every project where one approval alone could merge.
- **GitLab and GitHub**: GitLab (gitlab.com or self-managed) and GitHub.com; on GitHub one App is enough to review and approve.
- **Dashboard**: every review, finding, file and log in the browser, with teams and roles for who sees what, an audit log, and instances, secrets and the LLM managed without editing the config file.
- **Skill library**: skills written in the dashboard or installed from a git repository, plus repository skills and instructions read from the target branch.
- **Comment commands**: `@<bot> review`, `@<bot> review failed` and `@<bot> dismiss <id> <reason>` in a comment, with matching buttons on the dashboard's review page.
- **Readiness scan**: advice, per repository, on what is missing for a review to do well, without changing anything and without the LLM.
- **Job isolation**: a Job holds only a read-only clone token, the LLM key and a one-time result token, never the platform token, and runs under a NetworkPolicy.
- **A small Helm chart**: it creates a Service and no Ingress, so you route to it with whatever your cluster uses; PostgreSQL stays outside the chart.

## 🔁 How it works

When someone opens or pushes to a merge request (or pull request) in a repository you listed in the config:

1. GitLab sends a merge request webhook, or GitHub a `pull_request` webhook, to pergent.sh.
2. pergent.sh starts a Kubernetes Job. The Job checks out the change, runs `pergent agent` over it and posts the result back.
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub. It also posts each finding as a thread on its lines ([Inline findings](#-inline-findings)).
4. If the review found no blocking issue (one that people dismissed doesn't count), actually reviewed something and skipped no changed file except under the rules that leave it reviewed (outside a skill's scope, excluded by a skill, no content changes), the approver approves it: a second GitLab user on GitLab, on GitHub the review App itself (one App is enough), a second GitHub App or a bot user. A draft is reviewed the same way but is approved only once it is marked ready.

People can also act on a review where they already are, in a comment on the merge request (`@<bot> review`, `@<bot> review failed`, `@<bot> dismiss <id> <reason>`) or with the buttons on the dashboard's review page: run the review again, run only the skills that failed, and set aside a finding that isn't a real problem. See [Commands](#-commands).

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the LLM key and a one-time token to hand in its result.

This release covers GitLab (gitlab.com or any self-managed instance) and GitHub.com; see [GitHub](#-github) for the GitHub setup. An optional web [dashboard](#-dashboard) shows every review, lets admins manage teams, write and install skills, switch repositories on and change their settings, and lets the operator add GitLab and GitHub instances, their secrets and the LLM without editing the config file. An instance added in the dashboard never blocks startup: when pergent.sh can't reach it, it starts anyway and the dashboard shows what to fix. Comment [commands](#-commands) and the dashboard's buttons re-run a review or dismiss a finding; they never change settings, answer questions or run code (see [Limits](#-limits)).

In this README a "merge request" is also a GitHub pull request, and the "review bot" and "approve bot" are the GitHub review App and approver, unless a section says otherwise.

## 📋 Requirements

- Kubernetes, and permission to create a Role in the namespace where the Jobs run.
- PostgreSQL 13 or newer, reachable from the pergent.sh pod. pergent.sh creates and migrates its own tables at startup.
- Nodes that can pull `ghcr.io/gopaytech/pergent.sh` (the service) and `ghcr.io/gopaytech/pergent` (the Jobs). Both images are built for `linux/amd64` and `linux/arm64` (pergent.sh from v0.4.0, pergent from v0.1.2), so the nodes can be either; each node pulls the one for its own platform.
- On each GitLab instance you use, two users:
  - A **review bot** with the **Maintainer** role on every reviewed project, and an access token with the `api` scope. It creates the project webhook and the read-only deploy token, posts the comment and sets the commit status, and, for [commands](#-commands), reacts to and answers comments and looks up the role of whoever wrote one.
  - An **approve bot** that is an eligible approver of every reviewed project, with an access token with the `api` scope. It must be a different user from the review bot: pergent.sh doesn't start when both tokens belong to the same user. It only approves, and removes its approval when the merge request gets a new commit or a new target branch. If the project requires specific approvers or forbids approval by certain users, make sure this bot qualifies.

    > **Warning:** if a project needs only one approval and the approve bot is an eligible approver, the bot's approval alone unlocks the merge: a merge request can then be merged on a model's review without any human approving it. Give every reviewed project an approval rule that requires a human approval (for example, one approval from a group the approve bot is not in), so the bot's approval is never enough on its own.

    pergent.sh checks this at startup and every hour: for each reviewed project it reads the approval rules that apply to the default branch (`GET /projects/:id/approval_rules`; with none, the project's `approvals_before_merge`) and, when the most any of them requires is one or none, logs a warning naming the project: "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval". It doesn't know whether the approve bot is in a rule's approver list, so it warns whenever the count allows it. If the rules can't be read, that is logged once at info level; the check never stops a review.
- For GitHub, one GitHub App, which reviews and approves (a second App, or a bot user, can approve instead); see [GitHub](#-github).
- An API key for an LLM provider that opencode supports. It can be entered in the [dashboard](#-dashboard) or kept in the Secret.
- A way for GitLab or GitHub to reach pergent.sh over HTTPS. The chart creates only a Service; you route to it (Gateway API, your own Ingress, a load balancer). See [Exposing pergent.sh](#exposing-pergentsh).
- For the optional [dashboard](#-dashboard): an HTTPS host for it. To let people sign in with GitLab or GitHub, also a GitLab OAuth application per GitLab instance or user authorization on the GitHub review App. The operator signs in with a password and needs neither.

## 📦 Install with Helm

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

   config:
     listen: ":8080"
     public_url: https://pergent.example.com
     dashboard:
       url: https://pergent-dashboard.example.com
     jobs:
       namespace: pergent
   ```

   The skill library is not a value: skills are written and installed in the dashboard, so a review has none until an admin adds some ([Skills and instructions](#-skills-and-instructions)).

4. Install:

   ```bash
   helm repo add pergent-sh https://gopaytech.github.io/pergent.sh/charts/releases
   helm repo update
   helm install pergent-sh pergent-sh/pergent-sh -n pergent-sh -f values.yaml
   ```

   A released chart runs the image of its own release (chart `0.2.0` runs `ghcr.io/gopaytech/pergent.sh:v0.2.0`). To run the newest build of the `main` branch instead (every push to `main` publishes `main` and `sha-<short sha>`), install from a checkout with `helm install pergent-sh charts/pergent-sh ...` and `--set image.tag=main --set image.pullPolicy=Always`; that tag moves, so a pod restart can pick up a newer build.

5. Set up in the browser. With `dashboard.url` set and no instance in the config, pergent.sh starts and waits for one. Take the generated admin password from the log ([First sign-in](#first-sign-in)), sign in as `admin` and change it, then add a GitLab or GitHub instance under **Integrations** ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)), set the LLM under **Settings**, and switch repositories on. People who sign in with GitLab or GitHub wait until an admin puts them on a [team](#teams). To set the first password from a Secret instead of the log, see [First sign-in](#first-sign-in).

On startup pergent.sh creates the project webhook and a read-only deploy token on every GitLab project that is switched on. You do not create either by hand. The webhook sends merge request events and note (comment) events; the note events are what [commands](#-commands) are read from. A hook created by an earlier version is updated to send them too, at the next reconcile (startup, then every hour). On GitHub there is nothing per repository to create: the review App's webhook covers every repository it is installed on, provided the App subscribes to the events listed under [Create the review App](#1-create-the-review-app).

What the chart creates:

- A Deployment with **one replica** and the `Recreate` strategy. Do not scale it up: at startup pergent.sh requeues runs it had claimed but not launched, which is only safe when no other pod is dispatching. The pod runs as non-root with a read-only root filesystem, drops all capabilities, and has startup, readiness and liveness probes on `/healthz` (the startup probe allows 150 s, as pergent.sh starts listening only after the database and migrations and, for each instance in the config file, its GitLab tokens or GitHub Apps check out; instances added in the dashboard never delay it). The pod restarts when the config changes. When `adminPassword.existingSecret` is set, the pod also gets that Secret's `adminPassword.key` as the environment variable named by `config.dashboard.admin_password_env` (see [First sign-in](#first-sign-in)).
- A Service on port 8080 (`service.type`, `ClusterIP` by default). No Ingress: see [Exposing pergent.sh](#exposing-pergentsh).
- A ServiceAccount, and in `jobsNamespace` a Role and RoleBinding for it: `batch/jobs` (create, get, list, watch, patch, delete), `secrets` and `configmaps` (create, get, delete), `pods` (get, list) and `pods/log` (get, to keep each review's log). There are no wildcard rules and nothing outside `jobsNamespace`.
- A NetworkPolicy in `jobsNamespace` for the Job pods, review Jobs, [skill fetch Jobs](#installing-from-git) and [readiness scan Jobs](#readiness-scan) alike (those labelled `app.kubernetes.io/managed-by: pergent-sh`): no ingress; egress to DNS (UDP and TCP 53), to TCP 443 and 80 in `jobEgress.cidrs`, and to TCP 8080 of the pergent.sh pods for the result callback. A NetworkPolicy only takes effect when your CNI enforces them.
- A ConfigMap for `config.yaml`.

**Narrow `jobEgress.cidrs`.** The default `0.0.0.0/0` lets a Job reach any address on ports 80 and 443, including in-cluster services and the cloud metadata endpoint. Set it to the addresses of GitLab (or GitHub) and the LLM provider. An empty list (`cidrs: []`) allows no external egress at all: Jobs then reach only DNS and pergent.sh's port 8080. The optional `jobEgress.except` list is added to every `ipBlock` as NetworkPolicy `except` (for example `169.254.169.254/32`); it applies to every cidr, and Kubernetes rejects an entry outside its cidr, so use it with a broad cidr such as the default `0.0.0.0/0`.

**Public skill sources need `jobEgress.cidrs` too.** A skill fetch Job clones a public `https` URL under the same NetworkPolicy, so the host of every URL admins install skills from must be inside `jobEgress.cidrs`. With a narrow list, add the address of that host (for example GitHub's, for skills on github.com), or the fetch fails with `could not fetch <url> at <ref>; the URL, branch or tag may be wrong, or outside jobEgress.cidrs`. Skills of a repository on a connected GitLab or GitHub instance are read by the pergent.sh pod through the platform's API, not by a Job, and need nothing here.

`config.jobs.namespace` must equal `jobsNamespace` and `config.listen` must end in `:8080`; the chart refuses to render otherwise. The chart's `skills` value was removed with `skills_dir`: the skill library lives in the dashboard, and a values file that still sets `skills` fails to render.

If `public_url` points at a hostname that the Job pods reach from outside the cluster, they need egress to it on 443 (covered by the default `jobEgress.cidrs`). To keep Jobs off the internet path, set `callback_url` to the in-cluster Service, for example `http://pergent-sh.pergent-sh.svc:8080`.

### Exposing pergent.sh

The chart creates a Service (`pergent-sh.pergent-sh.svc:8080` for the install above) and nothing in front of it. Route to it with whatever your cluster uses: a Gateway API `HTTPRoute`, your own Ingress, or `service.type: LoadBalancer`. Terminate TLS in front of it; pergent.sh serves plain HTTP.

| Paths | Who calls them | Where to expose |
|---|---|---|
| `/webhooks/` | GitLab and GitHub | `public_url`'s host. GitHub.com and your GitLab must reach it. |
| `/runs/` | the review Jobs (result callback) | `public_url`'s host, or keep it in-cluster with `callback_url`. |
| `/skill-fetches/` | the skill fetch Jobs (result callback, only with the dashboard on) | `public_url`'s host, or keep it in-cluster with `callback_url`, as for `/runs/`. |
| `/readiness-scans/` | the readiness scan Jobs (result callback, only with the dashboard on) | `public_url`'s host, or keep it in-cluster with `callback_url`, as for `/runs/`. |
| `/`, `/api/`, `/auth/` | people, in the [dashboard](#-dashboard) | `dashboard.url`'s host, over https. It can be internal-only. |
| `/healthz` | probes | nowhere; the kubelet calls the pod directly. |

Expose only `/webhooks/`, `/runs/`, `/skill-fetches/` and `/readiness-scans/` on the public host, so the dashboard and its sign-in stay off the internet. Whatever sits in front must set `X-Forwarded-For` (see [Password sign-in limits](#password-sign-in-limits)).

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

For GitHub, put the Apps' private keys (the whole `.pem` file of each) and the review App's webhook secret in the Secret and use `github:` instead of `gitlab:`; see [GitHub](#-github). Without `dashboard.url`, pergent.sh needs at least one instance and the `llm:` block in the config file, and instances can only come from there.

### Environment variables

| Variable | Used for |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL. Required. |
| `PERGENT_SH_SECRET_KEY` | Encrypts the secrets you enter in the dashboard, the stored deploy tokens and the sign-in tokens (AES-256-GCM). 32 random bytes, standard base64. Required. **Changing it isn't supported** and there is no rotation: a stored secret that can't be opened with the new key shows as "not set" in the dashboard and must be entered again (the startup reconcile replaces the deploy tokens it can't open with new ones and revokes the old). |
| the variable named by `dashboard.admin_password_env` | The password of the first password admin, `admin`, at least 12 characters. Optional: without it pergent.sh generates one and logs it once. Read only on the start that creates the account. See [First sign-in](#first-sign-in). |
| the variable named by `llm.api_key_env` (default `OPENCODE_API_KEY`) | When the LLM is in the config file: the LLM key handed to each Job. Required then, and checked at startup. With the LLM set in the dashboard, the key is entered there. |
| each `gitlab[].review_token_env`, `approve_token_env`, `webhook_secret_env` | When the instance is in the config file: the review bot token, the approve bot token and the webhook secret of that instance. Required, and checked at startup. |
| each `github[].review_app.private_key_env` | When the instance is in the config file: the review App's private key, the whole PEM file GitHub gave you (PKCS#1 `RSA PRIVATE KEY` or PKCS#8 `PRIVATE KEY`). Required, and checked at startup. Never logged. |
| each `github[].approve_app.private_key_env` or `github[].approve_token_env` | When the instance is in the config file and doesn't set `approve_with_review_app`: the approve App's private key, or the approve bot user's fine-grained personal access token. Required, and checked at startup. With `approve_with_review_app: true` there is none: the review App's key approves. |
| each `github[].webhook_secret_env` | When the instance is in the config file: the review App's webhook secret. Required. |
| each `gitlab[].oauth_client_id_env`, `oauth_client_secret_env` | When the instance is in the config file: its OAuth application, for signing in to the [dashboard](#-dashboard). Both or neither; checked at startup when `dashboard.url` is set. |
| each `github[].oauth_client_id_env`, `oauth_client_secret_env` | When the instance is in the config file: the review App's client ID and a client secret, for signing in to the [dashboard](#-dashboard). Both or neither; checked at startup when `dashboard.url` is set. |

pergent.sh also reads `KUBECONFIG` when it runs outside a cluster, which is handy for local development.

## ⬆️ Upgrading

### Upgrading to 0.7.0

0.7.0 reviews only what a push changed. See [Incremental reviews](#-incremental-reviews).

1. **Pushes are incremental by default**, also on repositories already running. Turn it off per repository with **Incremental reviews** in its Settings, or with `incremental_reviews: false` in the config, or change the default under **Admin → Settings**. The accepted cost: an edit that breaks an untouched file can go unnoticed until a full review (`@<bot> review`), and a team that rebases or amends gets no speed-up yet.
2. **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.5.0`.** A config that pins an older pergent keeps full reviews, because only pergent v0.5.0 and later take the files to carry over.
3. **The first push after the upgrade is a full review**, because the last result comes from an older pergent and has no skill hashes. Incremental reviews start with the next push.
4. **Migration `0020` runs at startup.** It adds nullable columns and one `runs` column that is `NOT NULL DEFAULT false`, so 0.6.2 keeps working on a migrated database.
5. **To roll back to 0.6.2, first remove `incremental_reviews` from the config**, because 0.6.2 refuses unknown config keys. Results stored by 0.7.0 are complete, merged results.

### Upgrading to 0.6.2

0.6.2 limits who can use [commands](#-commands).

1. **Commands are now limited to the change's author and the repository's maintainers and owners**, also on repositories already running. Before, anyone with write access could re-run a review or dismiss a suggestion. To let anyone with write access use them, as before, turn the setting off per repository (**Only the author and maintainers use commands** in its Settings, or `commands_author_and_maintainers_only: false` in the config), or change the default under **Admin → Settings**.
2. **Migration `0019` runs at startup.** It adds nullable columns only, so 0.6.1 keeps working on a migrated database.
3. **A change's author with only read access**, for example someone opening a PR from a fork, can now re-run and dismiss suggestions in both modes, within the re-run caps.
4. **Refusals about who may now write `command_refused` audit entries** in both modes.
5. **To roll back to 0.6.1, first remove `commands_author_and_maintainers_only` from the config**, because 0.6.1 refuses unknown config keys. Values saved under 0.6.2 stay in the database across a rollback and come back on re-upgrade.

### Upgrading to 0.6.1

0.6.1 fixes the timeout default and how Admin → Settings follows the config.

1. **The default `jobs.timeout` is 30 minutes** (it was 10). Each run may take 40 steps since 0.5.4, and 10 minutes was too short for large reviews. A config that sets `jobs.timeout` keeps its value. pergent.sh now logs a warning at startup when a timeout in the config file allows less than 30 seconds a step (timeouts set in the dashboard are not checked; see 2).
2. **Admin → Settings saved before 0.6.1 pins every default.** That includes the old 10-minute timeout. Open Admin → Settings: a default saved there shows "Saved here; the config says …". Click **Use config value** on the ones that should follow the config, then save. Saving once lets every default equal to the config's value follow it again; **Use config value** is needed only where the saved value differs, such as the old 600-second timeout.
3. **Migration `0018` runs at startup.** It adds one column with a default, so 0.6.0 keeps working on a migrated database.

### Upgrading to 0.6.0

0.6.0 posts each finding as a thread on its lines. See [Inline findings](#-inline-findings).

1. **Migration `0017` runs at startup.** It adds a table and nullable or defaulted columns only, so 0.5.4 keeps working on a migrated database.
2. **Inline findings are on by default.** Turn them off per repository with **Inline findings** in its Settings, or with `inline_findings: false` in the config, to keep the single comment.
3. **GitHub: give the review App Pull requests Read and write and subscribe it to Pull request review comment.** Without the permission, findings stay in the comment. Without the event, in-thread dismissals are not received. Each installation's owner has to accept the new permission.
4. **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.4.0`.** A config that sets its own `jobs.image` keeps the image it names.
5. **To roll back to 0.5.4, first remove `inline_findings` from the config.** 0.5.4 refuses unknown config keys and won't start with it. Threads already posted stay on the platform.

### Upgrading to 0.5.4

0.5.4 makes the step limit of a review configurable per repository.

1. **The default step limit rises from 20 to 40.** Set it with `jobs.max_turns` in the config, or per repository with `repositories[].max_turns` or **Max steps** in the repository's Settings, 5 to 200. A review that hits the step limit now says where to raise it.
2. **Migration `0016` runs at startup.** It adds nullable columns only, so 0.5.3 keeps working on a migrated database.
3. **`jobs.timeout` stays 10 minutes.** It bounds the whole review, so a repository that raises `max_turns` may also need a higher `job_timeout`.
4. **To roll back to 0.5.3, first remove `jobs.max_turns` and `repositories[].max_turns` from the config.** 0.5.3 refuses unknown config keys and won't start with them.

### Upgrading to 0.5.3

0.5.3 moves to pergent v0.3.0 and its comment layout. There is no migration.

1. **The review comment looks different.** It starts with the verdict and the confidence score, then lists the findings, blocking first. Checks and files are collapsed at the bottom.
   - Each finding links to its lines at the reviewed commit.
   - Each finding carries its own dismiss command, ``Not a real problem? Reply `@<bot> dismiss <id> <reason>` ``.
   - The footer shows ``Run again: `@<bot> review` `` and the pergent version.
2. **pergent.sh draws the comment itself** from the Job's result, for every completed review, instead of posting the comment the Job wrote.
   - A config whose `jobs.image` pins an older pergent still gets the new layout.
   - The confidence score and suggested changes show only when that image is v0.2.0 or newer.
   - A failed batch shows its error message, but no longer the last lines of opencode's error output; those stay in the Job's log.
3. **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.3.0`.**
   - Before v0.3.0, batches that started at the same moment in a new Job could crash: opencode exited before the model's first step, and the batch showed as "Review failed".
   - A config that sets its own `jobs.image` keeps the image it names. Point it at v0.3.0 to get the fix.

### Upgrading to 0.5.2

0.5.2 changes only the default `jobs.image`, to `ghcr.io/gopaytech/pergent:v0.2.0`. With that image, a read outside the repository no longer ends the review without an answer. There is no migration.

### Upgrading to 0.5.1

0.5.1 changes only the default `jobs.image`, to `ghcr.io/gopaytech/pergent:v0.1.3`. That image ships ripgrep, so the model's file search (glob and grep) works in review Jobs; with v0.1.2 every search failed and reviews fell back to reading files one by one. A config that sets its own `jobs.image` keeps using the image it names: point it at v0.1.3 or newer. There is no migration.

### Upgrading to 0.5.0

0.5.0 adds the [readiness scan](#readiness-scan). Nothing is removed, but these things change:

1. **Migration `0015` runs at startup.** It adds the `readiness_scans` table. Back up PostgreSQL first and keep the backup: 0.4.0 does not know the new table.
2. **`/readiness-scans/` must be reachable by the Jobs**, like `/runs/` ([Exposing pergent.sh](#exposing-pergentsh)). A scan Job that can't post its result ends as `the scan took longer than 5 minutes` or `the scan Job ended without a result`. The scan Jobs use the NetworkPolicy the chart already makes for review Jobs, so there is no new chart value; the clone needs the GitLab or GitHub address in `jobEgress.cidrs`, as a review does.
3. **Scans need the dashboard** (`dashboard.url`) and the `PERGENT_SH_IMAGE` variable the chart sets. Without the dashboard no scan runs.
4. **Every repository that is switched on is scanned once after the upgrade**, if it was never scanned: oldest first, 2 at a time, starting before the first reconcile. These scans are not in the audit log. Expect new "Needs attention" items for repositories that lack project context or skill coverage.
5. **Optional:** to let the scan read classic branch protection on GitHub, give the review App **Administration: Read-only** ([Create the review App](#1-create-the-review-app)). Nothing breaks without it.

### Upgrading to 0.4.0

0.4.0 adds `linux/arm64` images; nothing else changes and there is no migration. The default `jobs.image` is now `ghcr.io/gopaytech/pergent:v0.1.2`, which is also built for both platforms. A config that sets its own `jobs.image` keeps using the image it names: to run review Jobs on arm64 nodes, point it at pergent v0.1.2 or newer (v0.1.1 and older are amd64 only).

### Upgrading to 0.3.0

0.3.0 moves the skill library from the chart into the database and the dashboard. These changes break an install of 0.2.0 until you act:

1. **The chart's `skills:` value is gone**, with its ConfigMap and volume. A values file that still sets it fails to render: `skills was removed: skills live in the dashboard (see the README, "Skills and instructions")`.
2. **`skills_dir` is gone from the config.** A config that still has it is refused at startup: `skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`. Nothing is imported: the new library starts empty.
3. **A repository with no `skills` setting now uses the default skill list**, not every skill. The default list starts empty. A repository whose `skills` is `[]` in the config is refused at startup (leave the key out for the default list; a list names at least one skill).
4. **With no skills, there is no review.** A repository that has no skill in its `.pergent/skills` folder and none turned on, and whose list (or the default list) names no library skill, is skipped: no Job, no comment, no status ([No skills, no review](#no-skills-no-review)). Until the library has skills and the default list (or the repository) names one, that is every repository without `.pergent/skills`.
5. **Migration `0014` runs at startup.** It adds the library, the default skill list and the new run columns, and lets a run be `skipped`. Back up PostgreSQL first and keep the backup: 0.2.0 does not know the new tables.
6. **Skill fetch Jobs** read public URLs. The chart passes `PERGENT_SH_IMAGE` for them (set it to the pergent.sh image if you do not use the chart). `/skill-fetches/` must be reachable by the Jobs like `/runs/`, and the host of a URL must be inside `jobEgress.cidrs` ([Exposing pergent.sh](#exposing-pergentsh), [Installing from git](#installing-from-git)). Without the dashboard (`dashboard.url`) there is no skill fetching.

To upgrade:

1. Before you upgrade, save the text of every skill in your old `skills:` value, and note which repositories listed `skills`.
2. Remove `skills:` from your values and `skills_dir` from your config, and upgrade.
3. Sign in as an admin. Create the skills again under **Skills → New skill**, or install them from git ([Installing from git](#installing-from-git)), tick **Add to default skills** for the ones every repository should run, and set a repository's own list under its **Settings** where it had one. The overview's setup checklist shows what is left.

## 🦊 GitLab project settings

- **Don't treat the `pergent` commit status as a gate.** GitLab lets anyone with the Developer role set a commit status of any name on a commit of the project, including the head of their own merge request, so a `pergent` status of `success` proves nothing about who set it. Don't rely on "Pipelines must succeed" (or any other check of commit statuses) to enforce the review. The approval is the gate: it comes from the approve bot, a separate user whose token only pergent.sh holds.
- **Require approval through approval rules.** Give the default branch an approval rule that needs a human approval as well as the bot's (for example two approvals, or one from a group the approve bot is not in); see the warning under [Requirements](#-requirements). pergent.sh logs a warning every hour for a project one approval can merge.
- **Turn on "Remove all approvals when commits are added to the source branch"** (Settings > Merge requests > Approval settings). It is GitLab's counterpart of GitHub's "Dismiss stale pull request approvals": pergent.sh removes its approval on every push, but this makes GitLab do it too, as a second layer for a webhook pergent.sh missed.
- **Turn on "Prevent editing approval rules in merge requests"** (same place). Otherwise the author of a merge request can lower its required approvals to one, and the approve bot's approval alone unlocks the merge.

Approval rules and approval settings are GitLab Premium and Ultimate features. On GitLab Free, approvals are optional and don't block a merge, so pergent.sh's approval is advice only.

## 🐙 GitHub

pergent.sh reaches GitHub.com through a GitHub App that you create (pergent.sh is self-hosted, so there is no shared App). **One App is enough and is the simplest setup:**

- the **review App** receives the `pull_request`, `issue_comment` and `pull_request_review_comment` webhooks, reads the pull request and the repository, posts the comment and the finding threads, sets the `pergent` Check Run, reacts to and answers [command](#-commands) comments, looks up a commenter's repository permission and mints each Job's clone token. It can also approve, and dismiss its own approvals when they go stale (`approve_with_review_app`, the default in the dashboard). For that it needs **Pull requests: Read and write**, and the pull request shows the same bot commenting and approving.

Who approves is a choice of three:

- **the review App** (one App, as above);
- **a separate approve App**, optional, for teams that want the identity that writes the review never to be the one that approves it. It only approves, and dismisses its own approvals when they go stale. It also keeps the review App's permissions smaller: people sign in to the dashboard with the review App, and with `Pull requests: Read and write` on it their stored sign-in tokens can write pull requests too (as that person, limited by what they may do), which pergent.sh never uses;
- **a bot user's token**, for when GitHub doesn't count an App's approval toward required reviews; see [Approving as a bot user](#approving-as-a-bot-user).

### 1. Create the review App

In the organization's settings, **Developer settings → GitHub Apps → New GitHub App**:

- **Webhook:** active. URL `<public_url>/webhooks/github/<instance name>`, for example `https://pergent.example.com/webhooks/github/gh`. Secret: a random string (`openssl rand -hex 32`); you enter it as the instance's webhook secret in the dashboard, or, for an instance in the config file, put it in the Secret as the instance's `webhook_secret_env`.
- **Repository permissions:**
  - Pull requests: **Read and write** when this App approves too (the one-App setup) or posts [inline findings](#-inline-findings), else **Read-only**. With Read-only, inline findings are off for that instance: findings stay in the comment, and the Integrations page says so.
  - Contents: **Read-only**
  - Checks: **Read and write**
  - Issues: **Read and write** (pull request comments are issue comments; this also covers the 👀, 👍 and 👎 reactions on a command comment)
  - Metadata: **Read-only** (always on; this also covers reading a commenter's repository permission)
  - Administration: **Read-only** (optional). It lets pergent.sh read the classic branch protection's conversation-resolution rule for [inline findings](#-inline-findings); without it only rulesets are read, and the classic rule is treated as off. It also lets the [readiness scan](#readiness-scan) read classic branch protection. Without it, on a branch that has classic protection, 3 rows of the scan can say "Couldn't check" and name this permission; nothing else changes.
- **Subscribe to events:** **Pull request**, **Issue comment** and **Pull request review comment**. The Issue comment event delivers the comments that commands are read from, and the Pull request review comment event the replies in a finding's thread.
- **Where can this GitHub App be installed:** "Only on this account", or "Any account" to install it on several organizations.

Create it, note its **App ID** and generate a **private key** (a `.pem` file).

**An existing review App** that should also approve needs **Pull requests: Read and write**: change it under **Permissions & events**, save, and accept the new permission on each installation (GitHub asks the organization's owner to approve it). Until that is accepted, pergent.sh finds (at startup and in the hourly pass) that the App can't approve on that repository: a clean review is published as not approved, with a note that the review App lacks Pull requests: write, and the dashboard shows "Can't approve".

Changing who approves (another App, the review App, a bot user) doesn't withdraw approvals the old approver left. Turn approval off for the repositories first and wait until their approvals are withdrawn, or dismiss them by hand.

**An existing review App** needs one change to run commands: under **Permissions & events → Subscribe to events**, tick **Issue comment** and save. It asks for no new permission, as it already has the ones above. Without the event the App still reviews, but no command is ever received.

**Administration: Read** is optional too. To turn it on, open **Permissions & events → Repository permissions**, set **Administration** to **Read-only** and save; the owner of each installation then has to accept the new permission. The [readiness scan](#readiness-scan) uses it for the rows "Approvals needed to merge", "Default branch protected" and "Required checks before merge". Reviews never need it.

### 2. Create the approve App (optional)

Skip this when the review App approves. For a separate approve App, create it the same way, with:

- **Webhook:** not active (uncheck "Active").
- **Repository permissions:** Pull requests: **Read and write**; Metadata: **Read-only**.
- No events.

Note its App ID and generate a private key.

### 3. Install the App or Apps

Install the App (both Apps, with a separate approve App) on the organization, on all repositories or on the ones you will review. Installing an App switches nothing on: only the repositories listed in the config or switched on in the [dashboard](#-dashboard) are reviewed. One `github:` instance covers every organization the Apps are installed on; pergent.sh looks up the installation per repository.

### 4. Add the instance

In the dashboard, a password admin adds the instance under **Integrations → Add GitHub instance**: the API URL, the review App's ID and private key (pasted whole), **Approve with** (the review App, the default; a separate approve App, which asks for its ID and private key too; or a bot user's token), the webhook secret, and the review App's client ID and client secret if people sign in with it ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)).

To keep the instance in the config file instead, put the keys in the Secret and name them in the config:

```bash
kubectl -n pergent-sh create secret generic pergent-sh-env \
  --from-literal=DATABASE_URL='postgres://pergent:CHANGE_ME@postgres.example.com:5432/pergent?sslmode=require' \
  --from-literal=PERGENT_SH_SECRET_KEY="$(openssl rand -base64 32)" \
  --from-literal=OPENCODE_API_KEY='CHANGE_ME' \
  --from-file=GITHUB_REVIEW_APP_KEY=review-app.private-key.pem \
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
      approve_with_review_app: true
      webhook_secret_env: GITHUB_WEBHOOK_SECRET
  repositories:
    - github: gh
      project: acme/widgets
```

With a separate approve App, add its key to the Secret (`--from-file=GITHUB_APPROVE_APP_KEY=approve-app.private-key.pem`) and replace `approve_with_review_app: true` with:

```yaml
      approve_app:
        app_id: 123457
        private_key_env: GITHUB_APPROVE_APP_KEY
```

Exactly one of `approve_with_review_app`, `approve_app` and `approve_token_env` is set. `approve_app` can't name the review App's ID or key; say `approve_with_review_app: true` for that.

At startup pergent.sh reads each App's slug (`GET /app`) and its bot user (`GET /users/<slug>[bot]`), and, for an instance in the config file, stops with an error naming the instance if a key doesn't work (an instance added in the dashboard is logged and recorded as "Needs attention" instead). The bot user lookup is unauthenticated (GitHub allows 60 an hour per address), which is plenty unless the pod restarts in a loop. Then, at startup and every hour, it looks each listed repository up: the review App's installation, the repository's ID and clone URL, and whether the approve side can reach it. A listed repository the review App isn't installed on is logged as an error on every pass and can't be reviewed.

### The `pergent` Check Run

The status of a review is a Check Run named `pergent` on the reviewed head commit, created by the review App:

- `in_progress` ("pergent review running") when the Job starts;
- then `completed` with the conclusion `success`, `failure`, `neutral` (a failed review with `failure_status: neutral`) or `cancelled` (the run was superseded: a newer push or a new target branch replaced it, or the pull request was closed). A review that couldn't be posted after 10 attempts ends as `failure`, titled "pergent couldn't post its review".

Its title is the status description from [How approval works](#-how-approval-works) and its summary points to the pull request comment; the findings are in the comment and, when [inline findings](#-inline-findings) are on, in threads on the diff. Every change is a new Check Run with the same name, and GitHub shows the latest. To make merging wait for the review, add `pergent` as a required status check in the branch's protection rule or ruleset, and **pin it to the review App**: otherwise GitHub accepts a check named `pergent` from any source, and a workflow or another App could satisfy it. In a branch protection rule, set the check's source to the review App instead of "Any source"; in a ruleset, choose the review App as the check's integration. See [Branch protection](#branch-protection).

### Branch protection

- **Turn on "Dismiss stale pull request approvals when new commits are pushed".** pergent.sh dismisses its approval on every push, but this makes GitHub do it too, as a second layer for a webhook pergent.sh missed.
- **Make a human approval necessary.** If a branch needs one approval and the approver's approval counts, the bot alone unlocks the merge. Require two approvals, or a code owner review where the approver is not a code owner. At startup and every hour, for each repository the approve side can reach, pergent.sh reads the required approving review count of the default branch from its rulesets (`GET /repos/{owner}/{repo}/rules/branches/{branch}`, which the review App's metadata permission allows) and from its classic branch protection (`.../branches/{branch}/protection/required_pull_request_reviews`, which needs the Administration read permission the review App doesn't ask for), takes the higher, and logs a warning naming the repository when it is one or none: "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval". When no ruleset requires reviews and the classic rule can't be read (a 403 or 404 without the Administration permission, or any other error), it logs once at info level that it couldn't check; the check never stops a review. It doesn't check code owner reviews.
- **Require the `pergent` check from the review App only.** Add `pergent` as a required status check and set its source to the review App: in a branch protection rule, pick the review App as the check's expected source instead of "Any source"; in a ruleset, choose the review App as the integration of the `pergent` check. With any source allowed, any Actions job or App that posts a check named `pergent` can satisfy the requirement.
- If you use **"Restrict who can dismiss pull request reviews"**, allow the App that approves (or the bot user). Otherwise GitHub refuses its dismissals. pergent.sh keeps the approval on record, logs the failure and tries again after a backoff: 30 seconds after the first failure, doubling after each one, at most 5 minutes. The 5-minute approval sweep tries it too. The stale approval stays until a dismissal works or GitHub dismisses it itself.
- **Check that the approval counts.** GitHub may not count a GitHub App's approving review toward a rule's required approvals; this release has not verified it. Use a test branch whose protection requires exactly **one** approval (with more, the result is ambiguous). Open a test pull request into it, let pergent.sh approve it, and look at the merge box: if the App's approval is listed but the required approvals are still missing, switch to a bot user.

### Approving as a bot user

Use this instead of an App (the review App or a separate approve App) when your branch protection doesn't count the App's approval.

Changing who approves (another App, the review App, a bot user) doesn't withdraw approvals the old approver left. Turn approval off for the repositories first and wait until their approvals are withdrawn, or dismiss them by hand.

1. Create a GitHub user for the bot and give it **Write** access to the reviewed repositories (GitHub counts approvals only from people with write access).
2. As that user, create a **fine-grained personal access token**: resource owner the organization, repository access the reviewed repositories, repository permission **Pull requests: Read and write** (Metadata read-only is included). If the organization requires approval of fine-grained tokens, approve it.
3. Put it in the Secret (for example `GITHUB_APPROVE_TOKEN`) and use `approve_token_env` instead of `approve_with_review_app` or `approve_app`:

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

### When the approve App isn't installed, or can't approve, on a repository

With a separate approve App, reviews still run and are posted with their comment and Check Run. A clean review is not approved: the comment says `**Not approved:** the pergent approve App isn't installed on this repository or lacks Pull requests: write.` (for the review App: `the pergent review App lacks Pull requests: write on this repository`) and the Check Run title says why. This is not retried. pergent.sh notices an installation at startup and in the hourly pass; from then on the next push or ready-for-review is approved. There is no catch-up pass over pull requests that were already reviewed. When the review App approves there is no separate installation to miss; what can be missing is its **Pull requests: Read and write** permission, which gets the same treatment. The hourly pass reads the installation's permissions, so the note goes away after the permission is accepted.

### Rotating an App's private key

A GitHub App can have several private keys at once:

1. In the App's settings, generate a new private key.
2. For an instance added in the dashboard, paste the new `.pem` file into its private key field under **Integrations**: it is checked before it is saved and used at once, with no restart. For an instance in the config file, update the Secret with the new `.pem` file and restart pergent.sh (`kubectl -n pergent-sh rollout restart deployment/pergent-sh`), then check that it started.
3. Delete the old key in the App's settings.

Installation tokens minted with the old key expire within an hour.

## 📊 Dashboard

The dashboard is a web app built into pergent.sh. It is off until `dashboard.url` is set. It has two kinds of account:

- **Password accounts** are the operators. The first is `admin`. They see every repository, and alone manage password accounts, instances and their secrets, OAuth clients and the LLM.
- **Platform accounts** are made the first time someone signs in with their GitLab or GitHub account. They see no repository until an admin puts them on a [team](#teams), and then only the repositories that team covers and that they can see on that platform.

With the dashboard on, every review run, its findings, its files and its log are there, and admins switch repositories on and off and change their settings.

### Turning it on

1. Pick the dashboard's address, for example `https://pergent-dashboard.example.com`. It must be `https` and a bare host, without a path: the dashboard, its API (`/api/`) and its sign-in routes (`/auth/`) are served at `/` of that host, and its cookies only work over https. It can be an internal-only host. Webhooks and Job results stay on `public_url`, which GitHub.com must still reach from the internet.
2. Set it in the config, and route that host's `/`, `/api/` and `/auth/` to the pergent.sh Service ([Exposing pergent.sh](#exposing-pergentsh)):

   ```yaml
   config:
     dashboard:
       url: https://pergent-dashboard.example.com
   ```

   Routing all of `/` there is fine: webhooks and results on that host still need a valid signature or result token, and `/healthz` only answers `ok`. Without `dashboard.url` pergent.sh doesn't serve the dashboard's routes at all.
3. [Sign in](#first-sign-in) as `admin`, add your instances under **Integrations** ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)), and set up sign-in with GitLab or GitHub on each instance people should use: [a GitLab OAuth application](#sign-in-with-gitlab) or [user authorization on the GitHub review App](#sign-in-with-github).

With `dashboard.url` set, pergent.sh starts with no instance at all and waits for one to be added in the dashboard. For an instance in the config file, `oauth_client_id_env` and `oauth_client_secret_env` (see [Config reference](#-config-reference)) turn on its sign-in; pergent.sh doesn't start when an instance names one of them and that variable is unset or empty. The error names the instance and the variable.

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

(`deploy/pergent-sh` is the Deployment of a release called `pergent-sh`, as in [Install with Helm](#-install-with-helm). The chart names the Deployment `<release>-pergent-sh`, or just `<release>` when the release name contains `pergent-sh`; `kubectl -n pergent-sh get deploy` shows it.) The command needs only `DATABASE_URL`, which the pod's environment has. It prints `new password for admin: ...` on stdout (never in the JSON log), clears the account's lock and ends its sessions, and the account must change that password at its next sign-in. The reset is recorded in the audit log with the actor `cli`.

### Password sign-in limits

- A wrong username or password always gets the same answer, `wrong username or password` (401), after the same work, so a guess doesn't tell whether the username exists.
- Five wrong passwords in a row lock that username for 15 minutes. A wrong current password at a password change counts the same.
- Each client IP may make 20 sign-in or password-change attempts in 5 minutes; further ones get 429 (`too many sign-in attempts, try again later`). The client IP is the **last** `X-Forwarded-For` entry, and the connection's own address when the header is absent. The proxy in front of pergent.sh **must set `X-Forwarded-For`** to the address it saw: when nothing does, a client sends its own header and can change address on every attempt, so the per-IP limit can be bypassed. The per-username lock holds either way. An L7 load balancer in front of that proxy that doesn't forward the client address makes every client share one address, and so one limit of 20 attempts in 5 minutes.
- Anyone who knows a username can keep it locked, with 5 wrong guesses every 15 minutes. `admin` is the one everybody knows. Add a second password admin with an unguessable username and remove `admin`, or restrict `/auth/password` at your proxy to the addresses your operators sign in from.
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
  | `viewer` | Read the covered repositories: reviews, runs, findings, files, logs and settings. Everyone with a team also reads the skill library. |
  | `maintainer` | Also change the settings of the covered repositories (approve clean reviews, review drafts, failure status, who uses commands, the skills a repository runs and the repository skills it turns on, Job timeout and memory, Max steps; not the re-run caps), and reset them to the defaults (the caps stay as they are). Press the review page's buttons: re-run, dismiss and restore ([Commands](#-commands)). Not switch repositories on or off, and not change teams. |
  | `admin` | Also change the re-run caps of the covered repositories, switch repositories on and off (**Add repository**), change the defaults on **Settings** (the caps and the default skill list among them), write, install, update, copy and delete the skills of the library, manage **Users** and **Teams**, and read the **Audit log**. An admin team covers every repository switched on for the instance its members signed in with that they can see on their platform. |

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

- **Overview:** the number of reviews, approvals, blocking reviews and failed reviews over the last 7, 30 or 90 days, the most recent reviews, and "Needs attention". Admins also see a setup checklist (the model and its key, a connected instance, a skill in the library, a repository switched on); each item links to its page, ticks when done, and the list goes away when all four are done.
- **Reviews:** filtered by repository, result and time. Each merge request has a page with every run (commit, what started it, result, duration) and, for a run, the status set on the platform, whether it was approved and why not, the pergent version, model, what an incremental review covered, tokens, each skill's verdict, findings (with the ones people dismissed, who dismissed them and why), checks, files (reviewed, skipped with the rule, not reviewed) and the log. Each skill shows where it came from: `library v3` (the library version the review used), or `repo (.claude/skills)` for a skill read from the repository. A run skipped because the repository has no skills shows as **Not reviewed** (result `skipped`, with its own filter), not as superseded, and is not counted as a review or a failure. Maintainers and admins of the repository also get the buttons **Re-run review** and **Re-run failed only**, and **Dismiss** and **Restore** on findings ([Commands](#-commands)).
- **Repositories:** each with its last 7 days and health; its reviews, its readiness scan, its setup checks and its skills and instructions. The [Readiness](#readiness-scan) tab is the checklist of what the repository gives a review. Skills & instructions shows what a review of the default branch would get, read with the review bot or App and kept for 5 minutes: each skill with its library version or repository folder, a link to its file for a repository skill, and the problem a review would fail on. A repository's **Settings** choose between "Use the default skills (a, b)" and "Choose skills", with a running total against the 900 KiB a review takes, and list the skills found in `.agents/skills`, `.claude/skills` and `.github/skills` with switches, off at first (`.pergent/skills` is always on; only the `SKILL.md` text is used). **Add repository** warns when the default skill list is empty.
- **Skills:** the skill library: each skill's name, version, where it came from (written in the dashboard, or the repository and branch or tag it was installed from), whether it is on the default list and when it changed, with its text in a plain-text box (never rendered as HTML or markdown) and its saved versions. A skill installed from a private repository is shown to everyone with a team, so installing is sharing; the repository's path is shown only to people who can see that repository, and others see `a private repository on <instance>`. Who saved each version is shown to admins only. See [Skills and instructions](#-skills-and-instructions).

**Admins** also get:

- the on/off switch of each repository and **Add repository**, which lists the repositories that both the admin (GitLab: Reporter or above as a member, a shared group's member included; GitHub: read access) and the review bot (GitLab: Maintainer) or review App (GitHub: installed) can reach, at most 500, with a search. A password admin has no platform account, so it sees what the review bot or App can reach;
- each repository's **Settings**: approve clean reviews, the failure status, review drafts, who uses commands (**Only the author and maintainers use commands**, on by default), **Incremental reviews** (on by default), the library skills to run, the Job's timeout (60 to 7200 seconds) and memory limit (from 256Mi, and not below `jobs.resources.requests.memory`, to 32Gi), Max steps (5 to 200: the steps each opencode run may take before pergent stops it), and the re-run caps per merge request and per repository per day (1 to 1000; only an admin of the repository changes these two). Each shows "overridden here" or "default", and "Reset to defaults" clears every override (a maintainer's reset leaves the caps);
- on **Skills**, the buttons **New skill**, **Install from git** and **Check for updates**, and per skill Edit (a written skill only), Copy and Delete ([Skills and instructions](#-skills-and-instructions));
- **Users**, **Teams** and the **Audit log**;
- **Integrations** (their own instance: connection, approver, each repository's setup and whether the approver reaches it, an approve token about to expire), read-only for platform admins, and **Settings**: the defaults a repository switched on in the dashboard gets (approve clean reviews, review drafts, failure status, **Only the author and maintainers use commands**, **Incremental reviews**, Job timeout and memory, Max steps, and the re-run caps per merge request and per repository; the config's repositories keep the config's own; each shows what the config gives and follows it unless saved; **Use config value** drops a saved one), and the LLM, which only password admins change.

Every write is checked on the server: it needs a session, the CSRF token, a role that allows it and a repository the user can see. A viewer calling the API directly gets 403 and nothing changes.

### Instances and secrets in the dashboard

Password admins add GitLab and GitHub instances, with their secrets, under **Admin → Integrations**, and set the LLM under **Admin → Settings**. Nobody else can: a platform admin sees its own instance there, read-only.

- **Adding a GitLab instance:** a name (`[a-z0-9-]`, at most 63 characters, part of the webhook URL), the instance's `https` URL without a trailing slash, the review token and the approve token, and optionally an OAuth application ID and secret for [sign-in](#sign-in-with-gitlab) (both or neither). The webhook secret is generated by pergent.sh, since pergent.sh creates the hooks itself.
- **Adding a GitHub instance:** a name, the API URL, the review App's ID and private key, its webhook secret (the one you set in the App), **Approve with**: the review App itself (the default, one App), a separate approve App (its ID and private key) or a bot user's token, and optionally the review App's client ID and a client secret for [sign-in](#sign-in-with-github). A private key is pasted as it is in the `.pem` file; the browser's one-line field loses its line breaks, and the dashboard puts them back.
- **Secrets are write-only.** A value is sent once and never comes back: not in a page, an API answer, a log, an audit entry or the browser's storage. The page shows only "set" or "not set" and when the secret last changed. Replacing a secret doesn't need the old value.
- **Checked before saving.** A save runs the checks startup runs for an instance in the config (the two bot tokens belong to different users, the App key signs a request GitHub accepts, ...) and is refused with the reason when one fails; nothing is stored then. A saved instance is used at once, without a restart.
- **Address and App IDs can't change.** The URL, API URL and App IDs are fixed once the instance is added (`an instance's address and App IDs can't change: remove it and add it again`); secrets and the OAuth client ID can be replaced at any time. Changing who approves (another App, the review App, a bot user) doesn't withdraw approvals the old approver left. Turn approval off for the repositories first and wait until their approvals are withdrawn, or dismiss them by hand.
- **Removing an instance** asks for confirmation and answers `removing`. Its repositories are switched off at once (queued runs are superseded), its users' sessions end and their accounts and team memberships are deleted. In the background pergent.sh then takes its webhooks and deploy tokens off the platform and deletes the instance and its secrets, within a minute or so; the page shows "removing" until then. Its runs, findings and logs stay in the database: adding an instance of the same name again attaches them to it, and password admins see them. When the platform keeps refusing that cleanup, a "Needs attention" item says so (`pergent.sh could not take N project(s) of the instance "name", which is being removed, off its platform; it tries again at the next reconcile`) and the removal waits for the next reconcile.
- **An instance that can't connect** (at startup, or right after it was saved) doesn't stop pergent.sh, so the dashboard where it is fixed stays up. pergent.sh logs the error, and an instance-wide "Needs attention" item (kind `instance_unreachable`) shows it to the operator and to that instance's admins; the item goes when the instance connects. Its webhooks are still accepted and the events queued, and reviews of its repositories wait queued (the log says `queued reviews wait: pergent.sh isn't connected to their instance; an operator fixes it on Admin → Integrations`) until it is saved again in **Integrations**. An instance in the config file that can't connect still stops startup.
- **The config file wins.** Instances in the config are listed as "config" and are read-only (409 `this instance is managed by the config file; change it there`), and so are the secrets they name by environment variable. An instance in the dashboard whose name is also in the config stops startup, naming both. Repositories in the config name instances in the config only; repositories of an instance added in the dashboard are switched on in the dashboard.
- **The LLM** (provider, model, base URL and API key) is set in the dashboard by password admins. The provider is picked from a list (Anthropic, OpenAI, Google, DeepSeek, OpenRouter, Groq, Mistral, xAI) or typed as any other opencode provider ID under **Other**; with a base URL, that can be any OpenAI-compatible server. The LLM is set there unless the config has an `llm:` block, which wins and makes it read-only (409 `the LLM is set in the config file; change it there`). The key is needed only when none is stored. With no LLM set, reviews wait queued, and the log says `no LLM is set: an operator sets it on Admin → Settings`.
- **Defaults.** The defaults on **Admin → Settings** (approve clean reviews, review drafts, failure status, **Only the author and maintainers use commands** (on until set), **Incremental reviews** (on until set), Job timeout, Job memory, Max steps (40 until set) and the re-run caps, 10 per merge request and 50 per repository a day until set, and the default skill list, picked among the library's skills with a running total against 900 KiB) apply to repositories switched on in the dashboard only; repositories in the config keep the config's own. Under each default the form says what the config gives (`jobs.timeout`, `jobs.max_turns`, `jobs.resources.limits.memory` and the built-in values). A default you leave at the config's value keeps following it, so a later change in the config reaches the dashboard's repositories; one you change is saved here, shows "Saved here; the config says …" and stays until you press **Use config value** and save. The Job image, concurrency, `max_running` and resources stay in the config and the chart.
- **`PERGENT_SH_SECRET_KEY`.** Secrets are stored encrypted with it. Changing it isn't supported and there is no rotation. A stored secret that can't be opened with the key shows as "not set" (startup logs which instance and fields, `secrets of an instance couldn't be opened with PERGENT_SH_SECRET_KEY; they count as not set`), and has to be entered again.

### Repositories in the config and in the dashboard

- A repository listed in `repositories` is shown as **managed by config** and is read-only in the dashboard; a write to it gets 409. Every dashboard setting has a config key (`approve`, `failure_status`, `review_drafts`, `inline_findings`, `incremental_reviews`, `skills`, `repo_skills`, `job_timeout`, `job_memory`, `max_turns`, `rerun_limit_per_mr`, `rerun_limit_per_repo`, `commands_author_and_maintainers_only`; see [Config reference](#-config-reference)).
- A repository switched on in the dashboard is stored in Postgres. pergent.sh sets it up (webhook and deploy token on GitLab) the same way as a config repository, in a reconcile of that repository alone it starts at once (switches made within 10 seconds of each other are handled together, at the end of those 10 seconds); switching it off removes them the same way. The hourly reconcile still covers every repository. Users whose teams cover it (an admin team covers every repository of its instance) see a newly switched-on repository from their next request, once the platform has confirmed they can see it.
- A repository can't be in both: when it is added to the config, the next reconcile deletes its dashboard record and the config's settings apply. Removing a repository from the config switches it off; it doesn't fall back to the dashboard. To hand a repository to the dashboard, remove it from the config, redeploy, then switch it on in the dashboard.

### Readiness scan

A review does better when the repository gives it something to work with. The **Readiness** tab of a repository (after Skills & instructions) shows what is missing and the exact fix. It only advises: it never changes a repository setting, a file or a branch, it opens no merge request, and it does not use the LLM.

**Who.** Everyone who can see the repository sees the tab; the same rule as the review list ([Who sees what](#who-sees-what)). Maintainers and admins of the repository press **Scan now**, repositories in the config included. A repository in the config is read-only: instead of the **Use these skills** button the tab shows the `skills` line to paste into the config file.

**When a scan runs.**

- When a repository is switched on, after its first reconcile. For a repository in the config, it is a reconcile that sets it up and finds no scan of it.
- On **Scan now**. At most 1 scan is queued or running per repository: a second request gets 409 `a scan is already queued or running` with that scan. A repository can be scanned on request once every 5 minutes, counted from when the latest scan of any kind was queued: sooner than that is 429 `this repository was scanned less than 5 minutes ago; try again in N minutes` (`1 minute` for 1), with `Retry-After` in seconds. Only a Scan now that was accepted is in the [audit log](#audit-log) (`readiness_scan_requested`).
- Once after the upgrade, for each switched-on repository that was never scanned ([Upgrading to 0.5.0](#upgrading-to-050)).
- Never on a schedule and never after a push. The tab says how old the scan is and who started it ("Scanned 3 days ago, by alice (gitlab main), at commit abcdef1"). A repository that is not scanned yet shows "Not scanned yet". A scan that was lost (pergent.sh restarted, the Job was evicted) is not retried: press **Scan now**.
- Scans run only with the dashboard on. A scan reads the default branch at one commit.

**The checks.** Each item is **Passed**, **Warning**, **Missing** or **Couldn't check**. Couldn't check is never shown as passed, and it never raises a "Needs attention" item. Each item that is not passed says why it matters to a review and what to do.

| Check | Item | Passed when | Otherwise |
| --- | --- | --- | --- |
| Project context | Agent context file | `AGENTS.md` or `CLAUDE.md` at the root, not empty | Missing |
| | README | a README at the root | Missing |
| | Contributing guide | `CONTRIBUTING.md` at the root, in `.github/` or in `docs/` | Warning |
| | Review instructions | `.pergent/instructions.md` | Warning |
| Ownership and process | CODEOWNERS | a file at the root, in `.github/`, `docs/` or `.gitlab/` with at least one rule | Warning |
| | Approvals needed to merge | 2 or more | Warning for 1 or none |
| | Default branch protected | protected against direct pushes | Missing when not protected; Warning when direct pushes still get in (GitLab: Developers can push; GitHub: classic protection that stops no push) |
| | Required checks before merge | GitHub: at least one required status check. GitLab: "Pipelines must succeed" is on | Warning |
| Skill fit | Skills that match files | at least one skill turned on for the repository matches a file (a skill with no `paths` matches every repository) | Missing |
| | Assigned skills that match nothing | none | Warning, naming them |
| | Languages no skill covers | none above 10% of the source files | Warning, naming them |
| | Suggested skills | not a check: library skills that are not turned on and whose `paths` match files | none |
| Size and noise | Generated, vendored and very large files | no file a review would read that looks like noise | Warning |
| | Skills plus instructions size | 900 KiB or less | Missing |

- **Skill fit is rule-based.** The scan uses the matcher a review uses (pergent's exported `scope` package), so "matches a file" and the suggestions are what a review would pick. Languages are counted by file extension in the scan Job, which skips the files a review skips.
- **Noise is what a review would read.** A review already skips lockfiles, `vendor/`, `node_modules/`, `third_party/`, paths set `linguist-generated` in `.gitattributes` and files with a "Code generated ... DO NOT EDIT." header. The scan lists only other files that look like noise: a generated or minified name (`*.pb.go`, `*_generated.*`, `*.min.js`), build output (`dist/`, `build/`), or a file larger than 512 KiB. The tab lists at most 50, largest first, and counts the rest. The fix shown is a `.gitattributes` line with `linguist-generated` (a review honours that; it ignores `linguist-vendored`) or, per skill that would read the files, an `exclude:` block to paste into its frontmatter. A path with whitespace, a quote, a backslash or a glob character (`[`, `*`, `?`) gets no `.gitattributes` line, because a review would not read it as that path; its `exclude` entry escapes the glob characters so it matches only that file.
- **Size.** The scan Job adds up the repository's own skills and `.pergent/instructions.md`; pergent.sh adds the library skills turned on now. The tab shows the size as it is now.

**What the settings rows read.** The Job reads the files. The approvals, branch protection and required checks rows are read by the pergent.sh pod with the review bot or App, when the scan starts, and are stored at once, so they show even when the Job fails. They need no new permission, except the optional one on GitHub. A read that is refused or fails is **Couldn't check** with the reason; the platform's own error text goes to the log only.

| Platform | Row | What is read | Couldn't check says |
| --- | --- | --- | --- |
| GitLab (the review bot, Maintainer) | Approvals needed to merge | The approval rules that apply to the default branch, as the hourly check reads them (with none, the project's `approvals_before_merge`). | `the review bot can't read the approval rules here` |
| | Default branch protected | Every protected branch rule that matches the default branch, by exact name and by wildcard. None is Missing (`main is not protected`). Any matching rule that lets Developers push is a Warning (`Developers can push to main directly`), because GitLab applies the most permissive. | `the review bot can't read branch protection here` |
| | Required checks before merge | The project's "Pipelines must succeed" setting. | `GitLab didn't show whether pipelines must succeed` |
| GitHub (the review App) | Approvals needed to merge | The default branch's rulesets (`rules/branches/{branch}`) and its classic branch protection (needs the optional **Administration: Read**). A branch with no ruleset and no classic protection is a Warning (no approval needed), not Couldn't check. | `the review App can't read this branch's rules`; or, when no ruleset requires reviews and the branch has classic protection the App can't read, `no ruleset requires reviews, and the review App can't read the classic branch protection (it needs the optional Administration: Read permission)`; or, when the approve side can't approve on the repository, its note: the approve App isn't installed or lacks Pull requests: write, the review App (when it approves) lacks Pull requests: write, or the approve bot can't reach the repository |
| | Default branch protected | A ruleset with a pull request or update rule, or classic protection that stops a direct push: it requires pull requests, restricts who can push, locks the branch, or requires status checks. Required checks the branch shows count as they are; the rest is read from `branches/{branch}/protection`, which needs the optional **Administration: Read**. GitHub's `protected` flag alone is not enough. Classic protection that stops no push (it only blocks force pushes or deletion, say) is a Warning (`main has protection but accepts direct pushes`). | `the review App can't read this branch's rules`; or, when GitHub calls the branch protected, no ruleset stops pushes and the App can't read the classic protection, `the review App can't read the classic branch protection (it needs the optional Administration: Read permission)` |
| | Required checks before merge | A ruleset with required status checks, or required checks the branch shows. A branch with no ruleset and no protection is a Warning (`no status checks are required`). A protected branch that shows no required check may be hiding them, so it is not a Warning. | `the review App can't see this branch's required checks`; for a protected branch with none shown, the same with `(it needs the optional Administration: Read permission)` |

The three rows that **Administration: Read** unlocks on GitHub are **Approvals needed to merge**, **Default branch protected** and **Required checks before merge**. When the hourly check has raised "one approval is enough to merge" for the repository, the approvals row shows a Warning whatever the scan read, because the hourly check is newer.

**What the Job reads.** A scan Job clones the default branch at the commit the pod resolved, with depth 1, over https only, with symbolic links not followed and hooks off, and runs `pergent-sh scan-repo` over it ([What a Job can and cannot see](#-what-a-job-can-and-cannot-see)). It is told each library skill's `paths` and `exclude` globs, never a skill's text, and reads the repository's own skill folders from the clone. It posts counts, matches, the presence of the context files and at most 50 noise paths. File contents never leave the Job. The pod treats everything it posts as untrusted text: one line, at most 500 bytes, and a text that holds a secret pergent.sh holds is withheld, or the result refused.

**Limits of a scan.** These are fixed in this version and are not chart values:

| Limit | Value |
| --- | --- |
| Time per scan Job | 5 minutes, clone and scan together |
| Disk | 2 GiB (`emptyDir`) |
| CPU and memory | 100m CPU and 128Mi requested, 512Mi memory limit |
| Scan Jobs at once | 2. They don't count toward `jobs.max_running`, so a review never waits for a scan |
| Largest result the Job posts | 1 MiB |
| Noise listed | 50 paths |
| Noise size | over 512 KiB |

**When a scan fails.** The settings rows still show. The file rows say `couldn't scan files: <reason>`, and the top line says `Last scan failed N ago: <reason>`. A failed scan changes no "Needs attention" item. The reasons:

| Reason | Meaning |
| --- | --- |
| `the repository has no default branch yet` | An empty repository. |
| `pergent.sh hasn't set this repository up yet` | The reconcile has not set it up (or the review App is no longer installed on it). |
| `pergent.sh has no clone token for this repository yet` | No deploy token (GitLab) or the App could not mint one (GitHub). |
| `couldn't read the default branch` | The platform refused or failed, or the instance is not connected. |
| `couldn't read the repository's settings; see the pergent.sh log` | The pod part failed on its own side. |
| `readiness scans need PERGENT_SH_IMAGE (set by the chart)` | The variable is unset. The scan stops before it reads anything, so the settings rows say they couldn't be checked too. |
| `couldn't start the scan Job; see the pergent.sh log` | The cluster refused the Job. |
| `the repository is too large to clone (the scan has 2 GiB of disk)` | The clone filled the disk. The settings rows still show. |
| `the scan ran out of memory` | The Job went over 512Mi. |
| `the scan took longer than 5 minutes` | The Job hit its deadline, or never posted. |
| `the scan Job ended without a result` | The Job ended, or is gone, with no result. |
| `the scan stopped before its Job started; scan again` | pergent.sh stopped between the pod part and the Job. |
| `the scan Job sent a result pergent.sh couldn't use`, `the scan Job sent a result over 1 MiB` | A result of another version or commit, a name that isn't plain, or too big. |
| `the scan Job couldn't scan the files` | The Job's own error text was empty or held a secret. |

A repository keeps its latest finished scan, and one that is queued or running. When the latest failed, the last done scan is kept beside it: the tab shows the failed one, so the file rows say they couldn't be scanned until a scan succeeds, and the items are still re-judged from the done one.

**"Needs attention".** A done scan raises or clears 3 items, on the overview and under the repository's **Setup checks** tab; each opens the Readiness tab ([Logs and "Needs attention"](#logs-and-needs-attention)).

**Use these skills.** The suggestions come with a box per skill and a **Use these skills** button for maintainers and admins. It saves the repository's skill list plus the ticked skills through the normal repository settings save, so the same 900 KiB check, audit entry and permission rules apply. A repository that follows the default list asks first (`This repository will stop following the default skill list.`) and saves today's default list plus the ticked skills as its own. Saving skills, here or in **Settings**, and saving the default list, re-judge skill fit and the 900 KiB total from the last done scan at once, with no new Job, so the "no skill covers the repository" item goes as soon as a skill matches. Editing a library skill's text or updating it from its source re-judges every repository the same way, and so does each start of pergent.sh for the repositories in the config file, whose skills change only with a restart.

Switching a repository off deletes its scans and drops its items.

### Audit log

Admins read the audit log under **Admin → Audit log**: who did what and when, for every change made in the dashboard. Each entry names its actor as `alice (gitlab main)` (username, platform and instance), `root (password)` for a password admin or `cli` for a reset from the command line. It records sign-ins (GitLab, GitHub and password), failed password sign-ins and lockouts, password changes and resets, accounts, teams (the members added and removed by username, and the role before and after), instances and which of their fields changed, the LLM and the defaults (which fields; the default skill list shows as `skills`), the skill library (`skill_created`, `skill_updated`, `skill_installed`, `skill_source_updated`, `skill_copied` and `skill_deleted`, each naming the skill, with its version or the skill it was copied from, and for an install or an update the source (a private repository by its instance only), the branch or tag and the short commit; a skill's text is never in an entry), repositories switched on and off with their settings changed or reset, `readiness_scan_requested` (a **Scan now** that was accepted; the scans started by switch-on or the upgrade are not audited), and the review actions: `review_rerun`, `review_rerun_failed`, `finding_dismissed` and `finding_restored`, from a comment or a button, each with the merge request, the head commit and, for a finding, its fingerprint and the reason given, and `command_refused`, a comment refused because of who wrote it ([Who may do what](#who-may-do-what)). It is kept for 90 days and never holds a secret value: an entry for a secret names the field, not its content. A platform admin doesn't see the entries about repositories it can't see, about other instances, or the password admins' own sign-ins and passwords.

### Logs and "Needs attention"

- **Logs.** When a review Job ends, or before the Job of a superseded run is deleted, pergent.sh keeps the last 200 KB of the pergent container's log, with the Job's clone token, LLM key and result token replaced by `[REDACTED]`. Logs are deleted after 30 days. A Job that is gone before its log is read (Jobs are removed an hour after they finish) has no log.
- **Needs attention** lists setup problems that pergent.sh detects, kept up to date by the hourly reconcile, the Job watcher and the readiness scan (not computed when the page loads):
  - one approval is enough to merge, so pergent's approval alone can unlock it;
  - the GitHub approve App isn't installed on the repository or lacks Pull requests: write (for the review App: it lacks that permission), or the approve bot user can't reach it;
  - reviews running out of memory (two or more in the last 7 days);
  - the GitHub approve bot's token expires within 14 days. This one concerns the whole instance and is shown to its admins and the password admins;
  - a run failed because a skill it needs is missing (`skill "x" is not in the skill library`, or `skill "x" is turned on but not found in the repository`), or was skipped because the repository has no skills (`this repository has no skills`). Either goes when a later run of the repository starts a Job ([Skills and instructions](#-skills-and-instructions));
  - the [readiness scan](#readiness-scan) found that no skill the repository uses matches its files (`no_skill_coverage`: `No skill this repository uses matches its files, so its reviews check nothing useful. The Readiness tab suggests skills that fit.`). It goes when a scan finds a match, or when the skills change and the last scan shows one;
  - skills plus instructions are over 900 KiB, so every review fails (`inputs_over_budget`: `Skills and instructions come to N KiB, over the 900 KiB a review can take, so every review fails. Use fewer skills or shorten the instructions.`). It goes when the size is back under, at a scan or when the repository's skills or the default list are saved. When a size isn't known (a skill with a problem, or one removed from the library) the item is left as it is;
  - the repository has no `AGENTS.md`, `CLAUDE.md` or README (`no_project_context`: `The repository has no AGENTS.md, CLAUDE.md or README, so reviews don't know what the project is. Add one.`). It goes when a scan finds one. A failed scan changes none of these 3 items;
  - an instance added in the dashboard couldn't be connected (`instance_unreachable`; it goes when the instance connects), or its removal is stuck because the platform refuses to give up its webhooks ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)). These concern the whole instance too: they are shown to the password admins and to that instance's admins.

### Limits of the access check

Each signed-in platform user costs one platform API call per repository their teams cover (an admin team covers every repository switched on for their instance), at most once every 5 minutes while they use the dashboard, plus one to read the user. A waiting user, with no team, costs that one call at sign-in and none after. Password admins cost none. On GitLab a repository whose own permissions show less than Reporter (an internal project, or one reached through a share) costs a second call. GitHub allows 5000 requests an hour per user, so a very large installation (several hundred repositories on one GitHub instance) can run a user into GitHub's rate limit; their check then fails as described above.

A sign-in, and each later access check, must finish within 30 seconds in all: the code exchange and reading the user, then the covered repositories, 8 at a time, each call with its own 10 second timeout. The repositories take about (number of repositories ÷ 8) × the platform's response time, so a user whose teams cover more than roughly 8 × 30 s ÷ that response time repositories can't be checked in time: about 900 at 250 ms a call, 2,400 at 100 ms, and half that on GitLab where most repositories need the second call. Past that, every sign-in of such a user fails with "Could not check which repositories you can see", and sessions already signed in see nothing once their last good check is 10 minutes old. The 30 seconds can't be configured: keep the repositories a team covers below that number (use several teams for different people), or split them across instances, and run pergent.sh close to GitLab or GitHub so each call is quick.

## 🧩 Skills and instructions

A review runs a set of **skills**: each is a markdown text (with optional frontmatter) that tells the model what to look for. pergent.sh builds the set for every review from three sources:

| Source | Where it lives | Who changes it |
| --- | --- | --- |
| **The skill library** | In pergent.sh's database. | Admins, in the dashboard: written there, or installed from a git repository. |
| **Repository skills** | In the reviewed repository: `.pergent/skills/` always runs; `.agents/skills/`, `.claude/skills/` and `.github/skills/` run only when a maintainer or admin turns a skill on. | The repository's own merge requests. |
| **Instructions** | The root `AGENTS.md` (or `CLAUDE.md` when there is no `AGENTS.md`) plus `.pergent/instructions.md`. | The repository's own merge requests. |

Skills are not in the Helm values or the config file. The chart's `skills:` value and the config's `skills_dir` were removed ([Upgrading to 0.3.0](#upgrading-to-030)).

### How a review picks its skills

1. **Library skills.** The repository's `skills` list names them. A repository that sets none (`null`) uses the **default skill list**, which admins set under **Admin → Settings**. A repository's own list is never empty: it names at least one skill, or it is unset and uses the default list.
2. **Repository skills.** pergent.sh reads `.pergent/skills/` from the target branch, and from `.agents/skills/`, `.claude/skills/` and `.github/skills/` only the skills the repository turned on (its `repoSkills` setting, or `repo_skills` in the config). A skill is a `*.md` file or a `<name>/SKILL.md` folder; of a folder only the `SKILL.md` text is used, and its other files are ignored.
3. **One set.** The skills are merged by name. When a name is in several places, the first of these wins: `.pergent/skills`, `.agents/skills`, `.claude/skills`, `.github/skills`, then the library. A skill in one of the last three folders that is not turned on is ignored and replaces nothing, even when it is malformed.

The library names are checked first, so a name the repository lists that is not in the library fails the run even when the repository has its own skill of that name.

Repository skills and instructions are read from the target branch at the merge request's `diff_refs.start_sha` (on GitHub, the pull request's `base.sha`), not from the merge request's own branch, so a merge request cannot change the rules it is reviewed by. The library is read when the review starts, so an edit made while a run waits is what reviews, and the library and the default list are read together: a review gets the old set or the new set, never a mix. Each run records where every skill came from and the library version of each library skill it used. The run page shows `library v3` or `repo (.claude/skills)` per skill.

| Case | What happens |
| --- | --- |
| A named library skill is not in the library | The run fails: `skill "x" is not in the skill library`. The repository is listed under "Needs attention". Nothing falls back to other skills, so a review is never approved with fewer or other skills than the repository asked for. |
| A repository skill that was turned on is gone from the target branch | The run fails: `skill "x" is turned on but not found in the repository`. |
| A repository skill that runs is malformed (one in `.pergent/skills`, or a turned-on one in another folder: its frontmatter doesn't parse, its name is invalid or taken twice in the folder, or it is over 128 KiB) | The run fails, and the message names the file and the problem, as `<path>: <problem>`. A skill that is turned off is ignored even when malformed. |
| A config repository names a skill the library lacks | Startup does not stop, so the dashboard stays up to fix it. The repository's runs fail with the message above, and "Needs attention" says why, until the skill exists. |
| The skills and instructions are over 900 KiB together | The previous review is dropped first. If the skills and instructions alone are too large, the run fails with a comment and no Job starts. |
| The repository has no skills at all | See [No skills, no review](#no-skills-no-review). |

### The skill library

Everyone with a team can read the library (**Skills** in the dashboard). Any admin, password admin or member of an admin team, writes it; maintainers and viewers only read. Every change applies to the next review, with no restart.

- **Write** a skill with **New skill**. The name is fixed after it is created and matches `[A-Za-z0-9][A-Za-z0-9._-]*`; a frontmatter `name`, when present, must equal it. The text is at most 128 KiB, its frontmatter must parse (`paths` and `exclude` are lists of strings, `cross_batch` is true or false; other fields are ignored), and it must not contain a secret pergent.sh holds (a token, a key, a webhook or OAuth secret, the LLM key): the save is refused with `the skill contains a secret pergent.sh holds`, naming no value. The library holds at most 200 skills.
- **Add to default skills** is a box on New skill and on Install, off at first, so a new skill reaches no repository until an admin chooses it.
- **Edit** works on a written skill. A save carries the version you opened: if another admin saved meanwhile, it is refused with 409 `this skill changed since you opened it`. An installed skill can't be edited (409 `an installed skill can't be edited; copy it`): **Copy** makes a new written skill under a new name, with its frontmatter `name` set to it.
- **Versions.** Every save, install and update adds a version, kept for good. The skill's page lists them, read-only. A name that is deleted and created again continues its numbering.
- **Delete** is refused with 409 `the skill is in use: take it off the repositories and the default list that name it first` while the default list or a repository, in the config or the dashboard, names the skill. The answer lists them; repositories you can't see are only counted. An admin of a platform instance sees the repositories it can see.
- **Budget.** Saving the default list or a repository's skill list is refused with 400 when the text of the library skills it names is over what a review takes, `the library skills come to N KiB; a review takes at most 900 KiB`. The pickers show a running total.
- The skill's text is shown in a plain-text box only, never as HTML or markdown.

#### Removing a version by hand

There is no button to delete a version, because history is meant to stay. If you must remove one (for example text that the secret check missed), do it in the database, as an operator, and rotate the secret too. A model provider may have seen the text in reviews already.

1. If the text is in the skill's newest version, first save a new version in the dashboard, so the version to remove is not the newest one. The next number is the highest one kept plus one, so removing the newest row would let its number be used again. A written skill: edit it and save. An installed skill can't be edited, so fix the text at its source and press **Update**, or **Copy** it under a new name (with the clean text) and delete the original once nothing names it.
2. Remove the old version, for example version 2 of `lint`:

   ```sql
   DELETE FROM library_skill_versions WHERE name = 'lint' AND version = 2;
   ```

3. A read of a source keeps the text it found for an hour. Drop the finished ones now:

   ```sql
   DELETE FROM skill_fetches WHERE status <> 'running';
   ```

A run that used the removed version still shows its number, and the version's page answers 404. Jobs, with their copy of the text, are removed an hour after they finish.

### Installing from git

**Install from git** (admins) copies skills from a git repository into the library. It reads the source, lists the skills it found (`*.md` files and `<name>/SKILL.md` folders in the branch or tag and folder you give) with their size and any problem, and installs the ones you tick. A skill whose name is already in the library can't be ticked: `name already in the library`. An installed skill records its source, branch or tag, file path and the **commit** it came from, and can't be edited.

There are two kinds of source:

| Source | How it is read | What it needs |
| --- | --- | --- |
| **A repository on a connected instance** (added under **Integrations** or in the config file) | By the pergent.sh pod, through the platform's API, with the review bot or review App it already has. No Job runs and you enter no secret. | **GitLab:** the review bot is a member of the repository with **Reporter** or above. **GitHub:** the review App is installed on that repository (it has Contents: Read-only already). A repository the bot or App can't read, or a branch that doesn't exist, is answered `the review bot or App can't read this repository or ref`, because the platform says the same for both. |
| **A public `https` URL** | By a short skill fetch Job, with no token of any kind (see [What a Job can and cannot see](#-what-a-job-can-and-cannot-see)). | The URL's host is inside `jobEgress.cidrs`. The Job posts what it read to `<callback_url>/skill-fetches/<id>/result`, so `/skill-fetches/` must be reachable on `public_url`'s host (or through `callback_url`, in-cluster); see [Exposing pergent.sh](#exposing-pergentsh). The chart sets `PERGENT_SH_IMAGE` for the Job. Only public repositories work: a URL with a user name or password is refused. |

- **A read is limited.** At most 200 skills and 4 MiB of text in one read (a smaller folder fixes it), 128 KiB per skill, and the pod runs 2 reads at once. A read lasts at most 2 minutes. For a URL, the **image pull and the Job's fixed limits count against those 2 minutes**: the clone container has 512Mi of memory and the Job 1Gi of scratch space. A slow image pull, a slow clone or a huge repository therefore ends as `reading the source took too long` (a read by the pod ends as `reading the repository took longer than 2 minutes`). Choose a smaller folder, or a tag with less history. The result of a read is kept for an hour for you to choose from.
- **The ceiling.** A password admin installs from anything the review bot or App can reach. A platform admin installs from repositories of its own instance that its user can see there, checked with the platform now, and from public URLs; otherwise it gets 403 `you can install only from repositories of your own instance that you can see`. **Installing is sharing:** everyone with a team can read an installed skill, even one from a private repository. A private repository's path is shown only to people who can see it; others see `a private repository on <instance>`.
- **A push to the source changes nothing.** Installed skills only change when an admin presses **Update**.

**Check for updates** (on **Skills**) reads a source again, at the skill's own branch or tag, or at a new one you type, to move from `v1.0` to `v1.1`. For each skill installed from that source and branch or tag it shows the difference, and **Update** saves the changed ones as new versions (and, for a new branch or tag, records it). Nothing changes before **Update**. A fetched skill is matched to an installed one by its file path at the source. A skill gone from that path shows as `removed at the source` and stays as it is. A skill whose name changed there shows as `renamed at the source`, and Update is refused for it: install it again under its new name. A text that now holds a secret pergent.sh holds, or is over 128 KiB, can't be updated.

### Repository skills

- **`.pergent/skills/` always runs.** A repository skill replaces a library skill of the same name.
- **The other three folders are detected, not run.** A repository's **Settings → Skills** lists the skills found in `.agents/skills`, `.claude/skills` and `.github/skills` (read from the default branch), off at first. A maintainer or admin of the repository turns each on (`repoSkills`; in the config, `repositories[].repo_skills`), which is what lets a repository reuse skills it already keeps for other tools. A turned-on skill that later disappears from the target branch fails the run, as above.
- Repository skills are changed through merge requests, not in the dashboard. The repository's **Skills & instructions** page shows the final set a review of the default branch would get (kept for 5 minutes), each skill's source and library version, and a link to each repository skill's file.

### No skills, no review

When a run ends up with no skill at all (the default list is empty and the repository names none and has none), there is **nothing to review with**, so pergent.sh does not review:

- No Job starts, and no comment and no commit status or Check Run is written. The run's state is `skipped`; the dashboard shows it as **Not reviewed** with the reason `this repository has no skills`, and "Needs attention" lists the repository with the same words. It is not counted as a review or a failure.
- A required `pergent` status therefore keeps the merge request blocked, which is the safe result. **If an earlier review of the same commit passed, its status stays** (pergent.sh sets none to replace it), but the bot's approval of that commit is withdrawn, because a skipped review decided nothing. Nothing approves it again until a review runs.
- Adding a skill to the default list (or to the repository) and re-running the review (a push, or `@<bot> review`) fixes it. **Add repository** warns when the default list is empty.

Without the dashboard (`dashboard.url` unset) the library can't be managed, but reviews still read it and the default list from the database. A fresh install has an empty library, so its repositories review with repository skills only.

## 🧵 Inline findings

pergent.sh posts each finding of a review as a thread on its lines, next to the single review comment.

- **Each finding is posted on its lines.** At most 20 pergent threads are open on a change at once: blocking findings go first, then the rest by file and line.
- **A small fix comes as a suggestion.** When the fix replaces 10 lines or fewer, all inside the finding's lines and within one diff hunk, people can apply it with GitHub's **Commit suggestion** or GitLab's **Apply suggestion**. A replacement with a line that starts with `/` in column 0 is never posted as a suggestion, because GitLab would run it as a quick action; the fix is shown as advice in words.
- **A later review keeps the threads current.**
  - A finding that is gone gets the reply ``Fixed in `<sha>`.`` and its thread is resolved. This happens only when its skill ran without failing and still reviewed the file. If the file left the change, the reply is "No longer in this change."
  - A finding that comes back reopens its thread, with ``Back in `<sha>`.``
  - A thread that a person resolved is left alone, and its finding is not posted again.
- **Dismissing resolves the thread.** A dismissal from a comment, from `@<bot> dismiss <reason>` as a reply in the finding's thread, or from the dashboard replies "Dismissed by …" and resolves the thread. Restoring it on the dashboard reopens it. See [Dismissing a finding](#dismissing-a-finding).
- **Findings that can't go inline stay in the review comment**, under **Not on the diff**, each with the reason: off the diff, over the limit of 20, posting failed, too long, or kept because the branch requires resolved threads.
- **When the target branch requires resolved threads, only blocking findings go inline.** On GitHub that is a ruleset's "Require conversation resolution" or the classic protection's rule; on GitLab it is "All threads must be resolved". The non-blocking findings stay in the comment, so they leave no open thread to hold up a merge.
- **Very large changes get no threads.** At 3000 changed files or more on GitHub, or when GitLab's diff passes its limits, that review posts no inline threads and every finding stays in the comment.

**Turning it off.** Inline findings are on by default. The switch is **Inline findings** in a repository's Settings, the default under **Admin → Settings**, or `repositories[].inline_findings` in the config ([Config reference](#-config-reference)). With it off, every finding stays in the review comment, as before 0.6.0.

**GitHub setup.**

- The review App needs **Pull requests: Read and write**. With Read-only, inline is off for that instance and the Integrations page says so.
- **Administration: Read** is optional. It lets pergent.sh read the classic branch protection's conversation-resolution rule. Without it, only rulesets are read and the classic rule is treated as off, so a branch whose only such rule is classic gets suggestions inline too. The Integrations page notes a repository where the App can't read it (when inline findings are available there).
- Subscribe the App to **Pull request review comment**. Without it, replies in a finding's thread are not received.

**GitLab setup.** "All threads must be resolved" is read from the project settings. No new webhook is needed: note events already arrive.

## ⏩ Incremental reviews

A push to a change that already has a review looks only at what the push changed. Files the last review already covered are carried over, and the comment says so, for example `Re-reviewed 2 files · 28 carried over from abc1234`. The review page shows the same line. The result is still one review of the whole change: approval and the status are decided on the combined result.

A review is incremental when all of these hold. Otherwise it is a full review, and the log says why.

- **Incremental reviews** is on for the repository (the default).
- A push started it. `@<bot> review` is always a full review.
- The change has a completed review for the same target branch, and its head is an ancestor of the new head. After a rebase or force-push it is not, so the review is full.
- The forge lists the files changed between the two heads.
- That review was made by pergent v0.5.0 or later.

A file is carried over, per skill, only when that skill reviewed it last time, the push didn't change it, and it has no open blocking finding. A file whose review failed, a new file, and every file of a new or edited skill are reviewed again. Open blocking findings are always checked again.

**What this gives up.**

- An edit to one file can break another file that didn't change. That goes unnoticed until a full review (`@<bot> review`). The model can read the other file, but nothing sends it there.
- Teams that amend or rebase get no speed-up yet: those pushes are full reviews.

**Turning it off.** The switch is **Incremental reviews** in a repository's Settings, the default under **Admin → Settings**, or `repositories[].incremental_reviews` in the config ([Config reference](#-config-reference)). With it off, every push is a full review.

## 💬 Commands

A comment on a merge request (or pull request) can ask the review bot to run the review again or to set a finding aside, and the dashboard's review page has the same actions as buttons. That is all they do: no command changes a setting, answers a question or fixes code.

| Command | What it does |
| --- | --- |
| `@<bot> review` | Runs the whole review again on the change's current head and target branch, as a full review: every file, not only what changed since the last review. |
| `@<bot> review failed` | Runs again only the skills that failed, over all their files, and keeps the other skills' results. |
| `@<bot> dismiss <id> <reason>` | Sets a finding aside as not a real problem. `<id>` is the finding's ID, shown as `id …` on its line in the review comment; each finding's own "Not a real problem?" line has the command ready to copy. |
| `@<bot> help` | Replies with the list of commands. |

In a finding's own thread the finding is already known, so `@<bot> dismiss <reason>` works as a reply there, with no ID (an ID in front of the reason is allowed). In any other thread it is answered with a pointer to the right command.

`<bot>` is what people type after `@` to address the review bot: its username on GitLab, the review App's slug on GitHub (`@pergent-review review` for an App called that). A comment that doesn't mention it is ignored, so people can write "review" in ordinary conversation. Restoring a dismissed finding has no command: it is a button on the dashboard.

### Writing a command

- The mention starts its line (up to 3 spaces of indentation; capitals don't matter) and the command follows it: `@pergent-review review`. A colon or comma after the mention is fine. A line that holds only the mention is passed over; the command is read from the next line that starts with the mention.
- Text that isn't the author's own is never read: lines in a fenced code block (` ``` ` or `~~~`), in an HTML comment, indented by 4 spaces or a tab, or quoted (starting with `>`). A mention inside a sentence is no command.
- Only the first command of a comment runs.
- `review`, `review failed` and `help` must be the whole rest of the line (a closing `.` or `!` is fine). `dismiss` takes a finding ID of 6 to 12 hexadecimal digits, then the reason on the same line. Anything else after the mention, such as `review please` or `dismiss` with a malformed ID, is answered like `help`.
- A reason is required to dismiss a blocking finding and optional for a suggestion. A reason is 500 characters at most: a comment's is cut there, and the dashboard refuses a longer one (400 `the reason is too long`). Markdown images (`![...](...)`, also by reference, and `<img>` tags) are removed from a reason before it is stored, so a review comment can't be made to load one.
- Only a newly created comment on a merge request counts. Edits, deletions and system notes never run a command. A reply in a thread counts as a command written in that thread (on GitHub only a reply, not the comment that starts the thread).
- A comment never runs a command when it was written by the review bot or the approve bot, by any account whose name ends in `[bot]`, or through a GitHub App, by a GitLab project, group or service-account bot (a username like `project_<id>_bot`, `group_<id>_bot_<token>` or `service_account_…`; the engine also asks GitLab, `GET /users/:id`, and drops the command with 👎 when the user's `bot` flag is set, or tries it again when GitLab can't say), or when it contains pergent's hidden marker (`<!-- pergent`), which every review comment carries. So neither pergent.sh's comment nor a model's text quoted in a finding can run a command.

### Who may do what

The role is read from the platform when the command is processed, for a comment, and from the repository role on the dashboard ([Teams](#teams)), for a button.

With **Only the author and maintainers use commands** on (the default), a comment is first checked for who wrote it: only the author of the change and the repository's maintainers and owners can use commands. Anyone else is refused, whatever else the table says. With the setting off, the table alone decides; the author is always allowed.

| Action | In a comment | Button |
| --- | --- | --- |
| Re-run (`review`, `review failed`) | The change's author, whatever their access; otherwise a maintainer or owner (GitLab Maintainer or above; GitHub `maintain` or `admin`). With the setting off: the author, or GitLab Developer or above, GitHub `write`, `maintain` or `admin` | Maintainer or admin of the repository |
| Dismiss a suggestion | The same as re-run | The same as re-run |
| Dismiss a blocking finding | GitLab Maintainer or above; GitHub `maintain` or `admin`; **never the change's author** | Maintainer or admin of the repository; never the change's author. A password admin has no platform identity, so the author rule doesn't apply to it; a merged or closed change refuses everyone |
| Restore | No command | The same as re-run, the change's author included, since restoring only makes the review stricter |

- GitLab: the commenter's membership of the project, inherited from its groups, read by the review bot (`GET /projects/:id/members/all/:user_id`). Only an active, unexpired membership counts: a blocked or deactivated user, one awaiting approval, or one whose membership has expired has no role.
- GitHub: the commenter's repository permission, read by the review App (`GET /repos/{owner}/{repo}/collaborators/{username}/permission`). `maintain` and `admin` can re-run and dismiss blocking findings; `write` can re-run only with the setting off; `read` and `triage` can do neither (unless they are the change's author). A custom role counts by its permissions.
- Maintainers and owners are GitLab Maintainer or Owner, and GitHub `maintain` or `admin`.
- **Only the author and maintainers use commands** is on by default. It is a repository's setting in the dashboard (**Repository → Settings**, under Reviews), a default under **Admin → Settings**, and `repositories[].commands_author_and_maintainers_only` in the config. Dashboard buttons are not affected: they already need a maintainer or admin of the repository.
- The author always can re-run and dismiss suggestions, in both modes, whatever their access. The author can never dismiss a blocking finding on their own change.
- Every refusal about who may is written to the [audit log](#audit-log) as `command_refused`, in both modes: the gate, "no permission" and "author of the change". Ordinary mistakes (an unknown ID, a missing reason, a cap) and commands the 2-minute throttle leaves unanswered are not recorded.
- The author is the one the platform reports for the change, compared by user ID, never by username. When either ID is unknown the commenter is taken for the author, so a blocking dismissal is refused. For being let in as the author (the setting, re-runs, suggestion dismissals), an unknown ID is nobody.
- The pergent.sh team roles apply to buttons only; a comment is judged by the platform alone.

### Re-running a review

- `review` queues a run for the change's head and target branch as the platform reports them now (the same rules as a push; the newer run replaces the one that was published). `review failed` also queues one but reviews only the skills that failed: a skill whose verdict is `failed`, or that is clean yet left files unreviewed, after dismissals. It keeps the other skills' results and publishes one merged review. With nothing failed it replies `nothing failed` and does nothing.
- `review failed` re-runs the whole review, and replies `the whole review failed, so all of it is re-run`, when there is no result per skill to build on: the earlier review was an error result, there is none for the current head and target branch, or it can't be read. It also reviews everything when the dispatcher, before it starts the Job, finds the earlier review can't be kept: it is of other commits, or a skill changed since: one was added, removed or renamed, a library skill it would keep has a new version (an edit, an install or an update; the run page's `library v3`), a skill moved between the library and a repository folder, or it names a skill twice. A re-run that could not be merged with the earlier results shows a note in the comment and is **never approved**.
- A head that is already queued, running or waiting to be published is not queued twice: the command still gets 👍. The caps are checked first, though: at a cap, a re-run of such a head gets the cap refusal and 👎. A re-run of a draft is refused when the repository doesn't review drafts, and a command on a closed or merged change gets 👎 and no reply.
- The new review is decided like any other (see [How approval works](#-how-approval-works)). If it isn't approved, the bot's approval of that head is withdrawn.
- While a re-run of a commit is queued or running, the review it replaces is set aside. A dismiss is resolved against the newest review still published. When there is none, a comment gets the reply `no finding <id> in the latest review` and a button the answer `there is no such finding in the latest review` (404). A dismissal or restore made meanwhile is stored, and applies when the re-run is published (a restore always acts, on the finding's fingerprint).

### Dismissing a finding

- A dismissal belongs to one merge request. It is stored by the finding's **fingerprint**, so it survives pushes and line moves: when a later review reports the same finding, it stays dismissed.
- It covers the severity it was made at, and below. A dismissed blocking finding stays dismissed if it comes back as a suggestion; a dismissed suggestion that comes back as blocking shows again, and needs a dismissal by someone allowed to dismiss blocking findings. Dismissing a suggestion a second time as blocking raises it.
- `dismiss` and **Dismiss** start no run. pergent.sh decides the latest review again and publishes it: the comment is written again with a collapsed **Dismissed** section (each finding's title and ID, who dismissed it and the reason; the verdict line reads `✅ **No blocking issues** (N dismissed)` when only dismissed findings were blocking), the status is set again, and a review whose blocking findings are all dismissed is approved under the usual rules (approval on, not a draft, nothing failed). The dismissals are applied to the newest review of the change only; one whose head has moved on applies to the next review.
- **Restore** (a button on a dismissed finding) removes the dismissal and decides the review again. If that makes it not approved, the bot's approval is withdrawn; the withdrawal is retried until it succeeds, and for a merged change the record is just dropped.
- A blocking dismissal of a merged or closed change is refused for everyone, password admins included: the button answers 409 `this change is merged` or `this change is closed` (and 502 when the platform can't say what state it is in). A comment on a change that isn't open gets 👎 and no reply, whatever the command.
- A dismissal that already exists at the same or a higher severity is left as it is: the reply is `finding <id> is already dismissed`.
- Dismissals are deleted when the repository is switched off ([Switching a repository off](#-switching-a-repository-off)).

### Reactions and replies

The bot reacts to the comment so everybody sees what became of it (GitLab award emoji `eyes`, `thumbsup`, `thumbsdown`; GitHub reactions `eyes`, `+1`, `-1`):

- 👀 when the command is picked up;
- 👍 when it is done, or there was nothing to do;
- 👎 when it is refused, and one short reply says why.

A reply never says more than the commenter could see on the platform, and never repeats what they wrote except a finding ID reduced to hexadecimal digits. A reaction that can't be made never stops the command. A command that meets a platform or database error is tried again up to 3 times (after 15, 30 and 60 seconds), then given up on with 👎 and no reply.

| Case | Reaction | Reply |
| --- | --- | --- |
| Done | 👍 | none |
| `help`, or a command the bot doesn't know | 👍 | ``Commands: `@<bot> review` (full review) · `@<bot> review failed` · `@<bot> dismiss <id> <reason>` `` |
| `review failed` with no failed skill | 👍 | `nothing failed` |
| `review failed` without a result per skill | 👍 | `the whole review failed, so all of it is re-run` |
| `dismiss` of a dismissed finding | 👍 | `finding <id> is already dismissed` |
| Someone who is neither the author nor a maintainer, with the setting on | 👎 | `Only the author of this PR and the repository's maintainers can use pergent commands.` (`MR` on GitLab), on their first refusal on a change; 👎 only after that |
| No permission (also a blocking dismissal without Maintainer) | 👎 | `you don't have permission to do that here` |
| The change's author dismisses a blocking finding | 👎 | `the author of a change can't dismiss its blocking findings` |
| Blocking dismissal without a reason | 👎 | ``dismissing a blocking finding needs a reason: `@<bot> dismiss <id> <reason>` `` |
| The ID isn't in the latest review | 👎 | `no finding <id> in the latest review` |
| A draft, in a repository that doesn't review drafts | 👎 | `drafts aren't reviewed in this repository` |
| A re-run cap is reached | 👎 | `re-run limit reached for this MR; it resets at 00:00 UTC` (`PR` on GitHub) |
| The same command again within 30 seconds, the repository switched off, a GitLab bot as the author, or the change closed, merged or gone | 👎 | none |
| The commenter had a command on the change answered without doing anything (a reply of help, a refusal, a command with nothing to do, a drop, a command given up on after errors) less than 2 minutes ago | none | none |

### Limits on commands

- **Once per head.** A re-run of a commit already queued, running or waiting to be published isn't queued twice (the caps are checked before this, so at a cap such a request is refused with the cap reply).
- **30 seconds.** A command that was carried out, or answered with nothing to do, is not accepted again for the same merge request, command and finding ID within 30 seconds (a refused one doesn't start the window).
- **Re-run caps.** Re-runs started by a comment or a button count; pushes and newly opened merge requests always run and never count. The defaults are 10 per merge request and 50 per repository, per UTC day, and the reply names the reset time (00:00 UTC). A re-run that was superseded still counts. The caps are set by admins only: in the dashboard under **Admin → Settings** (for repositories switched on in the dashboard), per repository in its **Settings**, and in the config as `repositories[].rerun_limit_per_mr` and `repositories[].rerun_limit_per_repo` (1 to 1000). Buttons are held to the caps too (HTTP 429 with the same text). Two buttons pressed at the same moment can overshoot a cap by a few.
- **One do-nothing answer per 2 minutes.** Every answer costs platform calls and a comment of the bot, so once a command of a person on a change has been answered without doing anything, their next command on that change within 2 minutes gets nothing at all: no reaction, no reply, no platform call, and it is not run. What counts is a `help` or an unknown command, a refusal (not the author or a maintainer, no permission, the author, a missing reason, an unknown ID, a draft, a cap), a command with nothing to do (`review failed` when nothing failed, a finding that is dismissed already), a drop and a command given up on after errors. The refusal for someone who is neither the author nor a maintainer starts the window too, also when it gets only a 👎. A command that was carried out (a re-run queued, a finding dismissed) does not start the window, so dismissing several findings in a row works, and so does a `review` followed by a `dismiss` once the re-run's review is published. While the re-run is still being reviewed there is no review to dismiss from: that `dismiss` is refused (`no finding <id> in the latest review`), and the refusal starts the window. A real command written within the window after a `help` or a refusal is throttled: write it again after the 2 minutes. The check is made before anything else, and commands that were themselves left unanswered don't extend the window.
- **Never hold up reviews.** The engine answers at most 10 commands per pass and never holds up reviews for more than about 30 seconds per pass. A command cut short because the pass ran out of that time is put off for 15 seconds without counting as a try.

### Setup

Commands need the platform to send comment events: GitLab note events (pergent.sh adds them to the hook it manages, see [Install with Helm](#-install-with-helm)), and for GitHub the **Issue comment** event on the review App ([Create the review App](#1-create-the-review-app)). Without them no command is received. Repositories that aren't switched on ignore commands, as they ignore pushes. Each command and button is recorded in the [audit log](#audit-log) as `review_rerun`, `review_rerun_failed`, `finding_dismissed` or `finding_restored`, and a refusal about who may as `command_refused`; the actor of a comment is `username (platform instance)`.

## ✅ How approval works

pergent.sh decides from what `pergent agent` reports. The result is checked again before anything is published: it must parse and be for the same base and head commits as the run, or it is posted as a failure.

| What happened | Comment | Commit status | Approved |
| --- | --- | --- | --- |
| Any skill has blocking issues that nobody dismissed (see [Commands](#-commands)) | The review | `failed` ("Blocking issues found") | No |
| Any skill failed, has an unknown verdict, or is "clean" yet lists files it did not review | The review | `failure_status` ("Review failed for N of M skills") | No |
| The Job died or sent nothing usable, the result is an error, has no skills, or has an unknown status | An error comment with the reason | `failure_status` | No |
| The repository has no skill to review with (see [No skills, no review](#no-skills-no-review)) | None | None. The run is `skipped` and shown as "Not reviewed"; an earlier status of the same commit stays | **No**; the bot's approval of that commit is withdrawn |
| Every skill is clean, but no file was reviewed because every changed file was skipped | The review plus a note listing the skipped files and the rules that skipped them | `success` ("Nothing to review: every changed file is skipped") | **No**, a human needs to look |
| Every skill is clean and at least one file was reviewed, but some changed file was skipped as `deleted`, `binary`, `vendored`, `lockfile`, `generated` or under a rule pergent.sh doesn't know | The review plus a note listing those files and the rules that skipped them | `success` ("No blocking issues; not approved: some changed files weren't reviewed") | **No**, a human needs to look |
| Every skill is clean, at least one file was reviewed and no file was skipped under those rules, but approval is turned off for the repository (`approve: false`, or the dashboard's switch) | The review plus `**Not approved:** approval is turned off for this repository.` (this note replaces the draft and approve App notes below) | `success` ("No blocking issues; not approved: approval is turned off") | **No**, see [Turning approval off](#turning-approval-off) |
| Every skill is clean, at least one file was reviewed and no file was skipped under those rules, but the merge request is a draft | The review plus a note that it will be approved when it's marked ready | `success` ("No blocking issues; not approved: draft") | **No**, until it is marked ready |
| As above and not a draft, but on GitHub the approve App isn't installed on the repository or lacks Pull requests: write (or the approve bot user can't reach it) | The review plus `**Not approved:** the pergent approve App isn't installed on this repository or lacks Pull requests: write.` (or `... the pergent review App lacks Pull requests: write on this repository.`, or `... the pergent approve bot can't reach this repository.`) | `success` ("No blocking issues; not approved: ...") | **No**, see [GitHub](#-github) |
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

A skill whose blocking findings are all dismissed counts as clean, and the comment is written again with its **Dismissed** section; a review whose other conditions hold is then approved like a clean one. A failed, partial or missing review is never `success` and is never approved, and neither is a re-run of some skills that could not be merged with the earlier results of the others.

Deciding a review again on the same head (a restored finding, a re-run, a draft that turns up) as not approved takes the approve bot's approval of that head off, as a new head does. The withdrawal is recorded first and tried again until it works. A review that fails for good, because it can't be posted after the retries (see [Pushes, edits and failures](#pushes-edits-and-failures)), takes the bot's approval of that commit off the same way: the withdrawal is recorded in the same step that gives the review up, so a crash can't lose it, and the approval sweep also withdraws the approval of any commit whose newest review is in that state, whatever was recorded. A commit whose newest review failed never keeps an approval an earlier review of it gave.

### Turning approval off

A repository's `approve: false` (or, for a repository the dashboard manages, its "Approve clean reviews" switch turned off) stops approvals there: reviews still run and are posted with their comment and status, and a clean review gets `**Not approved:** approval is turned off for this repository.` Only a review that would otherwise be approved changes; a blocking, failed, partial or draft review is decided as before.

Turning it off also withdraws every approval the approve bot already holds in the repository, on the next cleanup pass (every 2 seconds): GitLab's approve bot unapproves, and on GitHub the approve App or bot dismisses its approving reviews. An approval the bot can't withdraw is tried again with the same backoff as a stale approval (30 seconds, doubling up to 5 minutes). Turning approval back on approves nothing by itself: the next clean review of a merge request is approved.

### Pushes, edits and failures

- Only pushes, opening or reopening, changing the target branch, and a re-run asked for by a [comment or button](#-commands) start a run. Editing the title, labels, description or assignees does not. Closing or merging supersedes the MR's runs. On GitHub these are the `pull_request` actions `opened`, `reopened`, `synchronize`, `edited` (only when `changes.base` is present: a retarget) and `closed`; every other action and event (including `ping`) is answered 200 and ignored; the `issue_comment` event is read for [commands](#-commands) alone.
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
- Publishing is retried with backoff. After 10 failed attempts the run becomes `publish_failed`: pergent.sh makes one last attempt to set the head's status to `failed` with the description "pergent couldn't post its review" (GitHub: the Check Run completed with `failure` and that title), so a required check isn't left running, and posts nothing more to the merge request; it is never approved, and the approve bot's approval of that head, if an earlier review of it gave one, is withdrawn (recorded with the give-up itself, tried at once, and tried again by the approval sweep until it works). The failure itself is only in the logs.
- The comment pergent renders is held to 990,000 bytes on GitLab and 60,000 on GitHub, which refuses a comment over 65,536 characters; the rest of the limit is room for the note pergent.sh adds (the list of skipped files is at most 20 paths and 4000 bytes).

## 🔒 What a Job can and cannot see

A Job has two containers, started in order. Both run the pinned pergent image and have `allowPrivilegeEscalation: false` and all capabilities dropped. The pod never mounts a Kubernetes service account token.

**The checkout container** gets only a read-only clone token, through git's `http.extraHeader` setting passed in environment variables. On GitLab it is the project's deploy token (`read_repository`). On GitHub it is an installation token of the review App minted for this one Job, scoped to the one repository with `contents: read`, used as `x-access-token`; GitHub expires it after an hour and pergent.sh never stores it. The token is never written to disk, never in a URL and never in `.git/config`. It fetches only the base and head commits (not the whole repository history), disables hooks and gives the remote an invalid push URL.

**The pergent container** runs `pergent agent` and gets:

- the checkout, which is untrusted code under review, read-only to the agent;
- the skills, the instructions, the previous review and, for an incremental review, the files to carry over (`carried.json`), mounted read-only at `/in`;
- `OPENCODE_API_KEY` (the LLM key) and a one-time result token. The result token is random, stored only as a hash, valid for one result and expires shortly after the Job's deadline;
- the provider, model and, if set, base URL.

The agent in the pergent container can only read files, grep and list. It cannot edit files or run commands, and `*.env`, `*.pem`, `*.key` and `*.tfvars` files are denied. pergent deletes instruction files found in the checkout, so the reviewed repository cannot change the instructions.

A Job **cannot** see: the review bot token, the approve bot token, the GitHub App private keys and their installation tokens (other than its own clone token), the webhook secret, the database URL or the secret key, other Jobs' Secrets, the Kubernetes API (no service account token, and the Role is for pergent.sh's account only), or anything but DNS, the GitLab, GitHub and LLM addresses in `jobEgress.cidrs` and pergent.sh's own port 8080, when your CNI enforces the NetworkPolicy. Be aware that the LLM key is in the pergent container's environment, so use a key meant for this purpose, with a spending limit.

Each Job's Secret and inputs ConfigMap are owned by the Job, so deleting the Job removes them. Finished Jobs are removed after an hour.

**A skill fetch Job** reads the skills of a public `https` URL an admin gives in the dashboard, so the pergent.sh pod never fetches an address an admin typed. It runs in the Jobs namespace under the same NetworkPolicy, for at most 2 minutes (its image pull and its limits, 512Mi of memory for the clone and 1Gi of scratch space, count against them), as user 65532 with a read-only root filesystem and no service account token. Its `clone` container (the `jobs.image`) gets no token of any kind: it fetches the one branch or tag at depth 1 with hooks off and https only. Its `send` container (pergent.sh's own image, which the chart passes as `PERGENT_SH_IMAGE`) reads the skills of the folder without following any symbolic link (one where a skill would be is listed as a problem) and posts them to `<callback_url>/skill-fetches/<id>/result` with a one-time token, its only secret. pergent.sh then drops the text of any skill holding a secret it holds or over 128 KiB. A URL outside `jobEgress.cidrs` can't be reached; the fetch then fails with `could not fetch <url> at <ref>; the URL, branch or tag may be wrong, or outside jobEgress.cidrs`.

**A readiness scan Job** reads one repository's default branch for the [readiness scan](#readiness-scan). It is named `pergent-scan-<12hex>`, runs in the Jobs namespace as user 65532 with a read-only root filesystem and no service account token, and carries `app.kubernetes.io/managed-by: pergent-sh`, so the NetworkPolicy above applies to it. It has a 5 minute deadline, a 2Gi `emptyDir` for the clone, and asks for 100m CPU and 128Mi with a 512Mi memory limit. Its `clone` container (the `jobs.image`) gets only the read-only clone token, as a review's does (the GitLab deploy token, or a GitHub installation token for this one repository with `contents: read`), and fetches the one commit at depth 1 over https only, with hooks off. Its `scan` container (pergent.sh's own image, `PERGENT_SH_IMAGE`) gets only its one-time result token, as an environment variable, and its input as a read-only file: the library skills' names and `paths` and `exclude` globs, never their text. It follows no symbolic link and posts to `<callback_url>/readiness-scans/<id>/result`. The Job has **no LLM key and no token that can write**, and the clone never sees the result token.

## 🍴 Fork merge requests

The checkout first tries to fetch the base and head commits by SHA. When the server refuses that (this is common for merge requests from forks), it fetches `refs/merge-requests/<iid>/head` (GitHub: `refs/pull/<number>/head`) instead, then deepens the target branch (starting at 50 commits and doubling, up to 3200) until the merge base is there. If the merge base is still missing at that depth, the checkout fails and the merge request gets an error comment.

## 📴 Switching a repository off

A repository is on when it is listed in the config or switched on in the [dashboard](#-dashboard). To switch a config repository off, remove it from `repositories` and redeploy; a dashboard repository is switched off with its switch. On the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token. Runs still queued for it are dropped when they come up for dispatch; a review that was already running is still posted, but never approved, as with approval turned off. Its dismissals and the comment commands waiting for it are deleted with its record, and commands written there afterwards are ignored.

A GitHub repository has nothing to delete: removing it from the config deletes only pergent.sh's own record of it. Installing an App on a repository never switches it on; only the config list or the dashboard does.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days. An instance added in the dashboard is removed there instead, which takes its webhooks and deploy tokens off the platform first ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)).

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.

## 🌐 HTTP routes

| Route | Used by |
| --- | --- |
| `POST /webhooks/gitlab/{instance}` | GitLab merge request (`Merge Request Hook`) and note (`Note Hook`) webhooks; any other event is answered 200 and ignored. The `X-Gitlab-Token` header is compared in constant time with the instance's secret; a mismatch gets 401 and is otherwise ignored. An authenticated webhook whose payload can't be used (not JSON, no project id or merge request iid, a malformed head SHA) gets 200 and a warning in the log, because GitLab counts failed deliveries toward disabling the hook. |
| `POST /webhooks/github/{instance}` | The GitHub review App's `pull_request` and `issue_comment` webhooks (the second carries [commands](#-commands)); any other event is answered 200 and ignored. `X-Hub-Signature-256` (HMAC-SHA256 of the body with the instance's webhook secret) is compared in constant time; a missing or wrong signature gets 401 and is otherwise ignored. A repository that isn't in the config gets 200 and is ignored. |
| `POST /runs/{id}/result` | A review Job delivering its result, with its one-time bearer token. |
| `POST /skill-fetches/{id}/result` | A skill fetch Job delivering the skills it read from a public URL, with its one-time bearer token (only with the dashboard on). A wrong token gets 401, a second result or one after the fetch gave up 409, a body over 4 MiB 413. |
| `POST /readiness-scans/{id}/result` | A readiness scan Job delivering what it found in a repository, with its one-time bearer token (only with the dashboard on). A wrong token gets 401, a second result or one after the scan ended 409, a body over 1 MiB 413. |
| `GET /healthz` | Health check. It answers as soon as the server starts: pergent.sh serves right away and runs the startup reconcile in the background. |
| `GET /auth/{gitlab or github}/{instance}/login`, `GET .../callback`, `POST /auth/logout` | [Dashboard](#-dashboard) sign-in with GitLab or GitHub, and sign-out. Only with `dashboard.url` set. |
| `GET /auth/csrf`, `POST /auth/password`, `POST /auth/password/change` | Password sign-in. `GET /auth/csrf` hands out the sign-in form's CSRF token (a cookie and `{"csrfToken": ...}`); `POST /auth/password` takes `{"username", "password"}` with that token in `X-CSRF-Token` and answers 204 and the session cookie; `POST /auth/password/change` takes `{"current", "new"}` for a signed-in password account. See [Password sign-in limits](#password-sign-in-limits). Only with `dashboard.url` set. |
| `/api/...` | The dashboard's JSON API. Every route but `GET /api/instances` needs a session; writes need the `X-CSRF-Token` header. Only with `dashboard.url` set. The review page's buttons use `POST /api/reviews/{instance}/{project}/{number}/rerun` (`{"failed": true or false}`; 202 with `queued`, `already queued`, `nothing failed` or `whole review`; 429 at a re-run cap), `.../dismiss` (`{"fingerprint", "reason"}`; 200 with `dismissed` or `already dismissed`; a reason of up to 500 characters; 400 `the reason is too long` above) and `.../restore` (`{"fingerprint"}`; 200 with `restored` or `not dismissed`), for maintainers and admins of the repository, config repositories included. The Readiness tab uses `GET /api/repositories/{instance}/{project}/readiness` (anyone who can see the repository) and `POST .../readiness/scan` (maintainers and admins; 202 with the queued scan, 409 while one is queued or running, 429 within 5 minutes of the latest scan, 404 for a repository that isn't switched on or when scans are off). |
| `GET /` and every other path | The dashboard's pages. Only with `dashboard.url` set; without it they are 404. |

Logs are JSON on stdout. Secrets are never logged.

## 📏 Limits

This release is deliberately small:

- Commands are `review`, `review failed`, `dismiss` and `help`, and the dashboard has the matching buttons plus Restore. No chat, no questions, no asking the bot to explain or fix code, no commands that change settings, no questions in a finding's thread (only `dismiss`), and no dismissing a finding for every merge request of a repository: a dismissal belongs to one merge request. Restoring is a button only.
- Dashboard sign-in is a password (operators) or GitLab or GitHub. No OIDC, SAML or LDAP, no two-factor authentication, and no API tokens for scripts.
- Teams are kept in pergent.sh: they are not synced from GitLab groups or GitHub teams.
- A GitLab and a GitHub account of one person are two accounts, with no linking.
- `PERGENT_SH_SECRET_KEY` can't be rotated: see [Environment variables](#environment-variables).
- No advice on whether a human approval is still needed beyond the hourly warning for a repository that one approval can merge.
- No repository cache: every checkout is a fresh `git init` plus a shallow fetch.
- Skills: one library for the whole installation, with a default list and per-repository lists; no per-team or per-instance lists. Installed skills change only when an admin presses **Update**, never by themselves or from a webhook on the source. Only the `SKILL.md` text of a folder skill is used. Repository skills can't be edited in the dashboard. A skill's text has no preview and the history has no diff or restore for written skills. A URL source must be public and inside `jobEgress.cidrs`. No chat or API token to manage the library from scripts.
- Readiness scan: advice only, no drafting of `AGENTS.md` or CODEOWNERS and no merge requests. It reads the default branch, on switch-on, on request and once after the upgrade, never on a schedule. The limits of a scan Job (5 minutes, 2 GiB, 512Mi, 2 at once) are fixed. On GitHub, for a branch with classic protection, the approvals row (when no ruleset requires reviews), the protected row (when no ruleset stops pushes and no required check is shown) and the required checks row (when no required check is shown) say Couldn't check unless the review App has the optional Administration: Read permission.
- One replica, and PostgreSQL is external.
- GitHub.com only. `github[].api_url` accepts another URL, but GitHub Enterprise Server is untested.
- On GitHub, approving after the approve App is installed starts with the next push or ready-for-review; there is no catch-up pass over open pull requests.

## 🔧 Config reference

The config file is YAML (`pergent-sh serve --config <path>`, default `/etc/pergent-sh/config.yaml`). Unknown keys are an error. It holds no secrets: tokens and keys are named by environment variable. Every problem found is reported at startup, and pergent.sh does not start with an invalid config, a missing secret, a database it cannot reach, or, for an instance in the config file, a GitLab token that does not work or a GitHub App key or approve token that does not work (instances added in the dashboard never stop startup). The error names the instance and what failed, never the key or token. Without `dashboard.url`, at least one `gitlab` or `github` instance and an `llm:` block are required; with it, both can be added in the [dashboard](#-dashboard) instead. `dashboard.admins`, the old admin groups and teams, was removed: a config that still has it is refused at startup (`dashboard.admins was removed: admins are now teams in the dashboard (see the README, "Teams")`).

| Field | Default | Description |
| --- | --- | --- |
| `listen` | `:8080` | Address of the HTTP server. |
| `public_url` | required | HTTPS base URL where GitLab and GitHub reach pergent.sh, without a trailing slash. The GitLab webhook is created as `<public_url>/webhooks/gitlab/<instance name>`; the GitHub review App's webhook URL is `<public_url>/webhooks/github/<instance name>`. |
| `callback_url` | `public_url` | Base URL where Jobs post their result (`<callback_url>/runs/<id>/result`). `http` or `https`, no trailing slash. Set it to the in-cluster Service to keep that traffic inside the cluster. |
| `skills_dir` | removed | The skill library lives in the dashboard now. A config that still has it is refused at startup (`skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`). |
| `gitlab[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. |
| `gitlab[].url` | required | Base URL of the GitLab instance. It must be `https` (the bot tokens travel in a header), without a trailing slash. A redirect to another host is never followed. |
| `gitlab[].review_token_env` | required | Name of the env var with the review bot token. |
| `gitlab[].approve_token_env` | required | Name of the env var with the approve bot token. It must belong to a different user than the review bot token. |
| `gitlab[].webhook_secret_env` | required | Name of the env var with the webhook secret. GitLab sends it as `X-Gitlab-Token`. |
| `github[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. One instance is one pair of Apps and covers every organization they are installed on. |
| `github[].api_url` | `https://api.github.com` | GitHub's REST API. Only GitHub.com is supported: another `https` URL (GitHub Enterprise Server) is accepted but untested. |
| `github[].review_app.app_id` | required | The review App's numeric App ID. |
| `github[].review_app.private_key_env` | required | Name of the env var with the review App's private key. |
| `github[].approve_with_review_app` | one of `approve_with_review_app`, `approve_app` and `approve_token_env` | `true` to let the review App approve too, so one App is enough. The review App then needs the Pull requests: Read and write permission, and the pull request shows the same bot commenting and approving. No second key or token is read. |
| `github[].approve_app.app_id`, `.private_key_env` | one of `approve_with_review_app`, `approve_app` and `approve_token_env` | A separate approve App: its App ID and the env var with its private key, so the identity that writes the review is never the one that approves it. Both must differ from `review_app`'s (to approve with the review App, set `approve_with_review_app: true`). |
| `github[].approve_token_env` | one of `approve_with_review_app`, `approve_app` and `approve_token_env` | Name of the env var with the approve bot user's fine-grained personal access token, instead of an App. See [Approving as a bot user](#approving-as-a-bot-user). |
| `github[].webhook_secret_env` | required | Name of the env var with the review App's webhook secret. GitHub signs each delivery with it (`X-Hub-Signature-256`). |
| `gitlab[].oauth_client_id_env`, `gitlab[].oauth_client_secret_env` | none | Names of the env vars with the instance's OAuth application ID and secret, for [dashboard](#-dashboard) sign-in. Both or neither; with neither, nobody signs in with this instance. |
| `github[].oauth_client_id_env`, `github[].oauth_client_secret_env` | none | Names of the env vars with the review App's client ID and client secret, for [dashboard](#-dashboard) sign-in. Both or neither. |
| `dashboard.url` | none (dashboard off) | The dashboard's `https` address, a bare host without a port, path or trailing slash. Setting it turns the dashboard on, with password sign-in and the first password admin; instances and the LLM can then be added there. To let people sign in with GitLab or GitHub, an instance needs an OAuth client (`oauth_client_*_env` in the config, or entered in the dashboard). The sign-in callbacks are `<dashboard.url>/auth/<gitlab or github>/<instance name>/callback`. |
| `dashboard.admin_password_env` | none | Name of the env var with the first password admin's password (at least 12 characters), for the account `admin` made on the first start with the dashboard on. Empty or unset: pergent.sh generates a password and logs it once. The chart fills the variable from `adminPassword.existingSecret`. Needs `dashboard.url` to have an effect. See [First sign-in](#first-sign-in). |
| `repositories[].gitlab` | one of `gitlab` and `github` | Name of one of the `gitlab` instances. |
| `repositories[].github` | one of `gitlab` and `github` | Name of one of the `github` instances. |
| `repositories[].project` | required | Project path, such as `acme/widgets`; on GitHub exactly `owner/name`, matched case-insensitively. Each project appears once per instance. |
| `repositories[].skills` | the default list | Names of library skills to run: a non-empty list, or leave it out for the default list (an empty list is refused). A name that is not in the library does not stop startup: it is listed under "Needs attention" and the repository's runs fail naming it until the skill exists. |
| `repositories[].repo_skills` | none | Names of the skills found in the repository's `.agents/skills`, `.claude/skills` and `.github/skills` folders that run, besides the `.pergent/skills` ones, which always run. A turned-on name that is not found in the target branch fails the run (`skill "x" is turned on but not found in the repository`). The dashboard's switches on a repository's Settings are the same setting for a repository the dashboard manages. See [Skills and instructions](#-skills-and-instructions). |
| `repositories[].approve` | `true` | Approve clean reviews. With `false`, pergent.sh still reviews, comments and sets the status, but never approves, and withdraws any approval the approve bot already made in the repository. The dashboard's "Approve clean reviews" switch is the same setting for a repository the dashboard manages. See [Turning approval off](#turning-approval-off). |
| `repositories[].review_drafts` | `true` | Review drafts. With `false`, a draft merge request (GitLab `draft`, or `work_in_progress` on older versions) or draft pull request is not reviewed: no run, comment, status or approval, until it is marked ready, which starts its review. Closing or merging is handled as before. The dashboard's "Review drafts" switch is the same setting for a repository the dashboard manages. With `true`, a draft is reviewed but approved only once it is marked ready. See [Pushes, edits and failures](#pushes-edits-and-failures). |
| `repositories[].failure_status` | `failed` | Status used when a review could not be done: `failed` or `neutral`. GitLab has no neutral state, so `neutral` is set as `skipped`; a project can count skipped pipelines as successful through a GitLab setting. On GitHub it is the Check Run conclusion `failure` or `neutral`, and GitHub counts a `neutral` conclusion as passing for a required status check: with `neutral`, a failed review does not block the merge. `failed` is never counted as successful. Blocking findings are always `failed`. |
| `repositories[].job_timeout` | `jobs.timeout` | The review time budget of this repository's Jobs, such as `20m`. Must be positive. |
| `repositories[].job_memory` | `jobs.resources.limits.memory` | The memory limit of this repository's Jobs, such as `6Gi`. Only the memory limit changes; it must not be below `jobs.resources.requests.memory`. |
| `repositories[].inline_findings` | `true` | Post each finding as a comment on its lines, with a one-click fix when it is small. With `false`, every finding stays in the review comment. The dashboard's "Inline findings" switch is the same setting for a repository the dashboard manages. |
| `repositories[].incremental_reviews` | `true` | A push reviews only the files changed since the last review and carries the rest over. With `false`, every push is a full review. See [Incremental reviews](#-incremental-reviews). The dashboard's "Incremental reviews" switch is the same setting for a repository the dashboard manages. |
| `repositories[].max_turns` | `jobs.max_turns` | pergent's step limit per opencode run (`--max-turns`) for this repository, 5 to 200. The dashboard's "Max steps" is the same setting for a repository the dashboard manages. |
| `repositories[].commands_author_and_maintainers_only` | `true` | Only the author of a change and the repository's maintainers and owners can use [commands](#-commands) on it. With `false`, anyone with write access can. The author can always re-run and dismiss suggestions. The dashboard's "Only the author and maintainers use commands" switch is the same setting for a repository the dashboard manages. |
| `repositories[].rerun_limit_per_mr` | 10 | How many times a merge request or pull request can be re-run by [comment or button](#-commands) per UTC day, 1 to 1000. Pushes and opened requests always run and never count. |
| `repositories[].rerun_limit_per_repo` | 50 | The same cap for the whole repository, 1 to 1000. |
| `llm.provider` | required with an `llm:` block | opencode provider ID, such as `anthropic`. The whole `llm:` block is optional when `dashboard.url` is set (a password admin sets the LLM in the dashboard); a block that is present wins and makes the LLM read-only there, and any field set makes it present: it must then be complete (`provider`, `model` and the key variable). Without `dashboard.url` it is required. |
| `llm.model` | required with an `llm:` block | Model name, such as `claude-sonnet-4`. |
| `llm.base_url` | none | Custom API base URL for the provider. |
| `llm.api_key_env` | `OPENCODE_API_KEY` | Name of the env var with the LLM key. |
| `jobs.namespace` | required | Namespace the Jobs run in. |
| `jobs.image` | `ghcr.io/gopaytech/pergent:v0.5.0` | The pergent image the Jobs run. It must be pinned by a tag other than `latest`, or by digest. |
| `jobs.max_running` | `5` | Most Jobs running at once. Further runs wait in the queue. At least 1. |
| `jobs.timeout` | `30m` | pergent's own review budget per Job (`--timeout`). The Job's deadline adds 2 minutes to deliver and a clone allowance of at least 2 minutes. At startup pergent.sh warns about a timeout shorter than its step limit times 30 seconds. |
| `jobs.max_turns` | `40` | pergent's step limit per opencode run (`--max-turns`) for every repository, 5 to 200. |
| `jobs.concurrency` | `4` | pergent's `--concurrency`: opencode runs in flight inside one Job. At least 1. |
| `jobs.resources` | requests 500m CPU, 1Gi memory; limits 2 CPU, 4Gi memory | Kubernetes `requests` and `limits` for both containers of a Job. If you set either `requests` or `limits`, the default is not used at all. |

## 🛠️ Development

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

All other tests need no external service. Release images are built by the `release` workflow when a `v*.*.*` tag is pushed and published as `ghcr.io/gopaytech/pergent.sh:<tag>` for linux/amd64 and linux/arm64. The `ci` workflow also publishes `ghcr.io/gopaytech/pergent.sh:main` and `:sha-<short sha>` (both platforms) on every push to `main`, after the test and chart jobs pass; `latest` is only set by releases.

## 🚢 Publishing the chart (at release time)

The chart is published at `https://gopaytech.github.io/pergent.sh/charts/releases` (GitHub Pages, from `main`). To publish a version, bump `version` (and `appVersion`) in `charts/pergent-sh/Chart.yaml` and set `image.tag` in `charts/pergent-sh/values.yaml` to the release version (for example `v0.2.0`, with `image.pullPolicy: IfNotPresent`): `main` is a moving development build and must not ship in a released chart. Then run:

```bash
make helm.create.releases   # packages the chart into charts/releases and refreshes its index.yaml
```

and commit `charts/releases/` together with the bump, push `main`, then push the `v<version>` tag so the release workflow builds the image the chart names. The `.nojekyll` file at the repository root makes Pages serve the files as they are. `make readme` regenerates the chart reference (`charts/pergent-sh/README.md`) from the comments in `charts/pergent-sh/values.yaml` with [helm-docs](https://github.com/norwoodj/helm-docs); run it after changing a value.

## 📄 License

MIT. See [LICENSE](LICENSE).
