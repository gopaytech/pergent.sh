-- Whether the review App may read a GitHub repository's classic branch
-- protection (Administration: Read). Without it, a classic "require
-- conversation resolution" rule is treated as off and suggestion threads may
-- block a merge. Always true on GitLab.
ALTER TABLE projects ADD COLUMN protection_readable boolean NOT NULL DEFAULT true;
