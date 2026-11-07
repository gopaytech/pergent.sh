# Sign-in and access redesign

Status: agreed with the owner on 2026-11-07, in a grilling round that settled every open question. The owner approves this document before the build starts.

## Why

pergent.sh's people model grew in pieces, and using it shows that:

- There is no way to add a person. An account only exists after its owner signs in, and only through a GitHub or GitLab OAuth client that nothing tells an admin to set up.
- Without that client the sign-in page has a password form and nothing else, so only `admin` can get in, and nothing explains why.
- With it, on github.com, anyone on the internet can sign in and create an account.
- One kind of account has three names: password admin, operator, and `password` in the code.
- Someone on both GitHub and GitLab is two accounts.

The owner wants this simple. People sign in with the company's identity, and an admin decides what they can reach.

## The model

1. A username and password is for admins only.
2. Everyone else signs in through OIDC, or through a validated JWT from a proxy in front of pergent.sh.
3. The email from that sign-in is the user.
4. A new user can do nothing.
5. Admins create teams and put users on them.
6. An account is an **admin** or a **user**. On a team, a user is a **viewer** or a **maintainer**.
7. Admins assign teams to repositories.

## Goals

1. An admin can give a person access by typing their email, before that person has ever signed in.
2. Nobody outside the allowlist can create an account.
3. Access reads in one line: user → team → repository.
4. One name per thing, on every page.
5. pergent.sh works with no identity provider at all, for its local admins.

## Anti-goals

- **Access nobody granted.** A new user sees nothing. A failed or partial sign-in gives nothing. There is no default team.
- **A forged identity.** A token is trusted only after its signature, issuer, audience and expiry check out. An unsigned header is never read as an identity.
- **A locked-out install.** Local admins always work, whatever happens to OIDC, and the last local admin can't be removed.
- **People's email addresses on public PRs.** An email is an account's identity in the dashboard, not something pergent.sh posts to a platform.
- **A change to reviews.** The bot and Apps, webhooks, `@<bot>` commands and their platform permission checks are untouched.

## Non-goals

- Keeping GitHub and GitLab sign-in, as an option or otherwise. They are removed.
- Local users who aren't admins.
- Groups or roles read from the identity provider. Teams live in pergent.sh.
- Per-person dashboard layouts.
- SCIM or automatic removal of users.

## Accounts

| | Local admin | SSO account |
|---|---|---|
| Signs in with | Username and password | OIDC or JWT |
| Identity | Username | Lower-cased email |
| Role | Always admin | User by default; an admin can make it admin |
| Created by | An admin (`Add local admin`), or `admin` on first start | First sign-in, or an admin adding the email first |

- The account's role is `admin` or `user`. An admin reaches every repository and every admin page, with no team. Local and SSO admins have the same powers. This merges today's password admin and admin team, and there is no more per-instance admin.
- An SSO account added before its first sign-in shows as `invited`.
- A new user always starts with nothing, even on an allowed domain. There is no default team.
- An admin can't remove their own admin role or delete their own account.
- A user on no team sees the existing waiting page, reworded: "You're signed in as <email>. An admin needs to add you to a team."
- Removing an account ends its sessions and takes it off its teams. An SSO account comes back at its next sign-in if the allowlist still lets it in, with nothing.

## Sign-in

### OIDC

The authorization code flow with PKCE, against one provider. Discovery by the issuer URL. Scopes `openid email`.

- The ID token's signature, issuer, audience, expiry and nonce are checked. Only asymmetric algorithms are accepted, so a token can't be signed with the client secret.
- The `email` claim is required, and must parse as one email address. `email_verified` must be true.
- After that, the allowlist decides.
- Google Workspace is the provider built and tested first. With the issuer `https://accounts.google.com`, an email passes a domain rule only if the token's `hd` claim equals that domain, so a personal Google account on a company address can't pass. Other OIDC providers should work, and aren't promised yet.
- `/sign-in` shows one button, `Sign in with <name>`, where the name is an admin's label such as `Google`.

### JWT validation

For an install behind something that has already logged the person in, such as an auth proxy, Istio or Cloudflare Access. Off unless configured.

- Settings: the header name, the issuer, the audience, the JWKS URL and the email claim. Issuer and audience are required, so a token meant for another app behind the proxy is refused. A value of `Bearer <token>` or the bare token are both read.
- Signature, issuer, audience and expiry are checked on every request. `alg: none` and symmetric algorithms are refused. The email claim must parse as one email address, and `email_verified` is checked when present.
- **Starting a session.** Only a `GET` for a page or `/api/me`, carrying a valid token and no session, starts one. A write never does, and always needs an existing session's CSRF token. The proxy adds the header to any request the browser sends, so a cross-site `POST` must not start anything.
- **Each later request.** The cookie names the session, and the header must carry a valid token with the same email, or the session ends. So the proxy stays in charge.
- **A local admin's session** is exempt: it has no token. A proxy that demands login must leave `/admin/sign-in` and `/auth/password` out, or a local admin uses a direct route to the service. `admin reset-password` still works from inside the pod.
- **An outage.** If the JWKS can't be fetched and nothing is cached, requests get 503. No session ends because of it.
- **Sign-out** ends the session and shows a signed-out page that starts nothing. Signing out of the proxy is the proxy's business.
- OIDC and JWT are alternatives: an install uses one, or neither.

