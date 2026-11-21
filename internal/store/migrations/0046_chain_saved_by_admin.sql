-- Whether an admin saved a chain (owner ruling 2026-11-20). A repository
-- chain an admin saved may name global entries teams may not use, even when
-- a team owns the repository; one a team's maintainer saved may not. Every
-- chain stored before this migration was an admin's: the upgrade seeded the
-- repository chains from the model setting, which only admins changed, and
-- no other writer was released. It only adds, so the previous release still
-- runs on the migrated database; it ignores the column.
ALTER TABLE model_chains ADD COLUMN saved_by_admin boolean NOT NULL DEFAULT false;
UPDATE model_chains SET saved_by_admin = true;
