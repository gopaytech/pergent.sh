# pergent.sh Repository Readiness Scan: Design

**Date:** 2026-11-02
**Status:** Agreed in brainstorming, builder check done. A plan comes next.
**Roadmap:** piece 3 of `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md` (section 4, "Repository readiness scan"). This first version is the scan and its advice. Drafting missing files as MRs, which the roadmap also lists, is left for later.

---

## Why

pergent reviews a repository better when the repository gives it something to work with: a file that says what the project is, owners and approval rules that keep a human in the loop, skills that match the code, and no generated or vendored files filling the input. Today nobody can see any of this in one place. An admin finds out a repository was a poor fit only after its reviews come back thin, are skipped ("no skills"), fail ("larger than 900 KiB"), or after the bot's approval alone was enough to merge.

The readiness scan reads each repository once when it is switched on, and again when someone asks, and shows what is missing with the exact fix.

## Goals

- A **Readiness** tab on each repository page: a checklist of 4 checks, each passed, warning, missing or couldn't check, with why it matters and what to do.
- Scan when a repository is switched on, and when a maintainer or admin presses **Scan now**.
- Turn the worst gaps into "Needs attention" items, so they show on the overview without opening each repository.
- Suggest skills that fit the repository's files, and let a maintainer or admin apply them in one click.
- Work for repositories switched on in the dashboard and for repositories in the config file (read-only for those).
- The same scan for GitLab and GitHub.

## Non-goals (this piece)

- Drafting missing files (`AGENTS.md`, CODEOWNERS) or opening MRs. The scan only advises.
- A conventions check (linter and formatter configs, `.editorconfig`).
- Scanning on a schedule, or after every push to the default branch. The tab says how old the scan is instead.
- Scanning branches other than the default branch.
- Using the LLM. The scan is rule-based.
- Requiring new bot or App permissions. A check the bot can't read says so. (On GitHub, "Administration: Read" is documented as optional; it lets the classic branch protection rows read.)
- Changing any repository setting, file or branch.

## Anti-goals (a failure even if it works)

- A scan delays or blocks a review, or takes a Job slot a queued review was waiting for.
- A scan writes to the repository or the platform, or holds a token that can write.
- A check that couldn't be read is shown as passed. "Couldn't check" is never green.
- The tab shows file paths, file names or settings of a repository to someone who can't see that repository.
- The suggested skills differ from what a review of that repository would actually pick.
- A repository too large to clone breaks the scan for the settings checks too.

## Approaches

| | How | Gain | Cost |
|---|---|---|---|
| A | Everything in the pod, through the platform API | Seconds per scan, no Job | Many paged tree calls per repository; GitLab's tree API has no file sizes, so "very large files" is weak or costs one call per file; switching on many repositories at once spends the shared bot's rate limit inside the one pod |
| **B (picked)** | File checks in a short Job that clones the default branch; settings checks in the pod through the API | Exact sizes, `.gitattributes` and generated-file headers for any repository on both platforms; the pod stays light; reuses the fetch Job pattern (a Job, a result callback, a one-time token) | Needs the cluster; a scan takes tens of seconds; one more Job type and callback; a small pergent commit to export the matcher and classifier (no release) |
| C | The scan lives in the pergent CLI; pergent.sh runs it in the Job | CI-only users get `pergent readiness` too | Spans 2 repositories, needs a pergent release first, bigger first version |

B was picked because 3 of the 4 checks need the repository's files, and the size check is only right with real sizes. C can come later by moving the Job's scanner into pergent.

## Decisions (agreed)

