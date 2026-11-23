# Multi review (experimental)

**Status: experimental, off by default.** It stays experimental until it is measured on real merge requests. A repository with 1 reviewer reviews as before, byte for byte. Multi review is off for every image until a pergent release with `agent --multi-review` is the minimum (see [The image check](#the-image-check)).

A multi review runs each review on 1 to 5 reviewers at the same time. Each reviewer is a [model chain](models.md). One judge checks their findings and writes the one result that pergent.sh publishes.

## The reviewers and the judge

| Part | What it is |
| --- | --- |
| Reviewer 1 | The repository's model chain in effect. It is today's review. |
| Reviewers 2 to 5 | Each has its own model chain. At most 5 reviewers in all. |
| Judge | A model chain of its own. It checks the findings of every reviewer against the code. With 2 or more reviewers a judge is needed. |

The **Reviewers** card is on the repository's Settings tab (for a viewer, on **Skills & instructions**). Anyone who can see a repository sees its reviewers. Nobody sees a key. Admins and maintainers of the owner team change them. The rights are the model chain's.

- **1 reviewer** is today's review. No judge runs, and nothing changes.
- **Add a reviewer.** If the judge has no entry, it starts as the first entry of reviewer 2. You can change it.
- **Remove a reviewer.** The reviewers after it get a lower number. Removing the last of reviewers 2 to 5 clears the judge.
- **Same first model.** A save that gives 2 reviewers the same first model is refused. If it is already so, the card shows a warning, and the later reviewer runs on its next entry.
- **Cost hint.** With 2 or more reviewers the card says how many reviews each review now runs, and that it costs about that many times one review.
- A change to the reviewers is audited as `reviewers_changed`.

## The Jobs

A review with N reviewers runs N + 1 Jobs:

| Job | What it does |
| --- | --- |
| Reviewer 1 to N | Each reviews the same change on its own chain, at the same time. |
| Judge | Starts after every reviewer is final. Runs `pergent agent --multi-review` and puts the results together. |

- Reviewers run with skill verification off. They post nothing: no comment, note, thread, approval or status. Only the judge Job's result is published.
- The judge Job has a 10 minute timeout.
- The judge waits until each reviewer is final: done, or failed after its last attempt.

## What a reviewer does

- **Fallback per reviewer.** If a reviewer's model is out of quota or credit, or its key is rejected, that reviewer starts again on the next entry of its own chain. Each attempt gets the full timeout. Reviewers do not fall back for each other.
- **The same model is skipped.** A reviewer never takes a model that an earlier reviewer runs on. It takes the next entry of its own chain.
- **A reviewer that failed partway** still gives its blocking findings. They stay in the review, and the comment says that this reviewer could not review the skill.
- **Only reviewer 1 left.** If the other reviewers are skipped or fail, the judge works with reviewer 1's result.
- **Every reviewer failed.** The review fails. It is never a pass.

## Skipped reviewers

An extra reviewer can be skipped when it starts. It is never a failure of the review. The comment says why, in a note for each skipped reviewer.

| Reason | Why |
| --- | --- |
| `no different model left` | Every usable entry is a model an earlier reviewer runs on. |
| `the token quota` | A token quota or a model or team limit stops every entry. |
| `no usable entry` | No entry has a key that works, for example all are marked or removed. |

An extra reviewer never goes past a quota or a model limit, not even when an admin starts the review. The limits let a review start. They do not let it use more models. Reviewer 1 follows the usual [limit rules](token-quota.md#model-and-team-limits).

## When multi review does not run

Sometimes a repository has more than 1 reviewer but the review runs as one review. The comment footer says why, with the line `multi review off: <reason>`.

| Reason in the footer | Why |
| --- | --- |
| `the review image does not support it` | The review image is too old for `--multi-review`. |
| `pergent.sh's own image is not set` | `PERGENT_SH_IMAGE` is empty. The judge needs it to fetch the reviewers' results. The chart sets it. |
| `the token quota is used up` | Every extra reviewer was skipped because of a quota or a limit. |
| `no other reviewer can start` | Every extra reviewer was skipped for another reason. |
| `the reviewers could not be read` | A database error while reading the reviewers. The review still runs, with reviewer 1 alone. |

The run page shows the same reason in its **Multi review** section: [Dashboard](dashboard.md). A review that was asked for through the [Review API](../setup/review-api.md) never runs as a multi review.

## The judge

- The judge checks every finding against the code and merges the findings of the reviewers into one.
- A finding that the judge cannot check stays blocking. A finding that a person dismissed stays dismissed.
- **Fallback.** If the judge's provider says it is out of quota or credit, or rejects its key, the judge runs again on the next entry of the judge chain.
- **The judge ignores limits.** A model or team limit never holds or skips the judge. It ends a review that has started.
- **The judge fails.** If the judge Job gives no usable result, the review runs once more with no judge. The comment then says `Multi review: the judge did not run. Every blocking finding stays blocking.` If that fails too, the review fails.
- **A result with no `multi_review` block** fails the review. It is never a pass.
- **The judge does not run** when the token quota is used up, when there is no judge chain, or when no judge entry can be used. The review then merges the findings with no check, and the run page says why.
- **A lost reviewer result.** The judge Job downloads each reviewer's result. Only a 404 means that a reviewer has no result. Any other error, an auth failure included, fails the judge Job, so a lost reviewer never turns into a pass.

## What the comment shows

The comment names the reviewers and the judge by their **entry names**. It never shows a provider or a model name, or a base URL.

| Line | Where | Meaning |
| --- | --- | --- |
| `Multi review: <names> · judge <name>` | The header | The reviewers, and the judge when there was one. |
| `found by <n> of <m> reviewers (<names>)` | A finding | More than 1 reviewer found it. A finding that 1 reviewer found says `found by <name>`. |
| `judge disagreed: <reason>` | A finding | The judge rated it differently from a reviewer. |
| `<names> rated it a suggestion` | A finding | Some reviewers said suggestion, and others blocking. |
| `the judge could not check this finding; it stays blocking.` | A finding | The judge had no answer for it. |
| `Multi review: <name> was skipped: <reason>.` | A note | A reviewer that gave no result, and why. |
| `Multi review: <name> could not review this.` | A skill's note | A reviewer that failed on this skill. |
| `Multi review: the judge did not run. Every blocking finding stays blocking.` | A skill's note | The review ran with no judge. |

## Freeze at the split

When a review splits, pergent.sh copies the reviewers and the judge chain, with their models and keys. The review uses that copy to its end. A change to a chain, a model or a mark after the split does not change a review that is running. The next review uses the new reviewers.

When a team removes one of its models, the model also leaves the reviewer chains that name it. The audit log says so.

## Cost

All the Jobs spend tokens. Tokens are counted per model: [Usage](usage.md#multi-review). Every Job counts on the [token quota](token-quota.md).

## The image check

`PanelMinVersion` in the code is the first pergent release with `agent --multi-review`. It is empty until that release exists, so every image is refused. Adding a reviewer fails with `no review image supports multi review yet`. Removing a reviewer needs no check. If reviewers are already stored and the image is too old, the review runs with reviewer 1 alone, and the footer says `multi review off: the review image does not support it`.

## A known limit

A dismissal that someone makes while the judge Job runs is not in the list the judge got when it started. The comment still applies the dismissal. In a rare case this can hide a finding that the judge linked to the dismissed one.

## On the repository page

One line shows the reviewers:

| Line | Meaning |
| --- | --- |
| `Multi review: on (<n> reviewers, judge <name>)` | 2 or more reviewers. |
| `Multi review: on (<n> reviewers, no judge)` | With no judge. |
| `Multi review: off` | 1 reviewer. |
