-- Instances added in the browser, the LLM provider and the defaults of
-- repositories switched on in the browser. Instances in the config file
-- have no row here.

CREATE TABLE instances (
    name            text        PRIMARY KEY,
    platform        text        NOT NULL CHECK (platform IN ('gitlab', 'github')),
    url             text        NOT NULL DEFAULT '',
    api_url         text        NOT NULL DEFAULT '',
    review_app_id   bigint      NOT NULL DEFAULT 0,
    approve_app_id  bigint      NOT NULL DEFAULT 0,
    oauth_client_id text        NOT NULL DEFAULT '',
    -- removing is set while the instance's repositories are switched off
    -- and its data is deleted; the row goes last.
    removing        boolean     NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL
);

-- The instance's secrets (review_token, approve_token, webhook_secret,
-- private keys, OAuth client secret ...), each sealed with
-- PERGENT_SH_SECRET_KEY. field names which one.
CREATE TABLE instance_secrets (
    instance   text        NOT NULL REFERENCES instances (name) ON DELETE CASCADE,
    field      text        NOT NULL,
    sealed     bytea       NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (instance, field)
);

-- The LLM provider set in the browser: one row at most.
CREATE TABLE llm_settings (
    id             integer     PRIMARY KEY CHECK (id = 1),
    provider       text        NOT NULL,
    model          text        NOT NULL,
    base_url       text        NOT NULL DEFAULT '',
    api_key_sealed bytea,
    updated_at     timestamptz NOT NULL
);

-- What a repository switched on in the browser uses where it sets nothing
-- itself: one row at most; NULL is the built-in default.
CREATE TABLE repo_defaults (
    id                  integer     PRIMARY KEY CHECK (id = 1),
    approve             boolean,
    review_drafts       boolean,
    failure_status      text        CHECK (failure_status IN ('failed', 'neutral')),
    job_timeout_seconds integer     CHECK (job_timeout_seconds > 0),
    job_memory          text,
    updated_at          timestamptz NOT NULL
);
