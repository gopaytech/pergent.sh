# Noise profile

pergent.sh, next release, with a pergent release first. Status: design. Roadmap item 9.

## Why

Teams differ in how much review they want. What bothers people is nitpicks and style comments, and a comment that is long to read. *(chosen)* Today a repository can only turn inline findings on or off, which doesn't touch either.

The purpose is **a review that says less, without missing anything that blocks**.

## Goals

- A repository can choose `quiet`.
- On `quiet`, nitpicks mostly stop, suggestions leave the diff, and the comment is shorter.
- Blocking findings, failures, verdicts and approval are the same on both profiles.

## Non-goals

- A `thorough` profile. Nobody has asked for more comments, and it can come later. *(chosen)*
- A choice per skill or per change. *(chosen)*
- Hiding suggestions completely. They stay in the comment, collapsed. *(chosen)*
- Anything in pergent's CI mode or `--local`.

## Anti-goals

- A profile that changes what is blocking, a verdict or an approval.
- A failure, a failed check or a blocking finding left out of the comment.
- A prompt line that makes the model less careful about real problems.

## The setting

`noise_profile` is `default` or `quiet`.

- Per repository in its **Settings**, with a default under **Admin → Settings**, like **Inline findings** and **Incremental reviews**. *(chosen)* On a dashboard repository, unset means the admin default, and with none, `default`. A config repository doesn't read admin defaults, so unset there means `default`, as for **Inline findings**. *(my call)*
- Config key `noise_profile`, dashboard key `noiseProfile`. Migration `0025` adds a column to `dashboard_repos`, `repo_defaults` and `runs`. Another value is refused. A config repository is read-only in the dashboard, as today. *(my call)*
- Who can change it, and the audit entry, follow the repository's other settings. *(my call)*
- It is read when a review starts and recorded on the run, with NULL meaning `default`. The prompt, the thread rule and the comment layout all use the run's profile, also on a republish after a dismissal, so a run's comment never changes shape on its own. A setting change takes effect with the next review. *(my call)*

`default` changes nothing: the same prompt, threads and comment as a release without this feature, byte for byte. The pergent release this needs changes the comment for its own reasons (see Order of work).

## What `quiet` does

### Fewer nitpicks

One fixed line, added by pergent.sh to the instructions a review gets, under its own heading after the team's text: *(chosen: a prompt line; my call: through the instructions)*

> When you raise a suggestion of your own, raise it only if it would change how the code behaves or clearly helps the next person who reads it, and leave out style, naming, formatting and other nitpicks. This applies to nothing else: it does not change what is blocking, how you report or judge a checklist item, or how you check findings another reviewer raised. *(chosen: reworded so it can't reach those)*

The instructions already reach every run, so pergent needs no flag and the result schema doesn't change. A repository with no instructions gets a file holding only this. *(my call)* The line counts toward the 900 KiB budget.

This is a request to the model, so it lowers nitpicks and doesn't end them. The verification run gets the line too but can't see a skill's `## Blocking` section, so the line's last sentence is what keeps it from dropping a blocking finding about style. That rests on the model following the wording.

### No suggestion threads

Suggestions aren't posted as threads. *(chosen)* This reuses what `internal/threads` does for `OnlyBlocking`: pergent.sh treats `quiet` like a branch that requires threads resolved. *(my call)*

- A suggestion stays in the comment. The note it gets today (`NoteKeptRule`) names a branch rule, so `quiet` needs its own wording, or none. *(left to the builder)*
- On `quiet`, a thread whose finding is no longer reported closes with ``No longer reported in `abc1234`.`` instead of ``Fixed in `abc1234`.`` *(chosen)* The prompt line makes findings disappear with nothing fixed, so "Fixed" would be false. Threads carry no severity, so this applies to every thread on `quiet`, blocking ones too.

### A shorter comment

The comment leaves out the `Files` list and passed and n/a checks. *(chosen)* Failed checks, failures, all findings, the Dismissed list and the hidden findings block stay.

- New `render.Options` field, set by pergent.sh, sharing the layout the size-limit steps already use for passed checks. *(my call)*
- No `omitted to fit the comment size limit` note, since nothing was cut for size. The meta line's `N of M checks passed` stays. *(my call)*
- The size-limit steps still apply after this.
- The dashboard's review page still shows checks and files.

## With what's already there

- **Inline findings off:** nothing to do for threads; the prompt line and the shorter comment still apply.
- **Incremental reviews:** a carried finding stays as it is until its file changes. A repository that moves to `quiet` mid-change keeps earlier suggestions in the comment. *(my call)*
- **Learn from dismissals:** unchanged.
- **Skill results:** a `quiet` repository raises fewer suggestions, so its numbers before and after the switch aren't comparable. The tab doesn't say so. *(my call)*
- **Skill hashes:** the profile line is not part of a skill's hash, so **Edited N days ago** doesn't fire on a switch. *(my call)*

## Accepted costs

- A skill whose whole job is style goes mostly quiet on `quiet`, unless its `## Blocking` section lists what it cares about.
- The model may still raise nitpicks, or drop a useful suggestion.
- A reader of the comment no longer sees which files a skill skipped without opening the dashboard, unless skipping prevented approval, which the comment still says.
- A really fixed thread on `quiet` says "No longer reported", not "Fixed".
- A style item on a checklist keeps being reported, so a `quiet` review of such a skill still shows it.

## Order of work

`render` is in pergent, so the comment change ships in a pergent release from `master` first. pergent.sh then bumps the pin and the default `jobs.image` in the same release. *(my call: no backport)*

That bump is its own work whatever this feature does, since `master` has moved far past v0.5.3: `render` was rewritten, and pergent now refuses a bad or never-matching `paths` or `exclude` pattern (`skills_invalid`). `inputs.CheckSkill` must refuse the same patterns when a skill is saved, or a skill accepted today makes every review using it fail.

## Left to the builder

- The `render.Options` field's name, and how it joins the existing layouts.
- The note for a suggestion kept out of a thread on `quiet`.
- The heading above the prompt line, and where in `internal/inputs` it is added.
- The dashboard control's wording and layout.
- Keeping the size-limit steps from writing their `omitted to fit` note for what `quiet` already left out.

## Success

A maintainer sets `quiet`. The next review has no new suggestion threads and a shorter comment, with every blocking finding still there, and the suggestions a reader sees are the ones worth reading. Nothing measures this per review; **Raised** in Skill results falling is only a hint, as it also moves with how many changes merge.
