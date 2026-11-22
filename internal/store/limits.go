package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelLimit is a monthly token limit on a chain entry. TeamID 0 is the model
// limit, on every use of a global entry; a team's ID is that team's limit on
// the entry, on the repositories it owns. UpdatedBy is the actor as the audit
// log names it.
type ModelLimit struct {
	Ref           EntryRef
	TeamID        int64
	MonthlyTokens int64
	UpdatedAt     time.Time
	UpdatedBy     string
}

// ModelLimits returns every limit, by source, entry and team, the model limit
// of an entry first.
func (s *Store) ModelLimits(ctx context.Context) ([]ModelLimit, error) {
	rows, err := s.pool.Query(ctx, `SELECT source, entry_id, COALESCE(team_id, 0), monthly_tokens, updated_at, updated_by
		FROM model_limits ORDER BY source, entry_id, COALESCE(team_id, 0)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelLimit
	for rows.Next() {
		var l ModelLimit
		if err := rows.Scan(&l.Ref.Source, &l.Ref.ID, &l.TeamID, &l.MonthlyTokens, &l.UpdatedAt, &l.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetModelLimit stores l in place of the limit its entry and team had. The
// caller checks the entry and the team exist; a team that doesn't is a
// foreign key error.
func (s *Store) SetModelLimit(ctx context.Context, l ModelLimit) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO model_limits (source, entry_id, team_id, monthly_tokens, updated_at, updated_by)
		VALUES ($1, $2, NULLIF($3::bigint, 0), $4, $5, $6)
		ON CONFLICT (source, entry_id, (COALESCE(team_id, 0))) DO UPDATE SET monthly_tokens = EXCLUDED.monthly_tokens,
			updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		l.Ref.Source, l.Ref.ID, l.TeamID, l.MonthlyTokens, l.UpdatedAt, l.UpdatedBy)
	return err
}

// ClearModelLimit removes the limit of ref for team (0: the model limit). old
// is the number it had; found is false when there was none.
func (s *Store) ClearModelLimit(ctx context.Context, ref EntryRef, team int64) (old int64, found bool, err error) {
	err = s.pool.QueryRow(ctx, `DELETE FROM model_limits WHERE source = $1 AND entry_id = $2 AND COALESCE(team_id, 0) = $3
		RETURNING monthly_tokens`, ref.Source, ref.ID, team).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return old, true, nil
}
