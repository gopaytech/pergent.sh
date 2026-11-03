# Inline findings, one-click suggestions and a confidence level

Status: agreed with the owner on 2026-11-03 (brainstorm, then a grilling round that settled every open question). pergent v0.2.0 shipped the confidence and suggestion fields. The comment layout (§4) was revised the same day after the first live reviews: readers could not tell the finding code was an ID, and passed checks took more room than the findings. Spans two repositories: **pergent** (the model's answer, the renderer, CI mode) and **pergent.sh** (posting inline threads and keeping them in step with later reviews).

## Why

Today every finding lands in one long comment. A reader has to match "`internal/retry/retry.go:43`" to the code by hand, the author can't answer a finding where it is, and nothing on the code view changes when a finding is fixed. Hosted review bots (Greptile, for example) put each finding on its lines, let people reply there, resolve the thread once a later push fixes it, offer a one-click fix, and open with a short "how safe is this to merge" score. The owner wants pergent to work the same way. This comes before the skill-improvement signals (piece 4b, parked): the owner wants the review experience right before tuning skills from dismissals.

## Goals

1. **Confidence level.** Each skill's answer carries a 1–5 score for "how safe is this to merge, from what I reviewed" and one sentence on why. The review shows the lowest score across skills, capped by facts (below).
2. **Inline findings (pergent.sh).** Each finding becomes an inline comment on its lines in the merge request, blocking and suggestion alike, at most **20** inline threads per merge request.
3. **One-click suggestions.** When a fix is small, the finding carries the replacement code, shown as the platform's suggested change ("Commit suggestion" on GitHub, "Apply suggestion" on GitLab). Larger fixes stay as advice in words.
4. **Threads follow the code.** On every later review: a finding that is gone gets "Fixed in `<short sha>`" and its thread resolved; a finding still present keeps its thread untouched (no repost, no duplicate); a new finding gets a new thread.
5. **Dismiss from the thread.** `@<bot> dismiss <reason>` as a reply in a finding's thread dismisses that finding without an ID; the thread is then resolved.
6. **A comment you can act on at a glance.** The one summary comment (still edited in place) opens with one line of verdict and confidence, puts the findings first, gives each finding a ready-to-copy dismiss command, and folds everything that passed into one collapsed line (§4). The layout lives in `render`, so CI mode gets it too.
7. **CI mode (pergent alone)** gets the confidence and the suggestions in its single comment. It gets no inline threads.

## Non-goals

- Inline threads in pergent's CI mode (it has no memory of which thread is which finding).
- Confidence per finding. The score is per skill and per review.
- Repository-skill or library-skill proposals from dismissals (piece 4b, parked by the owner: 10 explicit dismissals per library skill would trigger a proposal; revisit after this ships).
- Changing what blocks or what approves.

## Anti-goals (a failure even if it works)

- **Confidence changes approval.** It is information only. A 5/5 never approves anything and a 1/5 never holds an approval back; approval stays decided by blocking findings and coverage exactly as today.
- **A thread resolved while its finding is still there**, or a finding posted twice on the same merge request.
- **Inline posting breaking the review.** If inline comments can't be posted (permission, API error, line not in the diff), the review still publishes; those findings go in the summary. A failure here is never `publish_failed` and never a false pass.
- **A suggestion that changes more than the lines it is attached to**, or a suggestion block posted where the platform would apply it to the wrong lines.

## Decisions (the owner's)

| Decision | Choice |
|---|---|
| What to build | Confidence level **and** inline findings with one-click suggestions |
| Suggestions | One-click code suggestions **only when the fix is small**; otherwise advice in words |
| Summary vs inline | Summary keeps confidence, verdicts, checks, files and the not-inline findings; findings go inline |
| Confidence's effect | **Information only**, never touches approval |
| When a finding is fixed | **Auto-resolve** its thread with "Fixed in `<sha>`" |
| Which findings go inline | **Both** severities, cap **20** per merge request, the rest listed in the summary, blocking first |
| CI mode | Gets confidence and suggestions in its one comment |
| Dismiss hint | A ready-to-copy `@<bot> dismiss <id> <reason>` under **each** finding (pergent.sh only); the ID never appears bare |
| Finding ID | The fingerprint's short code stays the ID (it survives pushes, so dismissals carry over); never per-review numbers |
| Comment layout | Headline line, one-sentence reason, findings first as collapsible rows, passed checks and files collapsed, footer with the re-run command (§4) |
| Merge-blocking thread rules | When the target branch requires threads resolved before merge, **only blocking findings go inline**; suggestions stay in the summary (§3) |
| Reworded findings | Same skill, same file, overlapping lines (or within 5) is the same finding: old thread kept, no repost |
| When "fixed" is said | Only when the skill ran fine and still covers the file; file left the change → "No longer in this change."; anything else → thread untouched |
| A finding that returns | Reopens its old thread with "Back in `<sha>`" |
| What the cap of 20 counts | pergent-owned threads open at the same time; over-cap findings move inline later, blocking first |
| Confidence stored | The model's own score; caps applied when rendering, with the reason shown |
| Confidence caps | Blocking ⇒ at most 3; failed ⇒ not scored; skipped changed files ⇒ at most 4 |
| Read-only review App | Inline off for that instance, no "Needs attention" item; documented |
| "Small" fix | At most **10** replaced lines, all within the finding's own lines |
| Per-repository switch | "Inline findings", **on by default** |
| Releases | pergent **v0.2.0** (shipped), pergent **v0.3.0** (comment layout), then pergent.sh **0.6.0** with GitHub and GitLab inline threads in the same release |
| Dismiss-in-thread event | The upgrade note tells GitHub users to subscribe to **Pull request review comment** |

The approved mockup is `docs/superpowers/specs/2026-11-03-inline-findings-mockup.html` (made-up content). Its summary comment (panel 1) is superseded by the layout in §4; its inline finding and resolved thread (panels 2 and 3) stand.

## How it works

### 1. The model's answer (pergent)

`skill.Prompt()`'s fixed output format gains:

- top level: `"confidence": 1-5` and `"confidence_reason": "one sentence"`;
- per finding, optional: `"end_line"` (the last line the finding covers; `line` stays the first) and `"suggestion"`: the exact replacement text for lines `line`..`end_line` of the head version, only when the fix is small and self-contained.

The prompt explains the scale (5: nothing found that should stop a merge, in what was reviewed; 1: a merge would very likely break something) and when to give a `suggestion` (the whole fix fits in the lines the finding covers; no edits elsewhere; the finding covers at most **10** lines). pergent drops a `suggestion` whose range `line`..`end_line` is longer than 10 lines. *Builder's call:* exact wording.

**Missing or invalid fields are not a format error.** A missing or out-of-range `confidence` means "not scored" for that skill; an invalid `suggestion` (no `line`, `end_line` before `line`, range outside the file) is dropped and the finding kept. Neither triggers a repair run.

**Caps by fact**, applied by pergent, not the model:
- a skill with a blocking finding (after verification and dismissals) scores at most 3;
- a failed skill is "not scored";
- a skill that skipped changed files under a rule that leaves them unreviewed scores at most 4.

The review's confidence is the lowest score among scored skills; with none scored it is "not scored".

The verification run (blocking findings re-checked) doesn't change scores itself; the cap applies to what survives it.

### 2. Carrying it through (pergent)

- `finding.Finding` gains `EndLine` and `Suggestion`; the per-skill result gains `Confidence` and `ConfidenceReason`. Fingerprints don't change (suggestion and end line are not part of them), so threads and dismissals keep matching across revisions.
- The agent result (`docs/agent-result-schema.md`) gains these as optional fields in **version 1** (additive; an older reader ignores them). *Builder's call:* confirm the schema doc's own compatibility rules allow this; if not, version 2.
- The hidden findings block carries them, so `--previous-review` and pergent.sh see them.
- `render` (public package) shows the confidence at the top of the comment and a suggestion block under a finding that has one. pergent.sh calls `render` for the summary with an option that lists only the findings it couldn't post inline (see 4). `testdata/golden` is updated.
- **CI mode:** the one comment shows the confidence line and the per-skill table with confidence, and each finding with a suggestion shows it as a fenced `suggestion` block. On a comment (not a review comment) GitHub and GitLab don't offer the apply button, so CI mode shows the replacement as a plain `diff` block instead; *builder's call* on the exact format, keeping it readable.

### 3. Placing a finding (pergent.sh)

A finding can go inline only when its `line` (and `end_line`, if any) fall inside a hunk of the merge request's diff on the head side. Otherwise it goes in the summary under "Not on the diff". Context lines inside a hunk count. A suggestion is posted only when its whole range is inside one hunk; otherwise the finding goes inline without the suggestion block (the fix stays in words).

Order for the cap of 20: blocking first, then suggestions, each in file then line order. Threads already open on the merge request count toward the 20.

**When open threads block the merge.** GitHub's branch protection "Require conversation resolution before merging" and GitLab's "All threads must be resolved" make every open thread a merge blocker. When pergent.sh reads that setting as on for the target branch, only blocking findings go inline and suggestions stay in the summary, so a suggestion never blocks a merge. If pergent.sh can't read the setting (no permission, API error), it treats it as off and says so on the instance page. *Designer's call:* suggestion threads already open when the setting turns on are left as they are; the summary's "Not on the diff" section names the reason ("kept here: this branch requires threads resolved before merge").

### 4. The comment layout (pergent `render`, used by CI mode and pergent.sh)

What a reader needs comes first: is it safe, what should I look at, what do I do about it. Everything that passed is one collapsed line.

1. **Headline**, one line: the overall verdict and the confidence, e.g. `✅ No blocking issues · Confidence 4/5`, `❌ 1 blocking issue · Confidence 3/5 (capped)`, `⚠️ 1 skill failed · Confidence not scored`. Blocking wins over failed, failed over clean.
2. **Meta line**, small text: the reviewed commit, the files reviewed, `N of M checks passed`, the finding counts.
3. **Reason**: one quoted sentence, the confidence reason of the lowest-scoring skill (with the cap's reason when a cap applied). The model's free-text summary paragraph is no longer shown.
4. **Skills table**, only when the review ran more than one skill: skill · verdict · confidence · findings. With one skill the headline already says it all.
5. **Findings**, blocking first, then suggestions, each in file then line order. Each finding is a `<details>` row whose summary line carries the severity, the title, a link to `file:line` (or `line–end_line`) at the reviewed commit and, with more than one skill, the skill name. The first blocking finding is open; the rest are collapsed. The body holds the explanation, the suggested change or fix, and in pergent.sh the dismiss line `Not a real problem? Reply @<bot> dismiss <id> <reason>`. In pergent.sh this section lists only the findings that are not inline, titled "Not on the diff" with the reason (off the diff, over the cap, inline posting failed, kept here because the branch requires resolved threads).
6. **Checks**: one collapsed `Checks: N of M passed` holding one table of every check (area · check · status · detail). A failed or warned check is listed above it, expanded.
7. **Files reviewed** and **skipped**, collapsed, as today.
8. **Footer**, small text: in pergent.sh `Run again: @<bot> review` and, when inline is on, `or reply @<bot> dismiss <reason> in a finding's thread`; the pergent version. CI mode has no commands and shows only the version.

Example (made-up content, pergent.sh, one skill, everything inline except one finding):

````markdown
### ✅ No blocking issues · Confidence 4/5
<sub>Commit `a1b2c3d` · 11 files · 5 of 5 checks passed · 3 suggestions</sub>

> The earlier blocking issue is fixed and covered by a new test; what's left are suggestions.

#### Not on the diff (1)
<details><summary>💡 <b>Backoff cap is never reached</b> · <a href="…/blob/a1b2c3d/internal/retry/policy.go#L88">policy.go:88</a></summary>

The cap is compared before the multiplier is applied, so …

**Fix:** compare after multiplying.

Not a real problem? Reply `@pergent-review dismiss b71e02 <reason>`
</details>

<details><summary>Checks: 5 of 5 passed</summary>

| Area | Check | Status | Detail |
|---|---|---|---|
| Plan alignment | Implementation matches plan | ✅ | … |
</details>

<details><summary>Files reviewed (11)</summary> … </details>

<sub>Run again: `@pergent-review review` · or reply `@pergent-review dismiss <reason>` in a finding's thread · pergent v0.3.0</sub>
````

**`render` options this needs** (*builder's call* on names): a file-link builder (path, line, end line → URL; none means plain text), the dismiss command prefix (empty means no dismiss line, as in CI mode), and the existing `Inline` and `Footer`. CI mode builds links from the platform's CI variables (`GITHUB_SERVER_URL`/`GITHUB_REPOSITORY`, `CI_PROJECT_URL`) and the head SHA. pergent.sh builds them from the instance and repository.

**HTML inside `<summary>`.** Neither platform renders Markdown inside `<summary>`, so the row uses `<b>`, `<a>` and `<code>`. Model text placed there (the title) is HTML-escaped as well as neutralised.

It stays the one comment edited in place, with the `<!-- pergent -->` marker and the hidden findings block. The golden files change: this is the release (pergent v0.3.0) where CI-mode output is allowed to differ from before.

### 5. Threads (pergent.sh)

**Storage.** A new table maps (instance, project, merge request, fingerprint) to the platform thread (GitHub review comment and thread node IDs; GitLab discussion ID), the commit it was posted on, and its state (open, resolved-fixed, resolved-dismissed). A migration adds it.

**Posting.** On GitHub, the new inline comments of one review go in one pull request review with event `COMMENT` (separate from the approval review). On GitLab, one diff discussion per finding, positioned with the merge request's `base_sha`, `start_sha` and `head_sha`. Suggestion syntax: GitHub ```` ```suggestion ````; GitLab ```` ```suggestion:-0+N ```` covering the finding's lines.

**On every later review of the same merge request** (push or command re-run):
- fingerprint still found → leave the thread alone, even if the model worded it differently or the lines moved;
- fingerprint gone → reply "Fixed in `<short sha>`." and resolve the thread (GitHub: GraphQL `resolveReviewThread`; GitLab: resolve the discussion);
- a finding that is new → post it (within the cap).

A review that **failed** for a skill changes none of that skill's threads: a finding missing because the run failed is not "fixed".

**Dismissals.** A dismissal (by command, in-thread reply or dashboard) replies "Dismissed by @x: <reason>" and resolves the thread. Restoring a dismissal on the dashboard unresolves it. A reply `@<bot> dismiss <reason>` in a thread pergent.sh owns dismisses that thread's finding; the same permission and author rules as the `dismiss` command apply (the author can't dismiss a blocking finding on their own change; a blocking dismissal needs a reason).

**People resolving threads by hand** is fine: pergent.sh doesn't reopen a thread a person resolved, and doesn't post the finding again while it stays found.

### 6. Permissions and events

- **GitHub:** inline review comments and resolving threads need **Pull requests: Read and write** on the review App. Today a review App paired with a separate approve App or a bot user has Pull requests: Read-only. Without write, inline findings are off for that instance (see "Settled", 6): every finding stays in the summary, nothing fails, and there is no "Needs attention" item. Reading the "conversation resolution" branch rule needs **Administration: Read** (already optional); without it the rule is treated as off (§3). Dismiss-in-thread needs the **Pull request review comment** event; without it, in-thread commands are never received (the `dismiss <id>` comment still works).
- **GitLab:** the review bot is a Maintainer already; diff discussions arrive as note events, which the project hook already sends. "All threads must be resolved" is read from the project settings.
- README: the review App permission and event lists, and an upgrade note that names the new **Pull request review comment** event and the Read and write requirement for inline findings.

### 7. Turning it off

A per-repository setting **"Inline findings"**, on by default, so a team can keep the single-comment style. Off means every finding stays in the summary; threads already posted are left as they are. Confidence and suggestions in the summary don't need a switch.

## Constraints

- Public repositories: no employer, internal hostnames, people or screenshots in code, docs, tests or commits. The owner's screenshots are private and must not be referenced.
- pergent's guarantees carry over: reviews are read-only, a failed review never counts as a pass, and secrets are redacted from everything posted, now including suggestion text and thread replies.
- The model's text in inline comments is neutralised the same way it is in the summary comment (no quick actions, no @-mentions that ping people).
- Approval logic does not change.

## Order of work

1. **pergent:** answer format (confidence, end line, suggestion), parsing and caps, result schema, hidden block, `render` (confidence, suggestion blocks, "only not-inline findings" option), CI mode. Released as pergent v0.2.0.
2. **pergent:** the comment layout of §4 in `render` (headline, findings first, per-finding dismiss line, collapsed checks, file links, footer), CI-mode links, new golden files. Release pergent (v0.3.0).
3. **pergent.sh:** bump the pergent pin and image; the summary comment in the new layout with the per-finding dismiss line (useful on its own); then inline threads on **GitHub and GitLab together**, the thread table, resolve-on-fix, dismiss-in-thread, the merge-blocking rule check, the per-repository switch, README and upgrade notes. Release pergent.sh (0.6.0).

## Settled after the builder check (proposed by the designer, confirmed by the owner)

1. **"Gone" is not "fixed" until it is safe to say so.** Fingerprints carry over only when the model copies the old one back, so a reworded finding can look new. Before treating an open thread's finding as gone, pergent.sh matches it against the new findings of the same skill in the same file: a new finding whose line range overlaps the thread's range (after mapping the old lines through the new diff, or within 5 lines when they can't be mapped) is the same finding. It keeps the old thread and isn't posted again. Only an unmatched finding counts as gone.
2. **When a gone finding is "fixed".** A thread's finding is resolved as fixed only when all of these hold in the new review: the skill ran without failing, the skill still applies to the file (still assigned, file in its scope, not skipped under any rule), and the file is still in the change. If the file is no longer in the change, the thread is resolved with "No longer in this change." instead. In every other case (skill failed, removed, scope changed, file now skipped) the thread is left as it is.
3. **A finding that comes back** after its thread was resolved as fixed reopens that thread with "Back in `<short sha>`." rather than posting a new one.
4. **The cap of 20 counts threads open at the same time**: pergent.sh-owned threads that are not resolved. Threads a person resolved by hand and dismissed threads don't count. Findings over the cap stay in the summary and move inline in a later review when a slot is free, blocking first.
5. **Confidence in the result is the model's own score.** The result and the hidden block carry the model's `confidence` and `confidence_reason` unchanged; `render` applies the caps when it renders (CI mode and pergent.sh alike), after dismissals, so dismissing the only blocking finding restores the model's score. When a cap lowers the score, the summary says why next to the model's reason, e.g. "3/5 (capped: 1 blocking finding)".
6. **A review App with Pull requests: Read-only** (chosen on purpose to keep sign-in tokens read-only) means inline findings are off for that instance: every finding stays in the summary, as today plus confidence. There is no "Needs attention" item; the instance page and the README say inline needs Read and write. Nothing else posts the threads (the approve App must never be the reviewer).

## Left to the builder

Prompt wording; CI mode's suggestion rendering; the exact headline wording and icons, and the `render` option names (§4); the GitHub/GitLab API details for positioning, resolving and reading the merge-blocking thread rule; how the 20 cap orders findings across skills beyond "blocking first"; whether the agent result schema stays version 1 (§2).
