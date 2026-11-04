-- Inline findings (pergent.sh 0.6.0). inline_threads maps a finding of a
-- merge request to the platform thread pergent.sh posted for it: the GitHub
-- review thread node ID and its first comment's ID, or the GitLab discussion
-- ID and its first note's ID. state is open, or how pergent.sh resolved it:
-- fixed (the finding is gone), gone (its file left the change) or dismissed.
-- A thread a person resolved by hand stays open here; the platform says it
-- is resolved.
CREATE TABLE inline_threads (
    instance    text    NOT NULL,
    project_id  bigint  NOT NULL,
    mr_iid      bigint  NOT NULL,
    fingerprint text    NOT NULL,
    skill       text    NOT NULL DEFAULT '',
    file        text    NOT NULL,
    line        integer NOT NULL,
    end_line    integer NOT NULL DEFAULT 0,
    commit_sha  text    NOT NULL,
    thread_id   text    NOT NULL,
    comment_id  bigint  NOT NULL,
    state       text    NOT NULL CHECK (state IN ('open', 'fixed', 'gone', 'dismissed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance, project_id, mr_iid, fingerprint),
    UNIQUE (instance, project_id, mr_iid, thread_id)
);
-- The per-repository switch: NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN inline_findings boolean;
ALTER TABLE repo_defaults   ADD COLUMN inline_findings boolean;
-- Whether the review App may post inline threads in a GitHub repository
-- (Pull requests: Read and write). Always true on GitLab.
ALTER TABLE projects ADD COLUMN inline_available boolean NOT NULL DEFAULT true;
-- A command written as a reply in a thread: the GitLab discussion, or the
-- GitHub review comment it replies to. Empty and 0 for a plain comment.
ALTER TABLE commands ADD COLUMN discussion_id text   NOT NULL DEFAULT '';
ALTER TABLE commands ADD COLUMN reply_to      bigint NOT NULL DEFAULT 0;
