# pergent.sh Skills Library (4a): Design

**Date:** 2026-11-01
**Status:** Agreed in brainstorming, the builder check and one grilling round (2026-11-01). It replaces the earlier draft of this file (import once from `skills_dir`), and the plan `docs/superpowers/plans/2026-11-01-skills-library.md` is out of date until it is written again from this design.
**Roadmap:** the first half of piece 4, "Skills", of `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md` (section 7). The dashboard design left editing skills to this piece. The second half, 4b (improvement signals and proposals), is future work.

---

## Why

pergent.sh decides which skills a repository gets and hands them to the review Job. Today the library is the chart's `skills:` value, mounted as `skills_dir` and read once at startup. Changing a skill means a Helm deploy, and the dashboard can only show the library.

The owner wants every skill kept in one place, pergent.sh, coming from three sources:

1. **Written in the dashboard.**
2. **Found in the repository itself**, in `.pergent/skills/` and in the common agent skill folders.
3. **Installed from another git repository**, such as a team's shared skills repo or a public collection.

Helm values hold no skills. Nobody runs pergent.sh yet, so breaking changes are fine and there is no migration.

## Goals

- Admins write, edit, install and delete library skills in the dashboard. A change applies to the next review, with no restart.
- Admins install skills from a git repository on a connected instance, or from a public URL, by branch or tag, choosing which skills to take.
- Installed skills change only when an admin checks for updates, reads the diff and presses **Update**.
- Admins set a **default skill list**. A repository uses it or picks its own list.
- pergent.sh detects skills in `.claude/skills/`, `.agents/skills/` and `.github/skills/`, and a maintainer or admin turns on the ones to run.
- Every library change is a version. Every review records which version of each library skill it used.
- A fresh install is guided: a setup checklist shows what to do next.

## Non-goals (this piece)

- **4b:** improvement signals and proposals.
- **Updates that apply by themselves**, on a schedule or from a webhook on the source repo.
- **Editing repository skills** in the dashboard. They change through MRs.
- **Per-team or per-instance skill lists.** Only the default list and per-repository lists.
- **Extra files in folder skills** (scripts, references). Only the `SKILL.md` text is used.
- **Skills in Helm values or the config file.** The chart's `skills:`, its ConfigMap and `skills_dir` are deleted.
- A rendered markdown preview, a diff or restore for dashboard versions, and a CLI for the library.

## Anti-goals (a failure even if it works)

- **A false pass.** A review approved or marked success with fewer or other skills than the repository should get. A named skill that is missing fails the run. Nothing falls back to other skills.
- **A push elsewhere changing reviews.** A push to a source repository changes nothing until an admin reads the diff and presses Update.
- **An MR changing its own rules.** Repository skills are read from the target branch, never from the MR's own branch, as today.
- **Loosening without the role.** Someone who is not an admin changes the library or the default list, by any path.
- **A secret leak.** A token reaches a Job beyond the one read-only token it needs, or a secret ends up in a skill, page, log or audit entry.
- **The pergent.sh pod fetching an address an admin typed.** The pod holds every secret, so fetching runs in a Job.
- **Skill text run as code** or shown as HTML.

## Approaches

| Choice | Picked | Not picked, and why |
|---|---|---|
| Where the library lives | **Postgres:** a table of skills and an append-only table of versions | A git repo that pergent.sh owns and commits to. Good history, but a second store to run, back up and secure. |
| How a source repo is fetched | **Split by source.** A repo on a connected instance is read through that platform's API by the pod, with the bot or App it already has. A public URL is fetched by **a short Kubernetes Job**, built like a review Job, with no token. | (a) Everything in a Job: on GitLab the Job needs a deploy token, which only a Maintainer can create, so the bot would need Maintainer on every skills repo it only reads. (b) Everything in the pod: the pod that holds every secret would reach any URL an admin types. (c) Platform APIs only: no generic git URLs. |
| Where skills come from | Dashboard, git install, repository folders | The chart's `skills:` value, imported once (the earlier draft). Dropped: no installs exist to keep. |

## Decisions

### Library

