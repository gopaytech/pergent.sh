-- A GitLab instance added in the dashboard can approve with its review bot,
-- with no approve token. This only adds a column: the previous release still
-- runs on the migrated database, and refuses to connect such an instance
-- because it has no approve token.
ALTER TABLE instances ADD COLUMN approve_with_review_bot boolean NOT NULL DEFAULT false;
