package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// APIDismissal is a finding of an API client's subject that a person set
// aside, by fingerprint. Severity is "blocking" or "suggestion". Skill, File
// and Title say which finding it was. Actor is who did it: "key <name>" or an
// admin's username. A zero CreatedAt is the database's now.
type APIDismissal struct {
	ClientID                               int64
	Tenant, Subject, Fingerprint, Severity string
	Skill, File, Title, Reason, Actor      string
	CreatedAt                              time.Time
}

// APIReviewEvent is a dismissal or a restore made on an API review.
type APIReviewEvent struct {
	At                                      time.Time
	Kind, Fingerprint, Title, Actor, Reason string
}

// The kinds of an APIReviewEvent.
const (
	APIEventDismissed   = "dismissed"
	APIEventUndismissed = "undismissed"
)

// DismissAPIFinding stores d for its client, tenant and subject and, when it
// is new, a "dismissed" event on the review runID, in one transaction. A
// finding that is dismissed already is left as it is, with no event: created
// is false.
func (s *Store) DismissAPIFinding(ctx context.Context, d APIDismissal, runID string) (created bool, err error) {
	if !validUUID(runID) {
		return false, fmt.Errorf("store: %q is not a run ID", runID)
	}
	var at *time.Time
	if !d.CreatedAt.IsZero() {
		at = &d.CreatedAt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `INSERT INTO api_dismissals (client_id, tenant, subject, fingerprint, severity,
			skill, file, title, reason, actor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, now()))
		ON CONFLICT DO NOTHING`,
		d.ClientID, d.Tenant, d.Subject, d.Fingerprint, d.Severity, d.Skill, d.File, d.Title, d.Reason, d.Actor, at)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_review_events (run_id, kind, fingerprint, title, actor, reason)
		VALUES ($1::uuid, $2, $3, $4, $5, $6)`,
		runID, APIEventDismissed, d.Fingerprint, d.Title, d.Actor, d.Reason); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// UndismissAPIFinding removes the dismissal of fingerprint for the client's
// tenant and subject and, when there was one, records an "undismissed" event
// on the review runID, in one transaction. It tells whether there was one.
func (s *Store) UndismissAPIFinding(ctx context.Context, clientID int64, tenant, subject, fingerprint, runID, actor string) (bool, error) {
	if !validUUID(runID) {
		return false, fmt.Errorf("store: %q is not a run ID", runID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var title string
	err = tx.QueryRow(ctx, `DELETE FROM api_dismissals
		WHERE client_id = $1 AND tenant = $2 AND subject = $3 AND fingerprint = $4 RETURNING title`,
		clientID, tenant, subject, fingerprint).Scan(&title)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_review_events (run_id, kind, fingerprint, title, actor)
		VALUES ($1::uuid, $2, $3, $4, $5)`, runID, APIEventUndismissed, fingerprint, title, actor); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// APIDismissals returns the dismissals of the client's tenant and subject,
// oldest first.
func (s *Store) APIDismissals(ctx context.Context, clientID int64, tenant, subject string) ([]APIDismissal, error) {
	rows, err := s.pool.Query(ctx, `SELECT client_id, tenant, subject, fingerprint, severity, skill, file, title,
			reason, actor, created_at
		FROM api_dismissals WHERE client_id = $1 AND tenant = $2 AND subject = $3
		ORDER BY created_at, fingerprint`, clientID, tenant, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIDismissal
	for rows.Next() {
		var d APIDismissal
		if err := rows.Scan(&d.ClientID, &d.Tenant, &d.Subject, &d.Fingerprint, &d.Severity, &d.Skill, &d.File,
			&d.Title, &d.Reason, &d.Actor, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// APIReviewEvents returns the dismissals and restores made on the review
// runID, oldest first. An unknown or malformed ID has none.
func (s *Store) APIReviewEvents(ctx context.Context, runID string) ([]APIReviewEvent, error) {
	if !validUUID(runID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT at, kind, fingerprint, title, actor, reason
		FROM api_review_events WHERE run_id = $1::uuid ORDER BY at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIReviewEvent
	for rows.Next() {
		var e APIReviewEvent
		if err := rows.Scan(&e.At, &e.Kind, &e.Fingerprint, &e.Title, &e.Actor, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
