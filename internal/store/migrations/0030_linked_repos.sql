-- Linked repositories: whether the impact check is on (NULL is the default,
-- off) and, per repository, its links as a JSON list of
-- {"target", "watched", "can_block"}. NULL is no links. The links are set by
-- admins only and belong to a repository: repo_defaults has no links.
ALTER TABLE dashboard_repos ADD COLUMN linked_repos_enabled boolean;
ALTER TABLE repo_defaults   ADD COLUMN linked_repos_enabled boolean;
ALTER TABLE dashboard_repos ADD COLUMN linked_repos jsonb;
-- The links a run used, read when it started. NULL is none: a run from before
-- this column, or one that never started.
ALTER TABLE runs ADD COLUMN linked_repos jsonb;
