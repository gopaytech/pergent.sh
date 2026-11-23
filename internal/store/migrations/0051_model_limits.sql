-- Model limits. It only adds, so the previous release still runs on the
-- migrated database and ignores all of it.
--
-- model_limits: a monthly token limit on a chain entry, named as chain
-- entries name it (source and entry_id, no foreign key: a removed entry
-- leaves its rows unused). team_id NULL is the model limit: every token on a
-- global entry, on every instance and repository. A team ID is that team's
-- limit on the entry: the tokens of the repositories it owns. A team entry
-- has no model limit (its team's limit covers every use of it), so only a
-- global row may have no team. Removing a team removes its rows.
-- One row per entry and team: NULLS NOT DISTINCT needs Postgres 15 and
-- pergent.sh supports 13, so the key is on COALESCE(team_id, 0), as
-- model_chains_key is (team IDs start at 1).
--
-- run_usage.owner_team_id and run_usage_models.owner_team_id: the owner team
-- of the run's repository when the row was written, NULL for none. A team
-- limit counts these, so a repository that moves to another team leaves its
-- earlier tokens with the old one. The previous release writes NULL.
--
-- run_usage_models (run_id): the limit sums leave out the run_usage rows of a
-- run that has rows per model.
CREATE TABLE model_limits (
    source         text        NOT NULL CHECK (source IN ('global', 'team')),
    entry_id       text        NOT NULL,
    team_id        bigint      REFERENCES teams (id) ON DELETE CASCADE,
    monthly_tokens bigint      NOT NULL CHECK (monthly_tokens BETWEEN 1 AND 1000000000000000),
    updated_at     timestamptz NOT NULL,
    updated_by     text        NOT NULL,
    CHECK (source = 'global' OR team_id IS NOT NULL)
);
CREATE UNIQUE INDEX model_limits_key ON model_limits (source, entry_id, COALESCE(team_id, 0));

ALTER TABLE run_usage ADD COLUMN owner_team_id bigint;
ALTER TABLE run_usage_models ADD COLUMN owner_team_id bigint;
CREATE INDEX run_usage_models_run_idx ON run_usage_models (run_id);
