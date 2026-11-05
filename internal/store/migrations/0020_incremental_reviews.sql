-- Whether a push reviews only the files changed since the last review.
-- NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN incremental_reviews boolean;
ALTER TABLE repo_defaults   ADD COLUMN incremental_reviews boolean;

-- incremental marks a run that reviews only some files. It can't be read from
-- base_run_id, which is set to NULL when the earlier run is pruned.
ALTER TABLE runs ADD COLUMN incremental boolean NOT NULL DEFAULT false;
-- The hash of each skill's content, by name.
ALTER TABLE runs ADD COLUMN skill_hashes jsonb;
