# 📊 Dashboard

The dashboard is a web app built into pergent.sh. It is off until `dashboard.url` is set. An account is an **admin** or a **user**, and there are two kinds:

- **Local admins** sign in with a username and a password, and are always admins. The first is `admin`.
- **SSO accounts** sign in through your identity provider ([Sign-in](#sign-in)), are identified by their email and are made by an admin adding the email, or the first time the person signs in. A new one is a user: it sees nothing until an admin puts it on a [team](#teams), and then only the repositories that team covers. An admin can make an SSO account an admin.

An admin sees every repository and manages everything: users, teams, instances and their secrets, the models, the defaults and the skill library. Local and SSO admins have the same powers. Nothing asks GitLab or GitHub what a person may read: the team is the only limit. With the dashboard on, every review run, its findings, its files and its log are there, and admins switch repositories on and off and change their settings.

## Two areas

The dashboard has a **user area** and an **admin area**, each with its own sign-in page:

| | User | Admin |
|---|---|---|
| Sign in | `/sign-in`: the SSO button only | `/admin/sign-in`: username and password, and the SSO button when SSO is set up |
| Lands on | `/` | `/admin` |
| Pages | Overview, Repositories, Reviews, Skills | Users, Teams, Integrations, Settings, Usage, Audit log (`/admin/users`, `/admin/teams`, `/admin/integrations`, `/admin/settings`, `/admin/usage`, `/admin/audit`) |
| Menu | The user pages. | The admin pages, with a link back to the user pages. An admin on the user pages has a link across to `/admin`. |

- `/sign-in` has no password form and no link to `/admin/sign-in`: **the admin path is this one, in the README**, and local admins use it. With no SSO set up, `/sign-in` says `Sign-in isn't set up yet. Ask your admin.` With [JWT sign-in](#sign-in) there is no button: loading a page signs the person in, and `/sign-in` says so and sends a signed-in browser on. A refused SSO sign-in comes back to `/sign-in?error=<code>`; `not_allowed` reads `Ask an admin to add you`.
- A signed-in admin who opens `/admin/sign-in` goes to `/admin`; a signed-in user goes to `/`. An SSO sign-in lands on `/admin` for an admin and on `/` for anyone else, whichever sign-in page it began on.
- **The server enforces the split on the data.** Every admin API route answers anyone who is not an admin `404` with the body an unknown path gets, never `403`: a viewer or a maintainer can't tell an admin route from one that isn't there. (A user waiting for a team is the exception: every other path gets them `403 an admin needs to add you to a team`, so they can tell an admin route from an unknown path. They still read nothing.) All of them are registered through one helper in the code (`Dashboard.adminRoute`), and a test walks them as a viewer, a maintainer and a user on no team. The one admin route the helper can't register is a repository's `.../teams`, which shares its path pattern with the repository's other pages: its handler answers a non-admin `404` itself, and a test pins that. A route both areas use, such as reading the skill library, is open to every signed-in user and checks inside its handler what each may do.
- **The web app enforces the page.** The server answers the web app for any path, so a user who opens `/admin/users` gets the app's "not found" page, and the data that page would load is the `404` above.
- The admin pages used to live at `/users`, `/teams`, `/integrations`, `/settings` and `/audit`. For an admin those paths go to `/admin/...`; for a user they are "not found". `/signin` goes to `/sign-in`, keeping `?error=`.
- `GET /api/me` says `email`, `role` (`admin` or `user`), `signIn` (`password`, `oidc` or `jwt`), `username` and, for an admin, `waitingUsers`. `GET /api/sign-in-options` is what the sign-in pages ask, with no session: `{"password": true, "sso": {"label": "Google"}, "jwt": false}`.

## Turning it on

1. Pick the dashboard's address, for example `https://pergent-dashboard.example.com`. It must be `https` and a bare host, without a path: the dashboard, its API (`/api/`) and its sign-in routes (`/auth/`) are served at `/` of that host, and its cookies only work over https. It can be an internal-only host. Webhooks and Job results stay on `public_url`, which GitHub.com must still reach from the internet.
2. Set it in the config, and route that host's `/`, `/api/` and `/auth/` to the pergent.sh Service ([Exposing pergent.sh](../setup/install.md#exposing-pergentsh)):

   ```yaml
   config:
     dashboard:
       url: https://pergent-dashboard.example.com
   ```

   Routing all of `/` there is fine: webhooks and results on that host still need a valid signature or result token, and `/healthz` only answers `ok`. Without `dashboard.url` pergent.sh doesn't serve the dashboard's routes at all.
3. [Sign in](#first-sign-in) as `admin` at `/admin/sign-in`, add your instances under **Integrations** ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)), and add the people who should get in.

With `dashboard.url` set, pergent.sh starts with no instance at all and waits for one to be added in the dashboard. Signing in with GitLab or GitHub is gone: if a config still names `oauth_client_id_env` and `oauth_client_secret_env` on an instance, nothing reads them and pergent.sh logs a warning for each at start. Everyone but the local admins signs in with OIDC: see [Sign-in](#sign-in).

## First sign-in

Local admins sign in at `/admin/sign-in`. The first start with the dashboard on creates the password account `admin`. Its password is one of:

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

More local admins are added by an admin under **Users → Add local admin**: pergent.sh makes a random password, shows it once, and the new admin must change it at its first sign-in. The last local admin can't be removed (`the last local admin can't be removed`), and nobody can remove their own account (`you can't remove your own account`).

## Lost password

A local admin who lost the password asks pergent.sh for a new one from inside the pod:

```bash
kubectl -n pergent-sh exec deploy/pergent-sh -- /pergent-sh admin reset-password admin
```

(`deploy/pergent-sh` is the Deployment of a release called `pergent-sh`, as in [Install with Helm](../setup/install.md#-install-with-helm). The chart names the Deployment `<release>-pergent-sh`, or just `<release>` when the release name contains `pergent-sh`; `kubectl -n pergent-sh get deploy` shows it.) The command needs only `DATABASE_URL`, which the pod's environment has. It prints `new password for admin: ...` on stdout (never in the JSON log), clears the account's lock and ends its sessions, and the account must change that password at its next sign-in. The reset is recorded in the audit log with the actor `cli`.

## Password sign-in limits

- A wrong username or password always gets the same answer, `wrong username or password` (401), after the same work, so a guess doesn't tell whether the username exists.
- Five wrong passwords in a row lock that username for 15 minutes. A wrong current password at a password change counts the same.
- Each client IP may make 20 sign-in or password-change attempts in 5 minutes; further ones get 429 (`too many sign-in attempts, try again later`). The client IP is the **last** `X-Forwarded-For` entry, and the connection's own address when the header is absent. The proxy in front of pergent.sh **must set `X-Forwarded-For`** to the address it saw: when nothing does, a client sends its own header and can change address on every attempt, so the per-IP limit can be bypassed. The per-username lock holds either way. An L7 load balancer in front of that proxy that doesn't forward the client address makes every client share one address, and so one limit of 20 attempts in 5 minutes.
- Anyone who knows a username can keep it locked, with 5 wrong guesses every 15 minutes. `admin` is the one everybody knows. Add a second local admin with an unguessable username and remove `admin`, or restrict `/auth/password` at your proxy to the addresses your admins sign in from.
- Passwords are checked, argon2id, at most 4 at a time. A sign-in that gets no slot within 5 seconds is answered 503 with the same `too many sign-in attempts, try again later`.

## Sign-in

Local admins sign in with a password (above). Everyone else signs in with **OIDC**, against one identity provider, and an **email allowlist** decides who may. Nothing is open until an admin sets it up: with no sign-in set, only local admins get in, and the sign-in page says `Sign-in isn't set up yet. Ask your admin.` Set it up on **Admin → Integrations** (the API is `GET` and `PUT /api/admin/sign-in`, described in [`openapi.yaml`](../../openapi.yaml)) or in the config file, below. Google Workspace is the provider built and tested first; another OIDC provider that follows the standard should work, but isn't promised yet.

**The Sign-in section.** On **Admin → Integrations** the section has a **Mode** (none, OIDC or JWT) and then only the fields of that mode: for OIDC the label, issuer, client ID and client secret, for JWT the header, issuer, audience, JWKS URL and email claim, and for both the allowed domains and emails (one per line). The client secret is write-only: the page says `set` or `not set` and never shows it, and leaving the field empty keeps the stored one. Changing the issuer needs the secret typed again, and the page says so beside the field. **Test** reads the discovery document or key set and shows only whether it worked. When `dashboard.sign_in` in the config file sets the sign-in, the section is read-only and says so.

**How it goes.** The sign-in page (`/sign-in`) shows one button, `Sign in with <label>` (the label is yours, such as `Google`). It starts the authorization code flow with PKCE, scopes `openid email`, a state and a nonce, kept in a short-lived sealed cookie. When the provider sends the browser back, pergent.sh checks the ID token's signature, issuer, audience, expiry and nonce. Only asymmetric signature algorithms are accepted, so a token can't be signed with the client secret, and nothing skips the issuer or the audience check. The `email` claim must be one email address and `email_verified` must be true. Then the allowlist decides. Only after that is an account made, and a session begun, ending the one the browser had.

**Google Workspace.**

1. In Google Cloud Console, pick or create a project, then **APIs & Services → OAuth consent screen**. Choose the user type **Internal**, so only your Workspace's own accounts can sign in.
2. **Credentials → Create credentials → OAuth client ID**, type **Web application**. Under **Authorized redirect URIs** add the dashboard's callback, `<dashboard.url>/auth/oidc/callback` (for example `https://pergent-dashboard.example.com/auth/oidc/callback`; **Admin → Integrations** shows it as the Redirect URI). Copy the client ID and the client secret.
3. In pergent.sh, **Admin → Integrations → Sign-in**: mode **OIDC**, label `Google`, issuer `https://accounts.google.com`, the client ID and the client secret, and your domain under **Allowed domains** (for example `example.com`). Press the test button, then save.
4. Sign out and use the new button. A person on the domain signs in and sees a waiting page until an admin puts them on a [team](#teams).

With the issuer `https://accounts.google.com` a **domain** rule also needs the token's `hd` claim to equal the domain, so a personal Google account that happens to carry a company address can't pass. An **exact email** rule needs no `hd`, so you can let one outside address in.

**The allowlist** is two lists, **allowed domains** and **allowed emails**. A sign-in passes if its email is on the email list, or the part after the last `@` is exactly on the domain list, or an admin added that email under **Users → Add user** (the account keeps that flag after its first sign-in). Anything else is refused: no account is made, no session begins, and the page says `Ask an admin to add you`. A domain matches exactly, so `example.com` does not let in `a@evil.example.com` or `a@example.com.evil.com`. Emails are lower-cased with ASCII rules only, so two different addresses never become one. **With both lists empty only added emails can sign in**; that is the default, so an install that uses plain Google login does not open up to every Google account. A person who passes still starts with nothing: there is no default team. The lists are read at each sign-in: tightening them stops the next sign-in and leaves running sessions alone. A refused sign-in is written to the log with the email's domain only, not to the audit log, so that nobody can fill the audit log by trying.

**Sessions.** An OIDC or JWT session ends after 8 hours without use, or 24 hours after sign-in, whichever comes first, so someone disabled at the provider is gone within a day. People usually sign in about once a day without typing anything. A local admin's session keeps its limits: 8 hours idle, 7 days at most.

**The client secret** is sealed with `PERGENT_SH_SECRET_KEY`, like an instance's secrets, and write-only: the API says whether one is set, never what it is. Saving without a secret keeps the stored one; clearing the field removes it. A skill that contains it is refused, like any secret pergent.sh holds.

**Changing it.** Changing the **mode** or the **issuer** ends every OIDC (and JWT) session in the same step, because a session records how it began; a local admin's session stays. Changing the **issuer** also needs the client secret entered again, and a stored secret is dropped when the issuer changes: it is never sent to a new token endpoint. Issuer and key-set URLs must be `https`; a discovery document that names an `http` endpoint is refused. A change made in the config file, or by going back to an older release, ends nothing by itself, so each start of pergent.sh ends the OIDC and JWT sessions whose sign-in mode or issuer is not the one in force, from the config file or the dashboard (all of them when no sign-in is set), before it serves; a local admin's session is never touched.

**The test button** reads the provider's discovery document (OIDC) or key set (JWT) and answers only whether that worked. It tests the address typed in the field, or the saved one when the field is empty (the page says which); when the config file sets the sign-in it tests the address set there. It does not show the response, an error, or an address. A press that reaches the provider is written to the audit log (`sign_in_tested`) with who pressed it and the mode, and not the address or the answer.

**JWT mode.** For an install behind something that has already logged the person in, such as an auth proxy, Istio or Cloudflare Access. Set the mode to `jwt` and give it the **header** that carries the token, the **issuer**, the **audience**, the `https` **JWKS URL** and the **email claim** (`email` when empty). The issuer and the audience are required, so a token meant for another app behind the same proxy is refused. A value of `Bearer <token>` or the bare token are both read. OIDC and JWT are alternatives: an install uses one, or neither. The allowlist applies as above, with no `hd` rule unless the issuer is Google's.

- **Checks.** The signature, issuer, audience and expiry are checked on every request. `alg: none` and symmetric algorithms (`HS256`) are refused, and so is a request that carries the header twice. The email claim must be one email address (lower-cased with ASCII rules). `email_verified` is checked only when the claim is present: `false` refuses, a missing claim or `true` (also the string `"true"`) passes.
- **Starting a session.** Only a `GET` of a page or of `/api/me`, with a valid token and no session, starts one. `/admin/sign-in` and its `GET /api/me?probe=1` never do, so a local admin behind the proxy can reach the form; anyone else follows that page's link to `/admin`. A write never does, and always needs an existing session's CSRF token, even when the header is valid: the proxy adds the header to any request the browser sends, so a cross-site `POST` must not start anything. The session is the same as an OIDC one (24 hours at most, 8 idle) and is written to the audit log as a `sign_in`.
- **Each later request.** The cookie names the session, and the header must carry a valid token with the same email, or the session ends (401). A token for another person, a missing header, an expired token and a changed sign-in mode or issuer all end it. So the proxy stays in charge: someone disabled there is out at their next request. A person who is not allowed gets `401` with `"code": "not_allowed"` from `GET /api/me`; no account is made.
- **An outage.** If the key set cannot be fetched and no key is cached, requests get `503` and no session ends because of it. A key that is cached keeps working.
- **Local admins.** A local admin's session has no token and skips all of this, even when the header is present and invalid. The admin sign-in page starts no session either: loading `/admin/sign-in`, and the page's quiet read `GET /api/me?probe=1`, never sign a person in from the header, so a local admin whose proxy identity is allowed still gets the password form. A proxy that demands a login on every path locks a local admin out. Leave `/admin/sign-in`, `/auth/csrf`, `/auth/password`, `/assets/` and `/api/` out of its login rule (and forward the proxy's header whenever it has one), so the sign-in page, its CSRF token and password, the page's scripts and the API calls work with no header. Or keep the proxy's rule as it is and let local admins use a route that doesn't pass through it, such as `kubectl port-forward` to the Service. `admin reset-password` works from inside the pod either way.
- **Sign-out** ends the session and goes to `/signed-out`, a page that starts nothing and never asks `/api/me`, even when the proxy adds a valid header. Signing out of the proxy itself is the proxy's business. If the proxy sends the header to `/signed-out`, the person is still signed out of pergent.sh; the next page they open signs them in again. That includes another tab left open: signing out in one tab doesn't stop a page load or a reload in another from signing the person in again while the proxy vouches for them. (The dashboard doesn't read `/api/me` on window focus for a JWT session, but any other call a tab makes, such as a list that refreshes, gets 401 with no session and sends that tab to `/sign-in`, where loading the page signs the person in again while the proxy vouches for them. From an admin page it goes to `/admin/sign-in`, which starts nothing and links on to `/admin`.)
- **Cross-site requests.** A request the browser marks `Sec-Fetch-Site: cross-site` that is not a navigation (`Sec-Fetch-Mode` is not `navigate`), such as an image, a script or a `fetch` from another site, starts no session, even with a valid header.
- **Unreadable settings.** If the sign-in settings can't be read (the database is down), a request with no session starts none and gets 401 from `/api/me`, as on an install that doesn't use a header; a session that jwt started is kept and the request gets `503`.
- **Caching.** A page response that sets the session cookie is marked `Cache-Control: private, no-store`. A session row past its limit counts as no session, so the next page load starts a new one. A token whose issuer, audience or expiry can't pass is turned away from its claims alone, before the key set is asked for anything.

**In the config file.** `dashboard.sign_in` sets the same values. When its `mode` is set it wins over the dashboard, which then shows the sign-in read-only (`PUT /api/admin/sign-in` answers 409), as for an instance in the config file. The client secret is named by an env var:

```yaml
config:
  dashboard:
    url: https://pergent-dashboard.example.com
    sign_in:
      mode: oidc
      label: Google
      issuer: https://accounts.google.com
      client_id: 1234567890-abc.apps.googleusercontent.com
      client_secret_env: PERGENT_SH_OIDC_CLIENT_SECRET   # a key of the Secret named by existingSecret
      allowed_domains: [example.com]
      allowed_emails: [contractor@example.org]
```

A local admin is not affected by any of this: it always works, whatever happens to OIDC.

## Teams

Everything an account may see or do comes from its role and the teams it is on, under **Admin → Teams**. Nothing asks GitLab or GitHub.

- **Adding people.** An admin adds a person before they have ever signed in, by typing the email under **Users → Add user** (and, if wanted, the teams to put them on, each with a role), or from a team: on **Teams**, open a team and pick a user or type an email. The account shows as `invited` until the person signs in. An email added this way can sign in whatever the allowlist says. Typing the email of an account that exists already marks it added the same way.
- **Waiting.** A user on no team sees only a page that says `You're signed in as <email>. An admin needs to add you to a team.`, with a sign-out button; the API answers everything else with 403 `an admin needs to add you to a team`. An admin adds them to a team from **Teams**; the account sees the change on its next request.
- **A team** has a name, members and the repositories it covers. Each member is an SSO account with its own role on the team, `viewer` or `maintainer` (a team has no role of its own). A team covers chosen repositories that are switched on, or all repositories of an instance, which also covers repositories switched on later. Local admins are never members: they need no team. A user on several teams has, on each repository, the highest role among the teams that cover it.
- **Roles.**

  | Role | Can |
  | --- | --- |
  | `viewer` | Read the covered repositories: reviews, runs, findings, files, logs and settings. Everyone with a team also reads the skill library. |
  | `maintainer` | Also change the settings of the covered repositories (approve clean reviews, review drafts, failure status, who uses commands, the skills a repository runs and the repository skills it turns on, Job timeout and memory, Max steps; not the re-run caps), and reset them to the defaults (the caps stay as they are). Press the review page's buttons: re-run, dismiss and restore ([Commands](commands.md)). Not switch repositories on or off, and not change teams. |
  | admin (the account's role) | Everything, on every repository, with no team: change the re-run caps, switch repositories on and off (**Add repository**), change the defaults on **Settings** (the caps and the default skill list among them), write, install, update, copy and delete the skills of the library, manage **Users**, **Teams** and the instances, their secrets and the list of models, choose a repository's model, and read the **Audit log**. |

  Only admins manage teams: creating them, their members, roles and repositories. An admin assigns a repository to teams from either side: the **Teams** page, or the **Teams** section of the repository's **Settings**, which lists every team with a tick for those that list the repository. Both change the same data (`GET` and `PUT /api/repositories/{instance}/{project}/teams`; a team that covers the whole instance is shown as such and not changed from the repository). A maintainer or viewer does not see that section, and the API answers them `404`.
- **Team changes apply on the next request.** The roles and teams are read on every request, so a member taken off a team, a team deleted or a repository dropped from it stops showing at once. A repository switched on that reconcile has not stored yet is shown as soon as a team covers it; its runs show once the repository is set up.
- **Users.** **Admin → Users** lists every account with its sign-in (`SSO` or `Local`), its role, its teams and its last sign-in. **Add user** takes an email and, if you like, teams with a role on each (`viewer` unless you pick `maintainer`); **Add local admin** makes a password account and shows its generated password once. With no sign-in set up, the page says so and links to **Integrations**. An SSO user who has signed in and is on no team is marked waiting, and the **Users** item in the sidebar counts them for admins (an invitation that never signed in does not wait). An admin can make an SSO account an admin or a user, but not their own account, so an admin is always left to change the others (`you can't change your own role`). Removing an account ends its sessions and takes it off its teams; an SSO account comes back at its next sign-in, on no team.
- **A repository that takes over a path** inherits its team's access: teams follow the repository's path, and nothing else checks. Only repositories an admin switched on are shown.

## Who sees what

- **Signing in.** A new sign-in ends the session the browser had. Local admins sign in with a password, everyone else with OIDC ([Sign-in](#sign-in)). Accounts of the old GitLab and GitHub sign-in are kept in the database, unused and hidden; nothing signs them in, and a session left from the previous release is ended at its next request.
- **Sessions** are a cookie (`HttpOnly`, `Secure`, `SameSite=Lax`) holding a random ID. A session ends after 8 hours without use, and after a maximum age counted from sign-in whether used or not: 7 days for a local admin, 24 hours for a session that began with OIDC or a JWT ([Sign-in](#sign-in)). pergent.sh deletes ended sessions every hour. Every write needs the session's CSRF token (`X-CSRF-Token`); the password form has its own, from `GET /auth/csrf`.
- **Checked on the server.** Every API answer is filtered by the account's role and teams, read on each request. A run is shown only when its repository's path and project ID both match what pergent.sh stored for the path, so a project created at the path of a deleted, renamed or transferred one never shows the earlier project's reviews. A repository, review or run the user can't see is answered exactly like one that doesn't exist.

**Everyone with a team** sees, for the repositories their teams cover:

- **Overview:** the number of reviews, approvals, blocking reviews and failed reviews over the last 7, 30 or 90 days, the most recent reviews, and "Needs attention". Admins also see a setup checklist (the model and its key, a connected instance, a skill in the library, a repository switched on); each item links to its page, ticks when done, and the list goes away when all four are done.
- **Reviews:** filtered by repository, result and time. Each merge request has a page with every run (commit, what started it, result, duration) and, for a run, the status set on the platform, whether it was approved and why not, the pergent version, provider and model, what an incremental review covered, a `quiet` label when the review was started quiet ([Quiet reviews](quiet-reviews.md)), tokens, each skill's verdict, findings (with the ones people dismissed, who dismissed them and why), checks, files (reviewed, skipped with the rule, not reviewed) and the log. Each skill shows where it came from: `library v3` (the library version the review used), or `repo (.claude/skills)` for a skill read from the repository. A run skipped because the repository has no skills shows as **Not reviewed** (result `skipped`, with its own filter), not as superseded, and is not counted as a review or a failure. Maintainers of the repository and admins also get the buttons **Re-run review** and **Re-run failed only**, and **Dismiss** and **Restore** on findings ([Commands](commands.md)).
- **Repositories:** each with its last 7 days and health; its reviews, its readiness scan, its setup checks and its skills and instructions. The [Readiness](#readiness-scan) tab is the checklist of what the repository gives a review. Skills & instructions shows what a review of the default branch would get, read with the review bot or App and kept for 5 minutes: each skill with its library version or repository folder, a link to its file for a repository skill, and the problem a review would fail on. A repository's **Settings** choose between "Use the default skills (a, b)" and "Choose skills", with a running total against the 900 KiB a review takes, and list the skills found in `.agents/skills`, `.claude/skills` and `.github/skills` with switches, off at first (`.pergent/skills` is always on; only the `SKILL.md` text is used). Anyone who can see a repository gets a **Skill results** tab: what became of each skill's findings ([Skill results](skill-results.md)). Maintainers of a repository and admins also get a **Past dismissals** tab: what teaches its reviews now ([Learn from dismissals](learn-from-dismissals.md)). **Add repository** warns when the default skill list is empty.
- **Skills:** the skill library: each skill's name, version, where it came from (written in the dashboard, or the repository and branch or tag it was installed from), whether it is on the default list and when it changed, with its text in a plain-text box (never rendered as HTML or markdown) and its saved versions. A skill installed from a private repository is shown to everyone with a team, so installing is sharing; the repository's path is shown to admins only, and others see `a private repository on <instance>`. Who saved each version is shown to admins only. See [Skills and instructions](skills.md).

**Admins** also get:

- the on/off switch of each repository and **Add repository**, which lists the repositories of an instance the admin names that the review bot (GitLab: Maintainer) or review App (GitHub: installed) can reach, 50 at a time with a **Load more** button, with a search (a new search starts again at the first page). On GitLab each page is one request to GitLab, taken in ID order so that no project falls between two pages and sorted by path within the page (the whole list is not in alphabetical order), and a page can hold fewer than 50 when some of its projects are switched on already; GitHub has no search on its side, so the App's repositories are read whole (up to 5000, kept for 5 minutes), sorted and cut into pages of 50, and the repositories switched on are left out of each page (so a page can hold fewer than 50 there too, and switching one on moves no later page). Switching one on asks the platform the same question as the list does (on GitLab, the project listing at Maintainer level, so access gained through an ancestor group or a share counts, and the path matched without regard to case), so a repository the list shows can be switched on. On GitLab it is switched on under GitLab's own spelling of the path, whatever case was sent;
- each repository's **Settings**: approve clean reviews, the failure status, review drafts, who uses commands (**Only the author and maintainers use commands**, on by default), **Incremental reviews** (on by default), **Learn from dismissals** (on by default: later reviews are told what people with write access rejected, and why), **Quiet reviews** (off by default: fewer nitpicks, no suggestion threads and a shorter comment, with blocking findings, failures and approval unchanged), **Linked repositories** (admins only: off by default, with the links; [Linked repositories](linked-repositories.md)), **Model** (admins only: the default until one is picked; [Models](models.md)), **Comment layout** (the blocks of the review comment, their order and whether each starts open or collapsed; [Comment layout](comment-layout.md)), the library skills to run, the Job's timeout (60 to 7200 seconds) and memory limit (from 256Mi, and not below `jobs.resources.requests.memory`, to 32Gi), Max steps (5 to 200: the steps each opencode run may take before pergent stops it), and the re-run caps per merge request and per repository per day (1 to 1000; only an admin changes these two). Each shows "overridden here" or "default", and "Reset to defaults" clears every override (a maintainer's reset leaves the caps and the linked repositories);
- on **Skills**, the buttons **New skill**, **Install from git** and **Check for updates**, and per skill Edit (a written skill only), Copy and Delete ([Skills and instructions](skills.md));
- **Users**, **Teams** and the **Audit log**;
- **Integrations** (each instance and its secrets, and the **Sign-in** section: [Sign-in](#sign-in)) and **Settings**: the defaults a repository switched on in the dashboard gets (approve clean reviews, review drafts, failure status, **Only the author and maintainers use commands**, **Incremental reviews**, **Learn from dismissals**, **Quiet reviews**, **Linked repositories** (on or off), **Comment layout**, Job timeout and memory, Max steps, and the re-run caps per merge request and per repository; the config's repositories keep the config's own; each shows what the config gives and follows it unless saved; **Use config value** drops a saved one), and the list of models ([Models](models.md)).

Every write is checked on the server: it needs a session, the CSRF token, a role that allows it and a repository the user can see. A viewer calling a write on a repository directly gets 403 and nothing changes; anyone but an admin calling an admin route gets `404` ([Two areas](#two-areas)).

## API clients

Admins let other systems ask for reviews under **API clients** (`/api-clients`, after Repositories in an admin's menu). A client has its own skills, model, limits and keys, and a key opens the `/v1/` review API for that client only, never the dashboard. A user never sees the page, and its routes answer anyone but an admin `404`. See [Review API](../setup/review-api.md).

## Instances and secrets in the dashboard

Admins add GitLab and GitHub instances, with their secrets, under **Admin → Integrations**, and the list of models under **Admin → Settings**. Nobody else can.

- **Adding a GitLab instance:** a name (`[a-z0-9-]`, at most 63 characters, part of the webhook URL), the instance's `https` URL without a trailing slash, the review token, who approves (a separate approve bot, the default, or the review bot; this can't change afterwards) and, for a separate bot, the approve token. The webhook secret is generated by pergent.sh, since pergent.sh creates the hooks itself. Each instance's card shows its webhook URL, for checking against the hooks on your projects.
- **Adding a GitHub instance:** a name, the API URL, the review App's ID and private key, its webhook secret (the one you set in the App), **Approve with**: the review App itself (the default, one App), a separate approve App (its ID and private key) or a bot user's token. A private key is pasted as it is in the `.pem` file; the browser's one-line field loses its line breaks, and the dashboard puts them back.
- **Secrets are write-only.** A value is sent once and never comes back: not in a page, an API answer, a log, an audit entry or the browser's storage. The page shows only "set" or "not set" and when the secret last changed. Replacing a secret doesn't need the old value.
- **Checked before saving.** A save runs the checks startup runs for an instance in the config (the two bot tokens belong to different users, the App key signs a request GitHub accepts, ...) and is refused with the reason when one fails; nothing is stored then. A saved instance is used at once, without a restart.
- **Address and App IDs can't change.** The URL, API URL and App IDs are fixed once the instance is added (`an instance's address and App IDs can't change: remove it and add it again`); secrets can be replaced at any time. Changing who approves (another App, the review App, a bot user) doesn't withdraw approvals the old approver left. Turn approval off for the repositories first and wait until their approvals are withdrawn, or dismiss them by hand.
- **Monthly token quota.** An admin sets an instance's [monthly token quota](token-quota.md) on its card, with the tokens used this month and a bar (amber from 80%, red when used up). Empty is unlimited. An instance in the config takes it from `monthly_token_quota` and shows it read-only. A repository's quota is on its **Settings**; the repository page shows its usage, and its instance's, for admins only.
- **Removing an instance** asks for confirmation and answers `removing`. Its repositories are switched off at once (queued runs are superseded), and the old platform accounts of the instance (nothing signs them in any more), their sessions and their team memberships are deleted. Its repositories drop off every team that covered them. In the background pergent.sh then takes its webhooks and deploy tokens off the platform and deletes the instance and its secrets, within a minute or so; the page shows "removing" until then. Its runs, findings and logs stay in the database: adding an instance of the same name again attaches them to it, and admins see them. When the platform keeps refusing that cleanup, a "Needs attention" item says so (`pergent.sh could not take N project(s) of the instance "name", which is being removed, off its platform; it tries again at the next reconcile`) and the removal waits for the next reconcile.
- **An instance that can't connect** (at startup, or right after it was saved) doesn't stop pergent.sh, so the dashboard where it is fixed stays up. pergent.sh logs the error, and an instance-wide "Needs attention" item (kind `instance_unreachable`) shows it to admins; the item goes when the instance connects. Its webhooks are still accepted and the events queued, and reviews of its repositories wait queued (the log says that queued reviews wait because pergent.sh isn't connected to their instance, and that it is fixed on Admin → Integrations) until it is saved again in **Integrations**. An instance in the config file that can't connect still stops startup.
- **The config file wins.** Instances in the config are listed as "config" and are read-only (409 `this instance is managed by the config file; change it there`), and so are the secrets they name by environment variable. An instance in the dashboard whose name is also in the config stops startup, naming both. Repositories in the config name instances in the config only; repositories of an instance added in the dashboard are switched on in the dashboard.
- **The models** (provider, model, base URL and API key, one entry each) are kept in the dashboard by admins: see [Models](models.md). The provider is picked from a list (Anthropic, OpenAI, Google, DeepSeek, OpenRouter, Groq, Mistral, xAI) or typed as any other opencode provider ID under **Other**; with a base URL, that can be any OpenAI-compatible server. A config file's `llm:` block adds a read-only entry, `config`, which is the default while it exists (409 `the config file has an llm: block, which is the default; change it there` on **Set as default**). With no model set, reviews wait queued, and the log says that no LLM is set and that it is set on Admin → Settings.
- **Defaults.** The defaults on **Admin → Settings** (approve clean reviews, review drafts, failure status, **Only the author and maintainers use commands** (on until set), **Incremental reviews** (on until set), **Learn from dismissals** (on until set), **Quiet reviews** (off until set), **Linked repositories** (off until set; the links are per repository), **Comment layout** (the default layout until set; the config has no layout default, so the form says whether one is saved here, and **Reset to default**, then save, drops a saved one), Job timeout, Job memory, Max steps (40 until set) and the re-run caps, 10 per merge request and 50 per repository a day until set, and the default skill list, picked among the library's skills with a running total against 900 KiB) apply to repositories switched on in the dashboard only; repositories in the config keep the config's own. Under each default the form says what the config gives (`jobs.timeout`, `jobs.max_turns`, `jobs.resources.limits.memory` and the built-in values). A default you leave at the config's value keeps following it, so a later change in the config reaches the dashboard's repositories; one you change is saved here, shows "Saved here; the config says …" and stays until you press **Use config value** and save. The Job image, concurrency, `max_running` and resources stay in the config and the chart.
- **`PERGENT_SH_SECRET_KEY`.** Secrets are stored encrypted with it. Changing it isn't supported and there is no rotation. A stored secret that can't be opened with the key shows as "not set" (startup logs which instance and fields, `secrets of an instance couldn't be opened with PERGENT_SH_SECRET_KEY; they count as not set`), and has to be entered again.

## Repositories in the config and in the dashboard

- A repository listed in `repositories` is shown as **managed by config** and is read-only in the dashboard; a write to it gets 409. Every dashboard setting has a config key (`approve`, `failure_status`, `review_drafts`, `inline_findings`, `incremental_reviews`, `learn_from_dismissals`, `quiet_reviews`, `comment_layout`, `skills`, `repo_skills`, `job_timeout`, `job_memory`, `max_turns`, `rerun_limit_per_mr`, `rerun_limit_per_repo`, `commands_author_and_maintainers_only`, `trusted_authors_only`, `trusted_authors`, `monthly_token_quota`; see [Config reference](../reference/configuration.md)).
- A repository switched on in the dashboard is stored in Postgres. pergent.sh sets it up (webhook and deploy token on GitLab) the same way as a config repository, in a reconcile of that repository alone it starts at once (switches made within 10 seconds of each other are handled together, at the end of those 10 seconds); switching it off removes them the same way. The hourly reconcile still covers every repository. Users whose teams cover it see a newly switched-on repository from their next request.
- A repository can't be in both: when it is added to the config, the next reconcile deletes its dashboard record and the config's settings apply. Removing a repository from the config switches it off; it doesn't fall back to the dashboard. To hand a repository to the dashboard, remove it from the config, redeploy, then switch it on in the dashboard.

## Readiness scan

A review does better when the repository gives it something to work with. The **Readiness** tab of a repository (after Skills & instructions) shows what is missing and the exact fix. It only advises: it never changes a repository setting, a file or a branch, it opens no merge request, and it does not use the LLM.

**Who.** Everyone who can see the repository sees the tab; the same rule as the review list ([Who sees what](#who-sees-what)). Maintainers and admins of the repository press **Scan now**, repositories in the config included. A repository in the config is read-only: instead of the **Use these skills** button the tab shows the `skills` line to paste into the config file.

**When a scan runs.**

- When a repository is switched on, after its first reconcile. For a repository in the config, it is a reconcile that sets it up and finds no scan of it.
- On **Scan now**. At most 1 scan is queued or running per repository: a second request gets 409 `a scan is already queued or running` with that scan. A repository can be scanned on request once every 5 minutes, counted from when the latest scan of any kind was queued: sooner than that is 429 `this repository was scanned less than 5 minutes ago; try again in N minutes` (`1 minute` for 1), with `Retry-After` in seconds. Only a Scan now that was accepted is in the [audit log](#audit-log) (`readiness_scan_requested`).
- Once after the upgrade, for each switched-on repository that was never scanned ([Upgrading to 0.5.0](../upgrading.md#upgrading-to-050)).
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

**What the Job reads.** A scan Job clones the default branch at the commit the pod resolved, with depth 1, over https only, with symbolic links not followed and hooks off, and runs `pergent-sh scan-repo` over it ([What a Job can and cannot see](job-isolation.md)). It is told each library skill's `paths` and `exclude` globs, never a skill's text, and reads the repository's own skill folders from the clone. It posts counts, matches, the presence of the context files and at most 50 noise paths. File contents never leave the Job. The pod treats everything it posts as untrusted text: one line, at most 500 bytes, and a text that holds a secret pergent.sh holds is withheld, or the result refused.

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

## Audit log

Admins read the audit log under **Admin → Audit log**: who did what and when, for every change made in the dashboard. Each entry names its actor as the full email of an SSO account, `root (password)` for a local admin or `cli` for a reset from the command line (entries made before this change read `alice (gitlab main)` and stay as they are). It records password sign-ins, failed password sign-ins and lockouts, OIDC and JWT sign-ins (`sign_in`; a refused one is not, see [Sign-in](#sign-in)), password changes and resets, accounts (added, removed, made an admin or a user), teams (the members added and removed by username, and a member's role before and after), instances and which of their fields changed, the models (added, changed, made the default and removed, with the provider and model, never the key), a repository's model and the defaults (which fields; the default skill list shows as `skills`), the sign-in settings (`sign_in_updated`: which fields changed, never a value or the client secret) and each press of the sign-in test button (`sign_in_tested`: who, and the mode, never the address or the answer), an instance's token quota (`instance_updated` with `monthly_token_quota=<n>` or `monthly_token_quota=none`; a repository's is a `settings_updated` with the detail `monthlyTokenQuota=<n>` or `monthlyTokenQuota=none`), the skill library (`skill_created`, `skill_updated`, `skill_installed`, `skill_source_updated`, `skill_copied` and `skill_deleted`, each naming the skill, with its version or the skill it was copied from, and for an install or an update the source (a private repository by its instance only), the branch or tag and the short commit; a skill's text is never in an entry), repositories switched on and off with their settings changed or reset, `readiness_scan_requested` (a **Scan now** that was accepted; the scans started by switch-on or the upgrade are not audited), and the review actions: `review_rerun`, `review_rerun_failed`, `review_start` (the review of an untrusted author's change started), `finding_dismissed` and `finding_restored`, from a comment or a button, each with the merge request, the head commit and, for a finding, its fingerprint and the reason given, `command_refused`, a comment refused because of who wrote it ([Who may do what](commands.md#who-may-do-what)), and `dismissal_stopped`, a **Stop using** on the Past dismissals tab, with the finding's fingerprint (only a stop that changed something), and the [API clients](../setup/review-api.md) (`api_client_created`, `api_client_updated`, `api_client_removed`, `api_key_created`, `api_key_revoked` and `api_callback_secret_rotated`, each naming the client, and a key by its name and prefix, never the key). It is kept for 90 days and never holds a secret value: an entry for a secret names the field, not its content. Every admin sees every entry.

## Logs and "Needs attention"

- **Logs.** When a review Job ends, or before the Job of a superseded run is deleted, pergent.sh keeps the last 200 KB of the pergent container's log, with the Job's clone token, LLM key and result token replaced by `[REDACTED]`. Logs are deleted after 30 days. A Job that is gone before its log is read (Jobs are removed an hour after they finish) has no log. A failed review also keeps the last 64 KB of the clone log (the `checkout` container), redacted the same way, and admins also get the pod's state; both are deleted after 30 days too ([Troubleshooting a failed review](../setup/troubleshooting.md#when-the-job-is-gone)).
- **Needs attention** lists setup problems that pergent.sh detects, kept up to date by the hourly reconcile, the Job watcher and the readiness scan (not computed when the page loads):
  - one approval is enough to merge, so pergent's approval alone can unlock it;
  - the GitHub approve App isn't installed on the repository or lacks Pull requests: write (for the review App: it lacks that permission), or the approve bot user can't reach it;
  - reviews running out of memory (two or more in the last 7 days);
  - clones of the repository failing (`clone_failing`: two or more reviews failed at the clone step in the last 7 days; check the clone token's access and the network, or the Job memory limit for a clone that ran out of memory). It goes when fewer do;
  - the GitHub approve bot's token expires within 14 days. This one concerns the whole instance and is shown to admins;
  - a run failed because a skill it needs is missing (`skill "x" is not in the skill library`, or `skill "x" is turned on but not found in the repository`), or was skipped because the repository has no skills (`this repository has no skills`). Either goes when a later run of the repository starts a Job ([Skills and instructions](skills.md));
  - the [readiness scan](#readiness-scan) found that no skill the repository uses matches its files (`no_skill_coverage`: `No skill this repository uses matches its files, so its reviews check nothing useful. The Readiness tab suggests skills that fit.`). It goes when a scan finds a match, or when the skills change and the last scan shows one;
  - skills plus instructions are over 900 KiB, so every review fails (`inputs_over_budget`: `Skills and instructions come to N KiB, over the 900 KiB a review can take, so every review fails. Use fewer skills or shorten the instructions.`). It goes when the size is back under, at a scan or when the repository's skills or the default list are saved. When a size isn't known (a skill with a problem, or one removed from the library) the item is left as it is;
  - the repository has no `AGENTS.md`, `CLAUDE.md` or README (`no_project_context`: `The repository has no AGENTS.md, CLAUDE.md or README, so reviews don't know what the project is. Add one.`). It goes when a scan finds one. A failed scan changes none of these 3 items;
  - an instance added in the dashboard couldn't be connected (`instance_unreachable`; it goes when the instance connects), or its removal is stuck because the platform refuses to give up its webhooks ([Instances and secrets in the dashboard](#instances-and-secrets-in-the-dashboard)). These concern the whole instance too: they are shown to admins.

## Limits of the access check

There is no access check against GitLab or GitHub. A person on a team reads findings and logs, which contain code, of every repository the team covers, including a repository they can't open on the platform; taking them off the repository there changes nothing here. Take them off the team, or remove the account, to end it. Removing a team's access takes effect at the next request; a session itself lasts until it ends (8 hours idle; 24 hours at most for an SSO session, 7 days for a local admin).
