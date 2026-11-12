-- Whether a review also reads the lines a change deletes and checks what still
-- uses deleted files and renamed files' old paths. NULL is the default (off).
-- A repository setting only: there is no instance default and no per-run copy.
ALTER TABLE dashboard_repos ADD COLUMN check_deleted boolean;
