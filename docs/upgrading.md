# ⬆️ Upgrading

## Unreleased

**Teams can add their own models, and a review can move to a backup model.** Each review runs on the first usable entry of a model chain: the repository's own, its owner team's or the global one. When the provider says an entry is out of quota, out of credit or rejects its key, the review starts again on the next entry: [Models](concepts/models.md). Nothing changes after the upgrade. Every review runs on the model it used before, and nothing falls back until a chain has a second entry.

- **Migrations `0045` and `0046`** run at startup. Back up Postgres first. They add 4 tables (`team_models`, `model_chains`, `model_chain_entries`, `model_entry_marks`) and columns with a default or NULL (`llm_models.name` and `teams_may_use`, `team_repos.owner`, `dashboard_repos.chain_mode`, `model_chains.saved_by_admin`, and columns on `runs` and `run_usage`). One step is not an addition: `0045` drops the unique rule on the provider, model and base URL of `llm_models`, so one model can have several keys. That only relaxes a rule, and the previous release still runs on the migrated database. Existing entries are named `provider/model`, and a second copy of the same pair `provider/model (2)`. A repository that picked a model gets a chain of that one entry, saved by an admin, so it runs as before.
- **Fallback needs a pergent image that reports `provider_error`.** pergent v0.10.0, the default `jobs.image`, does not. The release notes name the first pergent release that does. With an older image nothing falls back and no entry is marked.
- **New config keys:** `llm.fallbacks` (up to 4 backup entries after `config`) and `fallback.quota_cooldown` (default `1h`): [Config reference](reference/configuration.md). The previous release refuses a config key it doesn't know.
- **Breaking, dashboard API.** The web app changes in the same release.
  - `PUT /api/repositories/{instance}/{project}/settings` refuses the `model` key with 400 `a repository's model is its model chain now: PUT .../chain, or POST .../chain/reset to inherit`. The settings view and **Reset to defaults** no longer have it. `RepositoryDetailView` loses `modelChoices`, and its `model` is the first entry of the chain in effect.
  - A repository's chain is `GET` and `PUT .../chain`. Going back to inheriting is `POST .../chain/reset`, because `DELETE` on a repository is the admins' **Remove**.
  - `DELETE /api/admin/models/{id}` no longer refuses a model that repositories or chains use. Those chains keep the entry and skip it as removed. The default is still refused.
  - `POST /api/admin/models` needs a `name`. The same provider, model and base URL twice is no longer refused.
  - `/teams` is now the user area's **Teams** page, for every user. It no longer sends an admin to `/admin/teams`: update old bookmarks.
- **Team maintainers can now spend money.** They add their team's own models and keys, and can put a global entry in their chains only where an admin turned on **Teams may use it** (off by default). A team's entries run only for the repositories it owns, and an admin ticks the owner on the admin **Teams** page.
- **The instance token quota counts global keys only.** Under a used-up instance quota, a review runs on a team entry when its chain has one, and the re-run button, the `review` command and **Start review** start it instead of refusing: [Monthly token quota](concepts/token-quota.md). The repository quota counts every token, as before.
- **GitHub:** every status write of a review changes one `pergent` Check Run in place, the cancel of a superseded review included. Before, each write created a new Check Run.
- **Metrics:** `pergent_model_fallback_total` and `pergent_model_entry_exhausted`, and a **Model chains** row in the example Grafana dashboard. A dropped attempt is left out of `pergent_reviews_total`, `pergent_reviews_no_result_total` and `pergent_skill_runs_total`, and kept in `pergent_tokens_total`: [Metrics](concepts/metrics.md).
- **API:** the new outcome `fell_back`; `RunDetailView` has `entry` and `attempts`; `ReviewSummary` has `attempts`; `UsageRepositoryRowView` and `UsageRepositoryView` have `globalTokens` and `teamTokens`. The new routes are in [HTTP routes](reference/http-routes.md).
- **Usage CSV:** a new last column, `source` (`global` or `team`). A script that reads the columns by position keeps working.
- **The audit log** has `chain_changed`, `teams_may_use_changed`, `owner_set`, `owner_cleared`, `team_model_added`, `team_model_updated`, `team_model_removed`, `team_models_deleted`, `model_marked` (by `pergent`) and `model_mark_cleared`. Model entries name the entry.
- **To roll back, first remove `llm.fallbacks` and `fallback.quota_cooldown` from the config.** The previous release ignores the new tables and columns:
  - a repository whose chain starts with a team entry, and every repository that follows a team chain, runs on the global default until the next upgrade. A repository chain that starts with a global entry runs on that entry;
  - nothing falls back, and each earlier attempt of a review shows as a run of its own;
  - saving a team in the previous release clears that team's owner ticks;
  - the instance quota counts tokens on team keys again.

**Repositories, teams and the instance can save a chain B.** It is the second model chain of a review panel: [Review panel](concepts/review-panel.md). It is experimental and off by default. Reviews do not use it yet, so nothing changes in a review.

- **Migration `0048`** runs at startup. It adds two tables (`panel_chains`, `panel_chain_entries`) and one column with a default (`dashboard_repos.panel_chain_mode`), dropping nothing. Back up Postgres first, as for any migration. The previous release keeps working on the migrated database and ignores them.
- Saving a chain B with entries is refused with `no review image supports a review panel yet`, until a release supports it. Clearing it and `off` always work.
- The audit log has `panel_chain_changed`. There is no config and no chart change.

