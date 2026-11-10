ALTER TABLE runs ADD COLUMN failure_step text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN failure_detail text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN pod_name text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN checkout_seen smallint NOT NULL DEFAULT 0;

-- What a failed review Job left: its clone log (whole, and with linked
-- repositories' URLs masked) and the pod's state and events. Redacted.
CREATE TABLE run_diagnostics (
    run_id           uuid        PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    clone_log        text        NOT NULL,
    clone_log_masked text        NOT NULL,
    pod_state        text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_diagnostics_created_idx ON run_diagnostics (created_at);
