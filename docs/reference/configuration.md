# 🔧 Config reference

The config file is YAML (`pergent-sh serve --config <path>`, default `/etc/pergent-sh/config.yaml`). Unknown keys are an error. It holds no secrets: tokens and keys are named by environment variable. Every problem found is reported at startup, and pergent.sh does not start with an invalid config, a missing secret, a database it cannot reach, or, for an instance in the config file, a GitLab token that does not work or a GitHub App key or approve token that does not work (instances added in the dashboard never stop startup). The error names the instance and what failed, never the key or token. Without `dashboard.url`, at least one `gitlab` or `github` instance and an `llm:` block are required; with it, both can be added in the [dashboard](../concepts/dashboard.md) instead. `dashboard.admins`, the old admin groups and teams, was removed: a config that still has it is refused at startup (`dashboard.admins was removed: admins are now teams in the dashboard (see the README, "Teams")`).

| Field | Default | Description |
| --- | --- | --- |
| `listen` | `:8080` | Address of the HTTP server. |
| `metrics_listen` | none (metrics off) | Address of a second server that answers only `GET /metrics`, such as `:9090`: the [Prometheus counters](../concepts/metrics.md). Empty or left out: no metrics. It must be `host:port` or `:port` with a port from 1 to 65535, and must not use `listen`'s port on the same address or on every address. The chart sets it from `metrics.enabled` and `metrics.port`, and refuses it in `config`. |
| `public_url` | required | HTTPS base URL where GitLab and GitHub reach pergent.sh, without a trailing slash. The GitLab webhook is created as `<public_url>/webhooks/gitlab/<instance name>`; the GitHub review App's webhook URL is `<public_url>/webhooks/github/<instance name>`. |
| `callback_url` | `public_url`; the chart's Service with the chart | Base URL where Jobs post their result (`<callback_url>/runs/<id>/result`). `http` or `https`, no trailing slash. Without the chart, set it to the in-cluster Service to keep that traffic inside the cluster. |
| `skills_dir` | removed | The skill library lives in the dashboard now. A config that still has it is refused at startup (`skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`). |
| `gitlab[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. |
| `gitlab[].url` | required | Base URL of the GitLab instance. It must be `https` (the bot tokens travel in a header), without a trailing slash. A redirect to another host is never followed. |
| `gitlab[].review_token_env` | required | Name of the env var with the review bot token. |
| `gitlab[].approve_token_env` | one of `approve_token_env` and `approve_with_review_bot` | Name of the env var with the approve bot token. It must belong to a different user than the review bot token. |
| `gitlab[].approve_with_review_bot` | one of `approve_token_env` and `approve_with_review_bot` | `true` to let the review bot's own user approve too, with no approve token. See [Requirements](../setup/install.md#-requirements) for what this gives up. |
| `gitlab[].webhook_secret_env` | required | Name of the env var with the webhook secret. GitLab sends it as `X-Gitlab-Token`. |
| `gitlab[].trusted_authors_only` | `follow_visibility` | The default of the instance's repositories for [Trusted authors](../concepts/trusted-authors.md): `follow_visibility` (on for a public project, off for an internal or private one), `on` or `off` (every author reviewed). |
| `github[].name` | required | Instance name, `[a-z0-9-]+`, unique across `gitlab` and `github`. Part of the webhook URL. One instance is one pair of Apps and covers every organization they are installed on. |
| `github[].trusted_authors_only` | `follow_visibility` | As `gitlab[].trusted_authors_only`: on for a public repository, off for a private one. |
| `github[].api_url` | `https://api.github.com` | GitHub's REST API. Only GitHub.com is supported: another `https` URL (GitHub Enterprise Server) is accepted but untested. |
| `github[].review_app.app_id` | required | The review App's numeric App ID. |
| `github[].review_app.private_key_env` | required | Name of the env var with the review App's private key. |
| `github[].approve_with_review_app` | one of `approve_with_review_app`, `approve_app` and `approve_token_env` | `true` to let the review App approve too, so one App is enough. The review App then needs the Pull requests: Read and write permission, and the pull request shows the same bot commenting and approving. No second key or token is read. |
| `github[].approve_app.app_id`, `.private_key_env` | one of `approve_with_review_app`, `approve_app` and `approve_token_env` | A separate approve App: its App ID and the env var with its private key, so the identity that writes the review is never the one that approves it. Both must differ from `review_app`'s (to approve with the review App, set `approve_with_review_app: true`). |
| `github[].approve_token_env` | one of `approve_with_review_app`, `approve_app` and `approve_token_env` | Name of the env var with the approve bot user's fine-grained personal access token, instead of an App. See [Approving as a bot user](../setup/github.md#approving-as-a-bot-user). |
| `github[].webhook_secret_env` | required | Name of the env var with the review App's webhook secret. GitHub signs each delivery with it (`X-Hub-Signature-256`). |
| `gitlab[].oauth_client_id_env`, `gitlab[].oauth_client_secret_env`, `github[].oauth_client_id_env`, `github[].oauth_client_secret_env` | none | No longer used: sign-in with GitLab or GitHub is gone. An old config that names them still loads (both or neither); nothing reads the variables. |
| `dashboard.url` | none (dashboard off) | The dashboard's `https` address, a bare host without a port, path or trailing slash. Setting it turns the dashboard on, with password sign-in and the first local admin; instances and the LLM can then be added there. |
| `dashboard.sign_in` | none | How people other than the local admins sign in: OIDC, behind an email allowlist. Needs `dashboard.url`. When its `mode` is set it wins over what an admin saved on **Integrations**, which then shows it read-only. See [Sign-in](../concepts/dashboard.md#sign-in). |
| `dashboard.sign_in.mode` | none | `oidc`, or `jwt` (a proxy in front has logged the person in; see [Sign-in](../concepts/dashboard.md#sign-in)). Empty: only local admins sign in. |
| `dashboard.sign_in.label` | `SSO` | The name on the sign-in button, `Sign in with <label>`. At most 64 characters. |
| `dashboard.sign_in.issuer` | none | The provider's issuer URL, `https`, with no credentials or query. Discovery reads `<issuer>/.well-known/openid-configuration`. Required for `oidc` and `jwt`. For Google, `https://accounts.google.com`. |
| `dashboard.sign_in.client_id` | none | The OIDC client ID. Required for `oidc`. |
| `dashboard.sign_in.client_secret_env` | none | Name of the env var with the OIDC client secret. Required for `oidc`. Put the variable in the Secret named by the chart's `existingSecret`, which the pod mounts with `envFrom`. |
| `dashboard.sign_in.header`, `.audience`, `.jwks_url`, `.email_claim` | none | JWT validation: the header that carries the token, the audience and the `https` key-set URL (all three, and the issuer, required for `jwt`) and the claim that holds the email (`email` when empty). |
| `dashboard.sign_in.allowed_domains`, `.allowed_emails` | none | The allowlist: email domains and exact emails that may sign in, at most 500 each. Both empty: only emails an admin added. |
| `dashboard.admin_password_env` | none | Name of the env var with the first local admin's password (at least 12 characters), for the account `admin` made on the first start with the dashboard on. Empty or unset: pergent.sh generates a password and logs it once. The chart fills the variable from `adminPassword.existingSecret`. Needs `dashboard.url` to have an effect. See [First sign-in](../concepts/dashboard.md#first-sign-in). |
| `repositories[].gitlab` | one of `gitlab` and `github` | Name of one of the `gitlab` instances. |
| `repositories[].github` | one of `gitlab` and `github` | Name of one of the `github` instances. |
| `repositories[].project` | required | Project path, such as `acme/widgets`; on GitHub exactly `owner/name`, matched case-insensitively. Each project appears once per instance. |
| `repositories[].skills` | the default list | Names of library skills to run: a non-empty list, or leave it out for the default list (an empty list is refused). A name that is not in the library does not stop startup: it is listed under "Needs attention" and the repository's runs fail naming it until the skill exists. |
| `repositories[].repo_skills` | none | Names of the skills found in the repository's `.agents/skills`, `.claude/skills` and `.github/skills` folders that run, besides the `.pergent/skills` ones, which always run. A turned-on name that is not found in the target branch fails the run (`skill "x" is turned on but not found in the repository`). The dashboard's switches on a repository's Settings are the same setting for a repository the dashboard manages. See [Skills and instructions](../concepts/skills.md). |
| `repositories[].approve` | `true` | Approve clean reviews. With `false`, pergent.sh still reviews, comments and sets the status, but never approves, and withdraws any approval the approve bot already made in the repository. The dashboard's "Approve clean reviews" switch is the same setting for a repository the dashboard manages. See [Turning approval off](../concepts/approval.md#turning-approval-off). |
| `repositories[].review_drafts` | `true` | Review drafts. With `false`, a draft merge request (GitLab `draft`, or `work_in_progress` on older versions) or draft pull request is not reviewed: no run, comment, status or approval, until it is marked ready, which starts its review. Closing or merging is handled as before. The dashboard's "Review drafts" switch is the same setting for a repository the dashboard manages. With `true`, a draft is reviewed but approved only once it is marked ready. See [Pushes, edits and failures](../concepts/approval.md#pushes-edits-and-failures). |
| `repositories[].failure_status` | `failed` | Status used when a review could not be done: `failed` or `neutral`. GitLab has no neutral state, so `neutral` is set as `skipped`; a project can count skipped pipelines as successful through a GitLab setting. On GitHub it is the Check Run conclusion `failure` or `neutral`, and GitHub counts a `neutral` conclusion as passing for a required status check: with `neutral`, a failed review does not block the merge. `failed` is never counted as successful. Blocking findings are always `failed`. |
| `repositories[].job_timeout` | `jobs.timeout` | The review time budget of this repository's Jobs, such as `20m`. Must be positive. |
| `repositories[].job_memory` | `jobs.resources.limits.memory` | The memory limit of this repository's Jobs, such as `6Gi`. Only the memory limit changes; it must not be below `jobs.resources.requests.memory`. |
| `repositories[].inline_findings` | `true` | Post each finding as a comment on its lines, with a one-click fix when it is small. With `false`, every finding stays in the review comment. The dashboard's "Inline findings" switch is the same setting for a repository the dashboard manages. |
| `repositories[].incremental_reviews` | `true` | A push reviews only the files changed since the last review and carries the rest over. With `false`, every push is a full review. See [Incremental reviews](../concepts/incremental-reviews.md). The dashboard's "Incremental reviews" switch is the same setting for a repository the dashboard manages. |
| `repositories[].learn_from_dismissals` | `true` | Later reviews are told what people with write access rejected, and why. With `false`, reviews are not told. The dashboard's "Learn from dismissals" switch is the same setting for a repository the dashboard manages. |
| `repositories[].quiet_reviews` | `false` | A review says less: fewer nitpicks, no suggestion threads and a shorter comment, with blocking findings, failures, verdicts and approval unchanged. See [Quiet reviews](../concepts/quiet-reviews.md). The dashboard's "Quiet reviews" switch is the same setting for a repository the dashboard manages. |
| `repositories[].comment_layout` | the default layout | The blocks of the review comment, in order, each open or collapsed. A list of one-key entries, `block: state`, such as `- findings: collapsed`. The blocks are `summary`, `findings`, `dismissed`, `checks` and `files`; the states are `open` and `collapsed`; `findings` is required. A block left out is not in the comment. The config refuses a list with an unknown block, an unknown state, a block twice, no `findings` block, or no entries (`comment_layout: []` is an error, not the same as leaving the key out). For an unknown block, an unknown state and a block listed twice the error names the entry; the other two have no entry to name. See [Comment layout](../concepts/comment-layout.md). A review reads the layout when it starts and keeps it, so a change takes effect with the next review. The layout changes the comment only: no verdict, status, score or approval depends on it. The dashboard's layout editor is the same setting for a repository the dashboard manages, and a repository listed in the config reads no admin default. |
| `repositories[].linked_repos_enabled` | `false` | Whether the repository's [linked repositories](../concepts/linked-repositories.md) are used. While it is `false` the links are kept and ignored. A repository listed in the config reads no admin default. |
| `repositories[].linked_repos` | none | Up to 3 links, GitHub repositories only: a list of `target` (`owner/name`), `watched` (globs of this repository's files; none matches every change) and `can_block` (default `true`). The config refuses a GitLab repository with links, a bad glob, a target that is the repository itself or is listed twice, and more than 3 links. An empty list (`linked_repos: []`) is allowed and means no links. See [Linked repositories](../concepts/linked-repositories.md). |
| `repositories[].max_turns` | `jobs.max_turns` | pergent's step limit per opencode run (`--max-turns`) for this repository, 5 to 200. The dashboard's "Max steps" is the same setting for a repository the dashboard manages. |
| `repositories[].commands_author_and_maintainers_only` | `true` | Only the author of a change and the repository's maintainers and owners can use [commands](../concepts/commands.md) on it. With `false`, anyone with write access can. The author can always re-run and dismiss suggestions. The dashboard's "Only the author and maintainers use commands" switch is the same setting for a repository the dashboard manages. |
| `repositories[].rerun_limit_per_mr` | 10 | How many times a merge request or pull request can be re-run by [comment or button](../concepts/commands.md) per UTC day, 1 to 1000. The full review pergent.sh restarts by itself after a failed merge of an incremental result counts too. Pushes and opened requests always run and never count. |
| `repositories[].rerun_limit_per_repo` | 50 | The same cap for the whole repository, 1 to 1000. |
| `repositories[].trusted_authors_only` | the instance's | `on` or `off`: whether only [trusted authors](../concepts/trusted-authors.md) are reviewed without someone starting it. |
| `repositories[].trusted_authors` | none | The repository's allowlist: a list of `{id, username}`, the platform's numeric user ID (required) and the username, for reading. |
| `llm.provider` | required with an `llm:` block | opencode provider ID, such as `anthropic`. The whole `llm:` block is optional when `dashboard.url` is set (an admin sets the LLM in the dashboard); a block that is present wins and makes the LLM read-only there, and any field set makes it present: it must then be complete (`provider`, `model` and the key variable). Without `dashboard.url` it is required. |
| `llm.model` | required with an `llm:` block | Model name, such as `claude-sonnet-4`. |
| `llm.base_url` | none | Custom API base URL for the provider. |
| `llm.api_key_env` | `OPENCODE_API_KEY` | Name of the env var with the LLM key. |
| `jobs.namespace` | required | Namespace the Jobs run in. |
| `jobs.image` | `ghcr.io/gopaytech/pergent:v0.8.0` | The pergent image the Jobs run. It must be pinned by a tag other than `latest`, or by digest. |
| `jobs.max_running` | `5` | Most Jobs running at once. Further runs wait in the queue. At least 1. |
| `jobs.timeout` | `30m` | pergent's own review budget per Job (`--timeout`). The Job's deadline adds 2 minutes to deliver and a clone allowance of at least 2 minutes. At startup pergent.sh warns about a timeout shorter than its step limit times 30 seconds. |
| `jobs.max_turns` | `40` | pergent's step limit per opencode run (`--max-turns`) for every repository, 5 to 200. |
| `jobs.concurrency` | `4` | pergent's `--concurrency`: opencode runs in flight inside one Job. At least 1. |
| `jobs.work_size` | `20Gi` | The limit of a review Job's work volume, which holds the checkout. A change that needs more ends the Job with a failed review. |
| `jobs.resources` | requests 500m CPU, 1Gi memory; limits 2 CPU, 4Gi memory | Kubernetes `requests` and `limits` for both containers of a Job. If you set either `requests` or `limits`, the default is not used at all. |

# Instances and the LLM in the config file

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

For GitHub, put the Apps' private keys (the whole `.pem` file of each) and the review App's webhook secret in the Secret and use `github:` instead of `gitlab:`; see [GitHub](../setup/github.md). Without `dashboard.url`, pergent.sh needs at least one instance and the `llm:` block in the config file, and instances can only come from there.

# Environment variables

| Variable | Used for |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL. Required. |
| `PERGENT_SH_SECRET_KEY` | Encrypts the secrets you enter in the dashboard, the stored deploy tokens and the OIDC client secret (AES-256-GCM). 32 random bytes, standard base64. Required. **Changing it isn't supported** and there is no rotation: a stored secret that can't be opened with the new key shows as "not set" in the dashboard and must be entered again (the startup reconcile replaces the deploy tokens it can't open with new ones and revokes the old). |
| the variable named by `dashboard.admin_password_env` | The password of the first local admin, `admin`, at least 12 characters. Optional: without it pergent.sh generates one and logs it once. Read only on the start that creates the account. See [First sign-in](../concepts/dashboard.md#first-sign-in). |
| the variable named by `dashboard.sign_in.client_secret_env` | When `dashboard.sign_in.mode` is `oidc` in the config file: the OIDC client secret. Required then, and checked at startup: with the variable unset or empty pergent.sh does not start, and the error names the variable. With the sign-in set in the dashboard the secret is entered there. See [Sign-in](../concepts/dashboard.md#sign-in). |
| the variable named by `llm.api_key_env` (default `OPENCODE_API_KEY`) | When the LLM is in the config file: the LLM key handed to each Job. Required then, and checked at startup. With the LLM set in the dashboard, the key is entered there. |
| each `gitlab[].review_token_env`, `approve_token_env`, `webhook_secret_env` | When the instance is in the config file: the review bot token, the approve bot token (none with `approve_with_review_bot`) and the webhook secret of that instance. Required, and checked at startup. |
| each `github[].review_app.private_key_env` | When the instance is in the config file: the review App's private key, the whole PEM file GitHub gave you (PKCS#1 `RSA PRIVATE KEY` or PKCS#8 `PRIVATE KEY`). Required, and checked at startup. Never logged. |
| each `github[].approve_app.private_key_env` or `github[].approve_token_env` | When the instance is in the config file and doesn't set `approve_with_review_app`: the approve App's private key, or the approve bot user's fine-grained personal access token. Required, and checked at startup. With `approve_with_review_app: true` there is none: the review App's key approves. |
| each `github[].webhook_secret_env` | When the instance is in the config file: the review App's webhook secret. Required. |
| each `gitlab[].oauth_client_id_env`, `oauth_client_secret_env`, and the same on `github[]` | No longer used: signing in to the [dashboard](../concepts/dashboard.md) with GitLab or GitHub is gone. An old config that names them still loads, even half of a pair, and pergent.sh logs a warning for each instance that does; nothing reads the variables. A later release refuses them. |

pergent.sh also reads `KUBECONFIG` when it runs outside a cluster, which is handy for local development.
