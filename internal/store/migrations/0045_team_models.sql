-- Team models and backup chains. It only adds, except one rule it relaxes, so
-- the previous release (migrations up to 0044, the code graph, which this one
-- leaves alone) still runs on the migrated database.
--
-- team_models: a team's own entries and keys (sealed with
-- PERGENT_SH_SECRET_KEY). Kept apart from llm_models, so the previous release
-- never reads a team key as a global one. It ignores the table.
--
-- model_chains and model_chain_entries: an ordered list of up to 5 entries per
-- scope target (global, a team, a repository). An entry names a global
-- (llm_models, or 'config') or a team (team_models) entry by ID, with no
-- foreign key: a removed global entry stays and is skipped. No global chain is
-- stored here: no row means the default entry alone. The previous release
-- ignores both tables.
--
-- model_entry_marks: an entry the provider said is out of quota or credit
-- (until a time) or whose key it rejected (until the key is edited, so until
-- is NULL). The previous release ignores it.
--
-- llm_models.name is the entry's name, filled from provider/model, a later
-- copy of the same provider/model with " (2)", " (3)"... by age. It stays
-- nullable: the previous release inserts no name, and the store reads
-- provider/model for NULL. teams_may_use defaults to false. The unique rule on
-- (provider, model, base_url) goes, so one model can have several keys; the
-- previous release still inserts and updates as before.
--
-- team_repos.owner: at most one owner team per (instance, project), project ''
-- being the instance's own level. The previous release inserts no owner.
--
-- dashboard_repos.chain_mode: 'custom' for a repository with its own chain.
-- A repository with model_id set gets a custom 1-entry chain of that entry, so
-- it runs as today. The previous release still reads and writes model_id.
--
-- runs: which attempt of a review a run is (attempt_of: the first run, NULL
-- for the first), the chain entry it ran on, and the platform status it
-- shares (status_ref: a GitHub Check Run ID). run_usage.source and team_id say
-- whose key paid. The previous release ignores them; its rows read as a first
-- attempt on a global entry.
CREATE TABLE team_models (
    id             text        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    team_id        bigint      NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name           text        NOT NULL,
    provider       text        NOT NULL,
    model          text        NOT NULL,
    base_url       text        NOT NULL DEFAULT '',
    api_key_sealed bytea,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);
CREATE UNIQUE INDEX team_models_name_idx ON team_models (team_id, lower(name));

CREATE TABLE model_chains (
    id         bigserial   PRIMARY KEY,
    scope      text        NOT NULL CHECK (scope IN ('global', 'team', 'repo')),
    team_id    bigint      REFERENCES teams (id) ON DELETE CASCADE,
    instance   text        NOT NULL DEFAULT '',
    project    text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL,
    CHECK ((scope = 'global' AND team_id IS NULL AND instance = '' AND project = '')
        OR (scope = 'team' AND team_id IS NOT NULL AND instance = '' AND project = '')
        OR (scope = 'repo' AND team_id IS NULL AND instance <> '' AND project <> ''))
);
CREATE UNIQUE INDEX model_chains_key ON model_chains (scope, COALESCE(team_id, 0), instance, lower(project));

CREATE TABLE model_chain_entries (
    chain_id bigint NOT NULL REFERENCES model_chains (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 5),
    source   text   NOT NULL CHECK (source IN ('global', 'team')),
    model_id text   NOT NULL,
    PRIMARY KEY (chain_id, position)
);
CREATE INDEX model_chain_entries_model_idx ON model_chain_entries (source, model_id);

CREATE TABLE model_entry_marks (
    source   text        NOT NULL CHECK (source IN ('global', 'team')),
    model_id text        NOT NULL,
    class    text        NOT NULL CHECK (class IN ('quota', 'billing', 'auth')),
    until    timestamptz,
    set_at   timestamptz NOT NULL,
    PRIMARY KEY (source, model_id),
    CHECK ((class = 'auth') = (until IS NULL))
);

ALTER TABLE llm_models ADD COLUMN name text;
ALTER TABLE llm_models ADD COLUMN teams_may_use boolean NOT NULL DEFAULT false;
UPDATE llm_models m SET name = n.name
FROM (SELECT id, provider || '/' || model ||
        CASE WHEN row_number() OVER w > 1 THEN ' (' || row_number() OVER w || ')' ELSE '' END AS name
      FROM llm_models
      WINDOW w AS (PARTITION BY lower(provider || '/' || model) ORDER BY created_at, id)) n
WHERE m.id = n.id;
CREATE UNIQUE INDEX llm_models_name_idx ON llm_models (lower(name));
ALTER TABLE llm_models DROP CONSTRAINT llm_models_provider_model_base_url_key;

ALTER TABLE team_repos ADD COLUMN owner boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX team_repos_owner_idx ON team_repos (instance, lower(project)) WHERE owner;

ALTER TABLE dashboard_repos ADD COLUMN chain_mode text NOT NULL DEFAULT 'inherit'
    CHECK (chain_mode IN ('inherit', 'custom'));
UPDATE dashboard_repos SET chain_mode = 'custom' WHERE model_id IS NOT NULL;
INSERT INTO model_chains (scope, instance, project, updated_at)
SELECT 'repo', instance, project, updated_at FROM dashboard_repos WHERE model_id IS NOT NULL;
INSERT INTO model_chain_entries (chain_id, position, source, model_id)
SELECT c.id, 1, 'global', d.model_id
FROM model_chains c
JOIN dashboard_repos d ON d.instance = c.instance AND lower(d.project) = lower(c.project)
WHERE c.scope = 'repo' AND d.model_id IS NOT NULL;

ALTER TABLE runs ADD COLUMN attempt_of uuid;
ALTER TABLE runs ADD COLUMN chain_position integer;
ALTER TABLE runs ADD COLUMN chain_length integer;
ALTER TABLE runs ADD COLUMN model_source text;
ALTER TABLE runs ADD COLUMN model_team_id bigint;
ALTER TABLE runs ADD COLUMN model_entry_id text;
ALTER TABLE runs ADD COLUMN model_entry_name text;
ALTER TABLE runs ADD COLUMN status_ref bigint;
ALTER TABLE runs ADD COLUMN fallback_class text NOT NULL DEFAULT '';
CREATE INDEX runs_attempt_of_idx ON runs (attempt_of) WHERE attempt_of IS NOT NULL;

ALTER TABLE run_usage ADD COLUMN source text NOT NULL DEFAULT 'global' CHECK (source IN ('global', 'team'));
ALTER TABLE run_usage ADD COLUMN team_id bigint;
CREATE INDEX run_usage_source_idx ON run_usage (instance, source, reported_at);
