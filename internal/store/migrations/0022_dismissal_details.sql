-- What a dismissal needs to teach later reviews: which finding it was, and
-- whether the dismisser may teach.
ALTER TABLE dismissals
    ADD COLUMN skill text NOT NULL DEFAULT '',
    ADD COLUMN file  text NOT NULL DEFAULT '',
    ADD COLUMN title text NOT NULL DEFAULT '',
    -- Whether the dismisser had write access or better, so it teaches later reviews.
    ADD COLUMN teaches boolean NOT NULL DEFAULT false,
    -- The platform user ID of the dismisser; 0 when unknown.
    ADD COLUMN dismisser_platform_id bigint NOT NULL DEFAULT 0;
-- A blocking dismissal always came from someone with maintainer access.
UPDATE dismissals SET teaches = true WHERE severity = 'blocking';
CREATE INDEX dismissals_teaching_idx ON dismissals (instance, project_id, created_at) WHERE teaches;