| # | Decision |
|---|---|
| D1 | One global library in Postgres. A library skill is one markdown text with frontmatter, as pergent reads it. |
| D2 | Skills enter the library by being **written** in the dashboard or **installed** from a git repository. Both offer an **Add to default skills** checkbox, off at first, so a new skill reaches no repository until someone chooses it. |
| D3 | **Who:** any admin (password admins and admin-team members) creates, edits, installs, updates, copies and deletes library skills, and sets the default list. Maintainers and viewers read only. |
| D4 | **Name:** set at create or install and fixed after. It matches `^[A-Za-z0-9][A-Za-z0-9._-]*$`. A frontmatter `name`, when present, must equal it. |
| D5 | **Versions:** every save, install and update adds a version, kept forever. Each run records the library version of each skill it used. History is read-only, and there is no action to delete a version. The README documents a SQL step for an operator who must remove one by hand, for example text the secret check missed. |
| D6 | **Limits:** 128 KiB per skill, 200 library skills. Frontmatter must parse; `paths` and `exclude` are string lists and `cross_batch` is a boolean; other fields are ignored, as pergent ignores them. **Budget:** saving the default list or a repository's `skills` list is refused with 400, naming the total, when the library text alone is over 900 KiB, the Job's input limit. The pickers show a running total. |
| D7 | **Secrets:** content that contains any secret pergent.sh holds (tokens, keys, webhook and OAuth secrets, the LLM key) is refused on save and install, naming no value. |
| D8 | **Delete:** refused with 409 while a repository (config or dashboard) or the default list names the skill. The answer lists them; repositories the caller can't see are a count. |
| D9 | **Two admins editing:** a save carries the version it read; a stale one gets 409. |
| D10 | **Audit:** `skill_created`, `skill_updated`, `skill_installed`, `skill_source_updated`, `skill_copied`, `skill_deleted`, and `defaults_updated` naming `skills`. Never content. |
| D11 | **Visibility:** every signed-in user reads the library and its history, including skills installed from a private repository: installing is sharing, and the install page says so. A source repository's path shows only to users who can see that repository; others see "a private repository on <instance>". Who made each version shows to admins only. |
| D12 | **Fresh install:** the library starts empty. |

### Installing from git

| # | Decision |
|---|---|
| D13 | **Sources:** a repository on a connected GitLab or GitHub instance, or a public `https` git URL. |
| D14 | **Install:** the admin gives the source, a branch or tag, and optionally a folder. pergent.sh reads it (D17, D18) and lists the skills found (`*.md` and `<name>/SKILL.md`, the same layout as `.pergent/skills/`) with name, size and any problem. The admin ticks the ones to install. A skill whose name is already in the library can't be ticked (`name already in the library`), whatever that skill's origin. Each installed skill records its source, ref, its own file path and the **commit** it came from. |
| D15 | **Read-only:** an installed skill can't be edited. **Copy** makes a new, editable dashboard skill under a new name, and sets its frontmatter `name` to that name. |
| D16 | **Updates:** only on demand. **Check for updates** reads the source again, at the skill's ref or at a new branch or tag the admin types (to move from `v1.0` to `v1.1`), shows the diff for each installed skill from that source and ref, and **Update** saves new versions and, for a new ref, records it. Nothing changes before Update. A fetched skill is matched to an installed one by its **file path** at the source. A skill gone from that path is shown as "removed at the source" and stays as it is. A skill whose name changed at the source is shown as "renamed at the source", and Update is refused for it; the admin installs it again. |
| D17 | **A repository on a connected instance** (one added under Admin → Integrations or in the config file) is read by the pod through that platform's API, with the review bot (GitLab, Reporter or above is enough) or the review App (GitHub, installed on that repository). The admin enters no new secret. No Job runs. |
| D18 | **A public URL** is fetched by a short Job in the Jobs namespace, under the same NetworkPolicy as review Jobs, with no token. It clones one ref shallowly and sends back the skill files through the result callback with a one-time token. A URL outside `jobEgress.cidrs` can't be reached, and the page says so. Only `https` URLs are accepted. |
| D19 | **The ceiling:** a platform admin installs from repositories of its own instance that it can see, and from public URLs. Password admins install from anything the bot or App can reach. The same rule applies to Check for updates. |

### Assignment and repository skills

