# 🛠️ Development

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
