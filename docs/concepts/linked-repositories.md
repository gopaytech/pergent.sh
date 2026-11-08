# 🔗 Linked repositories

A change to an API or a shared package can break its callers in another repository, and a review that only has the checkout can't see that. A repository can **link** other repositories, so that a change touching the paths it watches can be checked against the code that uses them. pergent.sh matches the links against a change and gives the Job a clone of each matched target (see [A review with links](#a-review-with-links)). The impact check itself is run by pergent. A Job image that predates it ignores the clones and reports no `impact` skill: pergent.sh then writes one itself, `failed`, naming the repositories the Job was given, so the review is never approved on a check that did not happen (see [The result](#the-result-approval-and-publishing)). Only a review that matched a link is affected; with the setting off, or nothing matched, an older image reviews as before.

**Off by default.** The setting is **Linked repositories**, `linked_repos_enabled` in the config ([Config reference](../reference/configuration.md)). A repository that leaves it unset takes the admin default, which starts off; a repository listed in the config reads no admin default, so unset there means off. While the setting is off, a repository's links are kept and ignored.

**Links.** A link is a target repository (`owner/name`), a list of **watched paths** and a **Can block** switch. A repository has at most 3 links. Watched paths are globs, written as a skill's `paths` are (`render/**`; a directory is `render/**`, not `render/`; a pattern without a `/` matches a file name at any depth). A link with no watched paths matches every change; with paths, it matches a change that touches one. Can block is on unless set to `false`. Links belong to one repository: there is no admin default for them, and they are written by admins only, in the dashboard or in the config.

**Turning it on.** In the dashboard, an admin opens the repository's **Settings**, turns on **Linked repositories** and adds links (described below). Or set `linked_repos_enabled: true` and `linked_repos` on the repository in the config, or turn on the admin default for the setting. Both are needed: a link without the setting does nothing, and the setting without a link does nothing. The review App must be installed on the repository and on each target, in the same installation, with `contents: read` (the clone token is scoped to just those repositories). Use a `jobs.image` that runs the impact check, which means a pergent release that includes it: the default may predate it, and an older image leaves `impact` `failed` on every review that matches a link, until the link is removed.

**In the dashboard.** An admin sees a **Linked repositories** section in a repository's Settings, for a GitHub repository the dashboard manages (a config repository shows its links read-only, and a GitLab repository says links are not available). The on/off switch comes first and starts off. Under it, one row per link: the target, **Can block**, **Remove**, and a box of **watched paths**, one glob per line (empty: every change). The links stay editable while the switch is off, and the section says they are ignored until it is on. Below the rows is a search and a list of the repositories the review App reaches (200 at most, by name), each with its visibility and an **Add** button, or the reason it can't be linked in plain words: it is the repository itself, it is linked already, the App can't read it, it is in another installation, or it is more private than this repository. A fourth link can't be added. The section is shown to admins only, and the links are left out of what a maintainer or viewer is sent. **Admin → Settings** has only the on/off default; there is no default list of links.

The server enforces it on every save, not the page. Only an admin may write `linkedReposEnabled` or `linkedRepos` (a maintainer's save that names either is refused with 403, and a maintainer's **Reset to defaults** leaves both, as it leaves the re-run caps). A save of links is refused with 400 and nothing is stored unless the links pass the rules below, checked against GitHub at that moment; GitHub failing to answer is a 502 and nothing is stored, never a pass. The audit log records `settings_updated` with the key names, never the repository names. The list reads each repository's visibility and installation once (8 at a time, within 12 seconds, kept 5 minutes), so a repository it could not read in time is offered as **not checked yet** and is checked again when you save. The rules are checked again when a review starts and before it is published.

```yaml
repositories:
  - github: gh
    project: gopaytech/pergent
    linked_repos_enabled: true
    linked_repos:
      - target: gopaytech/pergent.sh
        watched: ["render/**", "scope/**"]
        can_block: false
```

**Rules for a link.**