| # | Decision |
|---|---|
| D20 | A repository's `skills` setting is **null** (use the default list) or a non-empty list of library names (exactly those). |
| D21 | `.pergent/skills/` skills **always run**, as today. |
| D22 | Skills in `.claude/skills/`, `.agents/skills/` and `.github/skills/` are **detected** and listed in the repository's settings, off at first. Only those a maintainer or admin turns on run. The setting is a list of names, `repoSkills`. A detected skill that is off is ignored, even when it is malformed. |
| D23 | **Same name:** `.pergent/skills/` wins, then `.agents/skills/`, then `.claude/skills/`, then `.github/skills/`, then the library. A detected skill that is not turned on replaces nothing. |
| D24 | Only the markdown text reaches the Job. A folder skill's other files are ignored, and the settings page says so. |
| D25 | **Config repositories** keep `repositories[].skills` (library names; empty means the default list) and gain `repositories[].repo_skills` (detected names to turn on). |

### Edge cases

| Case | Result |
|---|---|
| A named library skill is not in the library | The run **fails** with `skill "x" is not in the skill library`. "Needs attention" names the repository. No fallback. |
| A turned-on detected skill is gone from the target branch | The run fails with `skill "x" is turned on but not found in the repository`. |
| No skills at all | **No review:** no Job, no comment, no status. "Needs attention" says `this repository has no skills`. A required pergent status keeps the MR blocked, which is safe. |
| Skills plus inputs over 900 KiB | Fails as today. |
| A config repository names an unknown library skill | Startup does not stop. Its reviews fail and "Needs attention" says why, so the dashboard stays up to fix it. |
| No dashboard (`dashboard.url` unset) | The library can't be managed, but reviews still read it and the default list from Postgres. A fresh install without the dashboard has an empty library, so its repositories use repository skills only. |

## Design

### Data (migration `0014`)

| Object | What it holds |
|---|---|
| `library_skills` (new) | `name` PK, `version`, `content`, `updated_at`, `updated_by`, and the source when installed: `source_kind` (`instance` or `url`), `source_instance`, `source_repo` or `source_url`, `source_ref`, `source_path`, `source_commit`. NULL source means written in the dashboard. |
| `library_skill_versions` (new) | `name`, `version`, `content`, `sha256`, `source_commit`, `at`, `actor`. PK (`name`, `version`). No foreign key, so versions outlive a delete; a re-created name continues its numbering. |
| `skill_fetches` (new) | One read of a source, by API or Job: source, ref, folder, requester, status, the commit it read, the files found, error, expiry. Results are kept a short time, for the admin to choose from. |
| `repo_defaults` | add `skills text[]`, the default list. |
| `dashboard_repos` | add `repo_skills text[]`, the detected skills turned on. |
| `runs` | add `skill_versions jsonb`: library name to version. `skill_sources` gains the folder for repository skills. |

### How a review picks its skills

1. Library names: the repository's `skills`, or the default list when null.
2. Read repository skills from the target branch: `.pergent/skills/` always, and the turned-on names from the other three folders.
3. Merge by name, using D23's order.
4. A named skill that is missing fails the run. Zero skills means no review.
5. The library read and the default list are one read transaction, so a concurrent edit gives the old set or the new set, never a mix. The run records `skill_versions`.

A `review failed` re-run becomes a full re-run when a kept library skill's version changed since the earlier run.

### API

All routes sit behind `Auth.Require`, with CSRF on writes.

| Route | Who | What |
|---|---|---|
| `GET /api/skills` | signed in | list: name, version, size, source, in default list, updated |
| `GET /api/skills/{name}`, `.../versions/{v}` | signed in | content and versions |
| `POST /api/admin/skills` | admin | write a new skill |
| `PUT /api/admin/skills/{name}` | admin | edit a written skill, with `version`; 409 when stale; 409 for an installed one |
| `POST /api/admin/skills/{name}/copy` | admin | copy to a new name |
| `DELETE /api/admin/skills/{name}` | admin | 204, or 409 with users (D8) |
| `POST /api/admin/skill-fetches` | admin | start a fetch: `{instance, repo}` or `{url}`, `ref`, `path` |
| `GET /api/admin/skill-fetches/{id}` | admin | status and skills found, with diffs against installed ones |
| `POST /api/admin/skill-fetches/{id}/install` | admin | install the chosen names |
| `POST /api/admin/skill-fetches/{id}/update` | admin | update the chosen installed names |
| `PUT /api/admin/defaults` | admin | gains `skills` |
| `PUT /api/repositories/...` | maintainer, admin | `skills` and `repoSkills` |

