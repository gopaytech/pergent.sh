-- Path rules: the rule sets of the library, the settings that name them, and
-- the hash of the rule list each run was given. It only adds, so the previous
-- release runs on the migrated database and ignores them.
CREATE TABLE rule_sets (
    name       text        PRIMARY KEY CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*$'),
    content    text        NOT NULL,
    version    integer     NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- The default rule-set list (NULL: none) and the default built-in switch
-- (NULL: on); a repository's own list (NULL: the default list) and switch
-- (NULL: the default).
ALTER TABLE repo_defaults ADD COLUMN rule_sets text[];
ALTER TABLE repo_defaults ADD COLUMN built_in_rules boolean;
ALTER TABLE dashboard_repos ADD COLUMN rule_sets text[];
ALTER TABLE dashboard_repos ADD COLUMN built_in_rules boolean;

-- The hash of the rule list and built-in switch a run was given (NULL: a
-- run from before rules, which counts as a change).
ALTER TABLE runs ADD COLUMN rules_hash text;
