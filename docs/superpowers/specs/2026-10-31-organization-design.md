# pergent.sh Organization: Design

**Date:** 2026-10-31
**Status:** Agreed in brainstorming and two grilling rounds, including the builder check. A plan comes next.
**Roadmap:** this is piece 5, "Organization", of `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md`, pulled forward. It comes right after the dashboard (2c, `docs/superpowers/specs/2026-10-31-dashboard-design.md`) and before MR/PR commands (2d). It replaces the dashboard's admin model and its "connections stay in the config file" non-goal.

---

## Why

Today everything that connects pergent.sh to the world lives in env vars and the config file: GitLab tokens, GitHub App keys, webhook secrets, OAuth clients, the LLM key. Dashboard admins are whoever is in a GitLab group or GitHub team named in that file. Changing any of it means editing a Secret or the config and redeploying.

The user wants pergent.sh to work like other self-hosted tools (SonarQube, Grafana, Argo CD, Harbor):

1. A built-in admin to start with, signing in with a username and password.
2. Everyone else signs in with their GitLab or GitHub account.
3. A person signing in for the first time has no access until an admin puts them on a team.
4. Admins create teams inside pergent.sh and decide what each team gets.
5. Connections and their secrets are entered in an admin panel, not in env.

## Goals

- A fresh install is set up entirely from the browser: the built-in admin signs in, adds GitLab and GitHub instances with their tokens, keys and OAuth clients, sets the LLM key, creates teams, and switches repositories on.
- Only `DATABASE_URL` and `PERGENT_SH_SECRET_KEY` must come from env.
- Admins manage users and teams: a team has members, a role, and the repositories it covers.
- A new user waits on a "waiting for an admin" page until they are put on a team.
- The config file and env keep working for everything they do today, and win over the browser, so GitOps installs are unaffected.

## Non-goals (this piece)

- **OIDC, SAML, LDAP sign-in.** Users sign in with GitLab or GitHub only (see "My calls").
- **Syncing teams from GitLab groups or GitHub teams.** Wanted later, so nobody keeps two lists; not in this piece.
- **Two-factor authentication** for password accounts.
- **API tokens** for scripts.
- **Linking a GitLab and a GitHub account** into one person.
- **Rotating `PERGENT_SH_SECRET_KEY`.**

## Anti-goals (a failure even if it works)

- A user who signed in with GitLab or GitHub sees a repository, review, finding or log they can't see on that platform, whatever teams they are on.
- A secret entered in the panel is ever shown again: in a page, an API response, a log, an audit entry or browser storage.
- A user with no team sees anything beyond the waiting page.
- A viewer or maintainer can change instances, secrets, users or teams.
- A wrong password guess reveals whether the username exists, or unlimited guessing is possible.
- A platform admin (a GitLab or GitHub user on an admin team) gets around their platform's ceiling: by creating or resetting a password account, by changing an instance, secret, OAuth client or the LLM endpoint, or through an admin page naming repositories they can't see.
- The browser-managed setup changes something the config file manages.
- Losing the only admin password locks the operator out with no way back.

## Decisions (agreed)

- **Built-in admin with a password.** It exists from the first start. Its first password comes from a Kubernetes Secret, or, when none is given, is generated and printed once in the log. It must be changed at the first sign-in.
- **Everyone else signs in with GitLab or GitHub**, as today.
- **New users get no access** and see a waiting page until an admin assigns them to a team.
- **Teams live in pergent.sh.** Admins create them, add users, and give each a role on the repositories it covers.
- **The platform stays the ceiling.** A user who signed in with GitLab or GitHub sees a repository only when one of their teams covers it **and** the platform still lets them see it (GitLab Reporter or above or public; GitHub read or public), checked as today.
- **Roles:** admin, maintainer, viewer.
- **Secrets move to the admin panel.** Only `DATABASE_URL` and the encryption key stay in env.
- **Password admins see everything** pergent.sh reviews. They have no platform identity, so the ceiling can't apply; they are the operator.
- **Password accounts are admins only**, and admins can create more. The last one can't be removed. The first is named `admin`, and can be removed once another password admin exists.
- **Operator-only (password admins):** password accounts, instances and their secrets, OAuth clients, and the LLM provider, model, key and endpoint (`base_url`). Platform admins manage users, teams, switching repositories, settings and the audit log, and can never touch password accounts.
- **First password:** from a Secret, or generated when none is given; changed at first sign-in.
- **Lost password:** `pergent-sh admin reset-password <username>` in the pod.
- **Guessing:** 5 wrong passwords lock that username for 15 minutes, plus a per-IP limit; passwords are at least 12 characters.
- **Teams:**
  - a maintainer or viewer team covers chosen switched-on repositories, or "all repositories of instance X";
  - an admin team has no repository list: its members are admins on every repository their platform lets them see;
  - if two teams cover a repository, the higher role wins.