- GitHub only. A GitLab repository in the config with `linked_repos` is refused when the config loads.
- The target can't be the repository itself, can't be linked twice, and needs a plain `owner/name`: no scheme, no extra path, no `.` or `..` part and no `.git` suffix (the clone address adds that). The config refuses a bad glob and more than 3 links, naming the repository and the link.
- A repository may link one that is as open or more open: a public repository links public ones; an internal one, internal or public; a private one, anything. Visibility is read from GitHub, including `internal`, through the review App's installation, and never assumed: a repository the App can't read, or whose visibility GitHub does not say, is an error and is never taken for public.
- The target must be in the same installation of the review App as the repository, because one clone token covers the checkout and the targets and a token belongs to one installation. A target the App can't read, or one in another installation, can't be cloned.

## A review with links

When the setting is on and the repository has links, each review checks them again, whatever was checked when the links were saved:

1. **Match.** The links are matched against the files the review covers. A full review (and a re-run) counts the files of the whole change, a rename's old path as well as its new one, so moving a watched file out of its folder counts. An [incremental review](incremental-reviews.md) counts only the files changed since the last review. If the list of changed files can't be had, every link counts as matched. Nothing matched means nothing below happens: no request about the targets, no clone, no change to the Job.
2. **A later push.** While the last review's `impact` result is `failed`, or has a blocking finding nobody dismissed, every link counts as matched, whatever the push touched. A last result that can't be read counts as open. This keeps a push that touches no watched path from dropping the result and letting the bot approve.
3. **Check.** For each matched link the rules above are run again, with both visibilities read from GitHub. A link that is not valid, that is refused, whose visibility can't be read, or whose target the App can't reach or sits in another installation, is not cloned. It still goes to pergent as an entry with a short fixed `error`, so the check is `failed` and the comment says which repository was not covered. It is never a pass.
4. **Clone.** The checkout container clones the checkout first, then each target's default branch into `/work/linked-1`, `linked-2` and `linked-3` (never a directory named after the repository), at depth 1, over https only (`GIT_ALLOW_PROTOCOL=https`), with hooks off and symbolic links written as plain files. The clone address is `https://<host of the repository>/<owner>/<name>.git` and never holds a credential: the token travels in the environment, as for the checkout, and the clone's `.git/config` holds the plain address. One clone token, minted once, covers the repository and the targets with `contents: read`. Each clone has a 120 second limit and a 2 GiB limit, checked after it ends. A clone that fails, is too slow or is too big is removed and the Job goes on; pergent reads the missing directory as a failed clone.
5. **The Job.** `PERGENT_LINKED_REPOS` names `/in/linked-repos.json`, a list of `{name, path, watched, can_block, error}`, and is set only when there is an entry. The Job's deadline grows by 2 minutes per clone, and a limited work volume (`jobs.work_size`) by 2 GiB per clone. The matched links are recorded on the run.

With the setting off, or with nothing matched, the Job is what it was before this feature: the same containers, environment, inputs and work volume.

A re-run of some skills (`@<bot> review failed`) takes no links unless the check is one of the things it re-runs. See the next section.

## The result, approval and publishing

**The `impact` skill.** The name is reserved only while the check can run: with **Linked repositories** on, links set and a GitHub repository, a repository with a skill of its own named `impact` is refused (the run fails with a message saying the name is reserved; rename the skill or turn the setting off), so its result can never stand in for the check. With the setting off, or no links, a skill named `impact` is an ordinary skill everywhere: accepted by the result check, counted as reviewed files, re-run by `review failed`, followed to a fate, carried by an incremental review. A Job given linked repositories always reports a skill named `impact`: its findings are in the one findings list and its row is in the skills table, like any skill's. `impact` is not one of the repository's skills. A result with an `impact` skill for a run that recorded no links is refused as a skill the Job was not started with, so one cannot appear from nothing. A finding of `impact` has the changed code of this repository as its file and line; the caller in the linked repository is named in the explanation, and the structured `linked` field (`repo`, `file`, `line`) is stored but not drawn. Everything a linked repository or the model wrote is escaped like any other model text.

