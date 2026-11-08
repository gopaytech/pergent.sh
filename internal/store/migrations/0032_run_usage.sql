-- Model usage per run and skill, for the admin usage views, the CSV export,
-- the review page's token sum and the metrics. One row per skill the Job ran,
-- written when a usable result arrives and filled in from older results in
-- the background (runs.usage_recorded says which runs are done). It only
-- adds: the previous release still runs on the migrated database and ignores
-- both.
--
-- cache_read, cache_write and reasoning are NULL when the Job's pergent
-- didn't report them (before pergent's cache and reasoning counts), and 0
-- when it reported none.
CREATE TABLE run_usage (
    run_id       uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    skill        text        NOT NULL,
    instance     text        NOT NULL,
    project_id   bigint      NOT NULL,
    project_path text        NOT NULL,
    mr_iid       bigint      NOT NULL,
    trigger      text        NOT NULL,
    incremental  boolean     NOT NULL,
    model        text        NOT NULL,
    verdict      text        NOT NULL,
    reported_at  timestamptz NOT NULL,
    tokens_in    bigint      NOT NULL CHECK (tokens_in >= 0),
    tokens_out   bigint      NOT NULL CHECK (tokens_out >= 0),
    cache_read   bigint      CHECK (cache_read >= 0),
    cache_write  bigint      CHECK (cache_write >= 0),
    reasoning    bigint      CHECK (reasoning >= 0),
    findings     integer     NOT NULL,
    blocking     integer     NOT NULL,
    PRIMARY KEY (run_id, skill)
);

CREATE INDEX run_usage_reported_idx ON run_usage (reported_at);
CREATE INDEX run_usage_project_idx ON run_usage (instance, project_id, reported_at);

ALTER TABLE runs ADD COLUMN usage_recorded boolean NOT NULL DEFAULT false;
CREATE INDEX runs_usage_pending_idx ON runs (created_at) WHERE NOT usage_recorded AND result IS NOT NULL;
