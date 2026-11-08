# Review focus

Most of this code decides whether a change gets approved, so a wrong "clean" is the worst outcome. A review that fails loudly is much better than one that passes wrongly.

Treat these as blocking:

- A path that can approve, or report a clean result, for something not actually reviewed: a file counted as reviewed that never was, a partial result published as the whole review, an old head's result used for a new one.
- A failure of an optional feature that fails a review. Incremental reviews and past dismissals fall back to a full review, or to none of the extra input.
- Text from a change, a comment or a dismissal reason treated as trusted, or put in a prompt without being framed as data. A change's author must not be able to get around a rule through anything they can write.
- A way around who may do what: the author can't dismiss a blocking finding; only write access or better teaches later reviews.
- Secrets in a log, a reply, a stored result or a prompt.
- A migration or config key that stops the previous release from working on the same database, or that isn't in `docs/upgrading.md`.
- SQL whose column list, placeholders and scan order don't line up. Tests of it only run in CI.

Worth a suggestion:

- A test seeding rows that a behaviour change makes unrealistic.
- A reply or note changed without the docs text that quotes it.

Not worth raising: wording in `docs/superpowers`, which records plans and designs at the time they were written.
