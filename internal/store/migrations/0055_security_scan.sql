-- Security scans: a whole-repository scan a person starts, run as its own Job,
-- with findings that live on from scan to scan. It only adds, so the previous
-- release (migrations up to 0054) still runs on the migrated database:
--
-- runs: a scan owns one hidden run of the new kind 'scan', only so its token
-- usage (run_usage) and the entry it used reach the usage, quota and limit
-- SQL. It has no repository (instance '', project 0, mr_iid a number of its
-- own), it is stored superseded and with its usage recorded, so the previous
-- release never claims, dispatches, lists or fills it: it claims only queued
-- runs, lists only kind 'mr' and runs with an instance, and fills only runs
-- whose usage is not recorded. Its own inserts (kind 'mr', and 'api' with a
-- client) pass the wider checks.
--
-- The new tables (security_scans, security_findings, security_finding_events,
-- security_scan_settings, security_scan_chains, security_scan_chain_entries)
-- are unknown to the previous release, which reads none of them.
ALTER TABLE runs DROP CONSTRAINT runs_api_shape;
-- The kind's check was declared with its column (0039), so its name is
-- whatever Postgres gave it. runs_api_shape, the other check naming kind, is
-- dropped above.
DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
        WHERE conrelid = 'runs'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%kind%'
    LOOP
        EXECUTE format('ALTER TABLE runs DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check CHECK (kind IN ('mr', 'api', 'scan'));
ALTER TABLE runs ADD CONSTRAINT runs_api_shape
    CHECK (kind = 'mr' OR (instance = '' AND project_id = 0 AND (kind = 'scan' OR api_client_id IS NOT NULL)));
-- The mr_iid of a scan's run: a number of its own, as api_review_numbers is
-- for an API review.
CREATE SEQUENCE security_scan_numbers;

-- One row per scan of a repository, from queued to complete, partial or
-- failed. A repository has at most one queued or running scan (the unique
-- index, ignoring the case of the path). number counts the repository's
-- scans from 1. run_id is the scan's hidden run. token_hash is the hash of
-- the Job's result token: NULL until the Job exists and once spent.
-- state_files and sarif are what the Job sent; only the last scans keep
-- them. The other rows are kept for good.
CREATE TABLE security_scans (
    id                  uuid        PRIMARY KEY,
    run_id              uuid        NOT NULL UNIQUE REFERENCES runs (id) ON DELETE CASCADE,
    number              bigint      NOT NULL,
    instance            text        NOT NULL,
    project             text        NOT NULL,
    project_id          bigint      NOT NULL DEFAULT 0,
    branch              text        NOT NULL DEFAULT '',
    commit              text        NOT NULL DEFAULT '',
    state               text        NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'running', 'complete', 'partial', 'failed')),
    reason              text        NOT NULL DEFAULT '',
    started_by          text        NOT NULL DEFAULT '',
    past_quota          boolean     NOT NULL DEFAULT false,
    queued_at           timestamptz NOT NULL,
    started_at          timestamptz,
    finished_at         timestamptz,
    deadline            timestamptz,
    cancel_requested_at timestamptz,
    job_name            text        NOT NULL DEFAULT '',
    provider            text        NOT NULL DEFAULT '',
    model               text        NOT NULL DEFAULT '',
    entry_name          text        NOT NULL DEFAULT '',
    max_steps           integer,
    subagent_max_steps  integer,
    timeout_seconds     integer     NOT NULL,
    token_budget        bigint,
    skill_name          text        NOT NULL DEFAULT '',
    skill_commit        text        NOT NULL DEFAULT '',
    skill_updated       boolean     NOT NULL DEFAULT false,
    coverage_done       integer,
    coverage_planned    integer,
    usage_source        text        NOT NULL DEFAULT '',
    new_count           integer     NOT NULL DEFAULT 0,
    fixed_count         integer     NOT NULL DEFAULT 0,
    token_hash          bytea,
    state_files         jsonb,
    sarif               jsonb
);
CREATE UNIQUE INDEX security_scans_number ON security_scans (instance, lower(project), number);
CREATE UNIQUE INDEX security_scans_active ON security_scans (instance, lower(project))
    WHERE state IN ('queued', 'running');
CREATE INDEX security_scans_repo_idx ON security_scans (instance, lower(project), queued_at DESC);

