# Code graph context pack

Date: 2026-11-18. Size: project (pergent and pergent.sh).

## Why

The goal is **fewer LLM tokens per review**. Today the model finds context by itself with `read`, `grep`, `glob` and
`list`, up to `--max-turns` (default 20) steps. Every step sends the whole conversation again, so each
"who calls this function?" search costs a full round trip and the file text it reads.

A code graph answers that question without the model. pergent.sh builds a graph of each repository's default branch,
keeps it on a PVC, and gives a copy to every review Job of that repository. Before each batch run, pergent looks up the
callers and callees of the changed symbols and attaches a short list. The model gets the answer without spending steps.

We do not know yet how many tokens go to exploring. pergent counts tokens per run and per skill, but does not split
them into skill prompt, diff, file reads and output. So the work goes in 4 steps with 2 gates, and each gate can stop
it (see **Plan of work**).

## Goals

- A lower review cost (tokens times price) and fewer steps per review, with the same findings.
- A graph per repository, built from the default branch, stored by pergent.sh, never committed to the repository.
- A generic design: graphify is the first provider, and another one (for example a SCIP indexer) can replace it
  without changes to skills, prompts or the pack.

## Non-goals

- Graphs of linked repositories. The `impact` skill stays as it is.
- A query tool (MCP) for the model. It can come later on the same stored graph.
- Graph updates per MR. The review uses the default-branch graph as it is.
- An LLM pass in the graph build (graphify's semantic mode for docs, PDFs, images).
- Showing graph contents in the dashboard.

## Anti-goals

- **No false pass.** A missing, stale or broken graph never makes a review pass that would not pass without it.
- **No new leak.** The graph never shows the model a file that pergent's read rules hide. It never holds secret
  values or free text. The graph tool never runs next to the LLM key or a platform token.
- **Off means unchanged.** With the feature off, or with no graph, the review is byte for byte the same as today.

## Decisions (owner)

| Question | Decision |
|---|---|
| Where the graph lives | A ReadWriteOnce PVC mounted only on the pergent.sh pod |
| How Jobs get it | Review Jobs download their own repository's graph over HTTP with the per-run token |
| Which commit | The default branch only. No update for the MR's changes |
| How the model gets it | A context pack attached to each batch run (option A) |
| Provider | Generic, graphify first, one pinned graphify version |
| Order | Measure first, then pergent, then prove, then pergent.sh. pergent.sh work starts only after the proof passes |
| What counts | Estimated cost: input, output, cache read and cache write tokens, each times the model's public price |
| Step 3 details | PVC size, build timeout, head-check interval and the other pergent.sh numbers are confirmed after the proof, with real graph sizes and build times |
| Step 3 (2026-11-20) | Build it although the proof saved no cost: off by default, a switch per repository, off deletes the graph |
| The 7 hand-over calls | Confirmed: no pack for non-default branches, pergent downloads the graph, one shared read-deny filter, a pack failure means `graph: skipped`, head check at most every 10 minutes, cost-based success lines, off by default |

## Approaches

**Storage.**

| | Approach | Trade-off |
|---|---|---|
| Commit to the repository | Authors commit `graph.json` | Goes stale, can be faked by the author, adds noise to every MR. Rejected |
| Build in every review Job | No storage | Build time on every review, and the graph tool runs in the review Job. Rejected |
| Postgres | Already backed up | Large blobs in the database. Rejected by the owner |
| **PVC, RWO, on pergent.sh (picked)** | Jobs download | Works with any storage class. pergent.sh is the one place that decides which graph a Job gets |
| PVC, RWX, mounted into Jobs | No download | Needs an RWX storage class. A wrong `subPath` lets a review read another repository's graph. Rejected |

**Delivery to the model.**

| | Approach | Trade-off |
|---|---|---|
| **A. Context pack (picked)** | pergent attaches callers and callees of changed symbols | No new tool, and the same input gives the same pack, so it is easy to test and measure. Every batch run pays for the pack |
| B. Query tool | The model calls a pergent MCP tool | Pays only for what it asks, but each call is a step, and it adds MCP to opencode for the first time |
| C. Both | Pack plus tool | The most savings and the most work |

## Design

### Graph format `pergent-graph/v1` (pergent)

pergent owns the format. One JSON file per repository:

```json
{
  "format": "pergent-graph/v1",
  "provider": "graphify", "provider_version": "0.9.112",
  "commit": "<sha>", "built_at": "<RFC 3339>",
  "nodes": [{"id": "n1", "kind": "function", "name": "Dispatch", "file": "internal/engine/dispatch.go", "line": 120, "end_line": 180}],
  "edges": [{"from": "n2", "to": "n1", "kind": "calls"}]
}
```

- Node kinds: `file`, `function`, `method`, `type`, `config_key`. Edge kinds: `calls`, `imports`, `inherits`,
  `contains`. Anything else from a provider is dropped.
- `end_line` is optional. Some providers give only a start line.
- No other fields. Comment text, doc text, values and labels that are not names are never copied.

### Providers (pergent)

A provider is a pinned command plus a converter in Go: `pergent graph convert --provider graphify --in <dir> --out <file>`.
A new provider means a new converter and a new pinned tool in the build image. Nothing else changes.

The graphify converter accepts only the `schema_version` of one pinned graphify version. Any other version fails with a
clear error, so the pack is skipped and never built from a format the converter does not know.

The converter is the security gate:

1. It keeps only the fields of `pergent-graph/v1`.
2. It drops every node whose file matches pergent's read deny rules, the hardened set included (`*.env`, `*.pem`,
   `*.key`, `*.tfvars`, …). It uses the same list as the opencode config, not a copy. graphify indexes `.tfvars`, so
   this is needed.
