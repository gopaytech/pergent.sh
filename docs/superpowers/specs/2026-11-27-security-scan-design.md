# Security scan: a whole-repository security audit, started by hand

Status: agreed with the owner on 2026-11-27 in a design session. The owner approves this document before the build starts.

## Why

A review looks at one merge request: the diff and the files around it. Nothing looks at the whole repository for security problems: an API with no auth check, an injection in code nobody touched for a year, a secret written to a log. A full security audit is expensive, so it must never run on its own, and a team needs a place to see and triage what it found.

## Goals

- A **security scan** of a repository's default branch, started only by a person in the pergent.sh console.
- First class, like review: its own tab on the repository page, its own runs, a list of findings with a life cycle, usage, quotas, audit and metrics.
- The scan uses an existing, maintained security skill as it is. pergent writes no security prompt of its own.
- The findings are stored in a SARIF 2.1.0 shape, so any scanner that writes SARIF can be added later.
- Optional, per repository: a review gets the open findings in the files it changes, as data.

## Non-goals (v1)

- A scan started by a webhook, a schedule or a push.
- A choice of skill by users. The pergent developers choose the scanner and build it into the image.
- A `@bot security-review` comment command (see Later).
- A scan of a branch other than the default branch.
- Blocking a merge request because of a finding.
- Uploading SARIF to GitHub code scanning.
- A notification when a scan ends. The Security tab shows the result.

## Approaches considered

**How the scan runs.**
- **A new `pergent scan` command (chosen).** It reuses pergent's runner, opencode config, usage parsing, failure causes and result delivery. It has no diff, no comment and no verdict.
- `pergent agent` with a fake diff. No code, but the review wording, the diff budgets, the verdict and the fingerprints are all wrong for a scan. Useful only for a first cost test.
- A plain `opencode run` script in the Job. Loses pergent's hardening and its usage and failure handling.
- Another runner, such as the Anthropic security review action. It scans only a diff and works with one provider only.

**Which prompts.**
- **An existing skill, used as it is (chosen).** The pergent developers pick it. v1 uses Cloudflare `security-audit` (MIT), the only maintained skill found that audits a whole repository: map, hunt with sub-agents, verify each finding with a fresh agent, keep the rejected records.
- pergent's own prompts, adapted from other skills. The owner does not want to write and keep a security prompt.

**How a skill plugs in.**
- **A manifest per skill, SARIF as the common format (chosen).** A skill that writes SARIF needs only a manifest. A skill with its own format also gets a small converter in pergent.
- Every skill must write SARIF. One parser, but the model rewrites the skill's own output, which can add errors and drops details such as `needs_validation`.
- A Go adapter per skill. Exact, but every new skill is new pergent code.

**Where the skill lives.**
- **Inside a dedicated image (chosen).** The whole skill folder, pinned to one commit, with its licence.
- In the skill library. Today the library and repository skills use only the `SKILL.md` text and at most 128 KiB, and Cloudflare's skill is 14 files, about 183 KB.

## Design

### The image: `pergent-security`