-- A repository's findings, one per fingerprint: open, dismissed (with a
-- reason) or fixed (by a complete scan that no longer found it). The scans
-- they name are kept for good.
CREATE TABLE security_findings (
    id               bigserial   PRIMARY KEY,
    instance         text        NOT NULL,
    project          text        NOT NULL,
    fingerprint      text        NOT NULL,
    state            text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'dismissed', 'fixed')),
    rule_id          text        NOT NULL DEFAULT '',
    cwe              text        NOT NULL DEFAULT '',
    tags             text[]      NOT NULL DEFAULT '{}',
    severity         text        NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    title            text        NOT NULL DEFAULT '',
    description      text        NOT NULL DEFAULT '',
    exploit          text        NOT NULL DEFAULT '',
    fix_hint         text        NOT NULL DEFAULT '',
    confidence       text        NOT NULL DEFAULT '',
    file             text        NOT NULL DEFAULT '',
    start_line       integer     NOT NULL DEFAULT 0,
    needs_validation boolean     NOT NULL DEFAULT false,
    first_scan_id    uuid        NOT NULL REFERENCES security_scans (id),
    last_scan_id     uuid        NOT NULL REFERENCES security_scans (id),
    fixed_scan_id    uuid        REFERENCES security_scans (id),
    dismiss_reason   text
        CHECK (dismiss_reason IN ('false_positive', 'wont_fix', 'used_in_tests', 'mitigated', 'acceptable_risk')),
    dismiss_comment  text        NOT NULL DEFAULT '',
    dismissed_by     text        NOT NULL DEFAULT '',
    dismissed_at     timestamptz,
    updated_at       timestamptz NOT NULL
);
CREATE UNIQUE INDEX security_findings_fingerprint ON security_findings (instance, lower(project), fingerprint);
CREATE INDEX security_findings_state_idx ON security_findings (instance, lower(project), state);

-- What happened to a finding, oldest first. scan_id is the scan that found,
-- found again or fixed it; NULL for a person's dismiss or restore.
CREATE TABLE security_finding_events (
    id         bigserial   PRIMARY KEY,
    finding_id bigint      NOT NULL REFERENCES security_findings (id) ON DELETE CASCADE,
    scan_id    uuid,
    kind       text        NOT NULL CHECK (kind IN ('found', 'reopened', 'fixed', 'dismissed', 'restored')),
    actor      text        NOT NULL DEFAULT '',
    reason     text        NOT NULL DEFAULT '',
    comment    text        NOT NULL DEFAULT '',
    at         timestamptz NOT NULL
);
CREATE INDEX security_finding_events_finding_idx ON security_finding_events (finding_id, at, id);

-- A repository's scan settings. NULL is the default: the skill's step
-- limits, a 3 hour timeout, and no findings sent to reviews.
CREATE TABLE security_scan_settings (
    instance            text        NOT NULL,
    project             text        NOT NULL,
    max_steps           integer     CHECK (max_steps BETWEEN 5 AND 1000),
    subagent_max_steps  integer     CHECK (subagent_max_steps BETWEEN 5 AND 300),
    timeout_seconds     integer     CHECK (timeout_seconds BETWEEN 600 AND 21600),
    findings_in_reviews boolean,
    updated_at          timestamptz NOT NULL
);
CREATE UNIQUE INDEX security_scan_settings_repo ON security_scan_settings (instance, lower(project));

-- A repository's scan chain: the shape of model_chains and
-- model_chain_entries (0045, 0046), so the chain code reads and writes them,
-- at the repository level only. No row: the repository has no chain of its
-- own.
CREATE TABLE security_scan_chains (
    id             bigserial   PRIMARY KEY,
    scope          text        NOT NULL CHECK (scope IN ('repo')),
    team_id        bigint      REFERENCES teams (id) ON DELETE CASCADE,
    instance       text        NOT NULL DEFAULT '',
    project        text        NOT NULL DEFAULT '',
    saved_by_admin boolean     NOT NULL DEFAULT false,
    updated_at     timestamptz NOT NULL,
    CHECK (team_id IS NULL AND instance <> '' AND project <> '')
);
CREATE UNIQUE INDEX security_scan_chains_key ON security_scan_chains (scope, COALESCE(team_id, 0), instance, lower(project));

CREATE TABLE security_scan_chain_entries (
    chain_id bigint NOT NULL REFERENCES security_scan_chains (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 5),
    source   text   NOT NULL CHECK (source IN ('global', 'team')),
    model_id text   NOT NULL,
    PRIMARY KEY (chain_id, position)
);
CREATE INDEX security_scan_chain_entries_model_idx ON security_scan_chain_entries (source, model_id);