3. It drops edges that point to a dropped node.
4. It refuses output over the size limit.

graphify runs with `--code-only`, so no LLM is used and nothing leaves the pod.

The pergent.sh side (build, storage, serving, review Job, chart) is in **Step 3 in pergent.sh** below. It replaces the
first draft of these sections.

### Flags (pergent)

- `--graph=<file>` reads a local graph file. It works in `pergent` (local and CI mode, `cmd/main.go`) and in
  `pergent agent`. The proof uses it with `pergent --local`.
- There is no `--graph-url`. In pergent.sh an init container downloads the graph (see **Step 3 in pergent.sh**).
- With no flag, a missing or failed file, or a file that does not parse as `pergent-graph/v1`, pergent adds nothing, and
  the run is the same as today.
- CLI users build the graph themselves: install the pinned graphify, run it, then `pergent graph convert`. The README
  gets a short section on this. There is no CI helper and no auto-build in the CLI.

### The pack (pergent)

For each batch run, pergent writes `graph-context.md` next to `diff.patch` and names it in the run message.

- **Changed symbols:** for each hunk in the batch, take the old-side lines (the base is close to the default branch) and
  find the symbol that holds them. With `end_line`, the range must contain the line. Without it, use the last symbol in
  that file that starts at or above the line.
- **Per symbol:** its callers and callees as `name  file:line`. Also list the files that import the changed file.
- **Filter:** one filter for every skill: drop entries in files that `ReadDenied` blocks. pergent agent
  always uses the hardened set. A skill's `paths` picks the files it reviews, not the files it may read, so the pack does
  not filter by `paths`. Callers in other folders are the reason for the pack.
- **Limits:** at most 10 callers and 10 callees per symbol, and about 2,000 estimated tokens per pack
  (`EstimateTokens` in `internal/diff/classify.go`, 4 bytes per token). The limits stay fixed for the whole proof.
  Other limits are tested later, on a new set of MRs.
- **Order:** fill the budget with callers first, then the files that import the changed file, then callees. Callees are
  partly visible in the diff already, so they are cut first. Sort inside each group so the same input gives the same
  pack. When cut, end with one line that says how many were left out.
- **Header:** the graph commit, plus a short note: the graph is from the default branch, line numbers may have moved,
  and code added by this change is not in it.
- The pack goes to batch runs only. Repair, cross-batch, verify and `impact` runs do not get it.
- **Failure:** a pack that cannot be built or written follows the dismissals pattern
  (`review.go`, the dismissals attachment): the run goes on without the pack, and the result notes `graph: skipped`
  with the reason. A pack failure never makes a batch `notReviewed`, and never makes it pass. The pack header calls the
  list a hint, so the model still checks the code it relies on.

## Plan of work

| Step | What | Gate after it |
|---|---|---|
| **0. Measure** (pergent) | Per-step usage, released on its own | Exploring is at least 25% of the cost, or the graph work stops |
| **1. Graph** (pergent) | `pergent-graph/v1`, the graphify converter, `--graph=<file>`, the pack. Released | None |
| **2. Proof** | The same MRs, with and without the pack | 20% or more: step 3. 10% to 20%: keep the CLI flag only. Under 10%: remove the graph code |
| **3. Service** (pergent.sh) | Usage columns, PVC, build Job, endpoints, setting, chart, the `pergent-graph` image | None |

### Step 0: measure

`steps` is already counted (`internal/runner/parser.go`). Each run's usage also records:

- the tokens of each step, and which tools that step called
- the number of calls and the output bytes of `read`, `grep`, `glob` and `list`, each tool on its own
- the byte size of the skill prompt and of the attachments

