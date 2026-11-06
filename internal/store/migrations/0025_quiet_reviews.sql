-- Whether a review says less. NULL is the default (off).
ALTER TABLE dashboard_repos ADD COLUMN quiet_reviews boolean;
ALTER TABLE repo_defaults   ADD COLUMN quiet_reviews boolean;
-- Whether a run was quiet, read when it started. NULL is off: a run from
-- before this column, or one that never started.
ALTER TABLE runs ADD COLUMN quiet_reviews boolean;
