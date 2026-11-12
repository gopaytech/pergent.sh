# 🗑️ Deleted and renamed files

By default a review skips a deleted file: it has no lines left to read. Two risks go unseen. The deletion itself can be the risk (a removed auth check, a validation, a test, a migration). And something that stays may still use the file, through an import, a call, a config key, a doc link or a script. A renamed file has the second risk too: its old path can still be used.

A repository can turn on a setting that looks at both. It is **off by default**.

## Turning it on

The setting is **Review deleted and renamed files**, `check_deleted` in the config ([Config reference](../reference/configuration.md)). The help text under it reads:

> Reviews the lines a change deletes, and checks what still uses deleted files and renamed files' old paths. Costs more tokens. Needs a pergent image newer than v0.9.0; an older one ignores it.

- **Per repository only.** There is no admin default and no instance default. A repository that never sets it is off.
- **Who changes it.** Maintainers and admins, in the repository's **Settings** in the dashboard, or in the config as `repositories[].check_deleted`. A repository listed in the config is read-only in the dashboard, and the config value wins ([Dashboard](dashboard.md#repositories-in-the-config-and-in-the-dashboard)).
- **Reset to defaults** turns it off.
- **It costs more tokens.** Removed lines go into the review like any other changed lines, every batch with a deleted file also searches the repository, and a change with nothing else to review costs one more run per skill. This is why it is off by default. The [Usage](usage.md) page shows the real cost once a repository turns it on.

When it is on, pergent.sh sets the environment variable `PERGENT_CHECK_DELETED=true` on the review Job. When it is off, the variable is not set at all, so a Job is the same as it was before the setting existed. It is an environment variable, never a flag.

## It needs a newer pergent image

The switch is pergent's `--check-deleted`. A pergent image newer than v0.9.0 reads it. An older image ignores the variable and reviews as it always did, so turning the setting on with an old image does no harm and does nothing. pergent.sh's default image is still v0.9.0, so set `jobs.image` to a newer pergent image to use the setting ([Config reference](../reference/configuration.md)).

## What it does

pergent does the work, and its own page says how: [Deleted files](https://github.com/gopaytech/pergent/blob/master/docs/concepts/deleted-files.md). In short, with the setting on:

- A deleted file in a skill's scope is reviewed like a changed file. The run judges whether removing that code is safe.
- Each skill searches the repository for anything that still uses a deleted file, or the old path of a renamed file, and raises a finding on the line that does. That line can be in a file the change did not touch.
- A file or rename counts as reviewed only when a finished run saw it. If that run fails, the skill fails, so a change that deletes a lot can turn red when the model run fails. An unchecked deletion is never a pass.

## What changes in approval

See [How approval works](approval.md) for the full table. With the setting on:

- **A reviewed deletion no longer blocks approval.** It is a reviewed file, so a clean review of a change that deletes files can be approved.
- **A deletion that was still skipped as `deleted` blocks approval, as before.** That covers a binary, empty or no-content deletion, a deleted secret-looking file that the read rules deny (such as `*.env`), and any deletion outside a skill's `paths` or inside its `exclude`. Vendored, lockfile and generated deletions keep their own rules and block as before.
- **Approval still needs something reviewed.** A review where nothing was reviewed is not approved. A change that only renames files now has the renames checked, so it can have something reviewed. A pure rename never blocked approval; a rename-only change used to stop at "nothing reviewed".

## In the comment

- A finding on a reviewed deleted file shows as `path (deleted), line N`. It has no link, because the line is not in the head version, and no inline thread, because there is no line on the new side of the diff to put it on.
- The Files list marks `(deleted)` and `(renamed from old/path)` after a path.

## Incremental reviews

A push to a change that already has a review normally carries the files the last review covered ([Incremental reviews](incremental-reviews.md)). The setting changes one rule there: a suggestion, or a finding that people dismissed, on a file outside the files the skill reviewed no longer stops the skill from carrying. This applies when the skill's last result lists deleted or renamed paths and the finding is not on a deleted path itself. These are the findings that point at a file which still uses a deleted file, so they are often outside the change.

An open blocking finding outside the reviewed files still makes the skill review all its files again. Otherwise it would drop out of the combined result and a blocker would pass. With the setting off, nothing changes.

A file deleted in an earlier push and still deleted is carried, with its `(deleted)` mark. A file restored in a later push is reviewed again as a normal file.

## Skill results

A finding on a deleted file counts as Fixed only if the file is still deleted at the new head ([Skill results](skill-results.md)). A restored file is tracked like any other file.
