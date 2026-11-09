-- The text an admin sets for the footer of the review comment: one row at
-- most, none meaning the built-in text. This only adds: the previous release
-- still runs on the migrated database and ignores the table.
CREATE TABLE comment_settings (
    id          integer     PRIMARY KEY CHECK (id = 1),
    footer_note text        NOT NULL,
    updated_at  timestamptz NOT NULL
);
