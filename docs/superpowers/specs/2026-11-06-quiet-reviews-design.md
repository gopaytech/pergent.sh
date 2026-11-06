# Quiet reviews

pergent.sh, next release, with a pergent release first. Status: design. Roadmap item 9 (Noise profile).

## Why

Teams differ in how much review they want. What bothers people is nitpicks and style comments, and a comment that is long to read. *(chosen)* Today a repository can only turn inline findings on or off, which doesn't touch either.

The purpose is **a review that says less, without missing anything that blocks**.

## Goals

- A repository can turn on **Quiet reviews**.
- On a quiet review, nitpicks mostly stop, suggestions leave the diff, and the comment is shorter.
- Blocking findings, failures, verdicts and approval are the same either way.

## Non-goals

- A `thorough` level. Nobody has asked for more comments, and it can come later. *(chosen)*
- A cap on suggestions. Findings have no ranking, so a cap would drop useful ones as readily as nitpicks. Add ranking later only if nitpicks remain. *(chosen)*
- A choice per skill or per change. *(chosen)*
- Hiding suggestions completely. They stay in the comment, collapsed. *(chosen)*
- Anything in pergent's CI mode or `--local`.

## Anti-goals

- A switch that changes what is blocking, a verdict or an approval.
- A failure, a failed check or a blocking finding left out of the comment by a quiet review. The size-limit steps can still cut a failed check table on a very large comment, as they do today, with their note; blocking findings and failures are never cut.
- A prompt line that makes the model less careful about real problems.

## The setting

`quiet_reviews` is on or off, like the settings beside it. *(chosen: a switch, not a profile with levels)* A third level would need a migration, and that's a small cost for a guess.

- Per repository in its **Settings**, with a default under **Admin → Settings**, like **Inline findings** and **Incremental reviews**. *(chosen)* On a dashboard repository, unset means the admin default, and with none, off. A config repository doesn't read admin defaults, so unset there means off, as for **Inline findings**. *(my call)*
- Config key `quiet_reviews`, dashboard key `quietReviews`. Migration `0025` adds a column to `dashboard_repos`, `repo_defaults` and `runs`. A config repository is read-only in the dashboard, as today. *(my call)*
- Who can change it, and the audit entry, follow the repository's other settings. *(my call)*
- It is read when a review starts and recorded on the run, with NULL meaning off. The prompt, the thread rule and the comment layout all use the run's value, also on a republish after a dismissal, so a run's comment never changes shape on its own. A setting change takes effect with the next review. *(my call)*

Off changes nothing: the same prompt, threads and comment as a release without this feature, byte for byte. The pergent release this needs changes the comment for its own reasons (see Order of work).

## What a quiet review does

### Fewer nitpicks

One fixed line, added by pergent.sh to the instructions a review gets, under its own heading after the team's text: *(chosen: a prompt line; my call: through the instructions)*

