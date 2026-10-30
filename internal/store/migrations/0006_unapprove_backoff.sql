-- A stale approval the approve bot couldn't remove is tried again after a
-- backoff, not on every cleanup pass: unapprove_failures counts the failed
-- attempts since the approval was recorded, and unapprove_not_before is when
-- the next may start (NULL: at once). Both start over when an approval is
-- recorded or cleared.
ALTER TABLE merge_requests
    ADD COLUMN unapprove_failures   integer NOT NULL DEFAULT 0,
    ADD COLUMN unapprove_not_before timestamptz;
