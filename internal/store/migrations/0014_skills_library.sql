-- The skill library. A skill is one markdown text, written in the browser
-- (NULL source) or installed from a git source. Every save, install and
-- update adds a row to library_skill_versions, which is append-only and has
-- no foreign key, so a version outlives a delete and a re-created name
-- continues its numbering.
CREATE TABLE library_skills (
    name            text        PRIMARY KEY CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*$'),
    version         integer     NOT NULL CHECK (version > 0),
    content         text        NOT NULL,
    updated_at      timestamptz NOT NULL,
    updated_by      text        NOT NULL DEFAULT '',
    source_kind     text        CHECK (source_kind IN ('instance', 'url')),
    source_instance text        NOT NULL DEFAULT '',
    source_repo     text        NOT NULL DEFAULT '',
    source_url      text        NOT NULL DEFAULT '',
    source_ref      text        NOT NULL DEFAULT '',
    source_path     text        NOT NULL DEFAULT '',
    source_commit   text        NOT NULL DEFAULT ''
);

CREATE TABLE library_skill_versions (
    name          text        NOT NULL,
    version       integer     NOT NULL CHECK (version > 0),
    content       text        NOT NULL,
    sha256        bytea       NOT NULL,
    source_commit text        NOT NULL DEFAULT '',
    at            timestamptz NOT NULL,
    actor         text        NOT NULL DEFAULT '',
    PRIMARY KEY (name, version)
);

-- One read of a skill source, by the platform API or by a Job: what was
-- asked, who asked, what it found. purpose says whether the admin is
-- installing or checking for updates (from_ref is the ref the installed
-- skills came from then). The row is kept a short time (expires_at), for the
-- admin to choose from. token_hash is the hash of the Job's one-time result
-- token; NULL for an API read or once spent.
CREATE TABLE skill_fetches (
    id              uuid        PRIMARY KEY,
    kind            text        NOT NULL,
    instance        text        NOT NULL DEFAULT '',
    repo            text        NOT NULL DEFAULT '',
    url             text        NOT NULL DEFAULT '',
    ref             text        NOT NULL DEFAULT '',
    from_ref        text        NOT NULL DEFAULT '',
    path            text        NOT NULL DEFAULT '',
    purpose         text        NOT NULL CHECK (purpose IN ('install', 'update')),
    requested_by    text        NOT NULL DEFAULT '',
    requested_by_id bigint,
    status          text        NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
    commit          text        NOT NULL DEFAULT '',
    error           text        NOT NULL DEFAULT '',
    files           jsonb,
    token_hash      bytea,
    created_at      timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL
);

-- The default skill list (NULL: none) and the detected repository skills a
-- repository turned on (NULL: none).
ALTER TABLE repo_defaults ADD COLUMN skills text[];
ALTER TABLE dashboard_repos ADD COLUMN repo_skills text[];

-- Which library version of each skill a run used, and why a run that has no
-- skills at all was skipped.
ALTER TABLE runs ADD COLUMN skill_versions jsonb;
ALTER TABLE runs ADD COLUMN skip_reason text NOT NULL DEFAULT '';
ALTER TABLE runs DROP CONSTRAINT runs_state_check;
ALTER TABLE runs ADD CONSTRAINT runs_state_check
    CHECK (state IN ('queued', 'running', 'reported', 'published', 'publish_failed', 'superseded', 'skipped'));
