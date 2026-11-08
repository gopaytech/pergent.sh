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
    <a href="docs/setup/github.md">GitHub setup</a> ·
    <a href="docs/concepts/dashboard.md">Dashboard</a> ·
    <a href="docs/concepts/skills.md">Skills</a> ·
    <a href="docs/concepts/commands.md">Commands</a> ·
    <a href="docs/reference/configuration.md">Config reference</a>
  </p>
</div>

---

pergent.sh is a service that runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests. It runs inside your Kubernetes cluster and starts one Kubernetes Job per review.

## ✨ Features

- **A Job per review**: each review runs as its own Kubernetes Job in your cluster, started by a merge request or pull request webhook, so reviews run in parallel up to `jobs.max_running`.
- **One comment, one status**: pergent.sh posts a single comment, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub.
- **Inline findings**: each finding is also posted as a thread on its lines, with a one-click fix when it is small, and resolved when a later push fixes it. See [Inline findings](docs/concepts/inline-findings.md).
- **Incremental reviews**: a push reviews only the files it changed and carries the rest over from the last review. See [Incremental reviews](docs/concepts/incremental-reviews.md).
- **Learn from dismissals**: a review is told what people rejected earlier in the repository, and why, so a false positive stops coming back. See [Learn from dismissals](docs/concepts/learn-from-dismissals.md).
- **Quiet reviews**: a repository can ask for a review that says less: fewer nitpicks, no suggestion threads and a shorter comment, with everything that blocks still there. Off by default. See [Quiet reviews](docs/concepts/quiet-reviews.md).
- **Comment layout**: a repository can choose which blocks of the review comment it shows (summary, findings, dismissed, checks, files), in what order, and whether each starts open or collapsed. See [Comment layout](docs/concepts/comment-layout.md).
- **Skill results**: a tab for each repository shows how many of each skill's findings people fixed, dismissed or left open, so you can fix or drop the weak skills. See [Skill results](docs/concepts/skill-results.md).
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
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub. It also posts each finding as a thread on its lines ([Inline findings](docs/concepts/inline-findings.md)).
4. If the review found no blocking issue (one that people dismissed doesn't count), actually reviewed something and skipped no changed file except under the rules that leave it reviewed (outside a skill's scope, excluded by a skill, no content changes), the approver approves it: a second GitLab user on GitLab, on GitHub the review App itself (one App is enough), a second GitHub App or a bot user. A draft is reviewed the same way but is approved only once it is marked ready.

People can also act on a review where they already are, in a comment on the merge request (`@<bot> review`, `@<bot> review failed`, `@<bot> dismiss <id> <reason>`) or with the buttons on the dashboard's review page: run the review again, run only the skills that failed, and set aside a finding that isn't a real problem. See [Commands](docs/concepts/commands.md).

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the LLM key and a one-time token to hand in its result.

This release covers GitLab (gitlab.com or any self-managed instance) and GitHub.com; see [GitHub](docs/setup/github.md) for the GitHub setup. An optional web [dashboard](docs/concepts/dashboard.md) shows every review, lets admins manage teams, write and install skills, switch repositories on and change their settings, and lets admins add GitLab and GitHub instances, their secrets and the LLM without editing the config file. An instance added in the dashboard never blocks startup: when pergent.sh can't reach it, it starts anyway and the dashboard shows what to fix. Comment [commands](docs/concepts/commands.md) and the dashboard's buttons re-run a review or dismiss a finding; they never change settings, answer questions or run code (see [Limits](docs/reference/limits.md)).

In this README a "merge request" is also a GitHub pull request, and the "review bot" and "approve bot" are the GitHub review App and approver, unless a section says otherwise.

## 📋 Requirements