**Cost.** Every token type (input, output, cache read, cache write) is multiplied by the proof model's public price,
entered by hand in the proof sheet. pergent.sh has no price data yet. Raw token counts are reported next to the cost.

**Exploring share.** A step is exploring when it reads the result of a `grep`, `glob`, `list`, or a `read` of a
repository file. A `read` of pergent's own temp files (the on-demand patches and the diff index) is not exploring,
because a graph cannot replace it. The share is the cost of the exploring steps divided by the cost of all runs, summed
over the 10 MRs.

**Gate.** One run per MR, with no graph. If the share is under 25%, the graph work stops. Step 0 stays in pergent, and
the write-up names the biggest cost it found as the next thing to look at. pergent.sh stores the new fields only in
step 3.

### Step 2: proof

- **MRs:** 10 merged MRs, 5 from pergent and 5 from pergent.sh. They are the largest Go changes by changed lines, with no
  docs-only MRs. The step 0 baseline uses the same 10.
- **Graphs:** one graph per MR, built at that MR's base commit. A graph of today's default branch would already hold the
  MR's own change and show callers that did not exist yet.
- **Runs:** `pergent --local`, 2 runs per MR without the pack and 2 with it, so 40 runs. Every run uses only the built-in
  `code-review` skill, one pinned pergent commit, the same model (the one reviews use today) and the same
  `--max-turns`. Repo skills are turned off.
- **Failed runs:** a run that fails (context overflow, `--max-turns`) is run again once. If it fails again, that MR is
  removed from both sides and listed with the reason. If 3 or more MRs drop out, the proof does not count and new MRs are
  picked.
- **Findings:** a finding is lost only when it is a blocker in both runs without the pack and in neither run with it.
  The owner judges the blockers by hand.
- **Success:** at least 20% lower cost, with no finding lost. The 10% and 20% lines in the table apply to cost.
- **Who runs it:** pergent-developer, locally, with the owner's LLM key and no platform calls. Before the 40 proof runs,
  it shows the MR list and a cost estimate from the 10 baseline runs, and starts only after the owner's OK. It stops and
  asks again if the spend reaches 1.5 times that estimate.
- **Notes:** the script, the MR list, the costs and the blocker judgments are kept outside the public repositories.

### Step 3 in pergent.sh (confirmed 2026-11-20)

The proof cut steps by 22% and `grep` calls by 30%, but the cost did not move (-0.6%). By the gate that is "remove the
graph code". The owner chose to build step 3 anyway, **off by default and switched on per repository**, so it can be
tuned later. Off must cost nothing.

#### The switch

- `dashboard_repos.graph_context`, `boolean NOT NULL DEFAULT false` (migration `0044`), the shape of the pause switch
  (`0042`). A repository from the config file has no switch: it is always off. There is no config key, because a new key
  breaks a rollback.
- `PUT /api/repositories/{instance}/{project}/graph` with `{"enabled": true|false}`. Maintainers and admins of the
  repository. The same value again changes nothing. Each change writes an audit entry, `graph_on` or `graph_off`.
- Turning it on is `409` when the install has no graph storage (`graphs.enabled=false` in the chart). Turning it off
  always works.
- **Off deletes.** Turning the switch off deletes the stored graph file, the `repo_graphs` row and a running build Job.
  Kept and marked unused was rejected: a kept graph only goes stale (it is rebuilt from the new head when the switch
  comes back on anyway), it holds disk for a repository that asked for nothing, and every reader would have to check a
  second flag. Deleting keeps one rule: a graph file exists only for a repository whose switch is on. Removing a
  repository does the same. Pausing keeps the graph.

With the switch off nothing on the review path changes: dispatch reads no graph row, starts no head check and builds a
Job that is byte for byte the Job of today.

#### When it builds

- Dispatch of a review of a repository with the switch on asks the graph loop for a head check (a non-blocking send;
  a full queue drops it, the next review asks again). Turning the switch on asks too. The review never waits.
- The graph loop runs in its own goroutine, like the readiness scans. A head check runs at most every 10 minutes per
  repository (`repo_graphs.checked_at`, set in the same statement that decides, so it holds across restarts). It reads
  the default branch and its head with the review bot or App, as readiness does. When no graph is stored, or the stored
  commit is not the head, it queues a build of that commit.
- One build per repository: the row holds at most one build. A newer head replaces a queued build's commit; a running
  build is left alone, and the next check after it ends queues the newer head. One build Job runs at a time in the
  whole install.
- A build that fails keeps the graph stored before it, and records the error. The next head check after 10 minutes
  tries again.

#### Build Job

`pergent-graph-<12hex>` in `jobsNamespace`, built like the readiness scan Job:

