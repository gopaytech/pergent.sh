-- Review panel: chain B. It only adds, so the previous release (migrations up
-- to 0046) still runs on the migrated database and ignores all of it.
--
-- panel_chains and panel_chain_entries: chain B, the same shape as
-- model_chains and model_chain_entries (0045, 0046), at the same 3 levels.
-- No row at a level means that level has no chain B. A stored global chain B
-- is the only global one: with none, a review has no panel.
--
-- dashboard_repos.panel_chain_mode: 'custom' for a repository with its own
-- chain B, 'off' for one that turned the panel off whatever its team or the
-- global level has, 'inherit' otherwise.
CREATE TABLE panel_chains (
    id             bigserial   PRIMARY KEY,
    scope          text        NOT NULL CHECK (scope IN ('global', 'team', 'repo')),
    team_id        bigint      REFERENCES teams (id) ON DELETE CASCADE,
    instance       text        NOT NULL DEFAULT '',
    project        text        NOT NULL DEFAULT '',
    saved_by_admin boolean     NOT NULL DEFAULT false,
    updated_at     timestamptz NOT NULL,
    CHECK ((scope = 'global' AND team_id IS NULL AND instance = '' AND project = '')
        OR (scope = 'team' AND team_id IS NOT NULL AND instance = '' AND project = '')
        OR (scope = 'repo' AND team_id IS NULL AND instance <> '' AND project <> ''))
);
CREATE UNIQUE INDEX panel_chains_key ON panel_chains (scope, COALESCE(team_id, 0), instance, lower(project));

CREATE TABLE panel_chain_entries (
    chain_id bigint NOT NULL REFERENCES panel_chains (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 5),
    source   text   NOT NULL CHECK (source IN ('global', 'team')),
    model_id text   NOT NULL,
    PRIMARY KEY (chain_id, position)
);
CREATE INDEX panel_chain_entries_model_idx ON panel_chain_entries (source, model_id);

ALTER TABLE dashboard_repos ADD COLUMN panel_chain_mode text NOT NULL DEFAULT 'inherit'
    CHECK (panel_chain_mode IN ('inherit', 'custom', 'off'));
