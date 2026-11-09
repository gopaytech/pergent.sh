package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfigModelID is the fixed ID of the entry a config-file llm block
// supplies. It has no row in llm_models: a repository may name it, and a save
// naming it looks for no row.
const ConfigModelID = "config"

var (
	// ErrModelNotFound is returned when a model's ID is not in the list.
	ErrModelNotFound = errors.New("store: model not in the list")
	// ErrModelIsDefault is returned when the default model is deleted.
	ErrModelIsDefault = errors.New("store: the default model cannot be deleted")
)

// modelsLockKey is the pg_advisory_xact_lock key that serialises changes to
// the list, so there is never a second default and llm_settings is written
// in the order the defaults were set.
const modelsLockKey = 0x6d6f646c // "modl"

// LLMModel is an entry of the list of models. APIKeySealed is the API key
// sealed with PERGENT_SH_SECRET_KEY; nil when none is set.
type LLMModel struct {
	ID, Provider, Model, BaseURL string
	APIKeySealed                 []byte
	Default                      bool
	CreatedAt, UpdatedAt         time.Time
}

// ModelInUseError is returned by DeleteLLMModel for a model a repository
// names.
type ModelInUseError struct{ Repos []RepoRef }

func (e *ModelInUseError) Error() string {
	return fmt.Sprintf("store: model is in use by %d repositories", len(e.Repos))
}

const modelColumns = `id, provider, model, base_url, api_key_sealed, is_default, created_at, updated_at`

