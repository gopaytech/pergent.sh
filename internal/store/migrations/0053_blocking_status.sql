-- The status of a review that finds blocking issues, per repository and as
-- the admin default: 'failed' (the default) or 'neutral', which never sets a
-- failed status. NULL is "use the default". It only adds, so the previous
-- release runs on the migrated database and ignores it.
ALTER TABLE dashboard_repos ADD COLUMN blocking_status text CHECK (blocking_status IN ('failed', 'neutral'));
ALTER TABLE repo_defaults ADD COLUMN blocking_status text CHECK (blocking_status IN ('failed', 'neutral'));
