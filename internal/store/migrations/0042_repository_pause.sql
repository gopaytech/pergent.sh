-- Whether a repository switched on in the dashboard is reviewed. False is
-- paused: the row and its settings stay, but nothing reviews it, and reconcile
-- takes its webhook and deploy token off the platform. Existing rows stay on.
ALTER TABLE dashboard_repos ADD COLUMN enabled boolean NOT NULL DEFAULT true;