| Container | Image | Holds | Does |
|---|---|---|---|
| `clone` (init) | `jobs.image` | the read-only git token | `scan.sh`: a depth-1 clone of the default branch at the chosen commit |
| `graphify` (init) | the graph image | nothing | `graphify extract --code-only`, then `pergent graph convert --commit <commit>` |
| `post` | the pergent.sh image | the one-time build token | `pergent-sh post-graph`: posts the graph file, or the error |

- The graph tool never sees a token or the LLM key; the token that can post never shares a container with graphify.
- Every container: non-root, read-only root, all capabilities dropped, no service account token, `backoffLimit 0`,
  a 15-minute deadline, a 4 GiB work volume, a 2 GiB memory limit. The pod has the managed-by label, so `jobEgress`
  applies.
- **The graph image** is `ghcr.io/gopaytech/pergent.sh-graph`, built from `graph.Dockerfile` in pergent.sh: Python
  with `graphifyy==0.9.112` and the pergent binary of the version `go.mod` pins, so the converter writes the format
  pergent.sh reads. CI publishes it with the same tags as the pergent.sh image.

#### Storage and serving

- The graph PVC (ReadWriteOnce) is mounted only on the pergent.sh pod at `/var/lib/pergent-sh/graphs`
  (`PERGENT_SH_GRAPHS_DIR`). One file per repository, named by the SHA-256 of the instance and the lower-cased project,
  so no path from a platform becomes a path on disk. Written to a temp file and renamed: only the newest graph is kept.
- `repo_graphs` holds one row per repository with the switch on: the stored graph (branch, commit, built time, size,
  nodes, edges), the head check time, the build (state, id, commit, Job, token hash, deadline) and the last error.
- `POST /graphs/{id}/result` takes the build's post, with the one-time token, once. The body is the graph file or
  `{"error": "<text>"}`. pergent.sh decodes the graph with pergent's public `graph` package, refuses one whose commit is
  not the build's, encodes it again and stores that. An error text that is not one of the known messages is stored as
  a generic one, so no text from the Job reaches the dashboard.
- `GET /runs/{id}/graph` with the run's result token returns the graph of that run's repository, or 404. It checks the
  token without spending it, and only while the run is running. It never serves another repository's graph, nor one
  for an API review.

#### Review Job

- Dispatch adds the graph only when the switch is on, a graph is stored, and the change targets the branch the graph
  was built from. The branch is stored with the graph, so dispatch makes no platform call for it.
- Then the Job gets one more init container, `graph`, with the pergent.sh image and the result token only:
  `pergent-sh get-graph` downloads `/runs/<id>/graph` to `/graph/graph.json`, an `emptyDir` outside `/work`. It always
  exits 0: a failed download leaves no file. The pergent container mounts `/graph` read-only and gets
  `--graph=/graph/graph.json`. A missing or bad file is pergent's `graph: skipped`, and the review goes on as today.
- There is no `--graph-url`: pergent reads a local file, so the download stays in pergent.sh's own code, and pergent's
  container keeps only the tokens it had.
- **The image must know `--graph`.** An older pergent exits 2 on an unknown flag, so a repository with the switch on
  fails its reviews on an image older than the pergent release that adds `--graph`. The docs say so.

#### Chart

`graphs.enabled` (default `false`), `graphs.persistence.size` (default `5Gi`), `graphs.persistence.storageClass`,
`graphs.image.repository`, `graphs.image.tag` (default: the chart's appVersion). With `graphs.enabled=false` there is no
PVC, no `PERGENT_SH_GRAPHS_DIR` and no `PERGENT_SH_GRAPH_IMAGE`, and the switch can't be turned on.

#### Dashboard

The repository page has a **Code graph** switch and shows the stored graph's commit, built time, size, nodes and
edges, whether a build is queued or running, and the last error. Never the contents.

#### Release order

pergent first (the release with `graph`, `graph convert` and `--graph`), then pergent.sh: move the `go.mod` pin and
`defaultImage` to that release together. Until then pergent.sh pins the pergent master commit that merged the graph PR as a
pseudo-version, and the review image does not know `--graph`.

#### Open after the first slice

- `run_usage` columns for the step 0 fields, and the run page line "graph used: commit, pack size, cut".
- A switch for repositories from the config file.
- Push events as a build trigger.

## Left to the builder

- The JSON shape of `/graphs/<id>/result` and the build token scheme. Follow the readiness scan result
  (`callback/oneshot.go`).
- How step 0 reads per-step tokens and tool calls from the opencode event stream.
- Whether graphify gives an end line for code symbols. `end_line` stays optional either way.
- The size limit for one graph, the build timeout and the build Job resources.
- The mapping from graphify node and edge types to the `pergent-graph/v1` kinds.
- The exact wording of the pack header and of the line in the run message.
- How to keep graphify offline in the build (no update check, no telemetry).
