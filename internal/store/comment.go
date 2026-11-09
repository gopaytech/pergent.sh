package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CommentFooterNote returns the footer text an admin set, "" when none is set.
func (s *Store) CommentFooterNote(ctx context.Context) (string, error) {
	var note string
	err := s.pool.QueryRow(ctx, `SELECT footer_note FROM comment_settings WHERE id = 1`).Scan(&note)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return note, err
}

// PutCommentFooterNote stores the footer text. An empty note removes it, so
// the built-in text is used again.
func (s *Store) PutCommentFooterNote(ctx context.Context, note string, at time.Time) error {
	if note == "" {
		_, err := s.pool.Exec(ctx, `DELETE FROM comment_settings WHERE id = 1`)
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO comment_settings (id, footer_note, updated_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET footer_note = EXCLUDED.footer_note, updated_at = EXCLUDED.updated_at`, note, at)
	return err
}
