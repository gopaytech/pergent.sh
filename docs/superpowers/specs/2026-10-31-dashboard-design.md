# pergent.sh Dashboard: Design

**Date:** 2026-10-31
**Status:** Agreed in brainstorming, including the builder check. A plan comes next.
**Roadmap:** this is piece 2c of `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md`. MR/PR commands (2d) and the skills library (piece 4) come after it, in that order, each with its own design.

---

## Why

pergent.sh reviews and approves GitLab MRs and GitHub PRs, but everything is set up in one YAML file, and you can only see what happened in the MR/PR comment and in the logs.

- **Platform/DevOps engineers** need to switch repositories on, change per-repository settings, and spot setup problems without editing the config file and redeploying.
- **Developers** need to see why their MR or PR wasn't approved: every run, every skill's verdict, every finding, and why a review failed.

The dashboard is also the base for the next two pieces. Command buttons and the skills editor live in it.

## Goals

- Anyone with a GitLab or GitHub account signs in with that account and sees only the repositories they can see on that platform.
- Admins switch repositories on and off and change per-repository settings in the browser.
- Repositories listed in the config file keep working as they do now, and are shown read-only.
- Every review run is visible with its result, skills, findings, files and logs, including runs that failed or were superseded.
- Setup problems pergent.sh already detects are shown on the page instead of only in the logs:
  - one approval is enough to merge;
  - the approve App or approve bot can't reach the repository;
  - reviews running out of memory;
  - a token about to expire.
- Deployment stays one image, one Deployment and one Helm chart.

## Non-goals (this piece)

- **Action buttons** (re-run review, re-run failed only, dismiss finding). They arrive with the MR/PR commands piece (2d). The pages leave room for them.
- **Editing skills** in the browser, and suggested skill improvements. These belong to the skills library (piece 4). This piece only turns library skills on or off per repository.
- **Analytics** beyond the Overview numbers (charts, trends, per-author stats).
- **Company identity providers** (Okta, Google Workspace, SAML) connected to pergent.sh directly. Sign-in goes through GitLab or GitHub, so company sign-in still applies there.
- **Editing connections in the browser:** GitLab instances, GitHub Apps, the LLM, Kubernetes and admin groups stay in the config file.
- Members, roles beyond admin/developer, and an audit log. These are the Organization piece.

## Anti-goals (a failure even if it works)

- A user sees a repository, review, finding or log they can't see on GitLab or GitHub.
- A developer can change setup, or turn approval on or off.
- The dashboard changes a repository that the config file manages.
- A token, key or secret appears in any page, API response, log view or browser storage.
- Turning "approve clean reviews" off leaves an existing bot approval in place.
- The dashboard makes the review engine slower or less reliable. A dashboard bug must never block reviews.

## Decisions (agreed)

### Users and sign-in

- **Sign-in:** "Sign in with GitLab" and "Sign in with GitHub" buttons, using OAuth. pergent.sh has no passwords.
- **Visibility:** each user sees only the repositories pergent.sh reviews that they can access on that platform.
- **Admins:** members of a GitLab group or GitHub team named in the config file. Admins get the Admin menu, the on/off switch for each repository, and repository settings.
- **One platform per sign-in.** Someone who uses both GitLab and GitHub signs in with one account at a time, and sees that platform's repositories. Linking accounts can come with the Organization piece.
- **Admin rights are per instance.** A user is an admin only on the instance whose group or team they belong to.
- **Admins act only on repositories they can see themselves.** The "add repository" picker shows repositories that both the admin and the review bot or App can reach. Integrations shows only the admin's own instance. Every write checks visibility as well as the admin flag.
- **"Can see" means can read the code and MRs/PRs**, because findings and logs contain code. On GitLab that means Reporter or above, or a public project. On GitHub it means read access to the repository.
- **Developers:** everyone else who signs in. They see Overview, Reviews and Repositories for their own repositories, read-only.

### Look

- Like CodeRabbit or GitLab: a dark menu on the left, content on the right.
- **Menu:**
  - Overview, Repositories, Reviews and Skills;
  - an **Admin** section with Integrations and Settings.

  Organization appears only when that piece lands.
- The mockups agreed in brainstorming are the reference: `overview`, `review-detail` and `repo-settings`. They are kept with this document, in `docs/superpowers/specs/2026-10-31-dashboard-mockups/`.

### Pages