- Kubernetes, and permission to create a Role in the namespace where the Jobs run.
- PostgreSQL 13 or newer, reachable from the pergent.sh pod. pergent.sh creates and migrates its own tables at startup.
- Nodes that can pull `ghcr.io/gopaytech/pergent.sh` (the service) and `ghcr.io/gopaytech/pergent` (the Jobs). Both images are built for `linux/amd64` and `linux/arm64` (pergent.sh from v0.4.0, pergent from v0.1.2), so the nodes can be either; each node pulls the one for its own platform.
- On each GitLab instance you use, two users, or one with `approve_with_review_bot`:
  - A **review bot** with the **Maintainer** role on every reviewed project, and an access token with the `api` scope. It creates the project webhook and the read-only deploy token, posts the comment and sets the commit status, and, for [commands](docs/concepts/commands.md), reacts to and answers comments and looks up the role of whoever wrote one.
  - An **approve bot** that is an eligible approver of every reviewed project, with an access token with the `api` scope. It must be a different user from the review bot: pergent.sh doesn't start when both tokens belong to the same user, unless the instance approves with the review bot (below). It only approves, and removes its approval when the merge request gets a new commit or a new target branch. If the project requires specific approvers or forbids approval by certain users, make sure this bot qualifies.

    **One user for both.** When a second user is hard to get, set `approve_with_review_bot: true` in place of `approve_token_env`, or choose **The review bot (one user)** when you add the instance in the dashboard, where it can't change afterwards. There is then no approve token, and the review bot must be an eligible approver too. This gives up the split: the account that writes a review is the one that approves it, with the Maintainer role, and anything else that uses the account can approve as well. pergent.sh can't tell that user's other approvals from its own, and GitLab can refuse the approval on a merge request that user wrote. Changing it in the config file leaves the old approve bot's approvals in place until they are removed by hand, or by GitLab's "Remove all approvals when commits are added to the source branch".

    > **Warning:** if a project needs only one approval and the approve bot is an eligible approver, the bot's approval alone unlocks the merge: a merge request can then be merged on a model's review without any human approving it. Give every reviewed project an approval rule that requires a human approval (for example, one approval from a group the approve bot is not in), so the bot's approval is never enough on its own.

    pergent.sh checks this at startup and every hour: for each reviewed project it reads the approval rules that apply to the default branch (`GET /projects/:id/approval_rules`; with none, the project's `approvals_before_merge`) and, when the most any of them requires is one or none, logs a warning naming the project: "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval". It doesn't know whether the approve bot is in a rule's approver list, so it warns whenever the count allows it. If the rules can't be read, that is logged once at info level; the check never stops a review.
- For GitHub, one GitHub App, which reviews and approves (a second App, or a bot user, can approve instead); see [GitHub](docs/setup/github.md).
- An API key for an LLM provider that opencode supports. It can be entered in the [dashboard](docs/concepts/dashboard.md) or kept in the Secret.
- A way for GitLab or GitHub to reach pergent.sh over HTTPS. The chart creates only a Service; you route to it (Gateway API, your own Ingress, a load balancer). See [Exposing pergent.sh](#exposing-pergentsh).
- For the optional [dashboard](docs/concepts/dashboard.md): an HTTPS host for it. Local admins sign in with a password and need no identity provider; everyone else is an SSO account (identified by its email) that an admin puts on a [team](docs/concepts/dashboard.md#teams).

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

   Keep `PERGENT_SH_SECRET_KEY`: it encrypts every secret you enter in the dashboard and the stored deploy tokens. Changing it isn't supported; see [Environment variables](docs/reference/configuration.md).

   Instances, their tokens and keys, and the LLM key are entered in the dashboard (step 5), so they need no keys in the Secret. If you keep them in the config file instead, put their keys in this Secret too; see [Instances and the LLM in the config file](docs/reference/configuration.md).

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

   The skill library is not a value: skills are written and installed in the dashboard, so a review has none until an admin adds some ([Skills and instructions](docs/concepts/skills.md)).

4. Install:

   ```bash
   helm repo add pergent-sh https://gopaytech.github.io/pergent.sh/charts/releases
   helm repo update
   helm install pergent-sh pergent-sh/pergent-sh -n pergent-sh -f values.yaml
   ```

   A released chart runs the image of its own release (chart `0.2.0` runs `ghcr.io/gopaytech/pergent.sh:v0.2.0`). To run the newest build of the `main` branch instead (every push to `main` publishes `main` and `sha-<short sha>`), install from a checkout with `helm install pergent-sh charts/pergent-sh ...` and `--set image.tag=main --set image.pullPolicy=Always`; that tag moves, so a pod restart can pick up a newer build.

5. Set up in the browser. With `dashboard.url` set and no instance in the config, pergent.sh starts and waits for one. Take the generated admin password from the log ([First sign-in](docs/concepts/dashboard.md#first-sign-in)), sign in as `admin` and change it, then add a GitLab or GitHub instance under **Integrations** ([Instances and secrets in the dashboard](docs/concepts/dashboard.md#instances-and-secrets-in-the-dashboard)), set the LLM under **Settings**, and switch repositories on. Then set up [sign-in](docs/concepts/dashboard.md#sign-in) under **Integrations** and add people under **Users**: a person waits until an admin puts them on a [team](docs/concepts/dashboard.md#teams). To set the first password from a Secret instead of the log, see [First sign-in](docs/concepts/dashboard.md#first-sign-in).

On startup pergent.sh creates the project webhook and a read-only deploy token on every GitLab project that is switched on. You do not create either by hand. The webhook sends merge request events and note (comment) events; the note events are what [commands](docs/concepts/commands.md) are read from. A hook created by an earlier version is updated to send them too, at the next reconcile (startup, then every hour). On GitHub there is nothing per repository to create: the review App's webhook covers every repository it is installed on, provided the App subscribes to the events listed under [Create the review App](docs/setup/github.md#1-create-the-review-app).

What the chart creates:

- A Deployment with **one replica** and the `Recreate` strategy. Do not scale it up: at startup pergent.sh requeues runs it had claimed but not launched, which is only safe when no other pod is dispatching. The pod runs as non-root with a read-only root filesystem, drops all capabilities, and has startup, readiness and liveness probes on `/healthz` (the startup probe allows 150 s, as pergent.sh starts listening only after the database and migrations and, for each instance in the config file, its GitLab tokens or GitHub Apps check out; instances added in the dashboard never delay it). The pod restarts when the config changes. When `adminPassword.existingSecret` is set, the pod also gets that Secret's `adminPassword.key` as the environment variable named by `config.dashboard.admin_password_env` (see [First sign-in](docs/concepts/dashboard.md#first-sign-in)).
- A Service on port 8080 (`service.type`, `ClusterIP` by default). No Ingress: see [Exposing pergent.sh](#exposing-pergentsh).
- A ServiceAccount, and in `jobsNamespace` a Role and RoleBinding for it: `batch/jobs` (create, get, list, watch, patch, delete), `secrets` and `configmaps` (create, get, delete), `pods` (get, list) and `pods/log` (get, to keep each review's log). There are no wildcard rules and nothing outside `jobsNamespace`.
- A NetworkPolicy in `jobsNamespace` for the Job pods, review Jobs, [skill fetch Jobs](docs/concepts/skills.md#installing-from-git) and [readiness scan Jobs](docs/concepts/dashboard.md#readiness-scan) alike (those labelled `app.kubernetes.io/managed-by: pergent-sh`): no ingress; egress to DNS (UDP and TCP 53), to TCP 443 and 80 in `jobEgress.cidrs`, and to TCP 8080 of the pergent.sh pods for the result callback. A NetworkPolicy only takes effect when your CNI enforces them.
- A ConfigMap for `config.yaml`.

**Narrow `jobEgress.cidrs`.** The default `0.0.0.0/0` lets a Job reach any address on ports 80 and 443, including in-cluster services and the cloud metadata endpoint. Set it to the addresses of GitLab (or GitHub) and the LLM provider. An empty list (`cidrs: []`) allows no external egress at all: Jobs then reach only DNS and pergent.sh's port 8080. The optional `jobEgress.except` list is added to every `ipBlock` as NetworkPolicy `except` (for example `169.254.169.254/32`); it applies to every cidr, and Kubernetes rejects an entry outside its cidr, so use it with a broad cidr such as the default `0.0.0.0/0`.

**Public skill sources need `jobEgress.cidrs` too.** A skill fetch Job clones a public `https` URL under the same NetworkPolicy, so the host of every URL admins install skills from must be inside `jobEgress.cidrs`. With a narrow list, add the address of that host (for example GitHub's, for skills on github.com), or the fetch fails with `could not fetch <url> at <ref>; the URL, branch or tag may be wrong, or outside jobEgress.cidrs`. Skills of a repository on a connected GitLab or GitHub instance are read by the pergent.sh pod through the platform's API, not by a Job, and need nothing here.

`config.jobs.namespace` must equal `jobsNamespace` and `config.listen` must end in `:8080`; the chart refuses to render otherwise. The chart's `skills` value was removed with `skills_dir`: the skill library lives in the dashboard, and a values file that still sets `skills` fails to render.

The chart sets `callback_url` to its own Service (`http://pergent-sh.pergent-sh.svc:8080` for the install above), so Jobs post their results inside the cluster, over plain HTTP. Set `config.callback_url` to send them elsewhere. If that is a hostname the Job pods reach from outside the cluster, they need egress to it on 443 (covered by the default `jobEgress.cidrs`).

### Exposing pergent.sh

The chart creates a Service (`pergent-sh.pergent-sh.svc:8080` for the install above) and nothing in front of it. Route to it with whatever your cluster uses: a Gateway API `HTTPRoute`, your own Ingress, or `service.type: LoadBalancer`. Terminate TLS in front of it; pergent.sh serves plain HTTP.

| Paths | Who calls them | Where to expose |
|---|---|---|
| `/webhooks/` | GitLab and GitHub | `public_url`'s host. GitHub.com and your GitLab must reach it. |
| `/runs/` | the review Jobs (result callback) | nowhere: the chart points `callback_url` at its Service. Only a `callback_url` you set to a public host needs it there. |
| `/skill-fetches/` | the skill fetch Jobs (result callback, only with the dashboard on) | as for `/runs/`. |
| `/readiness-scans/` | the readiness scan Jobs (result callback, only with the dashboard on) | as for `/runs/`. |
| `/`, `/api/`, `/auth/` | people, in the [dashboard](docs/concepts/dashboard.md) | `dashboard.url`'s host, over https. It can be internal-only. |
| `/healthz` | probes | nowhere; the kubelet calls the pod directly. |

Expose only `/webhooks/` on the public host, so the dashboard and its sign-in stay off the internet. With a public `callback_url`, add `/runs/`, `/skill-fetches/` and `/readiness-scans/`. Whatever sits in front must set `X-Forwarded-For` (see [Password sign-in limits](docs/concepts/dashboard.md#password-sign-in-limits)).

## 📚 Documentation

**Setup**

| | |
| --- | --- |
| [GitHub](docs/setup/github.md) | Create the review App, approvals, branch protection |
| [GitLab project settings](docs/setup/gitlab.md) | Settings that make the approval a real gate |
| [Config reference](docs/reference/configuration.md) | Every config key and environment variable |
| [Upgrading](docs/upgrading.md) | What to do for each release, and how to roll back |

**Concepts**

| | |
| --- | --- |
| [Dashboard](docs/concepts/dashboard.md) | Sign-in, teams, repositories, readiness, audit log |
| [Skills and instructions](docs/concepts/skills.md) | How a review picks its skills, and the skill library |
| [Commands](docs/concepts/commands.md) | `@<bot> review`, `review failed`, `dismiss` |
| [How approval works](docs/concepts/approval.md) | When pergent.sh approves, and when it takes it back |
| [Usage](docs/concepts/usage.md) | Where the model tokens go |
| [Metrics](docs/concepts/metrics.md) | Prometheus metrics |
| [Skill results](docs/concepts/skill-results.md) | What became of each skill's findings |
| [Inline findings](docs/concepts/inline-findings.md) | Findings as threads on the diff |
| [Incremental reviews](docs/concepts/incremental-reviews.md) | Review only what changed |
| [Quiet reviews](docs/concepts/quiet-reviews.md) | Fewer nitpicks, shorter comments |
| [Comment layout](docs/concepts/comment-layout.md) | Choose what the review comment shows |
| [Learn from dismissals](docs/concepts/learn-from-dismissals.md) | Teach reviews what isn't a problem |
| [Linked repositories](docs/concepts/linked-repositories.md) | Check a change against the repositories that depend on it |
| [Trusted authors](docs/concepts/trusted-authors.md) | Who gets reviewed without being started |
| [What a Job can and cannot see](docs/concepts/job-isolation.md) | How a review is sandboxed |
| [Fork merge requests](docs/concepts/fork-merge-requests.md) | Changes from forks |
| [Switching a repository off](docs/concepts/switching-off.md) | What's removed and what's kept |

**Reference**

| | |
| --- | --- |
| [HTTP routes](docs/reference/http-routes.md) | Every route and who uses it |
| [Limits](docs/reference/limits.md) | What pergent.sh doesn't do |
| [OpenAPI](openapi.yaml) | The API spec |

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
