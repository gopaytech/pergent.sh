-- Monthly token quota. A repository, and the instance as the default of its
-- repositories, may have a monthly token budget (NULL: none). A run the
-- dispatcher holds because the budget is used up waits in the state 'waiting'
-- with skip_reason 'quota_used_up'; runs.quota_checked is true once the quota
-- has been looked at for the run (someone asked for it, or it was started),
-- so it is never held again. merge_requests.quota_noted_at is when the note
-- about a used-up quota was posted on the change (NULL: never); it is posted
-- once a month. This only adds: the previous release ignores all of it and
-- never claims a waiting run.
ALTER TABLE runs ADD COLUMN quota_checked boolean NOT NULL DEFAULT false;
ALTER TABLE merge_requests ADD COLUMN quota_noted_at timestamptz;
ALTER TABLE dashboard_repos ADD COLUMN monthly_token_quota bigint CHECK (monthly_token_quota > 0);
ALTER TABLE instances ADD COLUMN monthly_token_quota bigint CHECK (monthly_token_quota > 0);

-- The month's sum over an instance (TokensSince) reads run_usage by instance.
CREATE INDEX run_usage_instance_idx ON run_usage (instance, reported_at);