### Pages

| Page | What it shows |
|---|---|
| **Overview → Setup checklist** (new, admins) | LLM set, an instance connected, a library skill, a repository switched on. Each ticks when done and links to its page. It goes away when all four are done. |
| **Skills** | Name, version, source (written, or repo and ref), "default" badge, last change. Admins get **New skill**, **Install from git**, and Edit, Copy, Check for updates and Delete per skill. Content in `<pre>`, never HTML. |
| **Skill editor** | Name (fixed after create), monospace text, size counter, Save. "Changed since you opened it" on a stale save. Read-only history. |
| **Install from git** | Pick a connected instance and repository, or type a public URL. Branch or tag, optional folder. Then a list of the skills found with checkboxes, sizes and problems, and Install. |
| **Check for updates** | Per source and ref: each installed skill with its diff, or "removed at the source", and Update. |
| **Delete dialog** | On 409, lists the repositories and the default list that use the skill. |
| **Admin → Settings** | **Default skills** picker. |
| **Add repository** | A warning when the new repository would get no skills. |
| **Repository → Settings → Skills** | "Use the default skills (a, b)" or "Choose skills". Detected skills with on/off switches, from the default branch. `.pergent/skills/` shown as always on. |
| **Repository → Skills & instructions** | The final set, each skill's source and library version, and a link to each repository skill's file. |
| **Review page** | Each skill shows `library v3` or `repo (.claude/skills)`. |

### Config and chart

- Delete `skills_dir`, the chart's `skills:` value, its ConfigMap and volume. A config that still has `skills_dir` is refused at startup with a message to use the dashboard.
- Add `repositories[].repo_skills`.
- README: Skills and instructions, the roles table, Dashboard pages, audit actions, config reference, `jobEgress.cidrs` for public sources, the bot or App access a private source needs, and the SQL step for removing a version (D5).
- **Release:** app and chart **0.3.0**, with the breaking changes (no `skills:` or `skills_dir`, null `skills` means the default list) in the release notes.

## Test plan

| Layer | Tests |
|---|---|
| Store | Versions on create, edit, install, update. Numbering continues after delete. Delete 409 for each kind of user. A delete and a save naming the skill can't both succeed. |
| Fetch | The API path reads a ref on a GitLab and a GitHub fake; the Job path reads a public URL and gets no token. A bad ref, a too-large skill, a secret in a file and an unreachable URL each give a clear error. A new tag on Check for updates shows the diff against it. A platform admin can't fetch a repository it can't see. A user who can't see a private source sees no path. |
| Engine | Null setting gives the default list. Detected skills run only when on. D23 order. Missing name fails. Zero skills means no Job and no status. `skill_versions` recorded. The full re-run rule. |
| Dashboard API | 403 for viewers and maintainers on library writes. CSRF required. 409s hide repositories the caller can't see. Installed skills refuse edits. Audit carries no content. |
| End to end | Install a skill from a git fixture, review uses it, update to a new commit, next review shows the new version. |
| Web | Setup checklist, install flow, update diff, editor stale save, delete 409 list, default picker, repo skill switches. |
| Chart | `helm lint`; no skills ConfigMap rendered. |

## Constraints

- Everything in the dashboard's, organization's and commands' constraints holds: a public repository, so no internal names; one replica; the engine loop never waits on a handler; tokens never reach a Job, log, page or browser beyond what each Job needs.
- Review Jobs get skills as today: per-Job ConfigMap items under `/in/skills/<name>.md`. The pergent CLI needs no change.
- No second way to run or publish a review.

## Left to the builder

- Index details, and how the dispatch read transaction and the delete-versus-save locking are built.
- The fetch Job's image and command, how long fetch results are kept, and the fetch size and time limits.
- Which platform API calls read a source folder (tree and file reads) and how a ref is resolved to a commit.
- Grouping of installed skills by source and ref in the update page.
- Page layout, within the existing look.

## Future work (4b)

Improvement signals ("too noisy" from dismissals, "a gap" from human review comments) and proposals a human accepts. For a written library skill, a proposal is a new version. For an installed one, it is a change at its source. For a repository skill, it is an MR.
