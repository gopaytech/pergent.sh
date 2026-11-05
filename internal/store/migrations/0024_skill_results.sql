-- What became of each finding when its change was merged or closed, one row
-- per finding per change. A merge or close replaces the change's rows. Rows
-- go with their project or instance, and a year after decided_at.
CREATE TABLE finding_fates (
    instance    text        NOT NULL,
    project_id  bigint      NOT NULL,
    mr_iid      bigint      NOT NULL,
    fingerprint text        NOT NULL,
    skill       text        NOT NULL,
    -- The hash of the skill's content in the last review that reported the
    -- finding; '' when that review recorded none.
    skill_hash  text        NOT NULL DEFAULT '',
    severity    text        NOT NULL CHECK (severity IN ('blocking', 'suggestion')),
    fate        text        NOT NULL CHECK (fate IN ('fixed', 'dismissed', 'open', 'went_away')),
    merged      boolean     NOT NULL,
    -- Whether a bot opened the change; its findings are left out of the rates.
    by_bot      boolean     NOT NULL,
    decided_at  timestamptz NOT NULL,
    PRIMARY KEY (instance, project_id, mr_iid, fingerprint)
);
CREATE INDEX finding_fates_decided_idx ON finding_fates (instance, project_id, decided_at);

-- Changes whose fates are to be worked out: recorded by the webhook when a
-- change is merged or closed, finished when the rows are stored, given up on
-- after repeated failures (attempts).
CREATE TABLE fate_jobs (
    instance     text        NOT NULL,
    project_id   bigint      NOT NULL,
    mr_iid       bigint      NOT NULL,
    project_path text        NOT NULL,
    merged       boolean     NOT NULL,
    requested_at timestamptz NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0,
    not_before   timestamptz,
    finished_at  timestamptz,
    PRIMARY KEY (instance, project_id, mr_iid)
);
CREATE INDEX fate_jobs_pending_idx ON fate_jobs (requested_at) WHERE finished_at IS NULL;

-- When the sweep last looked at a change that has no job (it was still open),
-- so it looks at the others in turn.
CREATE TABLE fate_checks (
    instance   text        NOT NULL,
    project_id bigint      NOT NULL,
    mr_iid     bigint      NOT NULL,
    checked_at timestamptz NOT NULL,
    PRIMARY KEY (instance, project_id, mr_iid)
);

-- The sweep reads the runs with a decided review.
CREATE INDEX runs_decided_idx ON runs (created_at) WHERE outcome <> '';