**A clean run says so.** If the check ran and found nothing, the comment has one line, `Impact: checked <repositories>, nothing found`, written by pergent. A review that did not match a link says nothing about it.

**Approval.**

- `impact` reviews no file of this repository. It does not count toward the files a review covered: a change that only `impact` looked at is "nothing reviewed" and is not approved.
- A `failed` `impact` is a failed skill: the status follows the repository's failure status (failed, or neutral) and the bot does not approve. So is a Job image that returned no `impact` for a run that had links (pergent.sh adds a `failed` one), a clone that failed, a link that broke its rules, or a run that timed out.
- A blocking `impact` finding blocks like any blocking finding, and a maintainer of the repository can dismiss it under the same rule as any other.
- The other skills' results stand when `impact` fails.

**Publishing.** Before anything is posted, and on every publish that holds an `impact` result (a republish after a dismissal too), the visibility rule is read again on fresh visibilities, because a repository can go private while a review runs. If the repository is no longer as closed as every repository the check looked into, or any read fails, the findings, notes and summaries of `impact` are left out, the skill is `failed` with a fixed message, and the rest of the review is posted. The result stored on the run is changed the same way, and the comment pergent wrote (`comment.markdown`, `findings_block`) is replaced by a fixed line, so the dashboard, the next review and the database do not carry what was left out. This message names no repository: it is written when a linked repository may be more private than this one, and its name could be what is private.

**Threads.** `impact` lists no reviewed file, so a thread of an `impact` finding resolves when the check ran and finished and no longer reports it. If the check failed or did not run, its threads are left alone. A quiet review keeps `impact` suggestions in the comment, with no thread, as it does for any skill.

**A later push and re-runs.** An incremental review keeps an earlier `impact` result that is open or failed, if the new run has none (a guard behind the rule in [A review with links](#a-review-with-links)); when the setting is off or the links are gone, an open `impact` makes the push a full review so the stale check can't be kept for good. `@<bot> review` runs everything again. `@<bot> review failed` re-runs `impact` when it failed, and a re-run of other skills keeps the earlier `impact` result. pergent needs at least one skill to run, so a re-run of `impact` alone also re-runs one skill, the first by name, and merges both.

**Skill results.** `impact` shows in the Skill results tab with its own rates. It has no version: the result doesn't carry one, so its version is blank and a change to the prompt doesn't start a new row. It lists no reviewed file, so it counts as covering the whole change: a finding that a finished `impact` check stops reporting gets a "Fixed" or "Went away" fate like any skill's. If the check failed, or did not run on a later push, the finding stays open.

**Past dismissals.** When **Learn from dismissals** is on and a review is given linked repositories, the rejected `impact` findings of this repository (the same list, rules and limits as a skill's) go to pergent under the name `impact`, next to the skills'. A review that matches no link gets none of them.

## Accepted costs

- **Default branch only.** A caller already fixed in its own unmerged pull request still reads as broken. Dismissing the finding covers that.
- **The model can miss a caller.** A clean check means nothing was found, not that nothing breaks.
- **Someone can steer the run.** A pull request's author who can't read a linked private repository could get the model to quote it, and text in a target could try the same, or suppress or plant a finding. The prompt rule against pasted code is not a guarantee, and the comment is the only way out. Linking is an admin's trust decision.
- **A target's push can change this repository's result,** because the check reads its default branch.
- **A comment already posted stays** if a repository goes public later.
- **A pinned dependency relies on the switch.** For a repository that pins a version of the one it links (pergent.sh pins pergent), turn **Can block** off, or its findings can block when nothing is broken yet.
- **Watched paths need upkeep.** A path nobody listed triggers nothing.
- **A linked repository's details reach viewers of this one.** Someone who can read this repository's comments reads file names and descriptions from the target. Between private repositories that is the admin's decision.
- **A failed link holds back approval** until it is fixed or removed.
- **A re-run of `impact` alone costs one skill's review.**
- **The clone time is not learned.** The checkout time pergent.sh records for a repository (it sets the next review's deadline) ignores a run that cloned targets.
