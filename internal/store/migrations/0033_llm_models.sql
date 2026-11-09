-- A list of LLM models, one of them the default, and a model per repository.
-- The row of llm_settings, when there is one, becomes the first entry and the
-- default. llm_settings stays and is kept in step with the default entry, so
-- the previous release still runs on the migrated database, on that default.
-- It only adds, as 0032 does.
--
-- dashboard_repos.model_id names an entry; NULL follows the default. It has
-- no foreign key: the entry of a config-file LLM is the fixed id 'config' and
-- has no row. Deleting an entry that a repository names is refused in the
-- store, with the row locked.
--
-- runs.provider and run_usage.provider are the provider a review ran on,
-- beside the model they already hold. NULL and '' are for what ran before.
CREATE TABLE llm_models (
    id             text        PRIMARY KEY,
    provider       text        NOT NULL,
    model          text        NOT NULL,
    base_url       text        NOT NULL DEFAULT '',
    api_key_sealed bytea,
    is_default     boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (provider, model, base_url)
);

-- At most one default.
CREATE UNIQUE INDEX llm_models_default_idx ON llm_models (is_default) WHERE is_default;

ALTER TABLE dashboard_repos ADD COLUMN model_id text;
ALTER TABLE runs ADD COLUMN provider text;
ALTER TABLE run_usage ADD COLUMN provider text NOT NULL DEFAULT '';

INSERT INTO llm_models (id, provider, model, base_url, api_key_sealed, is_default, created_at, updated_at)
SELECT gen_random_uuid()::text, provider, model, base_url, api_key_sealed, true, updated_at, updated_at
FROM llm_settings;