**Repositories can turn on a code graph.** It is off by default, per repository, and nothing changes until both the install and a repository turn it on: [Code graph](concepts/code-graph.md). It is experimental: in our tests it did not lower the cost and did not find more bugs, so keep it off unless you are measuring it.

- **Migration `0044`** runs at startup. It adds one column with a default (`dashboard_repos.graph_context`, `false`) and one table (`repo_graphs`), dropping nothing. Back up Postgres first, as for any migration. The previous release keeps working on the migrated database and ignores both.
- **Chart:** new values `graphs.enabled` (default `false`), `graphs.persistence.size`, `graphs.persistence.storageClass` and `graphs.image`. With `graphs.enabled: false` the chart renders the same objects as before. `graphs.enabled: true` needs `config.dashboard.url`, adds a PersistentVolumeClaim, `fsGroup: 65532` on the pod and two environment variables, and uses the new image `ghcr.io/gopaytech/pergent.sh-graph`.
- The review Job passes the graph to pergent as the environment variable `PERGENT_GRAPH`. A pergent image older than the change that reads it, such as v0.10.0, ignores the variable and reviews without the graph, so turning the switch on never fails a review.
- With a public `callback_url`, route `/graphs/` like `/runs/`.
- The audit log has `graph_on` and `graph_off`. `RepositoryDetailView` has a new `graph` field.
- There is no config change, so a rollback needs no config edit. After a rollback the graphs volume and the `repo_graphs` rows stay unused.

**The switch on Repositories now pauses a repository instead of removing it.** A paused repository stays in the list with all its settings, its dismissals and its review history. pergent.sh takes its webhook and deploy token off the platform and reviews nothing for it until it is switched on again: [Pausing and removing a repository](concepts/switching-off.md). The switch calls `PUT /api/admin/repositories/{instance}/{project}` with `{"enabled": false}` to pause and `{"enabled": true}` to resume. **Remove repository**, on the repository's page, deletes it for good.

- **`DELETE /api/repositories/{instance}/{project}` now removes the repository for good, and also deletes its team links.** A script that used it to switch a repository off now loses the settings and team links: use the `PUT` above to pause instead.
- Team links left by repositories switched off before this release stay, and the Teams page shows them as `removed`. Untick them there.
- `POST /api/repositories` on a paused repository resumes it with its settings and answers `200`.
- The audit log has `repository_paused`, `repository_resumed` and `repository_removed`. Older entries keep `repository_off`.
- `RepositoryView` has a new `paused` field and `health` can be `paused`; `UsageRepositoryRowView` has a new `paused` field.

Migration `0042` runs at startup and adds one column with a default (`dashboard_repos.enabled`, `true`), so back up Postgres first, as for any migration. The previous release keeps working on the migrated database, **but it ignores the column: after a rollback every paused repository is switched on again** at the next reconcile. Remove the ones you don't want before you roll back. There is no config change.

**A repository can choose CI jobs to read and to gate on.** The setting is **CI jobs**, `repositories[].ci_jobs`: [CI jobs](concepts/ci-jobs.md). Nothing changes until a repository sets it.

Migration `0043` runs at startup and only adds columns (one to `dashboard_repos`, three to `runs`, two to `projects`) and one partial index, with NULL or a default, dropping nothing. Back up Postgres first, as for any migration.

- **GitHub: `Actions: Read` and the `Check run` event.** `Actions: Read` is needed only by repositories whose CI jobs read `log` or `report` from GitHub Actions; a `status` read uses Checks, which the App already has. The `Check run` event is optional: without it pergent.sh notices CI changes on its 2-minute check instead of at once. Add them under **Permissions & events** ([GitHub setup](setup/github.md)). GitHub applies a new permission to an installation only after its owner accepts it. Until then, logs and reports from Actions are `unreadable`. Readiness warns, for repositories that have CI jobs, while an installation lacks either one.
- **GitLab hooks gain Pipeline and Job events** on the next reconcile (startup, then every hour). pergent.sh turns them on itself. Repositories without `ci_jobs` ignore them.
- **The model sees the results only with a pergent image that reads `--ci-results`.** The version is named in the release notes. An older image ignores the file and reviews as before, and the gate still applies.
- **To roll back, first remove `repositories[].ci_jobs` from the config.** The previous release refuses an unknown key.
- **After a rollback, runs waiting for CI stay waiting** until someone starts them or a push supersedes them. GitLab hooks keep the extra events, and the previous release ignores them.

## Upgrading to 0.17.1

**On GitLab, `failure_status: neutral` now sets the `pergent` commit status to `success`.** It used to set `skipped`, which GitLab refuses once the status is `running`, so a review that couldn't be done kept its status `running` through about 50 minutes of retries and then ended `failed`, blocking the merge request. Now the pipeline passes. The review is still not approved, and its comment and the status description still say it failed.

**A review that can't be posted now ends with the repository's `failure_status`.** After the 10 publish attempts, the last status used to be `failed` whatever the repository asked for. With `failure_status: neutral` it is now `success` on GitLab and a `neutral` Check Run on GitHub, still with "pergent couldn't post its review" and never approved. A review with blocking findings is still `failed`, and so is one whose repository settings can't be read or whose repository is gone.

Repositories with `failure_status: failed` don't change. Nothing to migrate and no config change.

## Upgrading to 0.17.0

