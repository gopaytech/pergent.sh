-- The readiness scan: one row per scan of a repository, from queued to done
-- or failed. A repository has at most one queued or running scan (the unique
-- index, ignoring the case of the path, is what makes two requests at once
-- queue one), and keeps only its latest finished scan (the store deletes the
-- older ones when a scan finishes). started_by is the actor who asked, or
-- 'switch-on' / 'upgrade' for a scan nobody asked for. pod_result is what the
-- pod read through the platform API, job_result what the scan Job posted.
-- token_hash is the hash of the Job's one-time result token; NULL until the
-- Job exists and once spent.
CREATE TABLE readiness_scans (
    id          uuid        PRIMARY KEY,
    instance    text        NOT NULL,
    project     text        NOT NULL,
    branch      text        NOT NULL DEFAULT '',
    commit      text        NOT NULL DEFAULT '',
    state       text        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed')),
    started_by  text        NOT NULL DEFAULT '',
    queued_at   timestamptz NOT NULL,
    started_at  timestamptz,
    finished_at timestamptz,
    deadline    timestamptz,
    job_name    text        NOT NULL DEFAULT '',
    error       text        NOT NULL DEFAULT '',
    pod_result  jsonb,
    job_result  jsonb,
    token_hash  bytea
);
CREATE UNIQUE INDEX readiness_scans_active ON readiness_scans (instance, lower(project))
    WHERE state IN ('queued', 'running');
CREATE INDEX readiness_scans_repo_idx ON readiness_scans (instance, lower(project), queued_at DESC);
