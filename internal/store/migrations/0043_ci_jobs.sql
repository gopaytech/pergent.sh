-- CI job results. dashboard_repos.ci_jobs is the repository's chosen jobs
-- (NULL: off). runs.ci_checked is true once CI was looked at for the run, so it
-- is never held for it again; runs.ci_deadline is when a run held with
-- skip_reason 'ci_pending' stops waiting; runs.ci_jobs is the per-job
-- snapshot (name, result, reason, read, size, cut, blocks), never any CI
-- text. projects.actions_readable and projects.check_run_events are what the
-- review App could do on the repository when it was last reconciled (NULL:
-- not known yet).
ALTER TABLE dashboard_repos ADD COLUMN ci_jobs jsonb;
ALTER TABLE runs ADD COLUMN ci_checked boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN ci_deadline timestamptz;
ALTER TABLE runs ADD COLUMN ci_jobs jsonb;
ALTER TABLE projects ADD COLUMN actions_readable boolean;
ALTER TABLE projects ADD COLUMN check_run_events boolean;
CREATE INDEX runs_ci_pending_idx ON runs (ci_deadline) WHERE state = 'waiting' AND skip_reason = 'ci_pending';
