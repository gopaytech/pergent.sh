CREATE TABLE runs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instance         text        NOT NULL,
    project_id       bigint      NOT NULL,
    mr_iid           bigint      NOT NULL,
    project_path     text        NOT NULL DEFAULT '',
    head_sha         text        NOT NULL,
    base_sha         text        NOT NULL DEFAULT '',
    start_sha        text        NOT NULL DEFAULT '',
    target_branch    text        NOT NULL DEFAULT '',
    state            text        NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'running', 'reported', 'published', 'publish_failed', 'superseded')),
    job_name         text        NOT NULL DEFAULT '',
    -- SHA-256 of the single-use result token; NULL once spent or before dispatch.
    token_hash       bytea,
    token_expires_at timestamptz,
    result           jsonb,
    failure_reason   text        NOT NULL DEFAULT '',
    publish_error    text        NOT NULL DEFAULT '',
    attempts         integer     NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    dispatched_at    timestamptz,
    reported_at      timestamptz,
    not_before       timestamptz,
    cleaned_up       boolean     NOT NULL DEFAULT false
);

-- One live run per head of a merge request; superseded runs are history.
CREATE UNIQUE INDEX runs_live_head_idx
    ON runs (instance, project_id, mr_iid, head_sha)
    WHERE state <> 'superseded';

CREATE INDEX runs_mr_idx ON runs (instance, project_id, mr_iid, created_at);
CREATE INDEX runs_state_idx ON runs (state, created_at);

CREATE TABLE merge_requests (
    instance     text   NOT NULL,
    project_id   bigint NOT NULL,
    mr_iid       bigint NOT NULL,
    note_id      bigint NOT NULL DEFAULT 0,
    approved_sha text   NOT NULL DEFAULT '',
    PRIMARY KEY (instance, project_id, mr_iid)
);

CREATE TABLE projects (
    instance               text   NOT NULL,
    project_id             bigint NOT NULL,
    path                   text   NOT NULL DEFAULT '',
    http_url_to_repo       text   NOT NULL DEFAULT '',
    hook_id                bigint NOT NULL DEFAULT 0,
    deploy_token_id        bigint NOT NULL DEFAULT 0,
    deploy_token_username  text   NOT NULL DEFAULT '',
    deploy_token_sealed    bytea,
    deploy_token_expires_at timestamptz,
    clone_seconds          integer NOT NULL DEFAULT 0,
    PRIMARY KEY (instance, project_id)
);
