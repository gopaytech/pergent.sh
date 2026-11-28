-- Context window and max output on each model entry, in tokens. They tell
-- the review how much it may send and how long a reply may be. It only
-- adds, so the previous release runs on the migrated database: it ignores
-- both columns, and its inserts leave them NULL (not set).
--
-- An entry sets both values or neither: the API and the config loader
-- enforce that. The checks here hold only the ranges, and a NULL passes
-- each of them. When both are set, the max output must fit in the context
-- window.
ALTER TABLE llm_models ADD COLUMN context_window integer CHECK (context_window BETWEEN 4096 AND 10000000);
ALTER TABLE llm_models ADD COLUMN max_output integer CHECK (max_output BETWEEN 1024 AND 10000000);
ALTER TABLE llm_models ADD CONSTRAINT llm_models_max_output_fits CHECK (max_output <= context_window);

ALTER TABLE team_models ADD COLUMN context_window integer CHECK (context_window BETWEEN 4096 AND 10000000);
ALTER TABLE team_models ADD COLUMN max_output integer CHECK (max_output BETWEEN 1024 AND 10000000);
ALTER TABLE team_models ADD CONSTRAINT team_models_max_output_fits CHECK (max_output <= context_window);