> When you raise a suggestion of your own, raise it only if it would change how the code behaves or clearly helps the next person who reads it, and leave out style, naming, formatting and other nitpicks. This applies to nothing else: it does not change what is blocking, how you report or judge a checklist item, or how you check findings another reviewer raised. *(chosen: reworded so it can't reach those)*

It comes after the team's text and wins for suggestions: a skill or `AGENTS.md` that asks for style comments doesn't get them as suggestions. A style rule that matters belongs under `## Blocking`. *(chosen)*

The instructions already reach every run, so pergent needs no flag and the result schema doesn't change. A repository with no instructions gets a file holding only this. *(my call)* The line counts toward the 900 KiB budget.

This is a request to the model, so it lowers nitpicks and doesn't end them. The verification run gets the line too but can't see a skill's `## Blocking` section, so the line's last sentence is what keeps it from dropping a blocking finding about style. That rests on the model following the wording.

### No suggestion threads

Suggestions aren't posted as threads. *(chosen)* They go back into the comment, collapsed, so one list replaces many threads, and fewer nitpicks keep it short. This reuses what `internal/threads` does for `OnlyBlocking`: pergent.sh treats a quiet review like a branch that requires threads resolved. *(my call)*

- A suggestion stays in the comment. The note it gets today (`NoteKeptRule`) names a branch rule, so a quiet review needs its own wording, or none. *(left to the builder)*
- On a quiet review, a thread whose finding is no longer reported closes with ``No longer reported in `abc1234`.`` instead of ``Fixed in `abc1234`.`` *(chosen)* The prompt line makes findings disappear with nothing fixed, so "Fixed" would be false. Threads carry no severity, so this applies to every thread on a quiet review, blocking ones too.

### A shorter comment

The comment leaves out the `Files` list and passed and n/a checks. *(chosen)* Failed checks, failures, all findings, the Dismissed list and the hidden findings block stay.

- New `render.Options` field, set by pergent.sh, sharing the layout the size-limit steps already use for passed checks. *(my call)*
- No `omitted to fit the comment size limit` note, since nothing was cut for size. The meta line's `N of M checks passed` stays. *(my call)*
- The meta line says `quiet` (`MetaNote`), so nobody takes the shorter comment for a cut or failed review. *(chosen)*
- The size-limit steps still apply after this.
- The dashboard's review page still shows checks and files, and a small `quiet` label on a run that was quiet. *(chosen)*

## With what's already there

- **Inline findings off:** nothing to do for threads; the prompt line and the shorter comment still apply.
- **Incremental reviews:** a carried finding stays as it is until its file changes. A repository that turns it on mid-change keeps earlier suggestions in the comment. *(my call)*
- **Learn from dismissals:** unchanged.
- **Skill results:** a `quiet` repository raises fewer suggestions, so its numbers before and after the switch aren't comparable. The tab doesn't say so. *(my call)*
- **Skill hashes:** the prompt line is not part of a skill's hash, so **Edited N days ago** doesn't fire on a switch. *(my call)*

## Accepted costs

- A skill whose whole job is style goes mostly quiet, unless its `## Blocking` section lists what it cares about.
- A suggestion loses its one-click fix: on a quiet review the suggested change is text in the comment. *(chosen)*
- Moving suggestions out of threads adds a collapsed list to the comment.
- The model may still raise nitpicks, or drop a useful suggestion.
- A reader of the comment no longer sees which files a skill skipped without opening the dashboard, unless skipping prevented approval, which the comment still says.
- A really fixed thread on a quiet review says "No longer reported", not "Fixed".
- A style item on a checklist keeps being reported, so a quiet review of such a skill still shows it.

## Order of work

Build this after the current stack is out: pergent v0.5.4, then pergent.sh 0.8.1 with the `CheckSkill` fix, confirmed on the cluster. *(chosen)* `render` is in pergent, so this feature's comment change then needs another pergent release first, and pergent.sh bumps the pin and the default `jobs.image` in the same release. *(my call: no backport)*

The 0.8.1 bump is its own work, since `master` has moved far past v0.5.3: `render` was rewritten, and pergent now refuses a bad or never-matching `paths` or `exclude` pattern (`skills_invalid`). `inputs.CheckSkill` must refuse the same patterns when a skill is saved, or a skill accepted today makes every review using it fail.

## Left to the builder

- The `render.Options` field's name, and how it joins the existing layouts.
- The note for a suggestion kept out of a thread on a quiet review.
- The heading above the prompt line, and where in `internal/inputs` it is added.
- The dashboard control's wording and layout.
- Keeping the size-limit steps from writing their `omitted to fit` note for what a quiet review already left out.

## Success

First real use is `gopaytech/pergent`: read about five reviews before recommending it for a big repository. *(chosen)*

A maintainer turns on **Quiet reviews**. The next review has no new suggestion threads and a shorter comment, with every blocking finding still there, and the suggestions a reader sees are the ones worth reading. Nothing measures this per review; **Raised** in Skill results falling is only a hint, as it also moves with how many changes merge.
