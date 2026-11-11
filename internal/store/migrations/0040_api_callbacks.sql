-- One callback per finished or superseded API review, and its attempts.
CREATE TABLE api_callbacks (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid        NOT NULL UNIQUE REFERENCES runs (id) ON DELETE CASCADE,
    client_id    bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    next_at      timestamptz NOT NULL DEFAULT now(),
    attempts     integer     NOT NULL DEFAULT 0,
    delivered_at timestamptz,
    gave_up_at   timestamptz
);
CREATE INDEX api_callbacks_due_idx ON api_callbacks (next_at) WHERE delivered_at IS NULL AND gave_up_at IS NULL;
CREATE TABLE api_callback_attempts (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    callback_id uuid        NOT NULL REFERENCES api_callbacks (id) ON DELETE CASCADE,
    at          timestamptz NOT NULL,
    status_code integer     NOT NULL DEFAULT 0,
    error       text        NOT NULL DEFAULT '',
    duration_ms integer     NOT NULL
);
-- Findings people set aside, by subject: a later review of the same client,
-- tenant and subject honours them.
CREATE TABLE api_dismissals (
    client_id   bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    tenant      text        NOT NULL,
    subject     text        NOT NULL,
    fingerprint text        NOT NULL,
    severity    text        NOT NULL CHECK (severity IN ('blocking', 'suggestion')),
    skill       text        NOT NULL,
    file        text        NOT NULL,
    title       text        NOT NULL,
    reason      text        NOT NULL,
    actor       text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (client_id, tenant, subject, fingerprint)
);
-- Who dismissed or restored a finding, and when, on the review it was done on.
CREATE TABLE api_review_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    at          timestamptz NOT NULL DEFAULT now(),
    kind        text        NOT NULL CHECK (kind IN ('dismissed', 'undismissed')),
    fingerprint text        NOT NULL,
    title       text        NOT NULL,
    actor       text        NOT NULL,
    reason      text        NOT NULL DEFAULT ''
);
CREATE INDEX api_review_events_run_idx ON api_review_events (run_id, at);
