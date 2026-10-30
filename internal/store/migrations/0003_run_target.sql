-- A run reviews one head against one target branch: retargeting a merge
-- request is a new run, even for the same head.
DROP INDEX runs_live_head_idx;
CREATE UNIQUE INDEX runs_live_idx
    ON runs (instance, project_id, mr_iid, head_sha, target_branch)
    WHERE state <> 'superseded';

-- The target branch the recorded approval was made for. '' for approvals
-- recorded before this migration whose run is gone.
ALTER TABLE merge_requests ADD COLUMN approved_target text NOT NULL DEFAULT '';

UPDATE merge_requests m SET approved_target = COALESCE((
    SELECT r.target_branch FROM runs r
    WHERE r.instance = m.instance AND r.project_id = m.project_id AND r.mr_iid = m.mr_iid
      AND r.head_sha = m.approved_sha
    ORDER BY r.created_at DESC, r.id DESC LIMIT 1), '')
WHERE m.approved_sha <> '';
