# pergent.sh

A self-hosted server that reviews merge requests and pull requests. It takes GitLab and GitHub webhooks, runs `pergent agent` in a Kubernetes Job per review, publishes the result as a comment, inline threads and a status, and can approve. A dashboard manages repositories, skills and settings. pergent itself lives in `github.com/gopaytech/pergent`; it holds no platform token here and only returns a result.

## Layout

- `cmd/pergent-sh`: server entrypoint and admin commands
- `internal/engine`: the loop: dispatch, publish, commands, inline threads, approvals, incremental reviews
- `internal/result`: parses and merges the Job's result
- `internal/policy`: decides the outcome and renders the comment
- `internal/store`: Postgres; `migrations/` are applied at startup
- `internal/fates`: works out what became of each finding when a change ends (pure; the engine feeds it reviews and compares)
- `internal/dashboard`, `web/`: API and React UI
- `internal/forge`, `internal/gitlab`, `internal/github`: platform clients
- `internal/threads`: decides what happens to each inline thread
- `internal/kube`, `internal/inputs`: build the Job and what it reads from `/in`
- `charts/pergent-sh`: Helm chart

## Build and test

```bash
make build    # the server
make test     # go test ./...
make vet
cd web && npm ci && npx vitest run && npm run typecheck && npm run lint
```

Tests that need a database skip without `PERGENT_SH_TEST_DATABASE_URL`, so a local `make test` can pass while CI fails. Anything touching `internal/store`, or a test that seeds rows, needs that variable or a green CI `test` job before it counts as tested.

## Rules

- The repository is public: no private hostnames, company names or internal systems, in code, tests, docs or commit messages.
- A run's quiet value and comment layout (`comment_layout`, a list of `block:state` entries, NULL for the default) are read at dispatch and stored on the run. The prompt, threads and comment use the run's value, never the repository's. A layout changes the comment only: no decision, verdict, score or status may depend on it, and a list `render.ParseLayout` refuses gives the default comment.
- A migration must leave the previous release working on the migrated database: add columns with a default or NULL, never rename or drop in the same release.
- A new config key breaks rollback, because older releases refuse unknown keys: say so in the README's Upgrading section.
- pergent is pinned by `go.mod` and `defaultImage` in `internal/config`. Move them together. Its version gates behaviour, so a pinned image must really report that version.
- User-facing behaviour goes in the README in the same change. Changing a reply or a note means changing what the README quotes.
- `main` is protected: changes, including the release's chart commit, go through a PR, and `test`, `web` and `chart` must pass.
- Accounts: a **local admin** (kind `password`, always admin) or an **SSO account** (kind `sso`, identified by its email, lower-cased with ASCII rules by `store.NormalizeEmail`). The account's role is `admin` or `user`; a team member is a `viewer` or `maintainer`. The old platform accounts stay in the table, hidden and unused. In text people read, say "local admin": never operator, password admin or platform admin (the code may keep them).
- Access is the account's role plus `team_repos`, read on every request (`Auth.load`); nothing asks GitLab or GitHub. A team is the only limit on what a user reads. Every admin API route is registered through `Dashboard.adminRoute`, which answers a non-admin 404 (never 403); `routes_test.go` ties that helper to the router scan and `x-pergent-access: admin` in `openapi.yaml`. A repository's `.../teams` is admin-only inside its handler (the `{rest...}` routes are session routes).
- Sign-in is OIDC (code flow with PKCE) or a validated JWT header, behind an allowlist (domains, emails, or `added` by an admin). `dashboard.sign_in` in the config wins over the stored row and makes the page read-only. The client secret is sealed, write-only and dropped when the issuer changes; a change of mode or issuer ends the SSO sessions, and so does the start-up sweep. Never log a token, an ID token or the client secret. Paths: `/sign-in` (SSO only), `/admin/sign-in` (local admins), `/admin`.
- An email stays in the dashboard: the audit log keeps the full address, the platform gets only the part before `@` (`Dismissed by`).