- Built from the same `pergent` Dockerfile as a second target, released with the same version as the review image: `ghcr.io/gopaytech/pergent-security:vX.Y.Z`. Same `pergent` binary, same opencode version.
- Adds **node** (the skill's validators need it). Has **no curl or wget** in the last stage.
- Holds the built-in skills under `/opt/pergent/security-skills/<name>/`, each at a pinned commit, with its `LICENSE` and a `pergent-scan.yaml` manifest written by the pergent developers.
- A new skill version, or a new skill, is a new image release.

### The manifest: `pergent-scan.yaml`

```yaml
name: security-audit
entry: SKILL.md
tools: [bash, write, task]
steps:
  main: 600
  subagent: 100
task: >-
  Full audit mode. Target: {target}. Output directory: {output}.
  Prior run: {prior}. Do not ask questions. No OS sandbox is
  available: do not build, test or run the target's code.
output:
  path: findings.json
  format: cloudflare-findings   # or: sarif
state: [findings.json, coverage-ledger.json]
```

- `task` is a fixed line that starts the skill without questions. It is not a security prompt.
- `format: sarif` needs no converter. Any other format names a converter in pergent; v1 has `cloudflare-findings`.
- `state` lists the files kept for the next scan.

### `pergent scan`

1. Reads the manifest and builds the opencode config for the scan.
2. Runs one opencode session with the skill. The main agent may start sub-agents with `task`.
3. Reads the skill's output, converts it to SARIF 2.1.0 and checks it against the SARIF schema. For Cloudflare it also checks `findings.json` against the skill's `report-schema.json`.
4. Posts the envelope (below) to pergent.sh.

When a step limit, the token budget, the timeout or a cancel stops the run, pergent still sends what the skill wrote so far, with its usage and `status: partial`. pergent catches `SIGTERM`, so a cancel or a Job deadline also ends this way.

### Usage and the token budget

- **Sub-agent tokens are not in opencode's JSON output.** `opencode run --format json` prints only the events of the main session: it skips every event whose `sessionID` is not the main one (opencode v2.3.2, `packages/cli/src/run/noninteractive.ts:172`). So a sub-agent's `step_finish` tokens never reach pergent's parser.
- opencode stores every session, sub-agents included, in its SQLite database in the data directory (`packages/cli/src/database-path.ts`). pergent already gives each run a fresh data directory.
- `pergent scan` reads the usage with `opencode stats --all --models --json` on that data directory. It reports tokens per model (input, output, reasoning, cache read, cache write) over all sessions. pergent uses it:
  - **while the run goes**, every 30 seconds, for the budget;
  - **at the end**, for the `usage` in the envelope.
- **The budget comes from the quotas.** When the scan starts, pergent.sh works out the tokens left under each of the 5 quotas and limits that apply, and passes the smallest one as `PERGENT_TOKEN_BUDGET`. When the total reaches it, pergent stops opencode and sends a partial result, reason `token budget reached`. A scan can go past the budget by at most one 30-second poll.
- Not confirmed yet: that `opencode stats` reads the database while the main opencode process is still writing it. The first real scan checks it. If it fails, pergent counts the main session from the JSON stream during the run and reads the full total from the database only at the end.

### Tools in a scan

| Tool | Review | Scan |
|---|---|---|
| read, glob, grep | yes | yes |
| bash | no | yes |
| write, edit | no | yes, only in the output directory, outside the clone |
| task (sub-agents) | no | yes |

- **Every sub-agent gets the same permission block as the main agent.** opencode's defaults for its built-in agents allow shell and edits, so pergent sets the block on every agent it may start.
- pergent starts opencode with an allow-list of environment variables (`pergent/internal/runner/runner.go`), and keeps `PERGENT_RESULT_TOKEN` out of it. But bash runs as the same user as pergent, so it can read pergent's own environment through `/proc/<pid>/environ`. So bash can reach `OPENCODE_API_KEY`, the result token, any cloud credentials pergent keeps for a provider (`AWS_`, `GOOGLE_`, `VERTEX_`, `AZURE_`, `CLOUDFLARE_`) and the proxy variables. `GIT_TOKEN` stays in the checkout container only. Removing curl is not enough, because bash and node can open a network connection themselves. The real guard is the network:
  - **A scan Job has its own Kubernetes NetworkPolicy, `securityJobEgress.cidrs`, with no default.** Until an admin sets it (the LLM provider only), **Start scan** is off and the tab says why. It never falls back to `jobEgress.cidrs`, whose default is `0.0.0.0/0`.
  - The pergent.sh callback stays reachable on its own port, as for reviews.
- pergent.sh withholds any text that holds a secret it knows. This check covers every string in a scan result: finding messages, `reason`, `state` and the SARIF.

### The envelope

`POST /security-scans/{id}/result`, with the Job's result token:

```json
{
  "kind": "security",
  "status": "complete",
  "reason": "",
  "usage": { "...": "tokens per model, as a review sends" },
  "coverage": { "done": 48, "planned": 48 },
  "state": { "findings.json": "...", "coverage-ledger.json": "..." },
  "sarif": { "version": "2.1.0", "runs": [] }
}
```

- `status`: `complete`, `partial` or `failed`. `reason` is set for the last two.
- A dedicated route, like every other Job kind in `internal/server/server.go`, with its own body limit. The token is checked against the scan row, so a review Job cannot post to a scan.
- `usage` is saved with the same usage code reviews use, so the usage pages and all 5 quotas and limits count the scan.
- `state` is capped in size and stored with the scan. The next scan gets the `state` of the last **complete** scan as `{prior}`, plus the repository's dismissed findings as rejected records with the person's reason, so the skill does not report them again.
- pergent.sh keeps the SARIF it received, for **Download SARIF**.
- **Retention:** scan rows and findings, with their history, are kept for good. The raw SARIF and `state` are kept for the last 10 scans of each repository.

### Findings

One row per repository and fingerprint, in a SARIF shape:

| Field | From SARIF |
|---|---|
| fingerprint | `partialFingerprints` / `fingerprints`. The skill's own fingerprint when it has one |
| rule (CWE), tags (OWASP) | `ruleId`, rule `properties.tags` |
| severity | `level` and `properties.security-severity`: critical, high, medium, low |
| title, description, exploit scenario, fix hint | `message`, `properties`, `fixes` |
| file, start and end line | `physicalLocation`, `region` |
| needs validation | `properties`. Cloudflare's `needs_validation` |
| first, last and fixed scan | set by pergent.sh |

**States:**

| State | How |
|---|---|
| Open | The first scan that finds it. A fixed finding found again goes back to open |
| Dismissed | A maintainer or admin, with a reason: false positive, won't fix, used in tests, mitigated, acceptable risk, plus an optional comment. A later scan keeps it dismissed |
| Fixed | A **complete** scan does not find it. A partial or failed scan never sets it |

- **A new skill version:** each scan stores the skill's name and pinned commit. The first complete scan after a version change marks nothing fixed and shows `Skill updated: some findings may show as new`, because a new version may build fingerprints in a new way.
- `needs_validation` findings are open, with a **Needs validation** label.
- The skill's rejected records are kept in `state`. They are not shown as findings.
- No Confirmed state in v1.

### Who can do what

| Action | Viewer | Maintainer | Admin |
|---|---|---|---|
| See the Security tab, scans, findings, SARIF | yes | yes | yes |
| Start a scan | no | yes | yes |
| Dismiss or restore a finding | no | yes | yes |
| Change the scan settings | no | yes | yes |

Quotas apply to a scan as to a review. An admin may start a scan past a quota, as for a review. Start, dismiss and restore go into the audit log: `security_scan_started`, `security_finding_dismissed`, `security_finding_restored`.

### Settings per repository

A **Security scan** card on the Settings tab:

| Setting | Default | Range |
|---|---|---|
| Scan model | The repository's model chain | Any chain |
| Max steps, main agent | From the manifest (600) | 5 to 1000 |
| Max steps, each sub-agent | From the manifest (100) | 5 to 300 |
| Scan timeout | 3 h | 10 min to 6 h |
| Use security findings in reviews | Off | On or off |

There is no token budget setting. The budget comes from the quotas (see Usage and the token budget).

- **Model chain:** the scan uses the first usable entry of its chain when it starts. A model that fails during the scan does not start it again on the next entry. The scan ends partial or failed, and a person starts it again.
- **Paused and removed repositories:** a paused repository cannot start a scan. A removed repository's scans and findings follow the same rule as its reviews.

Instance level: `security.max_running` (default 1), so scans cannot take the Job slots reviews need. One scan per repository at a time. Helm: `jobs.security_image`, default `ghcr.io/gopaytech/pergent-security` at the chart's app version.

### The Security tab

Placed right after **Reviews**.

- **Last scan:** status, duration, tokens, branch and commit, the built-in skill's name, and counts by severity plus Needs validation. **Start scan** (maintainers and admins) opens a confirm box with the model, the step limits, the timeout, and the last scan's tokens and time as an estimate. While a scan runs, the button shows Running, and maintainers and admins get **Cancel**, which ends the scan partial, reason `cancelled`.
- **Findings:** filters by state (Open first), severity and CWE, a search by file or title, and the columns severity, finding, file and line, first seen.
- **Scans:** number, status, date, duration, new and fixed counts, and the reason for a partial or failed scan.
- **A finding's page:** description, exploit scenario, fix hint, the code location with a link to the platform, its history, and Dismiss or Restore.
- **A scan's page:** status and reason, coverage, usage, the log, new and fixed findings, and Download SARIF.

### Findings in reviews

With **Use security findings in reviews** on, the review Job gets `PERGENT_SECURITY_FINDINGS=/in/security-findings.md`: the repository's open findings in the files the merge request changes, at most about 32 KiB, as data. Like `PERGENT_CI_RESULTS`:

- an environment variable, so an older image ignores it;
- attached to the batch runs, never to a verify run;
- dropped first when the inputs are too large;
- never blocking, and a review never changes a finding's state.

### Metrics and Grafana

Scans by status, open findings by severity, and scan tokens in the usage metrics. New panels in `docs/grafana/pergent-sh.json`.

### Docs

- New `docs/concepts/security-scan.md`.
- `docs/reference/limits.md`, `docs/concepts/dashboard.md`, and the Helm values for `jobs.security_image`, `securityJobEgress` and `security.max_running`.
- `docs/upgrading.md`: the new image, the egress setting that must be set before the first scan, and the migration.

## Testing

Each task runs its own tests. The full run is on CI only.

- **pergent:** the manifest loader; the opencode config for a scan, with the permission block on every agent; the `cloudflare-findings` to SARIF converter on fixture files; the SARIF schema check; a partial run that still posts its findings.
- **pergent.sh store:** fingerprint matching across scans; open, dismissed and fixed moves; a partial or failed scan closes nothing; `state` comes from the last complete scan.
- **API:** the result route refuses a review's token and a body over its limit; secrets in any string are withheld; role checks for start, dismiss, restore and settings; `openapi.yaml` and `openapi_test.go`.
- **Engine:** Start scan is off with no `securityJobEgress.cidrs`; one scan per repository; `security.max_running`; quotas and the admin override.
- **Web (Vitest):** the tab order, the counts, the filters, Start scan only for maintainers and admins, the partial and failed rows.
- **pergent usage:** a fixture opencode database with a main session and a sub-agent session gives the summed tokens per model; the budget stop sends a partial result; `SIGTERM` sends a partial result.
- **A first real scan** on a small public repository, only with the owner's OK and a dollar limit, to measure tokens and time, how opencode counts sub-agent steps, and whether `opencode stats` works while the run writes the database.

## Risks

| Risk | Handling |
|---|---|
| A prompt injection in the scanned code makes the model send the API key out | No default egress. `securityJobEgress.cidrs` must be set to the LLM provider before a scan can start. No curl. Secrets are withheld from every result string |
| A sub-agent gets opencode's default rights | pergent sets the same permission block on every agent. A test checks it |
| A scan costs far more than expected | Manual start only, a confirm box with the last scan's cost, step limits, a timeout, a token budget from the quotas, Cancel, and `security.max_running` |
| Sub-agent tokens go uncounted | Usage is read from opencode's database with `opencode stats`, not from the JSON stream. A test checks that a run with a sub-agent reports its tokens |
| A partial scan marks real problems as fixed | Only a complete scan sets Fixed |
| The upstream skill changes its output format | The skill is pinned in the image. A new version is a new release, with the converter tests |
| Sub-agent steps are counted in a way the limits do not expect | Measured in the first real scan before the release |

## Later

- `@bot security-review` on a merge request. The options: scan the change only (the skill's scope list set to the changed files), the whole default branch, or the whole repository at the merge request's head.
- Upload SARIF to GitHub code scanning.
- More built-in skills, and a choice of skill per repository.
- A token budget per scan.

## Left to the builder

- The table and column names, and the migration, as long as a scan's usage counts in the existing usage and quota queries.
- How `pergent scan` passes the output directory and the prior `state` into the Job.
- The size caps for the envelope, `state` and the SARIF.
- The exact converter mapping from `findings.json` to SARIF, as long as the fields above are kept.