**Another system can be given API keys to the review API.** An admin makes a client under **API clients** and gives each caller a key: [Review API](setup/review-api.md). A caller can ask for a review (`POST /v1/reviews`), read it (`GET /v1/reviews/{id}`), get it by a signed callback to the client's callback URL when it ends, and dismiss its findings for the subject. Every client has limits: request size, files, reviews in flight, requests per hour and a monthly token budget. Nothing happens after the upgrade until an admin makes a client.

Migration `0038` runs at startup and only adds two tables, so back up Postgres first, as for any migration. The previous release keeps working on the migrated database and ignores them. There is no new config key, so to roll back nothing needs removing from the config. The `/v1/` routes exist only with `dashboard.url` set, as the dashboard does. Rolling back removes the clients and keys from use; they stay stored.

Migration `0039` runs at startup too. It adds a table, a sequence, two columns to `runs` (with a default, or NULL), two `CHECK` constraints (the inline `kind IN ('mr', 'api')` on the new `kind` column, and `runs_api_shape` on `runs`: an API review has no instance and no project) and indexes: `runs_api_client_idx` on `runs`, and four on the new table, among them the unique `api_reviews_idempotency_idx` (one Idempotency-Key once per client) and the partial `api_reviews_inputs_idx` (the reviews that still hold their files). It drops nothing. The previous release keeps running merge request reviews on the migrated database.

- **During a rolling upgrade, or after a rollback, the previous release meets API reviews it doesn't know.** It claims a queued one, finds no repository `''`, and supersedes it. It can't publish an API review that is reported. It lists API reviews in **Usage** under an empty repository. After a rollback, callers may see `superseded` reviews: they send their changes again.
- **Every limit of an API client holds.** A key past its client's requests per hour, and a client past its monthly token budget, get `429` ([Limits](setup/review-api.md#limits)). During a rolling upgrade or after a rollback, the previous release claims API reviews without the in-flight cap.

Migration `0040` runs at startup too. It adds four tables (`api_callbacks`, `api_callback_attempts`, `api_dismissals`, `api_review_events`) and two indexes (`api_callbacks_due_idx`, `api_review_events_run_idx`), dropping nothing. Back up Postgres first, as for any migration.

