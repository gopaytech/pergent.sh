-- The signed webhook deliveries already handled, so the same bytes don't count
-- twice. Pruned once they are too old to be accepted anyway.
CREATE TABLE webhook_deliveries (
    instance    text        NOT NULL,
    body_sha256 bytea       NOT NULL,
    received_at timestamptz NOT NULL,
    PRIMARY KEY (instance, body_sha256)
);
CREATE INDEX webhook_deliveries_received_at ON webhook_deliveries (received_at);
