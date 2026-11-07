-- The shape of the review comment: a list of "block:state" entries, in order
-- (for example "findings:collapsed"). NULL is the default layout.
ALTER TABLE dashboard_repos ADD COLUMN comment_layout text[];
ALTER TABLE repo_defaults   ADD COLUMN comment_layout text[];
-- The layout a run was started with, read when it started. NULL is the
-- default layout: a run from before this column, or one that never started.
ALTER TABLE runs ADD COLUMN comment_layout text[];