- **The pergent.sh pod needs egress to each client's callback URL.** It sends the callback itself, with a 10 second timeout, and follows no redirect. If your cluster limits what the pergent.sh pod may reach (a NetworkPolicy of your own, an egress proxy), allow the hosts admins give as callback URLs. The chart's NetworkPolicy covers the Job pods only. A callback that can't be sent is retried for 24 hours.
- **`/v1/` must be exposed for the callers,** on a host they reach, over https: [Exposing pergent.sh](setup/install.md#exposing-pergentsh). Nothing else of the dashboard needs to be public for them.
- **"Needs attention" has a new kind, `callback_failing`,** for an API client with 3 or more callbacks from the last 24 hours not delivered ([Dashboard](concepts/dashboard.md)). Admins see it; it clears when fewer are, or when the callback URL is emptied.
- **Metrics:** `pergent_api_reviews_total`, `pergent_api_tokens_total` and `pergent_api_callbacks_total`, labelled by `client` only, and an **API clients** row in the example Grafana dashboard ([Metrics](concepts/metrics.md)). The **Usage** page lists each client with its tenants.
- **Rolling back to the previous release stops the callbacks, the dismissals and the limits** with the rest of the API: it has no `/v1/` routes. Callbacks not yet sent are never sent, the results stay stored, and dismissals stay stored. API reviews the older release meets are superseded, as described above for migration `0039`: callers send their changes again.

**A repository can be asked to review deleted and renamed files.** The setting **Review deleted and renamed files** is off by default and per repository only: [Deleted and renamed files](concepts/deleted-files.md). Nothing changes after the upgrade until a maintainer or admin turns it on for a repository. It costs more tokens. It needs pergent v0.10.0 or newer, and an older image ignores it.

Migration `0041` runs at startup and only adds a nullable column, so back up Postgres first, as for any migration. The previous release keeps working on the migrated database and ignores the column. **To roll back, first remove `repositories[].check_deleted` from the config, if you set it.** The previous release refuses a config key it doesn't know. A value set in the dashboard stays stored and does nothing.

- **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.10.0`,** which reads the setting. A config that sets its own `jobs.image` keeps the image it names, so set it to v0.10.0 or newer to use the setting.

## Upgrading to 0.16.0

**A failed review can be traced to its Job.** Admins see the Job and pod on the review page, a failed review keeps its clone log and the pod's state, and the failure line names the step that failed: [Troubleshooting a failed review](setup/troubleshooting.md).

**Admins can give an instance and a repository a monthly token quota.** Once it is used up, an automatic review waits until the first push after the 1st or until an admin starts it, and a review asked for by hand is refused: [Monthly token quota](concepts/token-quota.md). Nothing is limited after the upgrade, and nothing needs turning on.

Migrations `0036` and `0037` run at startup. They add columns (nullable, or with a default), a table and an index, dropping nothing, so back up Postgres first, as for any migration. The previous release keeps working on the migrated database and ignores the new columns and table.

- **To roll back, first remove `gitlab[].monthly_token_quota`, `github[].monthly_token_quota` and `repositories[].monthly_token_quota` from the config, if you set them.** The previous release refuses a config key it doesn't know.
- **Rolling back removes the quota.** A quota set in the dashboard stays stored but nothing checks it. During a rolling upgrade, replicas of the previous release also start reviews without the check. They don't start a change a quota held by themselves, but their **Start review** and a writer's `review` command can.
- **The tokens counted are those in [Usage](concepts/usage.md).** A Job with no result and a superseded review count as zero.

- **The chart's Role needs `list` on `events`.** Apply the new chart. Without it a failed review's pod state has no events, and pergent.sh logs a warning, once per run. Nothing else is affected. If you manage the Role yourself, add `apiGroups: [""]`, `resources: ["events"]`, `verbs: ["list"]` to the one in `jobsNamespace`.
- **New Jobs carry labels and annotations.** The Job, pod, Secret and ConfigMap of a review get `pergent.sh/instance`, `pergent.sh/project-id`, `pergent.sh/mr` and `pergent.sh/head`, and the annotations `pergent.sh/project` and `pergent.sh/mr-url`. Jobs already running keep what they have.
- **Failure texts change wording, and name the step.** For example `the clone failed (checkout exited with code 128)` or `the review couldn't start: no node could run it`. A comment changes the next time it is updated. A text that matched `OOMKilled` still does.
- **A pod that can't be scheduled, and a Job that makes no pod, now fail the review after 10 minutes,** not at the Job's deadline. Pods whose containers are starting are not affected. The image error rule is unchanged, at 5 minutes. These reviews are not retried: press **Re-run**.
- **`pergent_reviews_no_result_total` has a new `step` label:** `starting`, `clone`, `review`, `delivery`, `unknown`, or `other` for a failure before a Job existed. Totals are unchanged, and `sum without (step) (...)` gives the old count. A query that already sums the metric needs no change. A repository with only superseded runs no longer gets a zero series. Reviews from before the upgrade count as `other`. See [Metrics](concepts/metrics.md).
- **"Needs attention" has a new kind, `clone_failing`,** for a repository whose clone failed twice or more in 7 days.
- **Reviews from before the upgrade have no clone log or pod state.** Only new failures keep them.

## Upgrading to 0.15.0

**Review comments end with a short text.** By default it links to pergent.sh. An admin can change it on the admin **Settings** page: [The footer text](concepts/comment-layout.md#the-footer-text). Existing comments change the next time they are updated.

Migration `0034` runs at startup and only adds a table, so back up Postgres first, as for any migration. The previous release keeps working on the migrated database, and to roll back nothing needs removing from the config: there is no new config key. It ignores the table, so its comments have no footer text.

## Upgrading to 0.14.0

**Reviews can run on a model from a list.** An admin keeps the list on the admin **Settings** page, picks the default, and can give a repository a model of its own: [Models](concepts/models.md).

Migration `0033` runs at startup and only adds a table and columns, so back up Postgres first, as for any migration. The previous release keeps working on the migrated database.

- **Nothing changes for your reviews.** The LLM you have set becomes the first entry and the default, and every repository follows the default. A config file's `llm:` block shows as a read-only entry. The upgrade is not a model change, so no full reviews follow it.
- **`GET` and `PUT /api/admin/llm` are replaced by `/api/admin/models`.** Move any script that calls them. The routes are in [HTTP routes](reference/http-routes.md).
- **The CSV export has a new last column, `provider`.** The others keep their places. The metrics are unchanged.
- **A model from a new provider needs its address in `jobEgress.cidrs`.** See [Models](concepts/models.md#a-new-provider-needs-network-access).
- **The LLM is copied into the list once, at the migration.** If you roll back, change the LLM on the older release and upgrade again, that change doesn't reach the list.
- **To roll back, nothing needs removing from the config:** there is no new config key. The default entry is kept in the old LLM setting, so the previous release runs on the current default. It has no models per repository, so after a rollback every repository runs on that default.

## Upgrading to 0.13.0

**Admins can see where the model tokens went.** [Usage](concepts/usage.md) is new, on the admin **Usage** page, and the Skill results tab has a cost section for admins. Nothing new appears for other people, and nothing needs turning on; only the review page's token count changes for everyone (below).

Migration `0032` runs at startup and adds a table and a column, dropping nothing, so back up Postgres first, as for any migration. 0.12.0 keeps working on the migrated database: it ignores both.

- **The usage of earlier reviews is filled in after the upgrade, in the background.** pergent.sh serves at once and reads the stored results one page at a time, oldest first, so the Usage page fills up as it goes. Only a review that left a result is filled in. For a merged incremental review filled in this way, the tokens of the `impact` skill are left out, except in a `review failed` re-run that ran `impact` itself. A result that cannot be read is marked and not read again.
- **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.9.0`,** which reports cache and reasoning tokens. A config that sets its own `jobs.image` keeps the image it names.
- **Cache and reasoning counts need the pergent release that reports them.** A Job that runs an older pergent shows them as unknown, not as zero. Input and output tokens are there for every review that left a result.
- **The review page counts a `review failed` re-run once.** It showed the tokens of the skills the re-run kept a second time.
- **Prometheus metrics are new and off by default.** Nothing needs doing. To turn them on, set `metrics.enabled: true` in the chart (or `metrics_listen` in the config) and scrape the new `-metrics` Service inside the cluster: see [Metrics](concepts/metrics.md). To roll back with them on, first set `metrics.enabled: false` (or remove `metrics_listen` from your config): the previous release refuses a config key it doesn't know.

## Upgrading to 0.12.0

**Public repositories stop reviewing strangers' changes automatically.** [Trusted authors](concepts/trusted-authors.md) is new and on for public repositories by default: a change from an author without write access, and not on the repository's allowlist, waits until someone with write access starts its review. Private and internal repositories are unchanged. To keep reviewing every author on an instance, set it to **always off**: `trusted_authors_only: off` under the instance in the config, or the admin **Integrations** page for an instance added in the dashboard.

Migration `0031` runs at startup and adds a run state, a run trigger and nullable or defaulted columns, dropping nothing, so back up Postgres first, as for any migration. 0.11.x keeps working on the migrated database: it never starts a waiting run, and shows one as `unknown`.

- **To roll back, first remove `gitlab[].trusted_authors_only`, `github[].trusted_authors_only`, `repositories[].trusted_authors_only` and `repositories[].trusted_authors` from the config.** 0.11.x refuses a config key it doesn't know.
- **Add repository on GitLab** lists 50 repositories at a time with **Load more**, instead of reading every project first, and switching on a project the review bot reaches as Maintainer through a parent group or a share now works.

## Upgrading to 0.11.0

Nothing changes unless you turn a new setting on. Migrations `0029` and `0030` run at startup and add nullable or defaulted columns, dropping nothing, so back up Postgres first, as for any migration. 0.10.0 keeps working on the migrated database.

- **[Linked repositories](concepts/linked-repositories.md)** is new, and off by default. With it on and a link set, a change that touches a watched path also gets an impact check, for callers it would break in the linked repository. A repository's own skill named `impact` is unaffected while the feature is off.
- **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.8.0`,** which has the impact check. A config that sets its own `jobs.image` to an older image gets a `failed` impact check on every matching review, and no approval, until it moves.
- **To roll back, first remove `linked_repos` and `linked_repos_enabled` from the config.** 0.10.0 refuses a config key it doesn't know.

- A GitLab instance can now approve with its review bot, with `approve_with_review_bot: true` in place of `approve_token_env`, or **The review bot (one user)** in the dashboard's add form. See [Requirements](setup/install.md#-requirements) for what this gives up. Two tokens of one user are still refused without it.
- **To roll back, first remove `approve_with_review_bot` from the config.** 0.10.0 refuses a config key it doesn't know, so it would not start with it. An instance added in the dashboard this way stays stored, but 0.10.0 can't connect it, because it has no approve token.

## Upgrading to 0.10.0

This release changes how people sign in and what they can reach, and it changes the review comment. Local admins keep working, and everyone else now signs in with OIDC or a JWT from your proxy ([Sign-in](concepts/dashboard.md#sign-in)). The order for an install:

1. **Upgrade.** Back up Postgres first. Migrations `0027` and `0028` run at startup and add columns and rows, dropping nothing, and every session ends, so everyone signs in again.
2. **Sign in as a local admin** at `/admin/sign-in` (the page for local admins; `/sign-in` has no password form). Your local admins and their passwords are unchanged. If you only ever signed in with GitHub or GitLab you may not know the `admin` password: it was logged once, when the account was made, and may be gone. Set a new one from inside the pod with `admin reset-password` ([Lost password](concepts/dashboard.md#lost-password)) before you start.
3. **Set up sign-in** in the **Sign-in** section of the admin **Integrations** page, or `dashboard.sign_in` in the config file. This can't be done before the upgrade.
4. **Add people** with **Add user** on the admin **Users** page (an email, and the teams they should be on), or from the admin **Teams** page.

What is gone, and what changes:

- **Signing in with GitHub or GitLab, and the platform check, are gone.** Nothing asks the platform what a person may read any more: the team is the only limit. Someone on a broad team reads findings and logs of repositories they can't open on the platform ([Limits of the access check](concepts/dashboard.md#limits-of-the-access-check)). People who signed in with GitHub or GitLab have no way in: their accounts stay in the database, unused and hidden.
- **Every team loses its members**, because only those accounts could be members. Teams keep their names and repositories. A team's old role isn't carried over: a member has a role on the team now, `viewer` or `maintainer`, and an admin adds people again by email.
- **Roles.** An account is an `admin` or a `user`. There is no per-instance admin: an admin reaches every repository and every admin page.
- **The admin pages moved to `/admin/...`.** An API route for admins answers anyone else `404` instead of `403`. `GET /api/instances` is now `GET /api/sign-in-options`, and `GET /api/integrations` is removed.
- **`hidden` is gone from the answer to deleting a skill that is in use** (409 `the skill is in use`). Only admins delete skills, and admins see every repository, so the answer lists them all.
- **`oauth_client_id_env` and `oauth_client_secret_env` warn and are ignored.** An instance in the config that still names them loads, and pergent.sh logs a warning for each at start. A later release refuses them.
- **The chart** drops the OAuth comments from `values.yaml`; add `dashboard.sign_in` under `config` and the client secret to the Secret `existingSecret` names if you set sign-in in the config file.
- **To roll back to 0.9.0, first remove `dashboard.sign_in` and `repositories[].comment_layout` from the config.** 0.9.0 refuses a config key it does not know, so it would not start with them. The rows this release added (SSO accounts, team members, the sign-in settings) stay in the database and are not used.

The review comment and the reviews:

- **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.7.0`.** A review now reads the change's title and description, which pergent.sh hands to the Job; a stated intent never excuses a blocking problem. A config that sets its own `jobs.image` keeps the image it names, and an older image ignores the description. The comment layout does not depend on the image: pergent.sh draws the comment itself from the result the Job sends.
- **The default comment changes for every repository.** The model's summary is back at the top, the checks are grouped by area and open, and the confidence reason shows only when the score is below 5. Anything that reads the comment's Markdown will need to follow. The hidden findings block does not change, so the next review reads the same findings as before.
- **A run from before this release has no recorded layout.** If it is published again (a dismissal does that), its comment takes the new default shape.
- **[Comment layout](concepts/comment-layout.md)** is a new per-repository setting for the comment's shape, unset by default. A null value means the default layout.

## Also in 0.10.0

The migration adds one table, `webhook_deliveries`. Back up Postgres first, as for any migration.

- Reviews can use the change's title and description. pergent.sh passes them to every Job, read when the Job starts, as data the author wrote; verification runs never get them. Editing them starts no review. An image older than the pergent release that reads them ignores them, so reviews keep working as before until `jobs.image` moves to it.
- Review Jobs now limit their work volume to `jobs.work_size`, 20Gi by default. A repository whose checkout is larger fails its review until you raise it.
- On GitLab, a comment command is read back from GitLab before it is acted on, so each one costs one more API call.
- The chart now sets `callback_url` to its own Service unless `config.callback_url` is set, so Jobs post their results inside the cluster over plain HTTP instead of through `public_url`. To keep the old path, set `config.callback_url` to your `public_url`. In a mesh that requires mTLS, check first that Jobs can reach the Service.
- GitHub: sending the same delivery again from the App's **Recent Deliveries** is ignored once it was handled. A delivery answered 5xx can still be sent again. To review a change again, use `review` or **Re-run review**.

## Upgrading to 0.9.0

This release adds [Quiet reviews](concepts/quiet-reviews.md): a review that raises fewer nitpicks, posts no suggestion threads and writes a shorter comment, with everything that blocks unchanged. It is off until you turn it on, so a repository that doesn't is reviewed as before.

- **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.5.5`.** That release changes nothing in the comment unless the review is quiet. A config that sets its own `jobs.image` keeps the image it names. The shorter comment does not depend on it: pergent.sh draws the comment itself from the result the Job sends.
- **Migration `0025` runs at startup.** It adds three nullable columns (`quiet_reviews` on `dashboard_repos`, `repo_defaults` and `runs`). A null value means off, and 0.8.2 keeps working on the migrated database, so you can roll back.
- **To roll back, first remove `quiet_reviews` from the config.** 0.8.2 refuses a config key it does not know, so it would not start with `repositories[].quiet_reviews` in it. The dashboard's setting needs no change: 0.8.2 ignores the column.

## Upgrading to 0.8.2

There is no migration, and nothing to change in the config or the chart. 0.8.1 keeps working on the same database, so you can roll back. Reviews still run pergent v0.5.4.

The **Integrations** page now shows each instance's webhook URL, with a copy button. [`openapi.yaml`](../openapi.yaml) at the root of the repository describes the HTTP API ([HTTP routes](reference/http-routes.md)).

## Upgrading to 0.8.1

Reviews now run pergent v0.5.4 (the default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.5.4`). There is no migration, and nothing to change in the config or the chart. 0.8.0 keeps working on the same database, so you can roll back.

**One action: check your library skills.** pergent v0.5.4 fails a skill whose `paths` or `exclude` has a pattern with bad glob syntax or one that can never match a file, such as `api/` (a directory is `api/**`) or `api/[x`. Every review that uses such a skill ends as `failed`, with the error `skills_invalid`. Open each library skill and fix the pattern. The skill editor and an install or fetch from git now refuse to save one, and the refusal names the pattern. A skill in the repository's own folders with a bad pattern fails the run with `<path>: <problem>`, and the readiness scan shows it as a problem. With a `jobs.image` that pins an older pergent, the pattern is still accepted by the reviewer and matches nothing.

The comment and the inline threads look a little different, because pergent now makes everything the model wrote inert:

- Code blocks lose their syntax highlighting. A block pergent shows has no language after its fence.
- A code span that is glued to other text, such as a word right before the backtick, shows the backticks as they are.
- Check cells in the tables have no code formatting.
- A bare URL that holds `@`, `[`, `]` or `|` is no longer a link.
- An inline thread has a blank line after its title.

A review can also read the patches of the changed files. pergent writes each one to a temporary folder in the Job and tells the agent where the index is, so a run can look at another file's change. The patches come from the checkout the agent already reads, and a file the read rules deny is left out.

## Upgrading to 0.8.0

Nothing to change in the config or the chart. Migration `0024` runs at startup, and 0.7.1 keeps working on the migrated database, so you can roll back.

- The migration builds `runs_decided_idx`, and building it blocks writes to `runs`. On a large `runs` table, expect a slower start. A very large table could outlast the chart's startup probe, which allows 150 seconds. The pod would then restart and begin the build again, so on such a table build the index first, without blocking writes, with `CREATE INDEX CONCURRENTLY IF NOT EXISTS runs_decided_idx ON runs (created_at) WHERE outcome <> ''`, the statement of the migration, which then has nothing to do. If that build fails it leaves an invalid index of the same name: drop it and run the statement again.
- The new **Skill results** tab has no numbers at first. They appear as changes merge or close ([Skill results](concepts/skill-results.md)).

## Upgrading to 0.7.1

Nothing to do. The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.5.3`, whose notes in the comment can't ping people or render model-written markdown. A thread told its finding was left out now says when the finding comes back, and an incremental review keeps at most 5 earlier left-out notes. The readiness scan no longer counts empty files as noise.

## Upgrading to 0.7.0

0.7.0 reviews only what a push changed, and tells each review what people rejected before. See [Incremental reviews](concepts/incremental-reviews.md) and [Learn from dismissals](concepts/learn-from-dismissals.md).

1. **Pushes are incremental by default**, also on repositories already running. Turn it off per repository with **Incremental reviews** in its Settings, or with `incremental_reviews: false` in the config, or change the default on the admin **Settings** page. The accepted cost: an edit that breaks an untouched file can go unnoticed until a full review (`@<bot> review`), and a team that rebases or amends gets no speed-up yet.
2. **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.5.2`.** A config that pins an older pergent keeps full reviews, because only pergent v0.5.2 and later report a version that allows it: v0.5.0 and v0.5.1 report 0.3.0. This is read from the image's tag (`v0.4.0`, also in front of a digest), so it holds even after running a newer pergent. An image pinned by digest alone to an older pergent can't be told apart, and neither can a tag that is not a plain version (`:prod`, `v0.4.0-fork`): turn **Incremental reviews** off first.
3. **The first push after the upgrade is a full review**, because the last result comes from an older pergent and has no skill hashes. Incremental reviews start with the next push.
4. **Reviews learn from dismissals by default**, also on repositories already running. See [Learn from dismissals](concepts/learn-from-dismissals.md). Turn it off per repository with **Learn from dismissals** in its Settings, or with `learn_from_dismissals: false` in the config, or change the default on the admin **Settings** page. Dismissals made before the upgrade count only when they were blocking: those always came from a maintainer. Older suggestion dismissals don't, because who made them wasn't stored. The feature needs pergent v0.5.0 or later in the Job; an older image ignores what it is given.
5. **Migrations `0020`, `0021`, `0022` and `0023` run at startup.** `0020` adds nullable columns and one `runs` column that is `NOT NULL DEFAULT false`. `0021` adds nullable columns. `0022` adds columns to `dismissals` that have a default, and an index, and marks the existing blocking dismissals as teaching. A step at startup then fills in the skill, file and title of dismissals from the last 90 days only, from the stored results; an older row, or one whose review has no stored result, never teaches. `0023` adds a column with a default to `inline_threads`. So 0.6.2 keeps working on a migrated database.
6. **To roll back to 0.6.2, first remove `incremental_reviews` and `learn_from_dismissals` from the config**, because 0.6.2 refuses unknown config keys, and let running reviews finish first: an incremental result is a partial one until 0.7.0 merges it, and 0.6.2 would publish it as a whole review. Results stored by 0.7.0 are complete, merged results.

## Upgrading to 0.6.2

0.6.2 limits who can use [commands](concepts/commands.md).

1. **Commands are now limited to the change's author and the repository's maintainers and owners**, also on repositories already running. Before, anyone with write access could re-run a review or dismiss a suggestion. To let anyone with write access use them, as before, turn the setting off per repository (**Only the author and maintainers use commands** in its Settings, or `commands_author_and_maintainers_only: false` in the config), or change the default on the admin **Settings** page.
2. **Migration `0019` runs at startup.** It adds nullable columns only, so 0.6.1 keeps working on a migrated database.
3. **A change's author with only read access**, for example someone opening a PR from a fork, can now re-run and dismiss suggestions in both modes, within the re-run caps.
4. **Refusals about who may now write `command_refused` audit entries** in both modes.
5. **To roll back to 0.6.1, first remove `commands_author_and_maintainers_only` from the config**, because 0.6.1 refuses unknown config keys. Values saved under 0.6.2 stay in the database across a rollback and come back on re-upgrade.

## Upgrading to 0.6.1

0.6.1 fixes the timeout default and how the admin **Settings** page follows the config.

1. **The default `jobs.timeout` is 30 minutes** (it was 10). Each run may take 40 steps since 0.5.4, and 10 minutes was too short for large reviews. A config that sets `jobs.timeout` keeps its value. pergent.sh now logs a warning at startup when a timeout in the config file allows less than 30 seconds a step (timeouts set in the dashboard are not checked; see 2).
2. **The admin Settings page saved before 0.6.1 pins every default.** That includes the old 10-minute timeout. Open the admin **Settings** page: a default saved there shows "Saved here; the config says …". Click **Use config value** on the ones that should follow the config, then save. Saving once lets every default equal to the config's value follow it again; **Use config value** is needed only where the saved value differs, such as the old 600-second timeout.
3. **Migration `0018` runs at startup.** It adds one column with a default, so 0.6.0 keeps working on a migrated database.

## Upgrading to 0.6.0

0.6.0 posts each finding as a thread on its lines. See [Inline findings](concepts/inline-findings.md).

1. **Migration `0017` runs at startup.** It adds a table and nullable or defaulted columns only, so 0.5.4 keeps working on a migrated database.
2. **Inline findings are on by default.** Turn them off per repository with **Inline findings** in its Settings, or with `inline_findings: false` in the config, to keep the single comment.
3. **GitHub: give the review App Pull requests Read and write and subscribe it to Pull request review comment.** Without the permission, findings stay in the comment. Without the event, in-thread dismissals are not received. Each installation's owner has to accept the new permission.
4. **The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.4.0`.** A config that sets its own `jobs.image` keeps the image it names.
5. **To roll back to 0.5.4, first remove `inline_findings` from the config.** 0.5.4 refuses unknown config keys and won't start with it. Threads already posted stay on the platform.

## Upgrading to 0.5.4

0.5.4 makes the step limit of a review configurable per repository.

1. **The default step limit rises from 20 to 40.** Set it with `jobs.max_turns` in the config, or per repository with `repositories[].max_turns` or **Max steps** in the repository's Settings, 5 to 200. A review that hits the step limit now says where to raise it.
2. **Migration `0016` runs at startup.** It adds nullable columns only, so 0.5.3 keeps working on a migrated database.
3. **`jobs.timeout` stays 10 minutes.** It bounds the whole review, so a repository that raises `max_turns` may also need a higher `job_timeout`.
4. **To roll back to 0.5.3, first remove `jobs.max_turns` and `repositories[].max_turns` from the config.** 0.5.3 refuses unknown config keys and won't start with them.

## Upgrading to 0.5.3

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

## Upgrading to 0.5.2

0.5.2 changes only the default `jobs.image`, to `ghcr.io/gopaytech/pergent:v0.2.0`. With that image, a read outside the repository no longer ends the review without an answer. There is no migration.

## Upgrading to 0.5.1

0.5.1 changes only the default `jobs.image`, to `ghcr.io/gopaytech/pergent:v0.1.3`. That image ships ripgrep, so the model's file search (glob and grep) works in review Jobs; with v0.1.2 every search failed and reviews fell back to reading files one by one. A config that sets its own `jobs.image` keeps using the image it names: point it at v0.1.3 or newer. There is no migration.

## Upgrading to 0.5.0

0.5.0 adds the [readiness scan](concepts/dashboard.md#readiness-scan). Nothing is removed, but these things change:

1. **Migration `0015` runs at startup.** It adds the `readiness_scans` table. Back up PostgreSQL first and keep the backup: 0.4.0 does not know the new table.
2. **`/readiness-scans/` must be reachable by the Jobs**, like `/runs/` ([Exposing pergent.sh](setup/install.md#exposing-pergentsh)). A scan Job that can't post its result ends as `the scan took longer than 5 minutes` or `the scan Job ended without a result`. The scan Jobs use the NetworkPolicy the chart already makes for review Jobs, so there is no new chart value; the clone needs the GitLab or GitHub address in `jobEgress.cidrs`, as a review does.
3. **Scans need the dashboard** (`dashboard.url`) and the `PERGENT_SH_IMAGE` variable the chart sets. Without the dashboard no scan runs.
4. **Every repository that is switched on is scanned once after the upgrade**, if it was never scanned: oldest first, 2 at a time, starting before the first reconcile. These scans are not in the audit log. Expect new "Needs attention" items for repositories that lack project context or skill coverage.
5. **Optional:** to let the scan read classic branch protection on GitHub, give the review App **Administration: Read-only** ([Create the review App](setup/github.md#1-create-the-review-app)). Nothing breaks without it.

## Upgrading to 0.4.0

0.4.0 adds `linux/arm64` images; nothing else changes and there is no migration. The default `jobs.image` is now `ghcr.io/gopaytech/pergent:v0.1.2`, which is also built for both platforms. A config that sets its own `jobs.image` keeps using the image it names: to run review Jobs on arm64 nodes, point it at pergent v0.1.2 or newer (v0.1.1 and older are amd64 only).

## Upgrading to 0.3.0

0.3.0 moves the skill library from the chart into the database and the dashboard. These changes break an install of 0.2.0 until you act:

1. **The chart's `skills:` value is gone**, with its ConfigMap and volume. A values file that still sets it fails to render: `skills was removed: skills live in the dashboard (see the README, "Skills and instructions")`.
2. **`skills_dir` is gone from the config.** A config that still has it is refused at startup: `skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`. Nothing is imported: the new library starts empty.
3. **A repository with no `skills` setting now uses the default skill list**, not every skill. The default list starts empty. A repository whose `skills` is `[]` in the config is refused at startup (leave the key out for the default list; a list names at least one skill).
4. **With no skills, there is no review.** A repository that has no skill in its `.pergent/skills` folder and none turned on, and whose list (or the default list) names no library skill, is skipped: no Job, no comment, no status ([No skills, no review](concepts/skills.md#no-skills-no-review)). Until the library has skills and the default list (or the repository) names one, that is every repository without `.pergent/skills`.
5. **Migration `0014` runs at startup.** It adds the library, the default skill list and the new run columns, and lets a run be `skipped`. Back up PostgreSQL first and keep the backup: 0.2.0 does not know the new tables.
6. **Skill fetch Jobs** read public URLs. The chart passes `PERGENT_SH_IMAGE` for them (set it to the pergent.sh image if you do not use the chart). `/skill-fetches/` must be reachable by the Jobs like `/runs/`, and the host of a URL must be inside `jobEgress.cidrs` ([Exposing pergent.sh](setup/install.md#exposing-pergentsh), [Installing from git](concepts/skills.md#installing-from-git)). Without the dashboard (`dashboard.url`) there is no skill fetching.

To upgrade:

1. Before you upgrade, save the text of every skill in your old `skills:` value, and note which repositories listed `skills`.
2. Remove `skills:` from your values and `skills_dir` from your config, and upgrade.
3. Sign in as an admin. Create the skills again with **New skill** on **Skills**, or install them from git ([Installing from git](concepts/skills.md#installing-from-git)), tick **Add to default skills** for the ones every repository should run, and set a repository's own list under its **Settings** where it had one. The overview's setup checklist shows what is left.
