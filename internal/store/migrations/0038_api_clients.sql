-- API clients: outside systems that ask for reviews through the review API
-- (/v1/), and their keys. Only adds: the previous release ignores both.
CREATE TABLE api_clients (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                   text        NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9-]+$' AND length(name) <= 63),
    skills                 text[]      NOT NULL DEFAULT '{}',
    model_id               text,
    callback_url           text        NOT NULL DEFAULT '',
    callback_secret_sealed bytea       NOT NULL,
    previous_secret_sealed bytea,
    previous_secret_until  timestamptz,
    max_request_bytes      integer     NOT NULL DEFAULT 524288 CHECK (max_request_bytes BETWEEN 1024 AND 819200),
    max_files              integer     NOT NULL DEFAULT 200 CHECK (max_files BETWEEN 1 AND 1000),
    max_in_flight          integer     NOT NULL DEFAULT 5 CHECK (max_in_flight BETWEEN 1 AND 50),
    requests_per_hour      integer     NOT NULL DEFAULT 600 CHECK (requests_per_hour BETWEEN 1 AND 100000),
    monthly_token_budget   bigint      NOT NULL DEFAULT 10000000 CHECK (monthly_token_budget > 0),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- A key is stored as the SHA-256 of the whole key; prefix finds the row.
CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    client_id    bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
    name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix       text        NOT NULL UNIQUE CHECK (prefix ~ '^[0-9a-f]{8}$'),
    key_hash     bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz
);
CREATE UNIQUE INDEX api_keys_name_idx ON api_keys (client_id, lower(name));
