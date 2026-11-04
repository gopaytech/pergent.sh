-- The step limit of a review Job (pergent's --max-turns), per repository:
-- the repository's own value, the browser default for repositories switched
-- on there, and the value each run was dispatched with. NULL is the
-- built-in default (jobs.max_turns); on a run it is one dispatched before
-- the limit was recorded. The range matches the one the config and the
-- settings accept. Nullable columns only, so an older pergent.sh keeps
-- working on a migrated database.
ALTER TABLE dashboard_repos ADD COLUMN max_turns integer CHECK (max_turns BETWEEN 5 AND 200);
ALTER TABLE repo_defaults   ADD COLUMN max_turns integer CHECK (max_turns BETWEEN 5 AND 200);
ALTER TABLE runs            ADD COLUMN max_turns integer;
