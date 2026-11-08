-- Trusted authors. A push of an author the repository doesn't trust waits in
-- the new state 'waiting' (its skip_reason says why) until someone with write
-- access starts it, which makes it a queued run with the new trigger 'start'.
-- The previous release claims only queued runs, so it never runs a waiting
-- one; it shows it as unknown.
ALTER TABLE runs DROP CONSTRAINT runs_state_check;
ALTER TABLE runs ADD CONSTRAINT runs_state_check
    CHECK (state IN ('queued', 'running', 'reported', 'published', 'publish_failed', 'superseded', 'skipped', 'waiting'));

-- The trigger's check was declared with its column (0012), so its name is
-- whatever Postgres gave it.
DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
        WHERE conrelid = 'runs'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%trigger%'
    LOOP
        EXECUTE format('ALTER TABLE runs DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('push', 'rerun', 'rerun_failed', 'start'));

-- A repository's own setting (NULL: the instance's) and its allowlist, a JSON
-- list of {"id", "username"} (NULL: nobody).
ALTER TABLE dashboard_repos ADD COLUMN trusted_authors_only text
    CHECK (trusted_authors_only IN ('on', 'off'));
ALTER TABLE dashboard_repos ADD COLUMN trusted_authors jsonb;

-- The instance's setting, the default of its repositories.
ALTER TABLE instances ADD COLUMN trusted_authors_only text NOT NULL DEFAULT 'follow_visibility'
    CHECK (trusted_authors_only IN ('follow_visibility', 'on', 'off'));

-- When the note about an untrusted author was posted on the change (NULL: never).
ALTER TABLE merge_requests ADD COLUMN untrusted_noted_at timestamptz;