- **Only admins manage teams:** creating them, membership and roles. Platform admins may add people to admin teams and remove them.
- **Team changes apply on the user's next request.** Only the platform check keeps its 5-minute refresh.
- **Platform admins' admin pages** show only repositories they can see:
  - Teams, Users, Audit log, Integrations and "Needs attention" are all filtered.
  - Editing a team keeps the repositories they can't see unchanged, shown as "+N repositories you can't see".
- **Waiting users:** admins see a count on the Users menu, and waiting accounts first on the Users page. No email.
- **The config file wins.** Its instances, LLM and repositories stay read-only in the browser. `dashboard.admins` is removed, and a config that still has it is refused at startup.
- **Defaults on Admin → Settings:**
  - Editable: approve clean reviews, review drafts, failure status, Job timeout and Job memory.
  - They apply to repositories switched on in the browser only. Config repositories keep the config's defaults.
  - The job image, concurrency, `max_running` and resources stay in the config and chart.
- **Browser-added instances keep their identity:**
  - URL, API URL and App ID can't change after creation; to move, remove the instance and add it again.
  - Removing one switches its repositories off (with cleanup), then deletes its accounts, team memberships, sessions and secrets.
  - A config instance with the same name as a browser-added one stops startup, naming both.
- **Config repositories name config instances only**, as today. Browser-added instances get their repositories in the browser.
- **Audit log** in this piece, admin-only, 90 days, never a secret value.
- **Changing `PERGENT_SH_SECRET_KEY` isn't a supported operation.** Nobody is expected to do it, and there is no rotation feature. If stored secrets can't be decrypted, startup logs which ones and marks them "not set".
- **Order:** this is built next, before 2d. Nothing has been released, so the dashboard's GitLab-group/GitHub-team admin model is replaced, not migrated.
- **Team sync from the platforms comes later.**

## My calls (made while writing; the user can overturn any)

### Accounts and sign-in

- **Two kinds of user account:**
  - **Password accounts** (local). Always admins (see Decisions).
  - **Platform accounts**, created on first sign-in with GitLab or GitHub. One per (instance, platform user ID), as today: someone who uses GitLab and GitHub is two accounts.
- **Sign-in page:** a username and password form, and below it a "Sign in with GitLab/GitHub" button for each instance with an OAuth client.
- **First password:**
  - from the env var named by `dashboard.admin_password_env` (the chart fills it from a Secret), or
  - when unset or empty on a database with no password admin yet, generated (24+ random characters) and logged once at startup at Warn: "initial admin password: …". This is the one place a secret is logged, and only on that first start.
  - Either way, the first sign-in forces a change. A later change of the env var does nothing (see the reset below).
- **Lost password:** `pergent-sh admin reset-password <username>` run in the pod (`kubectl exec`) sets a new random password, prints it, forces a change at next sign-in, and ends that account's sessions. It needs only database access, which is the operator's.
- **Passwords:**
  - at least 12 characters; no other composition rules;
  - stored as argon2id hashes;
  - a change ends the account's other sessions.
- **Guessing:**
  - five failed attempts for a username lock it for 15 minutes;
  - sign-in attempts per client IP are also limited;
  - "wrong username or password" is the only failure message, and both paths take the same time.
- **Sessions:** as the dashboard has them now (Postgres, `__Host-` cookie, 8 h idle, 7 days at most, CSRF on writes). Password accounts have no platform token. The login form itself carries a CSRF token.

### Roles and teams

- **Admin:**
  - users and teams;
  - switching repositories on and off;
  - every repository's settings and the defaults;
  - the audit log.
  - Password admins also get the operator-only pages (see Decisions).
- **Maintainer** (team role): the settings of the repositories their teams cover (approve clean reviews, drafts, failure status, skills, Job timeout and memory). They can't switch repositories on or off, or change teams.
- **Viewer** (team role): read-only on the repositories their teams cover.
- **A team has** a name, members (platform accounts only), one role, and, for maintainer and viewer teams, the repositories it covers.
- **Users page (admins):**
  - every account, its platform and instance, its last sign-in, and its teams;
  - waiting accounts come first;
  - an admin can remove an account, which is created again, with no team, at the next sign-in.