func scanModel(row pgx.Row) (LLMModel, error) {
	var m LLMModel
	err := row.Scan(&m.ID, &m.Provider, &m.Model, &m.BaseURL, &m.APIKeySealed, &m.Default, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

// LLMModels returns every entry, oldest first.
func (s *Store) LLMModels(ctx context.Context) ([]LLMModel, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+modelColumns+` FROM llm_models ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMModel
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// lockModelList takes the lock that serialises changes to the list until the
// transaction ends.
func lockModelList(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, modelsLockKey)
	return err
}

// writeDefaultToSettings writes the default entry to llm_settings, which the
// previous release reads, so a rollback runs on the current default. It is
// PutLLM's statement except for the key: it is copied as it is, a missing one
// included, because keeping the row's old key would pair this entry with the
// key of another provider.
func writeDefaultToSettings(ctx context.Context, tx pgx.Tx, m LLMModel, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO llm_settings (id, provider, model, base_url, api_key_sealed, updated_at)
		VALUES (1, $1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET provider = EXCLUDED.provider, model = EXCLUDED.model,
			base_url = EXCLUDED.base_url, updated_at = EXCLUDED.updated_at,
			api_key_sealed = EXCLUDED.api_key_sealed`,
		m.Provider, m.Model, m.BaseURL, m.APIKeySealed, at)
	return err
}

// CreateLLMModel adds m (its ID, Default and times are ignored; at is both
// times) and returns it as stored. The entry becomes the default when there
// is none, which the first one is. The same provider, model and base URL as
// an entry already there is ErrExists.
func (s *Store) CreateLLMModel(ctx context.Context, m LLMModel, at time.Time) (LLMModel, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LLMModel{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return LLMModel{}, err
	}
	out, err := scanModel(tx.QueryRow(ctx, `INSERT INTO llm_models (id, provider, model, base_url, api_key_sealed, is_default, created_at, updated_at)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, NOT EXISTS (SELECT 1 FROM llm_models WHERE is_default), $5, $5)
		RETURNING `+modelColumns, m.Provider, m.Model, m.BaseURL, m.APIKeySealed, at))
	if err != nil {
		if isUniqueViolation(err) {
			return LLMModel{}, ErrExists
		}
		return LLMModel{}, err
	}
	if out.Default {
		if err := writeDefaultToSettings(ctx, tx, out, at); err != nil {
			return LLMModel{}, err
		}
	}
	return out, tx.Commit(ctx)
}

// UpdateLLMModel changes the base URL and, unless keySealed is nil, the key
// of an entry; its provider and model never change. found is false when
// there is no such entry. A base URL that makes it the same as another entry
// is ErrExists. The default entry is written to llm_settings again.
func (s *Store) UpdateLLMModel(ctx context.Context, id, baseURL string, keySealed []byte, at time.Time) (found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return false, err
	}
	m, err := scanModel(tx.QueryRow(ctx, `UPDATE llm_models SET base_url = $2, api_key_sealed = COALESCE($3, api_key_sealed), updated_at = $4
		WHERE id = $1 RETURNING `+modelColumns, id, baseURL, keySealed, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if isUniqueViolation(err) {
			return false, ErrExists
		}
		return false, err
	}
	if m.Default {
		if err := writeDefaultToSettings(ctx, tx, m, at); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// SetDefaultLLMModel makes an entry the default, in place of the one that is,
// and writes it to llm_settings. found is false, and the default stays, when
// there is no such entry.
func (s *Store) SetDefaultLLMModel(ctx context.Context, id string, at time.Time) (found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llm_models WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	// The old default is cleared first: the index allows one default at every
	// step, not only when the statement is done.
	if _, err := tx.Exec(ctx, `UPDATE llm_models SET is_default = false WHERE is_default AND id <> $1`, id); err != nil {
		return false, err
	}
	m, err := scanModel(tx.QueryRow(ctx, `UPDATE llm_models SET is_default = true, updated_at = $2
		WHERE id = $1 RETURNING `+modelColumns, id, at))
	if err != nil {
		return false, err
	}
	if err := writeDefaultToSettings(ctx, tx, m, at); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// DeleteLLMModel removes an entry. The default is refused with
// ErrModelIsDefault, unless shadowed: the config file's llm: block is the
// default then and the flag is dormant, so the entry may go once no
// repository names it (that check answers first), and the oldest entry left
// takes the flag and is written to llm_settings; with none left there is no
// default. An entry a dashboard repository names is refused with a
// *ModelInUseError listing them. The entry's row is locked first, and a save
// of a repository naming it locks the row too (lockModel), so a delete and
// such a save cannot both succeed. A missing entry is ErrModelNotFound.
func (s *Store) DeleteLLMModel(ctx context.Context, id string, shadowed bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return err
	}
	var isDefault bool
	err = tx.QueryRow(ctx, `SELECT is_default FROM llm_models WHERE id = $1 FOR UPDATE`, id).Scan(&isDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrModelNotFound
	}
	if err != nil {
		return err
	}
	if isDefault && !shadowed {
		return ErrModelIsDefault
	}
	inUse := &ModelInUseError{}
	rows, err := tx.Query(ctx, `SELECT instance, project FROM dashboard_repos WHERE model_id = $1
		ORDER BY instance, lower(project)`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r RepoRef
		if err := rows.Scan(&r.Instance, &r.Project); err != nil {
			rows.Close()
			return err
		}
		inUse.Repos = append(inUse.Repos, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(inUse.Repos) > 0 {
		return inUse
	}
	if _, err := tx.Exec(ctx, `DELETE FROM llm_models WHERE id = $1`, id); err != nil {
		return err
	}
	if isDefault {
		// The row is gone, so the index allows a default again.
		next, err := scanModel(tx.QueryRow(ctx, `UPDATE llm_models SET is_default = true
			WHERE id = (SELECT id FROM llm_models ORDER BY created_at, id LIMIT 1)
			RETURNING `+modelColumns))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := writeDefaultToSettings(ctx, tx, next, next.UpdatedAt); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// lockModel locks the entry a save names FOR KEY SHARE, which holds off a
// concurrent delete until the transaction ends, and returns ErrModelNotFound
// when it is not in the list. No model, and ConfigModelID, which has no row,
// need no lock.
func lockModel(ctx context.Context, tx pgx.Tx, id *string) error {
	if id == nil || *id == ConfigModelID {
		return nil
	}
	var found string
	err := tx.QueryRow(ctx, `SELECT id FROM llm_models WHERE id = $1 FOR KEY SHARE`, *id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrModelNotFound
	}
	return err
}