- **Overview** (the page users land on):
  - a row of numbers for the last 7 days: reviews, approved by pergent, blocking findings and failed reviews;
  - **Recent reviews**: MR/PR, title, outcome pill (approved, running, blocking, draft, failed), reason, platform and age;
  - **Needs attention**: setup problems for the repositories the user can see.
- **Reviews:** a list that can be filtered by repository, result and time. Each MR/PR has a page with:
  - the MR/PR header and a link to it on GitLab or GitHub;
  - **review history**: every run, with its commit, trigger, result (blocking, approved, superseded, failed and why) and duration;
  - for the selected run:
    - the status shown on the platform, whether it was approved and why not;
    - the pergent version, model, tokens, and files reviewed and skipped;
  - **skills**: each skill's verdict, and whether it came from the library or the repository;
  - **tabs:** Findings (severity, title, ID, file and line, skill, explanation, fix), Checks, Files (reviewed, skipped with the rule, not reviewed) and Logs.
- **Repositories:** the list, showing platform, on/off, the last 7 days and health. Each repository has these tabs:
  - **Reviews** (filtered);
  - **Settings**;
  - **Skills & instructions**: a read-only view of the resolved skills and instructions for the target branch;
  - **Setup checks**: the "Needs attention" items for this repository.
- **Repository Settings** (admins only):
  - **approve clean reviews**, on or off (new);
  - the status when a review fails: `failed` or `neutral`;
  - review drafts, on or off. Off means no review while it's a draft, then a review once it's marked ready. This is an engine change: today drafts are always reviewed;
  - which library skills are on;
  - the review Job's timeout and memory limit.

  Each setting shows **"overridden here"** or **"default"**, and "Reset to defaults" clears all overrides.

  Switching review on or off is the repository's on/off switch in the list, not a separate setting. Off removes the webhook and the deploy token, as today.
