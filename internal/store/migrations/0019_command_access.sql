-- Whether only a change's author and the repository's maintainers and owners
-- can use pergent commands on it. NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN commands_author_and_maintainers_only boolean;
ALTER TABLE repo_defaults   ADD COLUMN commands_author_and_maintainers_only boolean;
