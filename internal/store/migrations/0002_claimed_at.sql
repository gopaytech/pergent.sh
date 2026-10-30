-- When the dispatcher last claimed a run. A running run without a Job whose
-- claim is old was stranded by a restart between the claim and the dispatch.
ALTER TABLE runs ADD COLUMN claimed_at timestamptz;
