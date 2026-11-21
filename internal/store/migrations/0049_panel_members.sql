-- Review panel: the members A and B of a panel review, the entries its merge
-- (Job P) judged with, and what each used per model. It only adds, so the
-- previous release (up to 0048) still runs on the migrated database. It
-- ignores these tables. A panel review's run waits (state 'waiting',
-- skip_reason 'panel_members') while its members run: the previous release
-- never claims a waiting run.
--
-- panel_members: one row per attempt of member A or B. run_id is the
-- review's first run (Job P's later attempts point at it with attempt_of).
-- One queued, running or reported row per run and role. 'off' is the one row
-- of a review whose panel could not start, with the reason.
CREATE TABLE panel_members (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    role             text        NOT NULL CHECK (role IN ('a', 'b')),
    attempt_of       uuid        REFERENCES panel_members (id) ON DELETE CASCADE,
    state            text        NOT NULL DEFAULT 'queued' CHECK (state IN
        ('queued', 'running', 'reported', 'done', 'failed', 'fell_back', 'superseded', 'off')),
    job_name         text        NOT NULL DEFAULT '',
    token_hash       bytea,
    token_expires_at timestamptz,
    result           jsonb,
    failure_reason   text        NOT NULL DEFAULT '',
    failure_step     text        NOT NULL DEFAULT '',
    failure_detail   text        NOT NULL DEFAULT '',
    pod_name         text        NOT NULL DEFAULT '',
    checkout_seen    integer     NOT NULL DEFAULT 0,
    provider         text        NOT NULL DEFAULT '',
    model            text        NOT NULL DEFAULT '',
    model_source     text        NOT NULL DEFAULT '',
    model_entry_id   text        NOT NULL DEFAULT '',
    model_entry_name text        NOT NULL DEFAULT '',
    model_team_id    bigint,
    chain_position   integer     NOT NULL DEFAULT 0,
    chain_length     integer     NOT NULL DEFAULT 0,
    fallback_class   text        NOT NULL DEFAULT '',
    attempts         integer     NOT NULL DEFAULT 0,
    usage_recorded   boolean     NOT NULL DEFAULT false,
    cleaned_up       boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    claimed_at       timestamptz,
    dispatched_at    timestamptz,
    reported_at      timestamptz,
    not_before       timestamptz
);
CREATE UNIQUE INDEX panel_members_live_idx ON panel_members (run_id, role)
    WHERE state IN ('queued', 'running', 'reported');
CREATE UNIQUE INDEX panel_members_off_idx ON panel_members (run_id) WHERE state = 'off';
CREATE INDEX panel_members_run_idx ON panel_members (run_id, created_at);
CREATE INDEX panel_members_state_idx ON panel_members (state, created_at);

CREATE TABLE panel_member_logs (
    member_id  uuid        PRIMARY KEY REFERENCES panel_members (id) ON DELETE CASCADE,
    text       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX panel_member_logs_created_idx ON panel_member_logs (created_at);

CREATE TABLE panel_member_diagnostics (
    member_id        uuid        PRIMARY KEY REFERENCES panel_members (id) ON DELETE CASCADE,
    clone_log        text        NOT NULL,
    clone_log_masked text        NOT NULL,
    pod_state        text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX panel_member_diagnostics_created_idx ON panel_member_diagnostics (created_at);

-- panel_judges: the entry each role of a Job P run judged with.
CREATE TABLE panel_judges (
    run_id           uuid    NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    role             text    NOT NULL CHECK (role IN ('a', 'b')),
    provider         text    NOT NULL,
    model            text    NOT NULL,
    model_source     text    NOT NULL,
    model_entry_id   text    NOT NULL,
    model_entry_name text    NOT NULL,
    model_team_id    bigint,
    chain_length     integer NOT NULL,
    PRIMARY KEY (run_id, role)
);

-- run_usage_models: tokens per model of a panel review. part 'member' is one
-- row per member attempt and skill; part 'merge' one per Job P run, skill
-- and role. run_usage keeps one row per run and skill, the total.
CREATE TABLE run_usage_models (
    id          bigserial   PRIMARY KEY,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    member_id   uuid        REFERENCES panel_members (id) ON DELETE CASCADE,
    part        text        NOT NULL CHECK (part IN ('member', 'merge')),
    role        text        NOT NULL CHECK (role IN ('a', 'b')),
    skill       text        NOT NULL,
    provider    text        NOT NULL,
    model       text        NOT NULL,
    entry_name  text        NOT NULL DEFAULT '',
    source      text        NOT NULL DEFAULT 'global' CHECK (source IN ('global', 'team')),
    team_id     bigint,
    reported_at timestamptz NOT NULL,
    tokens_in   bigint      NOT NULL CHECK (tokens_in >= 0),
    tokens_out  bigint      NOT NULL CHECK (tokens_out >= 0),
    cache_read  bigint      CHECK (cache_read >= 0),
    cache_write bigint      CHECK (cache_write >= 0),
    reasoning   bigint      CHECK (reasoning >= 0),
    CHECK ((part = 'member') = (member_id IS NOT NULL))
);
CREATE UNIQUE INDEX run_usage_models_member_key ON run_usage_models (member_id, skill) WHERE part = 'member';
CREATE UNIQUE INDEX run_usage_models_merge_key ON run_usage_models (run_id, skill, role) WHERE part = 'merge';
CREATE INDEX run_usage_models_reported_idx ON run_usage_models (reported_at);
