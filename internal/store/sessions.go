package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session is a dashboard sign-in, a row of sessions. IDHash is the SHA-256
// of the session ID in the browser's cookie; the ID itself is never stored.
// AccountID is the account that signed in. For a platform account,
// TokenSealed is its OAuth token, sealed by the caller; a password session
// has no token and an empty Platform and Instance and a zero UserID.
type Session struct {
	IDHash             []byte
	AccountID          int64
	Platform, Instance string
	UserID             int64
	Username           string
	IsAdmin            bool
	// AccessScope is what the last access check allowed the session.
	AccessScope string
	TokenSealed []byte
	CSRF        string
	// AccessCheckedAt is when the user's repositories and admin flag were
	// last checked with the platform; nil until the first check.
	AccessCheckedAt       *time.Time
	LastSeenAt, CreatedAt time.Time
}

// CreateSession stores a new session with its LastSeenAt and CreatedAt. Its
// account must exist. IsAdmin, AccessScope and AccessCheckedAt are not
// stored: SetSessionAccess sets them.
func (s *Store) CreateSession(ctx context.Context, se Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (id_hash, account_id, platform, instance, user_id, username,
			token_sealed, csrf, last_seen_at, created_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5::bigint, 0), $6, $7, $8, $9, $10)`,
		se.IDHash, se.AccountID, se.Platform, se.Instance, se.UserID, se.Username, se.TokenSealed, se.CSRF,
		se.LastSeenAt, se.CreatedAt)
	return err
}

// Session returns the session whose ID hashes to idHash; found is false when
// there is none.
func (s *Store) Session(ctx context.Context, idHash []byte) (Session, bool, error) {
	var se Session
	err := s.pool.QueryRow(ctx, `SELECT id_hash, account_id, COALESCE(platform, ''), COALESCE(instance, ''),
			COALESCE(user_id, 0), username, is_admin, access_scope,
			token_sealed, csrf, access_checked_at, last_seen_at, created_at
		FROM sessions WHERE id_hash = $1`, idHash).Scan(
		&se.IDHash, &se.AccountID, &se.Platform, &se.Instance, &se.UserID, &se.Username, &se.IsAdmin, &se.AccessScope,
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

// VisibleProject is a repository a session's user can see: its instance,
// its path and its platform ID (the GitLab project ID or GitHub repository
// ID, as runs.project_id holds it), which tells a project apart from a later
// one at the same path. A session's list is always of its own instance:
// SetSessionAccess and AddSessionProject store the session's instance, not
// the Instance given.
type VisibleProject struct {
	Instance, Path string
	ID             int64
}

// SetSessionAccess records a successful access check: the user's current
// username, the repositories the user can see on the session's instance
// (replacing the earlier list; duplicates that differ only in case are one,
// the first kept), the admin flag, the access scope and the time of the
// check. Every project needs an ID above 0. A session that is gone is left
// gone, and one with no instance (a password session) keeps no list.
func (s *Store) SetSessionAccess(ctx context.Context, idHash []byte, username string, isAdmin bool, scope string, projects []VisibleProject, checkedAt time.Time) error {
	for _, p := range projects {
		if p.ID <= 0 {
			return fmt.Errorf("store: project %s has no ID", p.Path)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var instance *string
	err = tx.QueryRow(ctx, `UPDATE sessions SET username = $2, is_admin = $3, access_scope = $5, access_checked_at = $4
		WHERE id_hash = $1 RETURNING instance`, idHash, username, isAdmin, checkedAt, scope).Scan(&instance)
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
	paths := make([]string, 0, len(projects))
	ids := make([]int64, 0, len(projects))
	for _, p := range projects {
		if key := strings.ToLower(p.Path); !seen[key] {
			seen[key] = true
			paths, ids = append(paths, p.Path), append(ids, p.ID)
		}
	}
	if len(paths) > 0 && instance != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project, project_id)
			SELECT $1, $2, p, id FROM unnest($3::text[], $4::bigint[]) AS u(p, id)`, idHash, instance, paths, ids); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AddSessionProject adds project to the repositories the session's user can
// see, on the session's instance, unless its path is there already
// (ignoring case). The time of the last check stays. A session that is gone,
// or has no instance, is left alone.
func (s *Store) AddSessionProject(ctx context.Context, idHash []byte, project VisibleProject) error {
	if project.ID <= 0 {
		return fmt.Errorf("store: project %s has no ID", project.Path)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project, project_id)
		SELECT id_hash, instance, $2, $3 FROM sessions s WHERE id_hash = $1 AND instance IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM user_access a
			WHERE a.session_hash = s.id_hash AND a.instance = s.instance AND lower(a.project) = lower($2))
		ON CONFLICT DO NOTHING`, idHash, project.Path, project.ID)
	return err
}

// ClearSessionAccess takes every repository, the admin flag and the access
// scope away from a session, keeping the time of its last successful check.
func (s *Store) ClearSessionAccess(ctx context.Context, idHash []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `UPDATE sessions SET is_admin = false, access_scope = '' WHERE id_hash = $1`, idHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_access WHERE session_hash = $1`, idHash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SessionProjects returns the repositories the session's user can see, as
// of the last check, ordered by path ignoring case.
func (s *Store) SessionProjects(ctx context.Context, idHash []byte) ([]VisibleProject, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance, project, project_id FROM user_access WHERE session_hash = $1
		ORDER BY lower(project)`, idHash)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[VisibleProject])
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