- **Integrations** (admins, read-only, for the admin's instance):
  - each GitLab instance and GitHub App, and whether it's connected;
  - installation status for each repository;
  - token expiry, for example the bot-user approver token.

### Repository setup lives in both places

- Repositories listed in the config file appear with a **"managed by config"** label. Their settings are read-only in the dashboard, and the file stays the source of truth.
- Repositories switched on in the dashboard are stored in Postgres and edited there.
- **A repository can't be in both.** When a repository is listed in config, the config takes it over and its dashboard record is deleted on the next reconcile.
- **Removing a repository from config switches it off**, as today. It does not fall back to the dashboard.
- **Handing a repository to the dashboard:** remove it from config, then switch it on in the dashboard.
- **Every per-repository setting also has a config key** (approve clean reviews, failure status, review drafts, skills, Job timeout and memory), so config-managed repositories can use everything the dashboard offers.
- The defaults that "default" refers to are the config file's defaults (for example `jobs.timeout`) and pergent.sh's built-in defaults.

### Approve clean reviews (new setting)

- When off, pergent.sh still reviews, comments and sets the status, but never approves.
- The comment says `**Not approved:** approval is turned off for this repository.`, matching the existing notes.
- **Turning it off withdraws any approval the bot has already made in that repository**, using the same path that removes stale approvals today.
- Config key: `approve: false` on a repository.

### Logs

- When a review Job ends, pergent.sh keeps **the last 200 KB** of the pergent container's log.
- Secrets are removed with the same redaction used for comments and failure reasons. This covers the LLM key, the git token and the result token.
- Kept logs are **deleted after 30 days**.

### Where the dashboard is reachable

- **Webhooks and result callbacks stay on `public_url`.** For GitHub.com, `public_url` must be reachable from the internet.
- **The dashboard gets its own address**, `dashboard.url`, served through a **separate, optional Ingress** in the chart, for example an internal host. It is off by default, and the existing webhook Ingress never exposes `/`, `/api/` or `/auth/`.
- OAuth callbacks use `dashboard.url`.

### Tech

- **Frontend:** React + TypeScript, built with Vite.
- **Packaging:** the built files are embedded in the `pergent-sh` binary with `go:embed`, so there's still one image, one Deployment and one Helm chart.
- **API:** the Go service gets a **JSON API** under `/api/` that the pages call. The same API later backs the commands buttons and the skills library.
- The webhook and result-callback routes don't change.

## My calls (made while writing; the user can overturn any)

- **Sign-in apps:**
  - **GitLab:** one OAuth application per GitLab instance, with scope `read_user read_api`. Config gains `oauth_client_id_env` and `oauth_client_secret_env` per instance.
  - **GitHub:** sign-in uses the review App's own user-authorization settings (its client ID and secret), so no second app is created. Config gains `oauth_client_id_env` and `oauth_client_secret_env` on the GitHub instance.
  - **GitHub admin check (agreed):** the review App gets one more read-only permission, **Organization: Members: read**, so team membership can be checked. The README's App permission list changes. There are no existing installs yet, so no re-approval is needed.
- **Sessions:**
  - **Cookie:** an HTTP-only, Secure, SameSite=Lax cookie holding a random session ID.
  - **Stored:** the session lives in Postgres with the user's platform, instance, user ID, username and admin flag.
  - **Expiry:** sessions expire after 8 hours of inactivity and are deleted after 7 days.
  - **The user's OAuth token** is stored sealed with the existing `PERGENT_SH_SECRET_KEY`, because pergent.sh needs it to check access. It is never sent to the browser.
- **Access checks:**
  - **Repositories:** the list of repositories a user can see is checked against GitLab or GitHub with that user's token at sign-in. It is refreshed after **5 minutes**, when the user next asks for it.
  - **Admin:** admin status comes from membership of the named group or team, and is refreshed the same way.
  - **Every API call** filters by this list on the server. The browser is never trusted to filter.
- **Config:** `dashboard.admins` lists `{instance, group}` for GitLab and `{instance, team: org/slug}` for GitHub. With no admins configured, nobody gets the Admin menu, and startup logs a Warn.
- **API writes:**
  - every write (settings, switching a repository on) is admin-only;
  - writes require a CSRF token tied to the session;
  - writes to a config-managed repository are refused with 409.
- **Switching a repository on in the dashboard:**
  - the admin picks from the repositories the review bot or App can reach on that instance;
  - pergent.sh then runs the same reconcile it runs for config repositories (hook, deploy token or installation);
  - switching it off removes them, as removing it from config does today.
- **Calls to GitLab/GitHub from the API:** allowed only for sign-in, access checks, the "add repository" picker and the Skills & instructions view, with short timeouts. The picker uses the admin's token intersected with what the review bot or App reaches. Skills & instructions shows the resolved skills and instructions on the **default branch**, read with the review bot's token and cached for **5 minutes**. Nothing here runs on the engine loop.
- **Admin > Settings:** a read-only view of the resolved defaults (Job timeout and memory, failure status, approval, LLM provider and model, but never keys) and the configured admin groups or teams.
- **Skills page:** a read-only list of library skills, with their contents, visible to every signed-in user. Editing comes with the skills library.
- **Numbers on Overview:** computed from the `runs` table on each request, for the user's visible repositories. No separate analytics store.
- **"Needs attention" items:** stored per repository by the hourly reconcile and the watcher, and cleared when the condition goes away. They are not recomputed on page load.
- **Route layout:**
  - the dashboard is served at `/`;
  - its API is at `/api/`;
  - OAuth callbacks are at `/auth/gitlab/{instance}/callback` and `/auth/github/{instance}/callback`;
  - `dashboard.url` is the base for all of them.
- **Chart:** one new optional Ingress for the dashboard (`dashboardIngress`), off by default. The Service is shared.

## Constraints

- **Public repository:** example names only, no employer or internal names.
- **pergent.sh's safety rules still hold:**
  - reviews are read-only;
  - a failed, partial or draft review is never approved;
  - tokens that can comment, approve or write never reach a Job, a log, a page or the browser.
- **One replica**, with the Recreate rollout strategy (unchanged). Sessions in Postgres survive restarts.
- **The review engine's single loop must not wait on dashboard requests.** API handlers only read and write Postgres, apart from the OAuth and access-check calls to GitLab and GitHub.
- **Go 1.24** for the backend. The frontend build is pinned and reproducible: a lock file, and a Node version that CI pins.
- **CI:** builds the frontend, runs its type check, lint and tests, then the Go build embeds the result. The Docker image build does the same, with no runtime Node.

## Left to the builder

- The frontend's libraries: router, data fetching, component library or plain CSS, chart rendering. Keep them few, and match the mockups' look.
- The JSON API's exact shape, pagination and error format.
- Table names and columns for overrides, sessions, logs and "needs attention" items, plus the migrations.
- How the review Job's log is fetched at the end of the run: Kubernetes pod logs read by the watcher before the Job's TTL.
- Test strategy for the frontend (component tests plus API contract tests), and how the end-to-end tests cover sign-in with fake OAuth servers.
