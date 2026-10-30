package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session is a dashboard sign-in, a row of sessions. IDHash is the SHA-256
// of the session ID in the browser's cookie; the ID itself is never stored.
// TokenSealed is the user's OAuth token, sealed by the caller.
type Session struct {
	IDHash             []byte
	Platform, Instance string
	UserID             int64
	Username           string
	IsAdmin            bool
	TokenSealed        []byte
	CSRF               string
	// AccessCheckedAt is when the user's repositories and admin flag were
	// last checked with the platform; nil until the first check.
	AccessCheckedAt       *time.Time
	LastSeenAt, CreatedAt time.Time
}

// CreateSession stores a new session with its LastSeenAt and CreatedAt.
// IsAdmin and AccessCheckedAt are not stored: SetSessionAccess sets them.
func (s *Store) CreateSession(ctx context.Context, se Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (id_hash, platform, instance, user_id, username,
			token_sealed, csrf, last_seen_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		se.IDHash, se.Platform, se.Instance, se.UserID, se.Username, se.TokenSealed, se.CSRF,
		se.LastSeenAt, se.CreatedAt)
	return err
}

// Session returns the session whose ID hashes to idHash; found is false when
// there is none.
func (s *Store) Session(ctx context.Context, idHash []byte) (Session, bool, error) {
	var se Session
	err := s.pool.QueryRow(ctx, `SELECT id_hash, platform, instance, user_id, username, is_admin,
			token_sealed, csrf, access_checked_at, last_seen_at, created_at
		FROM sessions WHERE id_hash = $1`, idHash).Scan(
		&se.IDHash, &se.Platform, &se.Instance, &se.UserID, &se.Username, &se.IsAdmin,
		&se.TokenSealed, &se.CSRF, &se.AccessCheckedAt, &se.LastSeenAt, &se.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	return se, true, nil
}

// DeleteSession removes a session and its repositories. Deleting one that
// isn't there is not an error.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, idHash)
	return err
}

// TouchSession records that the session was used at the given time.
func (s *Store) TouchSession(ctx context.Context, idHash []byte, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id_hash = $1`, idHash, at)
	return err
}

// SetSessionToken replaces the session's sealed OAuth token (a refreshed one).
func (s *Store) SetSessionToken(ctx context.Context, idHash, sealed []byte) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET token_sealed = $2 WHERE id_hash = $1`, idHash, sealed)
	return err
}

// SetSessionAccess records a successful access check: the repositories the
// user can see on the session's instance (replacing the earlier list;
// duplicates that differ only in case are one), the admin flag and the time
// of the check. A session that is gone is left gone.
func (s *Store) SetSessionAccess(ctx context.Context, idHash []byte, isAdmin bool, projects []string, checkedAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var instance string
	err = tx.QueryRow(ctx, `UPDATE sessions SET is_admin = $2, access_checked_at = $3
		WHERE id_hash = $1 RETURNING instance`, idHash, isAdmin, checkedAt).Scan(&instance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_access WHERE session_hash = $1`, idHash); err != nil {
		return err
	}
	seen := make(map[string]bool, len(projects))
	unique := make([]string, 0, len(projects))
	for _, p := range projects {
		if key := strings.ToLower(p); !seen[key] {
			seen[key] = true
			unique = append(unique, p)
		}
	}
	if len(unique) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project)
			SELECT $1, $2, unnest($3::text[])`, idHash, instance, unique); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ClearSessionAccess takes every repository and the admin flag away from a
// session, keeping the time of its last successful check.
func (s *Store) ClearSessionAccess(ctx context.Context, idHash []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `UPDATE sessions SET is_admin = false WHERE id_hash = $1`, idHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_access WHERE session_hash = $1`, idHash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SessionProjects returns the repositories the session's user can see, as
// of the last check, ordered ignoring case.
func (s *Store) SessionProjects(ctx context.Context, idHash []byte) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT project FROM user_access WHERE session_hash = $1
		ORDER BY lower(project)`, idHash)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PruneSessions deletes the sessions created before createdBefore or last
// used before idleBefore, and returns how many there were.
func (s *Store) PruneSessions(ctx context.Context, createdBefore, idleBefore time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE created_at < $1 OR last_seen_at < $2`,
		createdBefore, idleBefore)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
