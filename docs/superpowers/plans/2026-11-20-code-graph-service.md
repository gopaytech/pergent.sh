# Code graph, step 3 (pergent.sh) — plan

Spec: `docs/superpowers/specs/2026-11-18-code-graph-context-design.md`, section **Step 3 in pergent.sh**.
pergent side: the open graph PR (`graph/`, `pergent graph convert`, `--graph`), pinned here as a pseudo-version.

**Goal:** a per-repository Code graph switch, off by default. With it on, pergent.sh builds a graph of the default
branch in a Job, keeps the newest one on a PVC, and gives it to review Jobs of changes into that branch. With it off,
nothing on the review path changes.

**Anti-goals (the failure bar):** off means the Job of today, byte for byte; a missing or bad graph is a normal review,
never a pass and never a failure; graphify never shares a container with a token or the LLM key; a review downloads
only its own repository's graph.

Verify gate after every task: `go vet ./...`, `gofmt -l .`, the task's package tests with
`PERGENT_SH_TEST_DATABASE_URL` set (database tests skip without it).

## Task 1: the switch and the table (store)

- `internal/store/migrations/0044_code_graph.sql`: `dashboard_repos.graph_context boolean NOT NULL DEFAULT false`;
  table `repo_graphs` (one row per repository, unique on `(instance, lower(project))`): stored graph (branch, commit,
  built_at, size_bytes, nodes, edges), `checked_at`, the build (`build_state` '' / queued / running, `build_id`,
  `build_branch`, `build_commit`, `build_job`, `build_token_hash`, `build_deadline`, `queued_at`), `error`, `failed_at`.
- `DashboardRepo.GraphContext`, read with the other columns. `SetDashboardRepoGraph(instance, project, on) (found,
  changed)`, the shape of `SetDashboardRepoEnabled`.
- `internal/store/graphs.go`: `RepoGraph`, `TakeGraphCheck` (sets `checked_at` only when due, returns whether it did),
  `QueueGraphBuild` (new build, or a newer commit for a queued one; a running one is left), `ClaimGraphBuild` (cap,
  advisory lock), `StartGraphJob`, `GraphBuildFor` (id + token, running, before deadline), `StoreGraph` (by build id),
  `FailGraphBuild`, `ExpiredGraphBuilds`, `DeleteRepoGraph` (returns the build Job name).
- Tests first in `internal/store/graphs_test.go`: each outcome, case-insensitive project, one build per repository,
  the token checked and spent.

## Task 2: settings

`settings.Effective.GraphContext` from the dashboard row; a config repository is always false. Test in
`internal/settings`.

## Task 3: the Jobs (kube)

- `internal/kube/graph.go`: `GraphJobName`, `BuildGraphJob`, `BuildGraphSecret`, `LaunchGraph`. Containers `clone`
  (scan.sh, git token), `graphify` (graph image, no secret), `post` (pergent.sh image, build token). Volumes `work`
  and `out`, emptyDir with size limits. Deadline 15 min, memory limit 2Gi, non-root, read-only root.
- Review Job: `JobParams.GraphURL` + `GraphImage`. Set: an init container `graph` (`pergent-sh get-graph`, the result
  token only), an emptyDir `graph` at `/graph`, read-only in the pergent container, `--graph=/graph/graph.json`. Empty:
  the Job is unchanged (golden test `testdata/job-no-links.golden.json` stays as it is).
- Tests first: no secret in the graphify container, `backoffLimit 0`, the no-graph Job unchanged.

## Task 4: the Job commands (cmd/pergent-sh)

- `post-graph --out <dir> --work <dir> --result-url <url>`, token `PERGENT_SH_GRAPH_TOKEN`: posts `<out>/graph.json`
  (at most `graph.MaxBytes`) or `{"error": ...}` from the clone's or graphify's error file.
- `get-graph --url <url> --out <file>`, token `PERGENT_RESULT_TOKEN`: downloads at most `graph.MaxBytes`, writes a
  temp file and renames; always exits 0.
- Tests with `httptest`.

## Task 5: the engine

- `internal/engine/graphs.go`: `GraphsOn`, `WantGraph` (non-blocking), `RunGraphs` loop (head checks, claim + launch,
  sweep of expired builds), `CompleteGraph` (decode with pergent's `graph`, commit must match, encode, temp + rename
  under `graphMu`, `StoreGraph`), `GraphTooLarge`, `DropGraph` (Job, row, file under `graphMu`), `RunGraph` (serve a
  run's graph), `graphURL` for dispatch.
- dispatch: with `repo.GraphContext` only, `WantGraph` and `graphURL`; the switch off reads nothing.
- reconcile `removeProject` calls `DropGraph`.
- Tests: off leaves the Job unchanged and reads no row; on with a graph of the target branch adds the init container;
  another target branch gets none; complete stores and serves; a wrong commit is refused; drop removes the file.

## Task 6: routes

- `internal/callback/graph.go`: `POST /graphs/{id}/result` (oneShot, `graph.MaxBytes`), `GET /runs/{id}/graph`.
- `internal/server` routes; `cmd/pergent-sh` wiring (`PERGENT_SH_GRAPHS_DIR`, `PERGENT_SH_GRAPH_IMAGE`, the loop).

## Task 7: dashboard

- `PUT /api/repositories/{instance}/{project}/graph` `{"enabled": bool}`, maintainers and admins, 409 when graphs are
  off in the install, audit `graph_on` / `graph_off`; off calls `DropGraph`, on calls `WantGraph`.
- `RepositoryDetailView.graph`: `enabled`, `available`, the stored graph's fields, `building`, `error`.
- `openapi.yaml` and `openapi_test.go`. Web: a Code graph section with the switch and status on the repository page.

## Task 8: chart, image, docs

- Chart: `graphs.*` values, a PVC template, the env vars and the mount; `ci-values.yaml` turns it on; `make readme`.
- `graph.Dockerfile` (Python + `graphifyy==0.9.112` + pergent from the `go.mod` pin); `ci.yml` and `release.yml`
  publish `ghcr.io/gopaytech/pergent.sh-graph`.
- `docs/concepts/code-graph.md`, a line in `docs/README.md`, `docs/reference/http-routes.md`,
  `docs/reference/configuration.md` (env vars), `docs/upgrading.md` under Unreleased.

No release, no version bump.
