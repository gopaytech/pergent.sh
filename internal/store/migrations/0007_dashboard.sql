-- The dashboard's tables, all in one migration.

-- Repositories switched on in the dashboard. A NULL setting means "use the
-- default". A repository is one row per instance whatever the case of its
-- path (GitLab and GitHub paths are case-insensitive): the key is the unique
-- index on (instance, lower(project)), as a primary key can't hold an
-- expression.
CREATE TABLE dashboard_repos (
    instance            text        NOT NULL,
    project             text        NOT NULL,
    approve             boolean,
    review_drafts       boolean,
    failure_status      text        CHECK (failure_status IN ('failed', 'neutral')),
    skills              text[],
    job_timeout_seconds integer     CHECK (job_timeout_seconds > 0),
    job_memory          text,
    created_by          text        NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX dashboard_repos_key ON dashboard_repos (instance, lower(project));

-- The redacted tail of a review Job's log.
CREATE TABLE run_logs (
    run_id     uuid        PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    text       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_logs_created_idx ON run_logs (created_at);

-- Setup problems of a repository ("Needs attention"), one per kind.
CREATE TABLE repo_attention (
    instance   text        NOT NULL,
    project    text        NOT NULL,
    kind       text        NOT NULL,
    message    text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX repo_attention_key ON repo_attention (instance, lower(project), kind);

-- Dashboard sign-ins. The cookie holds the session ID; only its hash is
-- stored. token_sealed is the user's OAuth token sealed with
-- PERGENT_SH_SECRET_KEY.
CREATE TABLE sessions (
    id_hash           bytea       PRIMARY KEY,
    platform          text        NOT NULL,
    instance          text        NOT NULL,
    user_id           bigint      NOT NULL,
    username          text        NOT NULL DEFAULT '',
    is_admin          boolean     NOT NULL DEFAULT false,
    token_sealed      bytea       NOT NULL,
    csrf              text        NOT NULL,
    access_checked_at timestamptz,
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_last_seen_idx ON sessions (last_seen_at);

-- The repositories a session's user can see.
CREATE TABLE user_access (
    session_hash bytea NOT NULL REFERENCES sessions (id_hash) ON DELETE CASCADE,
    instance     text  NOT NULL,
    project      text  NOT NULL,
    PRIMARY KEY (session_hash, instance, project)
);

-- The merge request's title and web address when the run was queued.
ALTER TABLE runs
    ADD COLUMN title   text NOT NULL DEFAULT '',
    ADD COLUMN web_url text NOT NULL DEFAULT '';
