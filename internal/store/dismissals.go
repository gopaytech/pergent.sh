package store

import (
	"context"
	"time"
)

// Dismissal is a finding of a merge request dismissed by a person, by
// fingerprint, with the severity ("blocking" or "suggestion") it was
// dismissed at. Actor is the label shown for who dismissed it and ActorID the
// dashboard account when it was a dashboard button (nil for a comment).
type Dismissal struct {
	MR                    MRKey
	Fingerprint, Severity string
	Reason, Actor         string
	ActorID               *int64
	CreatedAt             time.Time
}

// Dismiss stores d. A finding dismissed before at a lower severity (a
// suggestion dismissed, now dismissed as blocking) is raised, d replacing the
// reason and actor; one dismissed at the same or a higher severity is left
// as it is. created tells whether anything was stored. A zero CreatedAt is
// the database's now.
func (s *Store) Dismiss(ctx context.Context, d Dismissal) (created bool, err error) {
	var at *time.Time
	if !d.CreatedAt.IsZero() {
		at = &d.CreatedAt
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO dismissals (instance, project_id, mr_iid, fingerprint, severity,
			reason, actor, actor_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()))
		ON CONFLICT (instance, project_id, mr_iid, fingerprint) DO UPDATE SET
			severity = EXCLUDED.severity, reason = EXCLUDED.reason, actor = EXCLUDED.actor,
			actor_id = EXCLUDED.actor_id, created_at = EXCLUDED.created_at
		WHERE dismissals.severity = 'suggestion' AND EXCLUDED.severity = 'blocking'`,
		d.MR.Instance, d.MR.ProjectID, d.MR.IID, d.Fingerprint, d.Severity, d.Reason, d.Actor, d.ActorID, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Restore removes the dismissal of a finding and tells whether there was one.
func (s *Store) Restore(ctx context.Context, mr MRKey, fingerprint string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dismissals
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND fingerprint = $4`,
		mr.Instance, mr.ProjectID, mr.IID, fingerprint)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Dismissals returns the dismissals of a merge request, oldest first.
func (s *Store) Dismissals(ctx context.Context, mr MRKey) ([]Dismissal, error) {
	rows, err := s.pool.Query(ctx, `SELECT fingerprint, severity, reason, actor, actor_id, created_at FROM dismissals
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3
		ORDER BY created_at, fingerprint`, mr.Instance, mr.ProjectID, mr.IID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dismissal
	for rows.Next() {
		d := Dismissal{MR: mr}
		if err := rows.Scan(&d.Fingerprint, &d.Severity, &d.Reason, &d.Actor, &d.ActorID, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
