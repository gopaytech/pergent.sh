-- GitHub repositories: the review App's installation that reaches the
-- repository (0 on GitLab), and whether the approve side can reach it (always
-- true on GitLab, whose approve bot is a user).
ALTER TABLE projects
    ADD COLUMN installation_id bigint NOT NULL DEFAULT 0,
    ADD COLUMN approve_available boolean NOT NULL DEFAULT true;
