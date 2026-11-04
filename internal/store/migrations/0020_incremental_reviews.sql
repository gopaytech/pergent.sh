-- Whether a push reviews only the files changed since the last review.
-- NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN incremental_reviews boolean;
ALTER TABLE repo_defaults   ADD COLUMN incremental_reviews boolean;
