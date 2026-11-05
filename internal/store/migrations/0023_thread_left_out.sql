-- Whether a thread was told that its finding was left out of a review because
-- of a past dismissal (it is told once, and stays open).
ALTER TABLE inline_threads ADD COLUMN left_out_noted boolean NOT NULL DEFAULT false;
