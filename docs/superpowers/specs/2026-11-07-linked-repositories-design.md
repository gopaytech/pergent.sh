# Linked repositories: an impact check across repositories

Status: agreed with the owner on 2026-11-07, in a grilling round after a builder check. The owner approves this document before the build starts.

Spans two repositories. **pergent** gets the impact run. **pergent.sh** gets the links, the clones and the settings page.

## Why

A change to an API or shared package can break its callers in another repository, and a review today can't see that. It only has the checkout. pergent.sh imports pergent's `render` and `scope`, so a `render` change is the case the owner has in hand. Greptile and CodeRabbit read linked repositories; pergent doesn't.

## Goals

1. A change to something other repositories depend on is checked against who uses it.
2. A change that touches nothing they depend on costs nothing: no clone, no extra run.
3. A real break can stop an approval. A guess can't.

## Anti-goals

- **Private code on a public PR.** A finding quotes code, and the comment is public on a public repository. A public repository never reads a private one.
- **A false pass.** An impact check that didn't finish never reads as clean, and the bot doesn't approve on it.
- **A linked repository steering the review.** Its contents are untrusted, like the checkout. Its instruction files are never loaded.
- **Access nobody granted.** A link lets a review read a repository and quote it on this one's PRs, so only an admin makes one.
- **Slower reviews for everyone.** With the feature off, or with no links, a review is unchanged, byte for byte.

## Non-goals

- Conventions or reference repositories: `AGENTS.md` and skills cover that.
- Contracts a repository must follow, such as a shared proto file. Same mechanism, wider prompt, and it can come later.
- GitLab, for now. A GitLab target needs its own deploy token on a project pergent.sh doesn't review, with its own lifecycle. The first version is GitHub only.
- Links across instances or App installations.
- Reading a matching branch of the linked repository.
- A change to the normal batch runs: they don't see the linked repositories.
- A command to run the check by hand.

## The model

A repository can have **links**. A link is a target repository, a list of **watched paths** as globs, and a **Can block** switch, on by default. At most 3 links per repository, fixed.

```
pergent  →  links  gopaytech/pergent.sh  watching  render/**, scope/**
```

A link with no watched paths matches every change.

With **Can block** off, every impact finding on that link is a suggestion, whatever the model says. It is for a repository that pins a version, such as pergent.sh pinning pergent: nothing there breaks until the pin moves.

### Rules for a link

- GitHub only, in the same App installation, so one token can cover the checkout and every target.
- A repository may link one that is as open or more open: public can link public; internal can link internal or public; private can link anything.
- The target can't be the repository itself.

Visibility is read from GitHub, including `internal`, never assumed: a failed read refuses the link. These rules are checked when a link is saved, when a review starts, and again before publishing, because visibility can change while a review runs. A link that fails at review time doesn't clone, and the impact check is `failed`. If the rule fails at publish, the `impact` findings are left out and the check is `failed`.

### Off by default

The feature is off until someone turns it on. A setting, **Linked repositories**, is on or off per repository, with an admin default that starts off, like Quiet reviews. Config key `linked_repos_enabled`, dashboard key `linkedReposEnabled`.

While it's off, a repository's links are kept but ignored: nothing is cloned, no run happens, the prompt is the same, and the comment says nothing. So both a link and the setting are needed before anything changes.

### Who sets them

Admins only, on a repository's settings page, in a **Linked repositories** section. They can also be set in the config as `repositories[].linked_repos`. As with other settings, the run records what it used.

## A review with links

1. **Dispatch.** With the feature on, pergent.sh reads the repository's links and matches the change's files against each link's watched paths. On an incremental review, only the new commits' files count. No match means nothing below happens.
2. **Clone.** The checkout container clones the checkout first, then each matched target's default branch, depth 1, into its own directory under `/work`. Each target clone has its own time limit and size limit, and a target clone failing doesn't stop the Job: it's recorded, and `impact` is `failed`. These clones use `scan.sh`'s hardening: `GIT_ALLOW_PROTOCOL=https`, `core.symlinks=false`, hooks off. Clone directories have neutral names, such as `linked-1`, so a repository's name can't match a denied read rule.
3. **Scrub.** Before any run starts, pergent removes instruction files, symlinks leaving a clone, and files the read rules deny (`*.env`, `*.pem`, `*.key`, `*.tfvars`) from each clone. Grep and glob don't honour read rules, so denied files have to be gone, not just denied. A scrub failure fails `impact`, not the review.
4. **Review.** pergent runs the normal review, unchanged. Normal runs can't read the clones: only the impact run's generated config allows that `external_directory`.
5. **Impact run.** One more opencode run, with a fixed prompt pergent owns. Its one job: for what this change alters, search the linked repositories for callers it breaks. It gets only that prompt, the diff of changed files matching watched paths, read access to the clones, and its own earlier findings as the previous result, so a dismissal stays attached if the wording changes. It gets no skills, repository instructions, quiet line or change description. On an incremental review, only the new commits' matching files count. If that's too big for one run, the check is `failed`.
6. **Verification.** A blocking finding goes through its own verification prompt, which reads the changed code and the caller in the clone. If verification can't confirm it, or fails, the finding becomes a suggestion.
7. **Result.** The findings go in the result as a skill named `impact`. In the comment it shows like a skill: a row in the skills table and findings in the one findings list.

