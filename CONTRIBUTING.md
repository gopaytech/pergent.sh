# Contributing

## Setting up

You need Go, Node and, for the database tests, PostgreSQL. The README's Development section has the database command.

```bash
make build
make test
make vet
cd web && npm ci && npx vitest run && npm run typecheck && npm run lint
```

Tests for the store and the end-to-end flow skip unless `PERGENT_SH_TEST_DATABASE_URL` is set. CI always runs them, so a passing `make test` without it doesn't mean they pass.

## Making a change

- Open a pull request against `main`. `main` is protected, and `test`, `web` and `chart` must pass before merging. pergent reviews every pull request.
- Keep commits small, with a subject like `fix: …`, `feat: …`, `docs: …` or `test: …`.
- Write tests first where you can, and test behaviour rather than mocks.
- Put user-facing changes in the README in the same pull request, and anything an operator must do in its Upgrading section.
- This repository is public: use example names, never a private hostname, company name or internal system.

## Database changes

Migrations in `internal/store/migrations` run at startup, in order. A migration must leave the previous release working on the migrated database, so add columns with a default or NULL. Don't edit a migration that has been released.

## Releasing

1. In a pull request: bump `version` and `appVersion` in `charts/pergent-sh/Chart.yaml` and `image.tag` in `values.yaml`, then run `make readme` and `make helm.create.releases`.
2. Once it has merged and CI is green on `main`, tag `vX.Y.Z` on that commit and create the GitHub release. The tag builds the image.
3. Check the image is published for `linux/amd64` and `linux/arm64`, and that the chart is in `https://gopaytech.github.io/pergent.sh/charts/releases/index.yaml`.