- **Platform check unchanged:** a user's repositories are checked against the platform every 5 minutes, as today, but only for repositories their teams cover, which is usually fewer calls.
- **Cross-instance teams:** a team may cover repositories on several instances. A member sees only those on the instance they signed in with.

### Connections in the panel

- **Admin → Integrations becomes editable.** It lists instances from the config file (read-only, marked "config") and instances added in the browser.
- **Adding a GitLab instance:**
  - the URL;
  - review token and approve token;
  - optionally an OAuth application ID and secret for sign-in.
  - The webhook secret is generated by pergent.sh, since pergent.sh creates the hooks itself.
- **Adding a GitHub instance:**
  - the API URL;
  - the review App's ID, private key, client ID and client secret, and its webhook secret (entered by the admin, as it is set in the App);
  - the approve App's ID and key, or the approve bot's token.
- **LLM (operator-only):** provider, model, API key and `base_url`, on Admin → Settings next to the defaults.
- **Secret fields are write-only.**
  - The API accepts a value and never returns one. Pages show "set" or "not set" and when it was last changed.
  - Replacing a value doesn't need the old one.
- **Checked before saving.** A save runs the same checks startup runs (token users differ, the App key signs a JWT the API accepts, the LLM key format) and refuses with the reason.
  - The engine picks up a change without a restart, at its next use.
  - An instance's first save triggers a reconcile of its repositories.
- **Encrypted at rest** with `PERGENT_SH_SECRET_KEY` (the existing secretbox), like deploy tokens and session tokens today.
- **The config file wins.**
  - An instance name in the config can't be added or changed in the browser.
  - An `llm:` block in the config makes the LLM settings read-only.
  - The env vars a config instance names stay its source.
- **Removing a browser-added instance:**
  - switches its repositories off first, with the same cleanup as today;
  - ends the sessions of its users;
  - deletes its secrets.

### Audit log

- Admins see who did what and when, for every write:
  - sign-ins, failed sign-ins and lockouts;
  - users, teams and roles;
  - instances and secrets (which field, never the value);
  - repositories switched on and off, and settings changed.
- Kept 90 days.

### Config and chart

- **`dashboard.admins` is removed.** A config that still has it is refused at startup with a message pointing at teams.
- **New config:** `dashboard.admin_password_env`.
- **Chart:**
  - an optional `adminPassword.existingSecret`;
  - the existing Secret no longer needs the instance tokens when instances are added in the browser.
- **The dashboard is still optional.** A config-file-only install with no `dashboard.url` works as today. Without the dashboard, instances can only come from the config file.
- **Startup with no instance at all** is now allowed when the dashboard is on: pergent.sh waits for one to be added in the browser. Without the dashboard it is still an error.

### Pages

- **Sign-in:** the password form, plus the platform buttons.
- **Waiting:** "You're signed in as … on …. An admin needs to add you to a team." There is a sign-out button and nothing else.
- **First-sign-in password change:** the only page a password admin sees until the change is done.
- **dashboard.url and public_url stay in the config file**: they are needed before anyone can sign in.
- **Admin menu:** Users, Teams, Integrations, Settings and Audit log. Integrations, and the LLM part of Settings, are editable by password admins only, and read-only for platform admins.
- **Menus by role:** viewers and maintainers see the pages they see today, limited to their teams' repositories. Maintainers get the settings form on those repositories.

## Constraints

- **Everything in the dashboard's constraints still holds:**
  - public repository, so no internal names;
  - one replica;
  - the engine loop never waits on the dashboard;
  - tokens never reach a Job, log, page or the browser beyond what each Job needs;
  - Go 1.24 and the pinned frontend build.
- **Secrets the engine uses** (LLM key, bot tokens, App keys) are read from the database by the engine when needed, and cached in memory decrypted only as long as it does today with env values.
- **Every write** needs a session, the CSRF token and the right role, checked on the server.

## Left to the builder

- Table names and columns: accounts, teams, memberships, team repositories, instance and credential tables, audit log.
- Where secrets live: one table of sealed values keyed by (instance, field) or columns per instance.
- How the engine reads instances from both the config and the database: a resolver like `settings.Resolver`, with config winning.
- The JSON API shape for users, teams, instances and the audit log.
- The argon2id parameters, and the rate-limit implementation.
- How the existing dashboard tests and the e2e test change from admin groups to teams.
- How config tracks which keys were actually set (the chart always renders `jobs:` and `llm:`), so "config wins" applies per key that is set, not to every key with a default.
- How the engine picks up instances added at runtime, webhook routing for them, and lockout clearing on a reset.
