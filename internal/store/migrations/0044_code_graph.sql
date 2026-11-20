-- The code graph (docs/concepts/code-graph.md). graph_context is the
-- repository's Code graph switch, off by default: with it off nothing builds,
-- stores or sends a graph. Only SetDashboardRepoGraph writes it.
ALTER TABLE dashboard_repos ADD COLUMN graph_context boolean NOT NULL DEFAULT false;

-- One row per repository whose switch is on (deleted when it goes off): the
-- stored graph (its file is on the graphs volume; commit '' is none yet), when
-- the default branch head was last read, and at most one build. A build is
-- '' (none), 'queued' or 'running'; build_token_hash is the hash of its Job's
-- one-time token, NULL until the Job exists. error is the last build's
-- failure, cleared when a build stores a graph.
CREATE TABLE repo_graphs (
    instance         text        NOT NULL,
    project          text        NOT NULL,
    branch           text        NOT NULL DEFAULT '',
    commit           text        NOT NULL DEFAULT '',
    built_at         timestamptz,
    size_bytes       bigint      NOT NULL DEFAULT 0,
    nodes            integer     NOT NULL DEFAULT 0,
    edges            integer     NOT NULL DEFAULT 0,
    checked_at       timestamptz,
    build_state      text        NOT NULL DEFAULT '' CHECK (build_state IN ('', 'queued', 'running')),
    build_id         uuid,
    build_branch     text        NOT NULL DEFAULT '',
    build_commit     text        NOT NULL DEFAULT '',
    build_job        text        NOT NULL DEFAULT '',
    build_token_hash bytea,
    build_deadline   timestamptz,
    queued_at        timestamptz,
    error            text        NOT NULL DEFAULT '',
    failed_at        timestamptz
);
CREATE UNIQUE INDEX repo_graphs_repo ON repo_graphs (instance, lower(project));
CREATE UNIQUE INDEX repo_graphs_build ON repo_graphs (build_id) WHERE build_id IS NOT NULL;
