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

// EntryTokens is what one chain entry used in the repositories of one owner
// team (OwnerTeam 0: rows with none, API reviews among them).
type EntryTokens struct {
	Ref       EntryRef
	OwnerTeam int64
	Tokens    int64
}

// entryTokensSQL sums the tokens (in plus out) reported from $1 on, per
// entry and owner team. A multi review's run has rows per model
// (run_usage_by_model, Ruling L8) and counts from those, each on its own
// entry: each reviewer attempt on its review_members entry, each judge run on
// its review_judges entry (none with --no-judge). Its run_usage rows, the
// total of all its Jobs, are left out. These rows have no owner team of
// their own: it is the one of the run_usage row they were added to (the same
// run and skill), so no migration is needed.
//
// Any other run counts its run_usage rows on the run's entry. A run with no
// entry (before chains) counts on none. A pinned Review API review counts on
// its entry, which its run records.
const entryTokensSQL = `SELECT source, entry_id, COALESCE(owner_team_id, 0), sum(tokens)::bigint FROM (
		SELECT COALESCE(r.model_source, 'global') AS source, r.model_entry_id AS entry_id, u.owner_team_id,
			u.tokens_in + u.tokens_out AS tokens
		FROM run_usage u JOIN runs r ON r.id = u.run_id
		WHERE u.reported_at >= $1 AND COALESCE(r.model_entry_id, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM run_usage_by_model b WHERE b.run_id = u.run_id)
		UNION ALL
		SELECT COALESCE(NULLIF(rm.model_source, ''), 'global'), rm.model_entry_id, u.owner_team_id, b.tokens_in + b.tokens_out
		FROM run_usage_by_model b JOIN review_members rm ON rm.id = b.member_id
			LEFT JOIN run_usage u ON u.run_id = b.run_id AND u.skill = b.skill
		WHERE b.part = 'reviewer' AND b.reported_at >= $1 AND rm.model_entry_id <> ''
		UNION ALL
		SELECT COALESCE(NULLIF(j.model_source, ''), 'global'), j.model_entry_id, u.owner_team_id, b.tokens_in + b.tokens_out
		FROM run_usage_by_model b JOIN review_judges j ON j.run_id = b.run_id AND NOT j.no_judge
			LEFT JOIN run_usage u ON u.run_id = b.run_id AND u.skill = b.skill
		WHERE b.part = 'judge' AND b.reported_at >= $1 AND j.model_entry_id <> '') x
	GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`

// EntryTokensSince returns what each entry used from since on, by reported
// time (entryTokensSQL), by source, entry and owner team.
func (s *Store) EntryTokensSince(ctx context.Context, since time.Time) ([]EntryTokens, error) {
	rows, err := s.pool.Query(ctx, entryTokensSQL, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryTokens
	for rows.Next() {
		var e EntryTokens
		if err := rows.Scan(&e.Ref.Source, &e.Ref.ID, &e.OwnerTeam, &e.Tokens); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
