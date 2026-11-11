-- API reviews. An API review is a run of kind 'api' with no merge request:
-- instance '', project_id 0, and mr_iid a number of its own, so the live-run
-- index never joins two of them. api_reviews holds what the caller sent;
-- inputs_sealed (the fast-import stream and the description) is cleared
-- once the Job has it. Only adds: the previous release reads neither column
-- and, finding no repository '', supersedes a queued API review.
ALTER TABLE runs ADD COLUMN kind text NOT NULL DEFAULT 'mr' CHECK (kind IN ('mr', 'api'));
ALTER TABLE runs ADD COLUMN api_client_id bigint REFERENCES api_clients (id) ON DELETE CASCADE;
ALTER TABLE runs ADD CONSTRAINT runs_api_shape
    CHECK (kind = 'mr' OR (instance = '' AND project_id = 0 AND api_client_id IS NOT NULL));
CREATE INDEX runs_api_client_idx ON runs (api_client_id, state) WHERE api_client_id IS NOT NULL;
CREATE SEQUENCE api_review_numbers;

CREATE TABLE api_reviews (
    run_id          uuid        PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    client_id       bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    tenant          text        NOT NULL DEFAULT '',
    subject         text        NOT NULL,
    title           text        NOT NULL DEFAULT '',
    key_name        text        NOT NULL DEFAULT '',
    idempotency_key text,
    request_hash    bytea,
    inputs_sealed   bytea,
    file_count      integer     NOT NULL,
    file_bytes      integer     NOT NULL,
    response        jsonb,
    finished_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_reviews_subject_idx ON api_reviews (client_id, tenant, subject, created_at);
CREATE INDEX api_reviews_client_idx ON api_reviews (client_id, created_at);
CREATE UNIQUE INDEX api_reviews_idempotency_idx ON api_reviews (client_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
