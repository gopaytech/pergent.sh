-- Merge request commands: re-run triggers, dismissed findings, comment
-- commands waiting to be answered, and the re-run caps.

-- How a run came to be: a push or an opened merge request ('push'), a
-- re-run of the whole review ('rerun') or of its failed skills only
-- ('rerun_failed'). skill_subset names the skills a run reviews (NULL: all);
-- base_run_id is the run whose results a partial re-run keeps.
ALTER TABLE runs
    ADD COLUMN trigger      text   NOT NULL DEFAULT 'push' CHECK (trigger IN ('push', 'rerun', 'rerun_failed')),
    ADD COLUMN skill_subset text[],
    ADD COLUMN base_run_id  uuid   REFERENCES runs (id) ON DELETE SET NULL;

-- Re-runs are counted per merge request and per repository and day.
CREATE INDEX runs_rerun_idx ON runs (instance, project_id, mr_iid, created_at) WHERE trigger <> 'push';

-- A finding dismissed on a merge request, by fingerprint, with the severity
-- it was dismissed at.
CREATE TABLE dismissals (
    instance    text        NOT NULL,
    project_id  bigint      NOT NULL,
    mr_iid      bigint      NOT NULL,
    fingerprint text        NOT NULL,
    severity    text        NOT NULL CHECK (severity IN ('blocking', 'suggestion')),
    reason      text        NOT NULL DEFAULT '',
    actor       text        NOT NULL DEFAULT '',
    actor_id    bigint,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance, project_id, mr_iid, fingerprint)
);

-- Commands found in comments, kept until the engine has answered them. A
-- redelivered webhook finds its comment already stored.
CREATE TABLE commands (
    id           bigserial   PRIMARY KEY,
    instance     text        NOT NULL,
    project_id   bigint      NOT NULL,
    mr_iid       bigint      NOT NULL,
    project_path text        NOT NULL,
    platform     text        NOT NULL,
    kind         text        NOT NULL CHECK (kind IN ('review', 'review_failed', 'dismiss', 'help', 'unknown')),
    arg          text        NOT NULL DEFAULT '',
    reason       text        NOT NULL DEFAULT '',
    comment_id   bigint      NOT NULL,
    author       text        NOT NULL DEFAULT '',
    author_id    bigint      NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    claimed_at   timestamptz,
    finished_at  timestamptz,
    outcome      text        NOT NULL DEFAULT '',
    UNIQUE (instance, project_id, mr_iid, comment_id)
);
CREATE INDEX commands_unclaimed_idx ON commands (id) WHERE claimed_at IS NULL;
CREATE INDEX commands_recent_idx ON commands (instance, project_id, mr_iid, kind, created_at);

-- Re-run caps per day; NULL is the built-in default.
ALTER TABLE dashboard_repos
    ADD COLUMN rerun_limit_per_mr   integer CHECK (rerun_limit_per_mr > 0),
    ADD COLUMN rerun_limit_per_repo integer CHECK (rerun_limit_per_repo > 0);
ALTER TABLE repo_defaults
    ADD COLUMN rerun_limit_per_mr   integer CHECK (rerun_limit_per_mr > 0),
    ADD COLUMN rerun_limit_per_repo integer CHECK (rerun_limit_per_repo > 0);