### The allowlist

Two lists: email domains and exact emails. A sign-in passes if its email is on either, or an admin added that account (an `added` flag, which stays set after the first sign-in). Otherwise it is refused with "Ask an admin to add you", and no account is created.

With both lists empty, only added emails can sign in. That is the default. So an install using plain Google login doesn't open up to every Google account.

A domain matches exactly on the part after the last `@`. Emails are lower-cased with ASCII rules only, so two different addresses can't fold into one. The lists are read at sign-in: tightening them stops the next sign-in and leaves running sessions alone. Removing an account on an allowed domain doesn't block it: it can come back, with nothing.

### Local admins

`/auth/password` works as today: lockout, must-change-password and `admin reset-password` included. It accepts admin accounts only, which is all a password account can be.

### Sessions

An SSO session ends after 8 hours idle or 24 hours at most, so someone disabled at the provider is gone within a day. People sign in about once a day, usually without typing anything. A local admin keeps today's limits: 8 hours idle, 7 days at most.

## Two areas

One app and one session, with two areas:

| | User | Admin |
|---|---|---|
| Sign in | `/sign-in`: the SSO button only | `/admin/sign-in`: username and password, and the SSO button for SSO admins |
| Lands on | `/` | `/admin` |
| Pages | Repositories, Reviews, Skills | Users, Teams, Integrations, Settings, Audit log |
| Sidebar | User pages only | Admin pages, with a link across to the user pages |

- A user going to an `/admin` page gets "not found", and the server enforces that for pages and for their API routes. Hiding links is not the check.
- `/sign-in` has no password form and no link to `/admin/sign-in`. With no SSO set up it says "Sign-in isn't set up yet. Ask your admin." The admin path is in the README.
- A user sees only the repositories their teams cover. The skill library stays readable by everyone on a team.

### Configuration

Set on **Admin → Integrations**, in a **Sign-in** section, by an admin. The client secret is stored sealed, like instance secrets, and write-only. The config file can set the same values, with the secret named by an env var, and then the dashboard shows them read-only, as it does for an instance in the config file.

Issuer and JWKS URLs must be `https`. A test button reads the discovery document or JWKS and says only whether it worked.

Changing the mode or the issuer ends every SSO session, because a session doesn't say how it began otherwise; a session records which it was. Changing the issuer also needs the client secret entered again, so a stored secret is never sent to a new token endpoint.

## Teams and repositories

- A team has a name, members and repositories. The team's own role goes.
- Each member has a role on that team: `viewer` or `maintainer`.
- A team covers chosen repositories, or all repositories of an instance, which also picks up repositories switched on later, as `team_repos` does today.
- An admin assigns teams from either side: the **Teams** page, or a **Teams** section on a repository's settings page.
- On a repository, a user has the highest role among the teams that cover it.

| Role | Can |
|---|---|
| `viewer` | Read the covered repositories: reviews, runs, findings, files, logs and settings, and the skill library |
| `maintainer` | Also what a maintainer can do today: change those repositories' settings, except the re-run caps, and press re-run, dismiss and restore |
| admin (account) | Everything: every repository, switching repositories on and off, defaults, skills, users, teams, instances, the LLM, the audit log |

## The Users page

- **Add user:** an email, and optionally teams with a role. The account shows as `invited`.
- **Add local admin:** today's flow, renamed.
- Columns: user, how they sign in (`SSO` or `Local`), role, teams, last sign-in.
- An admin can make an SSO account an admin, or take that away.
- With no sign-in set up, the page says so and links to **Integrations**.
- The sidebar reads `<name> · admin` or `<name> · user`.

The word operator leaves everything people read. The code may keep it.

## What goes away

- `GET /auth/{platform}/{instance}/login` and `callback`, `internal/dashboard/oauth.go`, and the OAuth client ID and secret of each instance.
- The platform check: `checkAccess`, `canSee`, the 5-minute recheck, `user_access`, and each session's OAuth token.
- With it, "The platform is always the ceiling". The team is now the only limit on what someone reads.
- Platform accounts and "platform admins handle their own instance".

## What changes

