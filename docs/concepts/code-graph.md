# 🕸️ Code graph

## What it is

A code graph lists the files, functions, methods and types of a repository's default branch, and who calls and imports what. With it, pergent gives each batch of a review a short hint: the callers and callees of the code the batch changes, and the files that import the changed files. The model then needs fewer `read` and `grep` steps to find related code. pergent's own [Code graph](https://github.com/gopaytech/pergent/blob/master/docs/concepts/code-graph.md) page says what the hint holds.

It is **off by default, per repository**. In a test on 10 changes it cut the steps by about 22% and the `grep` calls by about 30%, but the token cost did not change. Turn it on where you want to try it.

With the switch off, nothing changes: pergent.sh builds no graph, stores none, and the review Job is the same as before.

## Turn it on

1. The install must keep graphs: set `graphs.enabled: true` in the Helm chart. It needs the dashboard (`config.dashboard.url`). The chart then makes a PersistentVolumeClaim (`graphs.persistence.size`, default `5Gi`; `graphs.persistence.storageClass`), mounts it on the pergent.sh pod only, and sets `PERGENT_SH_GRAPHS_DIR` and `PERGENT_SH_GRAPH_IMAGE`.
2. The review image must know pergent's `--graph` flag: the pergent release after v0.10.0. **An older image exits 2 on the unknown flag, so every review of a repository with the switch on fails.** Keep the switch off until `jobs.image` is that release.
3. On the repository's page, open **Settings** and tick **Build and use a code graph**. Maintainers and admins of the repository can change it. A repository from the config file has no switch: it is always off.

The same page shows the stored graph: its branch and commit, nodes, edges and size, when it was built, a build that is queued or running, and why the last build failed. Never the graph itself.

The API is `PUT /api/repositories/{instance}/{project}/graph` with `{"enabled": true}` or `{"enabled": false}`. Turning it on in an install with no graph storage is `409`. Each change writes an audit entry, `graph_on` or `graph_off`.

## When it builds

- Turning the switch on, and every review of the repository, asks for a **head check**. The review never waits for it.
- A head check reads the default branch and its head commit with the review bot or App, **at most once every 10 minutes per repository**. When no graph is stored, or the stored graph is of another commit, it queues a build of the head.
- A repository has at most one build. A newer head replaces the commit of a queued build. A running build finishes first, and the next check queues the newer head.
- One build Job runs at a time in the whole install.
- A build that fails keeps the graph stored before it. The next head check after 10 minutes tries again.

So the first review after a merge uses a graph that is one merge old. Push events don't start a build.

## The build Job

`pergent-graph-<12hex>`, in the Jobs namespace. Its pod runs as user 65532 with a read-only root filesystem, no service account token, `backoffLimit: 0`, a 15 minute deadline, a 4Gi work volume, a 2Gi output volume, and a 2Gi memory limit per container. It carries the managed-by label, so the `jobEgress` NetworkPolicy applies.

| Container | Image | Holds | Does |
|---|---|---|---|
| `clone` (init) | `jobs.image` | the read-only clone token | clones the default branch at the chosen commit, depth 1, https only, hooks off |
| `graphify` (init) | the graph image | **nothing** | `graphify extract --code-only`, then `pergent graph convert`; it reads the clone read-only |
| `post` | pergent.sh's image | the build's one-time token | posts the graph, or the error, to `<callback_url>/graphs/<id>/result` |

graphify never shares a container with a token or the LLM key, and it runs with `--code-only`, so it uses no LLM. The graph image is `ghcr.io/gopaytech/pergent.sh-graph`: Python with `graphifyy==0.9.112` and the pergent CLI of the version pergent.sh is built with. It has the same tags as the pergent.sh image (`graphs.image.tag`, empty for the chart's appVersion).

## Storage

- One file per repository on the graphs volume, named by a hash of the instance and the path. A new graph is written to a temp file and renamed, so **only the newest graph is kept**.
- pergent.sh checks every graph it gets with pergent's `graph` package: the pergent-graph/v1 format, names only and no free text, files the read rules allow, at most 64 MiB. A graph of another commit than the build's is refused. It stores its own encoding of the graph, not the bytes it got.
- A build's error is shown only when it is one of the known messages; any other text is shown as `the graph build failed; see the log of its Job`.
- **Turning the switch off deletes the graph**, its row and a running build Job. A kept graph would only go stale, and the graph is built again from the head when the switch comes back on.
- **Removing the repository** deletes its graph too. **Pausing keeps it.**

## The review

- A review gets the graph only when the switch is on, a graph is stored, and the change targets the branch the graph was built from. A change into another branch gets none, because its line numbers can be far off.
- Then the review Job has one more init container, `graph`, with pergent.sh's image and the run's result token only. It downloads `GET /runs/<id>/graph` into `/graph/graph.json`, outside the checkout, and pergent gets `--graph=/graph/graph.json`. The pergent container mounts `/graph` read-only.
- The download answers only the run's own repository, only while the run is running, and only with that run's token, which it doesn't spend. Anything else is `404`.
- A failed download leaves no file, and the `graph` container still exits 0. pergent then logs `graph: skipped` and reviews as it would without a graph. A graph never makes a review pass or fail.

## Exposing

The build Jobs post to `/graphs/`, and review Jobs read `/runs/<id>/graph`. The chart points `callback_url` at its Service, so nothing needs exposing. With a public `callback_url`, route `/graphs/` as well as `/runs/` ([Exposing pergent.sh](../setup/install.md#exposing-pergentsh)).
