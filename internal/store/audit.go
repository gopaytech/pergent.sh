package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditEntry is one thing an admin did. ActorID is nil when the actor has no
// account (and entries outlive their actor's account); Instance and Project
// name what it was done to, when it was done to a repository. Detail never
// holds a secret.
type AuditEntry struct {
	ID                                       int64
	At                                       time.Time
	ActorID                                  *int64
	Actor, Action, Instance, Project, Detail string
}

// Audit records an entry.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_log (at, actor_id, actor, action, instance, project, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, e.At, e.ActorID, e.Actor, e.Action, e.Instance, e.Project, e.Detail)
	return err
}

// AuditLog returns up to limit entries, newest first, older than the entry
// with ID beforeID (0 starts at the newest).
func (s *Store) AuditLog(ctx context.Context, beforeID int64, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, at, actor_id, actor, action, instance, project, detail FROM audit_log
		WHERE $1 = 0 OR id < $1 ORDER BY id DESC LIMIT $2`, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AuditEntry])
}

// PruneAudit deletes the entries from before the given time and returns how
// many there were.
func (s *Store) PruneAudit(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM audit_log WHERE at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
