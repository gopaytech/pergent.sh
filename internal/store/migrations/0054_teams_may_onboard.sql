-- Whether teams may onboard repositories of the instance themselves (their
-- maintainers switch a repository on and claim it for their team). Off by
-- default: only admins onboard. It only adds, so the previous release runs on
-- the migrated database and ignores it.
ALTER TABLE instances ADD COLUMN teams_may_onboard boolean NOT NULL DEFAULT false;