- **Add repository** lists what the review bot or App can reach, and only admins use it. Today it also needs the signed-in user's token.
- **Installing a skill from a repository** uses the bot's access, and only admins can do it.
- **`Dismissed by …` on a thread and the comment** shows the part of the email before `@`, so an address isn't posted on a PR. A local admin keeps the form it has. The audit log keeps the full email.
- **Dismissing in the dashboard.** The rule that an author can't dismiss their own blocking finding goes from the dashboard, because SSO accounts have no platform identity. A maintainer can dismiss any finding on a repository they maintain, and the merged or closed state rule stays. In `mayDismissBlocking` and `buttonsFor` the author check and `msgAuthorUnknown` go. `@<bot> dismiss` on the PR keeps its checks. `actions.PastDismissals` can't drop an SSO dismissal as the author's own, because it stores `dismisser_platform_id` 0, and that is accepted.
- **Add repository** needs an instance selector, because `available`, `onProjects`, `reachable` and `switchOn` are keyed on the session's instance today. `needsPlatform` goes.
- **Access checks** move from platform project IDs held on the session (`Session.Visible`, `CanSee`, `CanSeeRun`, `visibleCond`, `projectVisibility`) to what `team_repos` covers. A repository switched on but not yet reconciled must not stay hidden.
- **Per-instance scoping goes** wherever it hangs on `se.Instance`: `cover()`, `seesAccount`, `seesTeamRepo`, `teamMembers`, `coversHidden`, `seesAudit`, `attention()`, `CountWaitingAccounts`, `integrations()`, `adminSettings`, `startSkillFetch`.
- **Actor names.** `actorLabel` feeds the audit log, `Dismissal.Actor`, `skillActor` and readiness. An SSO user's audit label is the full email. `Dismissal.Actor` is shared by the comment, the thread and the dashboard, so the short form needs its own path. Old `alice (gitlab main)` rows stay as they are.
- **Skills.** `seesSourceRepo`, `sightOf`, `sightCache`, `withinCeiling` and `userCanSee` go. A skill's source repository is shown to admins only.
- **Wiring.** `SetProvider`, `connect.go`, the startup check in `main.go`, `secretFields`, `HeldSecrets` (which gains the client secret), `GET /api/instances`, `meView`, `openapi.yaml`, the chart's `values.yaml`, and the web pages `Layout`, `Teams`, `Users`, `SkillInstall`, `Waiting` and `Overview`.
- **Config keys** `oauth_client_id_env` and `oauth_client_secret_env` warn and are ignored for this release, so an old config doesn't stop reviews. A later release refuses them.

## Upgrading

The repository's rule stands: never rename or drop in the same release, and the previous release must work on the migrated database. So this takes two releases.

**This release adds, and stops using.** One migration, the next free number when the build starts (`0026` is taken and `0027` is the comment layout's).

- Every session ends, so everyone signs in again.
- `accounts` gains `sso` as a kind, `email` with a unique index, `role` and `added`.
- `team_members` gains `role`, default `viewer`. Old `platform` rows are kept but never used: no sign-in reaches them, and pergent.sh hides them.
- So **every team is left with no members**, because only platform accounts could be members. Teams keep their names and repositories. Their old role isn't carried over. An admin adds people again, by email.
- `user_access`, `teams.role`, `instances.oauth_client_id`, the platform session columns and the `oauth_client_secret` secret rows are left in place, unused.

**The next release drops them,** including the sealed OAuth secrets and the platform accounts, and `DeleteInstanceData`'s `kind = 'platform'` query changes with it.

The order for an install: upgrade, sign in as a local admin, set up sign-in on **Integrations**, add people. Sign-in can't be set up before the upgrade. The README's note says this first.

## Accepted costs

- **Nothing checks what a person can read on GitHub or GitLab.** Findings and logs contain code, so someone on a broad team reads snippets of a repository they can't open on the platform, and removing them on the platform doesn't remove them here. The owner chose this for simplicity.
- **Leaving is only as fast as the session.** Someone disabled in the identity provider keeps a session until it ends: 8 hours idle, 24 hours at most. JWT mode ends it at once, because each request needs a valid token. An admin can remove the account sooner.
- **An email is the identity.** If the provider reassigns an address to a new person, that person inherits the account. A changed address is a new account.
- **Existing platform users are lost** and must be added again, with no record of what their team's role was.
- **A team follows a repository's path.** A repository that takes over an old path inherits the team's access; without the platform check, nothing catches that. Only repositories an admin switched on are shown, which limits it.
- **A removed user can come back.** Removing an account on an allowed domain doesn't block it; it returns with nothing.
- **Maintainers are limited only by their team,** for writes too: repository settings, re-run and dismiss, not just reads.
- **An author can dismiss their own blocker in the dashboard,** if they're a maintainer there, and the bot can then approve. The dismissal is in the audit log and on the thread. A later review can also learn from that dismissal. The owner chose this for simplicity.
- **Every admin holds every power.** That includes review tokens, the LLM key, sign-in settings and local admin accounts, which SSO admins can reach. A compromised or reassigned admin mailbox can repoint sign-in, or create a local admin that outlives its removal from the identity provider. Keep the admin list short.
- **Open-source users with no identity provider** have local admins only.
- **Two people with the same mailbox name** look alike on `Dismissed by`. The audit log tells them apart.

## Releases

pergent.sh only, after 0.10.0: 0.11.0, a minor version with a breaking upgrade note, then the release that drops the old columns. The owner confirms the versions. OIDC and JWT validation both ship in the first of these. The owner names the version and gives the word for the tag. pergent itself doesn't change.

## Left to the builder

- The OIDC and JWT library, kept to a small and well-known one.
- How the sign-in settings are stored, following instance secrets.
- The order of the work, as long as no release has a window where nobody but `admin` can sign in by accident.
- Wording of pages and errors, in the README's plain style.
