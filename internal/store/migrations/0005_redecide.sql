-- A reported run whose change was marked ready while it was being published:
-- the publish under way read the draft flag before, so the run goes back to
-- reported once it is marked published, and the publisher decides again.
ALTER TABLE runs ADD COLUMN redecide boolean NOT NULL DEFAULT false;
