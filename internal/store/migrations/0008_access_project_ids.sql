-- A session's visible repositories also record each one's platform ID (the
-- GitLab project ID or GitHub repository ID, as in runs.project_id): a
-- project deleted, renamed or transferred leaves its path free for another,
-- and the dashboard shows a run only when its project's ID is visible too.
-- Lists stored before this have no IDs: they are dropped, and each session
-- is checked again on its next request, seeing nothing if that check fails.
DELETE FROM user_access;
UPDATE sessions SET access_checked_at = NULL, is_admin = false;
ALTER TABLE user_access ADD COLUMN project_id bigint NOT NULL CHECK (project_id > 0);
