package store

import (
	"context"
	"time"
)

// Dismissal is a finding of a merge request dismissed by a person, by
// fingerprint, with the severity ("blocking" or "suggestion") it was
// dismissed at. Actor is the label shown for who dismissed it and ActorID the
// dashboard account when it was a dashboard button (nil for a comment).
//
// Skill, File and Title say which finding it was, so a later review can be
// told. Teaches is whether the dismisser had write access or better, which a
// dismissal needs to teach later reviews. DismisserPlatformID is the
// dismisser's user ID on the platform, 0 when unknown.
type Dismissal struct {
	MR                    MRKey
	Fingerprint, Severity string
	Reason, Actor         string
	ActorID               *int64
	CreatedAt             time.Time

	Skill, File, Title  string
	Teaches             bool
	DismisserPlatformID int64
}

// Dismiss stores d. A finding dismissed before at a lower severity (a
// suggestion dismissed, now dismissed as blocking) is raised, d replacing the
// reason, actor and details. So is a dismissal that doesn't teach when d teaches
// and has a reason, so a read-only author's dismissal can't keep a maintainer's
// from teaching; its severity never goes down. Any other earlier dismissal is
// left as it is. created tells whether anything was stored. A zero CreatedAt is
// the database's now.
func (s *Store) Dismiss(ctx context.Context, d Dismissal) (created bool, err error) {
	var at *time.Time
	if !d.CreatedAt.IsZero() {
		at = &d.CreatedAt
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO dismissals (instance, project_id, mr_iid, fingerprint, severity,
			reason, actor, actor_id, created_at, skill, file, title, teaches, dismisser_platform_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()), $10, $11, $12, $13, $14)
		ON CONFLICT (instance, project_id, mr_iid, fingerprint) DO UPDATE SET
			severity = CASE WHEN dismissals.severity = 'blocking' THEN 'blocking' ELSE EXCLUDED.severity END,
			reason = EXCLUDED.reason, actor = EXCLUDED.actor,
			actor_id = EXCLUDED.actor_id, created_at = EXCLUDED.created_at,
			skill = EXCLUDED.skill, file = EXCLUDED.file, title = EXCLUDED.title,
			teaches = EXCLUDED.teaches, dismisser_platform_id = EXCLUDED.dismisser_platform_id
		WHERE (dismissals.severity = 'suggestion' AND EXCLUDED.severity = 'blocking')
			OR (EXCLUDED.teaches AND NOT dismissals.teaches AND EXCLUDED.reason <> '')`,
		d.MR.Instance, d.MR.ProjectID, d.MR.IID, d.Fingerprint, d.Severity, d.Reason, d.Actor, d.ActorID, at,
		d.Skill, d.File, d.Title, d.Teaches, d.DismisserPlatformID)
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
	rows, err := s.pool.Query(ctx, `SELECT fingerprint, severity, reason, actor, actor_id, created_at,
			skill, file, title, teaches, dismisser_platform_id FROM dismissals
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3
		ORDER BY created_at, fingerprint`, mr.Instance, mr.ProjectID, mr.IID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dismissal
	for rows.Next() {
		d := Dismissal{MR: mr}
		if err := rows.Scan(&d.Fingerprint, &d.Severity, &d.Reason, &d.Actor, &d.ActorID, &d.CreatedAt,
			&d.Skill, &d.File, &d.Title, &d.Teaches, &d.DismisserPlatformID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// maxTeaching is how many dismissals TeachingDismissals returns at most, for
// the whole repository (not per skill).
const maxTeaching = 2000

// TeachingDismissals returns the dismissals of a repository, from all its
// changes, that may teach a review: made by someone who may teach, with a
// reason, a known skill and no earlier than since. Blocking ones come first,
// then the newest first, so the limit cuts the oldest suggestions.
func (s *Store) TeachingDismissals(ctx context.Context, instance string, projectID int64, since time.Time) ([]Dismissal, error) {
	rows, err := s.pool.Query(ctx, `SELECT mr_iid, fingerprint, severity, reason, actor, actor_id, created_at,
			skill, file, title, teaches, dismisser_platform_id FROM dismissals
		WHERE instance = $1 AND project_id = $2 AND teaches AND reason <> '' AND skill <> '' AND created_at >= $3
		ORDER BY (severity = 'blocking') DESC, created_at DESC, fingerprint
		LIMIT $4`, instance, projectID, since, maxTeaching)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dismissal
	for rows.Next() {
		d := Dismissal{MR: MRKey{Instance: instance, ProjectID: projectID}}
		if err := rows.Scan(&d.MR.IID, &d.Fingerprint, &d.Severity, &d.Reason, &d.Actor, &d.ActorID, &d.CreatedAt,
			&d.Skill, &d.File, &d.Title, &d.Teaches, &d.DismisserPlatformID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StopTeaching makes the dismissals of a finding, on every change of the
// repository, stop teaching, and tells whether any was teaching. The rows
// stay as they are, so the changes keep their dismissals.
func (s *Store) StopTeaching(ctx context.Context, instance string, projectID int64, fingerprint string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE dismissals SET teaches = false
		WHERE instance = $1 AND project_id = $2 AND fingerprint = $3 AND teaches`,
		instance, projectID, fingerprint)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// BackfillDismissals fills in skill, file and title of the dismissals made
// since that have none (made before they were stored), from the newest stored
// result of the same change that reports the same fingerprint. A dismissal
// whose finding can't be found stays empty. It tells how many it filled in.
//
// It runs at startup, not in a migration, so a stored result of an odd shape
// can't keep the server from starting: every step reads JSON that isn't the
// type it should be as nothing.
func (s *Store) BackfillDismissals(ctx context.Context, since time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `WITH found AS (
		SELECT DISTINCT ON (r.instance, r.project_id, r.mr_iid, f->>'fingerprint')
		       r.instance, r.project_id, r.mr_iid, f->>'fingerprint' AS fingerprint,
		       s->>'name' AS skill, f->>'file' AS file, f->>'title' AS title
		FROM dismissals d
		JOIN runs r USING (instance, project_id, mr_iid)
		CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(r.result->'skills') = 'array' THEN r.result->'skills' ELSE '[]' END) s
		CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(s->'findings') = 'array' THEN s->'findings' ELSE '[]' END) f
		WHERE d.skill = '' AND d.created_at >= $1 AND f->>'fingerprint' = d.fingerprint
		ORDER BY r.instance, r.project_id, r.mr_iid, f->>'fingerprint', r.created_at DESC
	)
	UPDATE dismissals d SET skill = found.skill, file = COALESCE(found.file, ''), title = COALESCE(found.title, '')
	FROM found
	WHERE d.instance = found.instance AND d.project_id = found.project_id AND d.mr_iid = found.mr_iid
	  AND d.fingerprint = found.fingerprint AND found.skill IS NOT NULL`, since)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
