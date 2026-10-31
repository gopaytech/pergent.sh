-- A review that was approved and is now decided as not approved (a dismissal
-- restored, the change a draft again) owes a withdrawal of the approve bot's
-- approval of that head. The flag keeps the debt until the withdrawal is
-- made, so a withdrawal that failed is retried by the approval sweep. It
-- belongs to the recorded approval: a new or cleared approval resets it.
ALTER TABLE merge_requests ADD COLUMN withdraw_pending boolean NOT NULL DEFAULT false;
