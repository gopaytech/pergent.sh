-- Whether later reviews are told what people with write access dismissed, and
-- why. NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN learn_from_dismissals boolean;
ALTER TABLE repo_defaults   ADD COLUMN learn_from_dismissals boolean;
