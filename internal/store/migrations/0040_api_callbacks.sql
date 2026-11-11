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
-- (Task 18 appends api_dismissals and api_review_events to this file.)