### A later push

`result.MergeIncremental` only keeps skills the new run reported, so a push touching no watched path would lose an earlier `impact` result, and the bot could approve. So while the last `impact` result has an open blocking finding or is `failed`, every push runs the check again, even if no watched path changed. It stops when the check comes back clean.

### Severity

A `blocking` finding must name the caller in structured fields, a linked repository, file and line, and pergent checks that they exist in the clone. Without them it becomes a suggestion, as it does with **Can block** off. Anything less, such as a likely problem or a caller it can't confirm, is a `suggestion`.

A finding's location is changed code in this repository, because that's what a thread can attach to; pergent enforces that, and moves anything else to the comment. The prompt limits a finding to a file, a line and a short description, with no pasted code from a linked repository.

### On a clean run, and with other features

- If the check ran and found nothing, the comment says so in one line, such as `Impact: checked pergent.sh, nothing found`. If nothing matched and it didn't run, the comment says nothing.
- A quiet review still runs it: its suggestions stay in the comment, with no threads, like any skill. The quiet prompt line doesn't apply to this run, because a broken caller isn't a nitpick.
- `@<bot> review` and the re-run button run it again when paths match. `review failed` re-runs it if it failed.
- It follows the repository's comment layout, and appears in **Skill results** with its own rates.
- A maintainer of this repository can dismiss an impact finding, blocking ones too, under the same rule as any finding.

### When it fails

If a clone or scrub fails, a link fails its rules, the Job's image doesn't know the feature, or the impact run fails or times out, the `impact` skill's verdict is `failed`. pergent.sh writes that result itself when pergent couldn't, so it can't read as clean. The comment says which repositories weren't covered. Other skills' results stand. The bot doesn't approve, as with any failed skill.

### What it costs

One clone per matched link, and one model run per review, plus verification for blocking findings. The impact run shares the review's time budget and `--concurrency` limit. A repository whose links match nothing pays nothing.

## pergent

- `pergent agent` takes the linked repositories through an environment variable, `PERGENT_LINKED_REPOS`, naming a JSON file of `{name, path, watched, can_block, error}` entries. An older image ignores it, so `jobs.image` can lag. A flag would exit 2 on an older image.
- Paths must be outside `--repo-path`, as other trusted inputs are, and the generated config's read rules allow them.
- The prompt is fixed in pergent, not a skill. Skills must not carry output format, and a skill's text must not change what this run does.
- `impact` is a reserved name only when the feature is on with a link: then a skill called `impact` is refused. An existing skill with that name on any other repository is unaffected.
- People's rejected `impact` findings are collected like any skill's and go to the next impact run, when **Learn from dismissals** is on.
- With no linked repositories, nothing changes.
- CI mode has no links.

## pergent.sh

- `linked_repos` per repository, in the config and the dashboard, recorded on the run.
- The Job's clone token is one installation token covering the checkout and the targets, read-only.
- `CloneAllowance` counts every clone, so a slow target can't use up pergent's time, and the Job's scratch volume is sized for them.
- `checkSkills` expects `impact` when the run started it. `impact` doesn't count as reviewed files for approval: a change only `impact` looked at is still "nothing reviewed".
- A re-run treats `impact` as part of the run, not as "skills changed", and `review failed` can re-run it alone.
- The settings page lists repositories the bot or App reaches on that instance, and says why one can't be linked.
- The dashboard's review page shows `impact` like a skill.
- Skill results count `impact` as a skill, with the fixed prompt's hash as its version.

## Accepted costs

- **Default branch only.** A caller already fixed in its own unmerged PR still reads as broken. Dismissing it covers that.
- **The model can miss a caller.** A clean impact check means nothing was found, not that nothing breaks.
- **Someone can steer the run.** A PR's author who can't read a linked private repository could get the model to quote it, and text in a target could try the same, or suppress or plant a finding. The prompt rule against pasted code is not a guarantee. The comment is the only way out, because the model has no web access. Linking is an admin's trust decision.
- **A target's push can change this repository's result,** because the check reads its default branch.
- **A comment already posted stays** if a repository goes public later.
- **A pinned dependency relies on the switch.** An admin has to turn **Can block** off for a repository that pins a version, or its findings can block when nothing is broken yet.
- **Watched paths need upkeep.** A path nobody listed triggers nothing.
- **A linked repository's details reach viewers of this one.** Someone who can read this repository's comments reads file names and descriptions from the target. Between private repositories that's the admin's decision.
- **A failed link holds back approval** until it's fixed or removed.

## Upgrading

Additive. A migration adds nullable columns. The feature is off by default, so nothing changes until someone turns it on. `linked_repos` in the config means removing it before a rollback, because the previous release refuses unknown keys.

## Releases

pergent first, then pergent.sh moves to it. The owner names both versions and gives the word for each tag.

## Left to the builder

- Where clones live in the Job, and how the JSON file is laid out.
- The impact prompt's wording, within the rules above.
- The settings page's layout.
