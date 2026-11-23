-- Multi review: reviewers 2 to 5 and a judge per repository, the members of a
-- multi review, its frozen judge chain, the judge of each judge Job run, and
-- tokens per model. It only adds. The previous release (up to 0049) still
-- runs on the migrated database and ignores all of it: its panel code reads
-- only the 0048 and 0049 tables, which stay, unused. A multi review's run
-- waits with skip_reason 'review_members', which the previous release never
-- claims.
--
-- reviewer_chains: one row per repository and slot. scope and team_id are
-- fixed so the chain code reads it as it reads model_chains. An extra
-- reviewer's chain always has an entry; a judge chain may have none (its
-- entries were removed), and reviews then run with --no-judge.
CREATE TABLE reviewer_chains (
    id             bigserial   PRIMARY KEY,
    scope          text        NOT NULL DEFAULT 'repo' CHECK (scope = 'repo'),
    team_id        bigint      CHECK (team_id IS NULL),
    instance       text        NOT NULL CHECK (instance <> ''),
    project        text        NOT NULL CHECK (project <> ''),
    slot           text        NOT NULL CHECK (slot IN ('r2', 'r3', 'r4', 'r5', 'judge')),
    saved_by_admin boolean     NOT NULL DEFAULT false,
    updated_at     timestamptz NOT NULL
);
CREATE UNIQUE INDEX reviewer_chains_key ON reviewer_chains (instance, lower(project), slot);

CREATE TABLE reviewer_chain_entries (
    chain_id bigint NOT NULL REFERENCES reviewer_chains (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 5),
    source   text   NOT NULL CHECK (source IN ('global', 'team')),
    model_id text   NOT NULL,
    PRIMARY KEY (chain_id, position)
);
CREATE INDEX reviewer_chain_entries_model_idx ON reviewer_chain_entries (source, model_id);

-- review_members: one row per attempt of reviewer 1 to 5. run_id is the
-- review's first run. chain is the reviewer's chain as it was at the split
-- (scope, saved_by_admin, entries with source, id, name, provider, model;
-- never a key or a base URL). 'skipped': an extra reviewer that could not
-- start at the split. 'off': the one row of a review whose multi review did
-- not start (reviewer 1, no chain).
CREATE TABLE review_members (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    reviewer         integer     NOT NULL CHECK (reviewer BETWEEN 1 AND 5),
    attempt_of       uuid        REFERENCES review_members (id) ON DELETE CASCADE,
    state            text        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running',
        'reported', 'done', 'failed', 'fell_back', 'superseded', 'skipped', 'off')),
    chain            jsonb,
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
    not_before       timestamptz,
    CHECK ((state = 'off') = (chain IS NULL))
);
CREATE UNIQUE INDEX review_members_live_idx ON review_members (run_id, reviewer)
    WHERE state IN ('queued', 'running', 'reported');
CREATE UNIQUE INDEX review_members_off_idx ON review_members (run_id) WHERE state = 'off';
CREATE INDEX review_members_run_idx ON review_members (run_id, created_at);
CREATE INDEX review_members_state_idx ON review_members (state, created_at);

CREATE TABLE review_member_logs (
    member_id  uuid        PRIMARY KEY REFERENCES review_members (id) ON DELETE CASCADE,
    text       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX review_member_logs_created_idx ON review_member_logs (created_at);

CREATE TABLE review_member_diagnostics (
    member_id        uuid        PRIMARY KEY REFERENCES review_members (id) ON DELETE CASCADE,
    clone_log        text        NOT NULL,
    clone_log_masked text        NOT NULL,
    pod_state        text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX review_member_diagnostics_created_idx ON review_member_diagnostics (created_at);

-- review_judge_chains: the judge chain of a multi review as it was at the
-- split, by the review's first run. No entries: no judge.
CREATE TABLE review_judge_chains (
    run_id uuid  PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    chain  jsonb NOT NULL
);

-- review_judges: what each judge Job run judged with: every reviewer of the
-- review, in --reviewer order, and its entry, or no_judge and why.
CREATE TABLE review_judges (
    run_id           uuid      PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    reviewers        integer[] NOT NULL,
    no_judge         boolean   NOT NULL,
    no_judge_reason  text      NOT NULL DEFAULT '',
    provider         text      NOT NULL DEFAULT '',
    model            text      NOT NULL DEFAULT '',
    model_source     text      NOT NULL DEFAULT '',
    model_entry_id   text      NOT NULL DEFAULT '',
    model_entry_name text      NOT NULL DEFAULT '',
    model_team_id    bigint,
    chain_position   integer   NOT NULL DEFAULT 0,
    chain_length     integer   NOT NULL DEFAULT 0
);

-- run_usage_by_model: tokens per model of a multi review. part 'reviewer' is
-- one row per member attempt and skill, on the run that was split; part
-- 'judge' one per judge run and skill. run_usage keeps the total.
CREATE TABLE run_usage_by_model (
    id          bigserial   PRIMARY KEY,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    member_id   uuid        REFERENCES review_members (id) ON DELETE CASCADE,
    part        text        NOT NULL CHECK (part IN ('reviewer', 'judge')),
    reviewer    integer     CHECK (reviewer BETWEEN 1 AND 5),
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
    CHECK ((part = 'reviewer') = (member_id IS NOT NULL)),
    CHECK ((part = 'reviewer') = (reviewer IS NOT NULL))
);
CREATE UNIQUE INDEX run_usage_by_model_reviewer_key ON run_usage_by_model (member_id, skill) WHERE part = 'reviewer';
CREATE UNIQUE INDEX run_usage_by_model_judge_key ON run_usage_by_model (run_id, skill) WHERE part = 'judge';
CREATE INDEX run_usage_by_model_reported_idx ON run_usage_by_model (reported_at);
