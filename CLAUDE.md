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
- A migration must leave the previous release working on the migrated database: add columns with a default or NULL, never rename or drop in the same release.
- A new config key breaks rollback, because older releases refuse unknown keys: say so in the README's Upgrading section.
- pergent is pinned by `go.mod` and `defaultImage` in `internal/config`. Move them together. Its version gates behaviour, so a pinned image must really report that version.
- User-facing behaviour goes in the README in the same change. Changing a reply or a note means changing what the README quotes.
- `main` is protected: changes, including the release's chart commit, go through a PR, and `test`, `web` and `chart` must pass.