- **First version:** scan and advice only.
- **Checks:** project context, ownership and process, skill fit, size and noise.
- **When:** when a repository is switched on, and on **Scan now**. No background refresh; the tab shows "scanned N days ago, by X".
- **What it reads:** the default branch.
- **Display:** a checklist on a Readiness tab, plus "Needs attention" items.
- **Who sees it:** everyone who can see the repository.
- **Who can press Scan now:** maintainers and admins of that repository.
- **Skill fit:** suggests skills, with a one-click **Use these skills** for maintainers and admins, saved through the normal repository settings save.
- **Approval:** the existing hourly "one approval is enough" check stays as it is; its result also shows in the tab.
- **Size and noise:** lists the files, with the exact fix to copy.
- **Needs attention items:** no skills cover the repository; the approval risk; skills plus instructions over 900 KiB; no project context at all.
- **Config-file repositories:** scanned too, read-only. Instead of the apply button the tab shows the lines to paste into the config file.
- **Build:** approach B.
- **Agreed in grilling (2026-11-02):**
  - **After the upgrade,** every switched-on repository that was never scanned is scanned once, 2 at a time, oldest first. After that, scans run only on switch-on and Scan now.
  - **Saving skills** (a repository's list or the default list) re-judges skill fit and the 900 KiB total from the last scan, with no new Job, so **Use these skills** clears "no skills cover" at once.
  - **Warning, not missing:** one approval or none, no CODEOWNERS, required checks off, and GitLab branches where Developers can push. **Missing:** no README and no agent context file, no branch protection, no skill covering the repository.
  - **GitHub classic branch protection:** the README offers "Administration: Read" on the review App as optional; without it, the rows it would fill say "couldn't check" and name the permission.
  - **Lost scans** are not retried. Scan now is the retry, and its 5-minute limit counts from any scan.
  - **Fixed limits** in this version: 2 GiB disk, 5 minutes and 512 MiB memory per scan Job, 2 Jobs at once, noise above 512 KiB. Not chart settings yet.
  - **Dashboard required:** scans run only with the dashboard on, like skill fetches.

## Design

### The checks

Each item has a state: **passed**, **warning**, **missing**, or **couldn't check** (with the reason, e.g. "the bot can't read branch protection here"). Each item that isn't passed says why it matters to the review and what to do.

| Check | Item | Where it's read | Passed when |
|---|---|---|---|
| Project context | Agent context file: `AGENTS.md` or `CLAUDE.md` at the root | Job | present and not empty |
| | README at the root | Job | present |
| | Contributing guide: `CONTRIBUTING.md` at the root or in `.github/` / `docs/` | Job | present (warning when missing, never "missing") |
| | `.pergent/instructions.md` | Job | present (warning when missing) |
| Ownership and process | CODEOWNERS (root, `.github/`, `docs/`, `.gitlab/`) | Job | present and has at least one rule |
| | Approvals needed to merge | Pod, read at scan time with the hourly check's own functions | 2 or more; "couldn't check" when the read fails or the approve side can't reach the repository |
| | Default branch protected | Pod | protected against direct pushes |
| | Required checks before merge | Pod | GitHub: at least one required status check; GitLab: "pipelines must succeed" on |
| Skill fit | Skills that match files | Job | at least one assigned skill (library or repository) matches at least one file |
| | Assigned skills that match nothing | Job | none (warning, listing them) |
| | Languages no skill covers | Job | none above 10% of source files (warning, listing them) |
| | Suggested skills | Job | library skills not assigned whose `paths` match files; shown as a suggestion, never a failure |
| Size and noise | Generated, vendored and very large files | Job | none outside what `.gitattributes` or a skill `exclude` already drops |
| | Skills plus instructions size | Pod | 900 KiB or less, the Job's input limit |

**Skill fit is rule-based:** a skill matches the repository when its `paths` globs, minus its `exclude` globs, match at least one file, using the same matcher a review uses to pick skills. A skill with no `paths` matches every repository and counts as covering it. Languages come from file extensions counted in the Job (my call; the playback proposed the platform languages API, but the Job already has the files, and counting there skips the generated and vendored files the platform counts).

**The approvals item (my call, from the builder check):** the hourly check stores only its Needs attention item, so "no item" can mean 2 or more approvals, a failed read, or an unreachable approve side. The scan's pod part therefore reads the count itself, with the same functions the hourly check uses (`gitLabRequiredApprovals`, `gitHubRequiredApprovals`), and stores the count or the error in the scan. The hourly check is unchanged. When the hourly `single_approval` item is set now, the tab shows it as a warning on this row whatever the scan read, since it is newer.

**Noise is what a review would actually read (my call, from the builder check):** a review already skips lockfiles, `vendor/`, `node_modules/`, `third_party/`, `linguist-generated` paths and files with a "Code generated ... DO NOT EDIT." header (pergent's diff classifier), unless a skill's `paths` bring them back. Listing those would be advice that changes nothing. So the Job runs the same classifier and lists only files a review would include that look like noise: a generated or minified name (`*.pb.go`, `*_generated.*`, `*.min.js`, `dist/`, `build/`) or larger than 512 KiB. The fix shown is a `.gitattributes` line with `linguist-generated` (the attribute the review honours; `linguist-vendored` is ignored by the review, so it is never suggested), or an `exclude` glob for the skills that match the file. The tab lists at most 50 paths, largest first, with a count of the rest.

**One matcher and one classifier (my call, from the builder check):** pergent's glob matcher, skill selection and diff classifier live in pergent's `internal/` packages. They move to exported packages in the pergent repository, and pergent.sh imports them pinned to a commit, the same way the commands piece took pergent's comment formatter. No pergent release is needed. This keeps "suggested skills" and "noise" identical to what a review does.

### Needs attention

| Kind | Raised when | Cleared when |
|---|---|---|
| `single_approval` | exists today, hourly | as today |
| `no_skill_coverage` (new) | the latest scan finds no assigned skill matching any file | a scan finds a match, or the skills list changes and a scan finds one |
| `inputs_over_budget` (new) | skills plus instructions are over 900 KiB (`inputs.ConfigMapLimit`) | the size is back under; checked at scan time and when the repository's skills or the defaults change |
| `no_project_context` (new) | no `AGENTS.md`, `CLAUDE.md` or README | a scan finds one |

**The 900 KiB total (my call):** the Job reports the sizes of the repository-side inputs (its own skill folders and `.pergent/instructions.md`). The pod adds the assigned library skills' sizes. When the repository's skills or the default list change, the pod recomputes from the last scan's repository-side sizes, with no platform reads.

The existing `no_skills` item (a run skipped because no skills resolved) stays as it is. `no_skill_coverage` is the earlier warning: skills are assigned but none fits the code. Switching a repository off drops its items, as today.

### How a scan runs

1. **Trigger:** switching a repository on (dashboard toggle, or a config-file repository reconcile sees for the first time), **Scan now**, or, once after the upgrade, every switched-on repository that was never scanned (2 at a time, oldest first, not audited). A scan is queued as a row; the request returns at once.
2. **One at a time:** at most one queued or running scan per repository. Scan now while one is queued or running returns the existing one. Scan now is accepted at most once per repository per 5 minutes (my call), and refused with the time to wait.
3. **The pod part** resolves the default branch to a commit, then reads branch protection and required checks with the review bot or App. These results are stored at once, so the tab shows them even if the Job fails.
4. **The Job part** has its own small cap, at most 2 scan Jobs at a time, and does not count against `jobs.max_running`, as fetch Jobs don't today (my call, from the builder check). A review therefore never waits for a scan, and a scan never waits on queued runs that can't start (no LLM set, instance not connected, in backoff), since a scan needs no LLM. The Job:
   - clones that one commit shallowly, with the same clone token and NetworkPolicy a review Job gets, and no LLM key;
   - receives the assigned and library skills' names, `paths` and `exclude` globs (never skill text), and the repository's own skill folders are read from the clone;
   - runs `pergent-sh scan-repo` and posts a compact result (counts, matches, at most 50 noise paths, the presence of context files) to `POST /readiness-scans/{id}/result` with a one-time token. File contents never leave the Job.
5. **Result:** the pod stores it, computes the Needs attention items, and the tab updates. A Job that fails, times out or finds the repository too large to clone stores "couldn't scan files: <reason>" for the file checks; the settings checks still show.

### Data (migration `0015`)

- `readiness_scans`: id, instance, project, branch, commit, state (`queued`, `running`, `done`, `failed`), started_by (actor, or `switch-on`), queued_at, finished_at, error, the pod's results, the Job's result (both JSON), and the one-time token's hash. Only the latest finished scan and any queued or running scan per repository are kept; older rows are deleted when a new scan finishes (my call).
- No new columns on `dashboard_repos` or `repo_defaults`.

### API

| Method and path | Who | What |
|---|---|---|
| `GET /api/repositories/{instance}/{project...}/readiness` | anyone who can see the repository | the latest scan, any queued or running scan, and the checklist |
| `POST /api/repositories/{instance}/{project...}/readiness/scan` | maintainers and admins of the repository | queue a scan; 409 with the existing scan while one is queued or running; 429 with the wait inside the 5 minutes |
| `POST /readiness-scans/{id}/result` | the Job, with its one-time token | the Job's result; the token is spent on first use |

The project is the rest of the path, as in the existing repository routes, so GitLab group paths stay whole. The README's list of paths to expose gains `/readiness-scans/` next to `/runs/` and `/skill-fetches/`.

**Use these skills** calls the existing repository settings save with the current list plus the ticked suggestions, so the same budget check, audit entry and permission rules apply. For a repository that follows the default list (`skills` null), the saved list is today's defaults plus the ticked suggestions, and the confirm says so: "This repository will stop following the default skill list." (agreed after the builder check). For a config-file repository the tab shows the `skills` lines to paste instead.

**Audit:** `readiness_scan_requested` (actor, repository). A scan started by switch-on is not audited separately; the switch-on already is.

### Page

- A **Readiness** tab on the repository page, after "Skills & instructions". The existing "Setup checks" tab stays and shows the new Needs attention items with the others.
- Top line: "Scanned <N days> ago, by <who>, at commit <short sha>", and **Scan now** for maintainers and admins. While a scan runs: "Scanning…".
- One section per check, each item with its state, why, and the fix to copy.
- Never scanned yet (only until its first scan runs): "Not scanned yet" and Scan now.

## Constraints

- Everything in the dashboard, organization, commands and skills library designs still holds: public repository, so no internal names; one replica; the engine loop never waits on a handler; tokens never reach a Job, log, page or the browser beyond what each Job needs.
- The scan Job gets only the read-only clone token and its one-time result token. No LLM key, no write token.
- Visibility follows the repository: the same `canSee` rule as the review list.
- No required new bot or App permissions. README lists what each settings check needs, the optional GitHub "Administration: Read", and what the tab says when a permission is missing.

## Left to the builder

- Exactly which platform calls read branch protection and required checks with today's permissions on each platform, and the "couldn't check" wording when a call is refused.
- The Job's image, command, resource limits, time limit, disk limit and how it reads file sizes cheaply in a shallow clone.
- The JSON shape of the stored results, and the result body's size limit.
- How config-file repositories are detected as "switched on for the first time".
- The extension-to-language table.
- Retrying a scan Job that was evicted or lost.
