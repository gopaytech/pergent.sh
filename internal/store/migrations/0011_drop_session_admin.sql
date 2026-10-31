-- Who is an admin comes from the teams, read on every request, not from the
-- platform's admin groups at the last access check. With the admin groups
-- goes the setup problem of one the platform wouldn't show.
ALTER TABLE sessions DROP COLUMN is_admin;
DELETE FROM repo_attention WHERE kind = 'admin_team_unreadable';
