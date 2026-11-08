# 🧩 Skills and instructions

A review runs a set of **skills**: each is a markdown text (with optional frontmatter) that tells the model what to look for. pergent.sh builds the set for every review from three sources:

| Source | Where it lives | Who changes it |
| --- | --- | --- |
| **The skill library** | In pergent.sh's database. | Admins, in the dashboard: written there, or installed from a git repository. |
| **Repository skills** | In the reviewed repository: `.pergent/skills/` always runs; `.agents/skills/`, `.claude/skills/` and `.github/skills/` run only when a maintainer or admin turns a skill on. | The repository's own merge requests. |
| **Instructions** | The root `AGENTS.md` (or `CLAUDE.md` when there is no `AGENTS.md`) plus `.pergent/instructions.md`. | The repository's own merge requests. |

Skills are not in the Helm values or the config file. The chart's `skills:` value and the config's `skills_dir` were removed ([Upgrading to 0.3.0](../upgrading.md#upgrading-to-030)).

## How a review picks its skills

1. **Library skills.** The repository's `skills` list names them. A repository that sets none (`null`) uses the **default skill list**, which admins set under **Admin → Settings**. A repository's own list is never empty: it names at least one skill, or it is unset and uses the default list.
2. **Repository skills.** pergent.sh reads `.pergent/skills/` from the target branch, and from `.agents/skills/`, `.claude/skills/` and `.github/skills/` only the skills the repository turned on (its `repoSkills` setting, or `repo_skills` in the config). A skill is a `*.md` file or a `<name>/SKILL.md` folder; of a folder only the `SKILL.md` text is used, and its other files are ignored.
3. **One set.** The skills are merged by name. When a name is in several places, the first of these wins: `.pergent/skills`, `.agents/skills`, `.claude/skills`, `.github/skills`, then the library. A skill in one of the last three folders that is not turned on is ignored and replaces nothing, even when it is malformed.

The library names are checked first, so a name the repository lists that is not in the library fails the run even when the repository has its own skill of that name.

Repository skills and instructions are read from the target branch at the merge request's `diff_refs.start_sha` (on GitHub, the pull request's `base.sha`), not from the merge request's own branch, so a merge request cannot change the rules it is reviewed by. The library is read when the review starts, so an edit made while a run waits is what reviews, and the library and the default list are read together: a review gets the old set or the new set, never a mix. Each run records where every skill came from and the library version of each library skill it used. The run page shows `library v3` or `repo (.claude/skills)` per skill.

| Case | What happens |
| --- | --- |
| A named library skill is not in the library | The run fails: `skill "x" is not in the skill library`. The repository is listed under "Needs attention". Nothing falls back to other skills, so a review is never approved with fewer or other skills than the repository asked for. |
| A repository skill that was turned on is gone from the target branch | The run fails: `skill "x" is turned on but not found in the repository`. |
| A repository skill that runs is malformed (one in `.pergent/skills`, or a turned-on one in another folder: its frontmatter doesn't parse, its name is invalid or taken twice in the folder, or it is over 128 KiB) | The run fails, and the message names the file and the problem, as `<path>: <problem>`. A skill that is turned off is ignored even when malformed. |
| A config repository names a skill the library lacks | Startup does not stop, so the dashboard stays up to fix it. The repository's runs fail with the message above, and "Needs attention" says why, until the skill exists. |
| The skills and instructions are over 900 KiB together | The previous review is dropped first. If the skills and instructions alone are too large, the run fails with a comment and no Job starts. |
| The repository has no skills at all | See [No skills, no review](#no-skills-no-review). |

## The skill library

Everyone with a team can read the library (**Skills** in the dashboard). Any admin writes it; maintainers and viewers only read. Every change applies to the next review, with no restart.

- **Write** a skill with **New skill**. The name is fixed after it is created and matches `[A-Za-z0-9][A-Za-z0-9._-]*`; a frontmatter `name`, when present, must equal it. The text is at most 128 KiB, its frontmatter must parse (`paths` and `exclude` are lists of strings, each pattern valid for pergent: bad glob syntax or one that can never match, such as `api/`, is refused and named; `cross_batch` is true or false; other fields are ignored), and it must not contain a secret pergent.sh holds (a token, a key, a webhook or OAuth secret, the LLM key): the save is refused with `the skill contains a secret pergent.sh holds`, naming no value. The library holds at most 200 skills.
- **Add to default skills** is a box on New skill and on Install, off at first, so a new skill reaches no repository until an admin chooses it.
- **Edit** works on a written skill. A save carries the version you opened: if another admin saved meanwhile, it is refused with 409 `this skill changed since you opened it`. An installed skill can't be edited (409 `an installed skill can't be edited; copy it`): **Copy** makes a new written skill under a new name, with its frontmatter `name` set to it.
- **Versions.** Every save, install and update adds a version, kept for good. The skill's page lists them, read-only. A name that is deleted and created again continues its numbering.
- **Delete** is refused with 409 `the skill is in use: take it off the repositories and the default list that name it first` while the default list or a repository, in the config or the dashboard, names the skill. The answer lists them.
- **Budget.** Saving the default list or a repository's skill list is refused with 400 when the text of the library skills it names is over what a review takes, `the library skills come to N KiB; a review takes at most 900 KiB`. The pickers show a running total.
- The skill's text is shown in a plain-text box only, never as HTML or markdown.

### Removing a version by hand

There is no button to delete a version, because history is meant to stay. If you must remove one (for example text that the secret check missed), do it in the database, and rotate the secret too. A model provider may have seen the text in reviews already.

1. If the text is in the skill's newest version, first save a new version in the dashboard, so the version to remove is not the newest one. The next number is the highest one kept plus one, so removing the newest row would let its number be used again. A written skill: edit it and save. An installed skill can't be edited, so fix the text at its source and press **Update**, or **Copy** it under a new name (with the clean text) and delete the original once nothing names it.
2. Remove the old version, for example version 2 of `lint`:

   ```sql
   DELETE FROM library_skill_versions WHERE name = 'lint' AND version = 2;
   ```

3. A read of a source keeps the text it found for an hour. Drop the finished ones now:

   ```sql
   DELETE FROM skill_fetches WHERE status <> 'running';
   ```

A run that used the removed version still shows its number, and the version's page answers 404. Jobs, with their copy of the text, are removed an hour after they finish.

## Installing from git

**Install from git** (admins) copies skills from a git repository into the library. It reads the source, lists the skills it found (`*.md` files and `<name>/SKILL.md` folders in the branch or tag and folder you give) with their size and any problem, and installs the ones you tick. A skill whose name is already in the library can't be ticked: `name already in the library`. An installed skill records its source, branch or tag, file path and the **commit** it came from, and can't be edited.

There are two kinds of source:

| Source | How it is read | What it needs |
| --- | --- | --- |
| **A repository on a connected instance** (added under **Integrations** or in the config file) | By the pergent.sh pod, through the platform's API, with the review bot or review App it already has. No Job runs and you enter no secret. | **GitLab:** the review bot is a member of the repository with **Reporter** or above. **GitHub:** the review App is installed on that repository (it has Contents: Read-only already). A repository the bot or App can't read, or a branch that doesn't exist, is answered `the review bot or App can't read this repository or ref`, because the platform says the same for both. |
| **A public `https` URL** | By a short skill fetch Job, with no token of any kind (see [What a Job can and cannot see](job-isolation.md)). | The URL's host is inside `jobEgress.cidrs`. The Job posts what it read to `<callback_url>/skill-fetches/<id>/result`, which the chart points at its own Service; see [Exposing pergent.sh](../../README.md#exposing-pergentsh). The chart sets `PERGENT_SH_IMAGE` for the Job. Only public repositories work: a URL with a user name or password is refused. |

- **A read is limited.** At most 200 skills and 4 MiB of text in one read (a smaller folder fixes it), 128 KiB per skill, and the pod runs 2 reads at once. A read lasts at most 2 minutes. For a URL, the **image pull and the Job's fixed limits count against those 2 minutes**: the clone container has 512Mi of memory and the Job 1Gi of scratch space. A slow image pull, a slow clone or a huge repository therefore ends as `reading the source took too long` (a read by the pod ends as `reading the repository took longer than 2 minutes`). Choose a smaller folder, or a tag with less history. The result of a read is kept for an hour for you to choose from.
- **Who.** Any admin installs from anything the review bot or App can reach, and from public URLs; nothing asks what the admin may read on the platform. **Installing is sharing:** everyone with a team can read an installed skill, even one from a private repository. A private repository's path is shown to admins only; others see `a private repository on <instance>`.
- **A push to the source changes nothing.** Installed skills only change when an admin presses **Update**.

**Check for updates** (on **Skills**) reads a source again, at the skill's own branch or tag, or at a new one you type, to move from `v1.0` to `v1.1`. For each skill installed from that source and branch or tag it shows the difference, and **Update** saves the changed ones as new versions (and, for a new branch or tag, records it). Nothing changes before **Update**. A fetched skill is matched to an installed one by its file path at the source. A skill gone from that path shows as `removed at the source` and stays as it is. A skill whose name changed there shows as `renamed at the source`, and Update is refused for it: install it again under its new name. A text that now holds a secret pergent.sh holds, or is over 128 KiB, can't be updated.

## Repository skills

- **`.pergent/skills/` always runs.** A repository skill replaces a library skill of the same name.
- **The other three folders are detected, not run.** A repository's **Settings → Skills** lists the skills found in `.agents/skills`, `.claude/skills` and `.github/skills` (read from the default branch), off at first. A maintainer or admin of the repository turns each on (`repoSkills`; in the config, `repositories[].repo_skills`), which is what lets a repository reuse skills it already keeps for other tools. A turned-on skill that later disappears from the target branch fails the run, as above.
- Repository skills are changed through merge requests, not in the dashboard. The repository's **Skills & instructions** page shows the final set a review of the default branch would get (kept for 5 minutes), each skill's source and library version, and a link to each repository skill's file.

## No skills, no review

When a run ends up with no skill at all (the default list is empty and the repository names none and has none), there is **nothing to review with**, so pergent.sh does not review:

- No Job starts, and no comment and no commit status or Check Run is written. The run's state is `skipped`; the dashboard shows it as **Not reviewed** with the reason `this repository has no skills`, and "Needs attention" lists the repository with the same words. It is not counted as a review or a failure.
- A required `pergent` status therefore keeps the merge request blocked, which is the safe result. **If an earlier review of the same commit passed, its status stays** (pergent.sh sets none to replace it), but the bot's approval of that commit is withdrawn, because a skipped review decided nothing. Nothing approves it again until a review runs.
- Adding a skill to the default list (or to the repository) and re-running the review (a push, or `@<bot> review`) fixes it. **Add repository** warns when the default list is empty.

Without the dashboard (`dashboard.url` unset) the library can't be managed, but reviews still read it and the default list from the database. A fresh install has an empty library, so its repositories review with repository skills only.
