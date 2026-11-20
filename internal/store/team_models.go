package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNameTaken is returned when an entry's name is already used, ignoring
// case, by another entry of the same team or by another global entry.
var ErrNameTaken = errors.New("store: an entry with this name exists")

// TeamModel is one of a team's own entries. APIKeySealed is the API key
// sealed with PERGENT_SH_SECRET_KEY; nil when none is set. It runs only for a
// repository its team owns.
type TeamModel struct {
	ID                             string
	TeamID                         int64
	Name, Provider, Model, BaseURL string
	APIKeySealed                   []byte
	CreatedAt, UpdatedAt           time.Time
}

const teamModelColumns = `id, team_id, name, provider, model, base_url, api_key_sealed, created_at, updated_at`

func scanTeamModel(row pgx.Row) (TeamModel, error) {
	var m TeamModel
	err := row.Scan(&m.ID, &m.TeamID, &m.Name, &m.Provider, &m.Model, &m.BaseURL, &m.APIKeySealed, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

func (s *Store) teamModels(ctx context.Context, where string, args ...any) ([]TeamModel, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+teamModelColumns+` FROM team_models `+where+` ORDER BY team_id, created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TeamModel, error) { return scanTeamModel(row) })
}

// TeamModels returns a team's entries, oldest first.
func (s *Store) TeamModels(ctx context.Context, teamID int64) ([]TeamModel, error) {
	return s.teamModels(ctx, `WHERE team_id = $1`, teamID)
}

// AllTeamModels returns every team's entries, by team and then oldest first.
func (s *Store) AllTeamModels(ctx context.Context) ([]TeamModel, error) {
	return s.teamModels(ctx, ``)
}

// CreateTeamModel adds m to its team (its ID and times are ignored; at is
// both times) and returns it as stored. A name the team already uses,
// ignoring case, is ErrNameTaken.
func (s *Store) CreateTeamModel(ctx context.Context, m TeamModel, at time.Time) (TeamModel, error) {
	out, err := scanTeamModel(s.pool.QueryRow(ctx, `INSERT INTO team_models (team_id, name, provider, model, base_url, api_key_sealed, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING `+teamModelColumns,
		m.TeamID, m.Name, m.Provider, m.Model, m.BaseURL, m.APIKeySealed, at))
	if isUniqueViolation(err) {
		return TeamModel{}, ErrNameTaken
	}
	return out, err
}

// UpdateTeamModel changes the name and base URL, each unless nil, and the
// key unless keySealed is nil, of an entry of teamID; its provider and model
// never change. found is false when the team has no such entry. A name taken
// is ErrNameTaken. A new key clears the entry's auth mark in the same
// transaction: the key the provider rejected is gone.
func (s *Store) UpdateTeamModel(ctx context.Context, teamID int64, id string, name, baseURL *string, keySealed []byte, at time.Time) (found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `UPDATE team_models SET name = COALESCE($3, name), base_url = COALESCE($4, base_url),
		api_key_sealed = COALESCE($5, api_key_sealed), updated_at = $6
		WHERE team_id = $1 AND id = $2`, teamID, id, name, baseURL, keySealed, at)
	if err != nil {
		if isUniqueViolation(err) {
			return false, ErrNameTaken
		}
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if keySealed != nil {
		if err := clearAuthMark(ctx, tx, EntryRef{SourceTeam, id}); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// DeleteTeamModel removes an entry of teamID, takes it out of every chain
// that names it and drops its mark, in one transaction, and returns the
// chains it changed as they are now. A team with no such entry is
// ErrModelNotFound.
func (s *Store) DeleteTeamModel(ctx context.Context, teamID int64, id string) (removedFrom []Chain, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `DELETE FROM team_models WHERE team_id = $1 AND id = $2`, teamID, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrModelNotFound
	}
	chains, err := removeFromChains(ctx, tx, SourceTeam, []string{id}, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM model_entry_marks WHERE source = $1 AND model_id = $2`, SourceTeam, id); err != nil {
		return nil, err
	}
	return chains, tx.Commit(ctx)
}

// clearAuthMark drops an entry's auth mark, leaving a quota or billing mark.
func clearAuthMark(ctx context.Context, tx pgx.Tx, ref EntryRef) error {
	_, err := tx.Exec(ctx, `DELETE FROM model_entry_marks WHERE source = $1 AND model_id = $2 AND class = 'auth'`, ref.Source, ref.ID)
	return err
}
