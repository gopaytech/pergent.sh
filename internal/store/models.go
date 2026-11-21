package store

import (
	"context"
	"errors"
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
// sealed with PERGENT_SH_SECRET_KEY; nil when none is set. Name is unique
// among global entries ignoring case; a row with no name (the previous
// release writes none) reads as provider/model. TeamsMayUse lets team and
// repository chains name it.
type LLMModel struct {
	ID, Name, Provider, Model, BaseURL string
	APIKeySealed                       []byte
	Default, TeamsMayUse               bool
	CreatedAt, UpdatedAt               time.Time
}

const modelColumns = `id, COALESCE(name, provider || '/' || model), provider, model, base_url, api_key_sealed, is_default,
	teams_may_use, created_at, updated_at`

func scanModel(row pgx.Row) (LLMModel, error) {
	var m LLMModel
	err := row.Scan(&m.ID, &m.Name, &m.Provider, &m.Model, &m.BaseURL, &m.APIKeySealed, &m.Default, &m.TeamsMayUse,
		&m.CreatedAt, &m.UpdatedAt)
	return m, err
}

// nameTaken reports whether a global entry other than except reads as name,
// ignoring case. An entry with no name reads as provider/model, so that is
// taken too, which the index on name alone can't see.
func nameTaken(ctx context.Context, tx pgx.Tx, name, except string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llm_models
		WHERE lower(COALESCE(name, provider || '/' || model)) = lower($1) AND id <> $2)`, name, except).Scan(&taken)
	return taken, err
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
// is none, which the first one is. A name taken is ErrNameTaken; the same
// provider, model and base URL as another entry is allowed, as names tell
// them apart. An empty name stores none, which reads as provider/model.
func (s *Store) CreateLLMModel(ctx context.Context, m LLMModel, at time.Time) (LLMModel, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LLMModel{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return LLMModel{}, err
	}
	if m.Name != "" {
		if taken, err := nameTaken(ctx, tx, m.Name, ""); err != nil || taken {
			if taken {
				err = ErrNameTaken
			}
			return LLMModel{}, err
		}
	}
	out, err := scanModel(tx.QueryRow(ctx, `INSERT INTO llm_models (id, name, provider, model, base_url, api_key_sealed, is_default,
			teams_may_use, created_at, updated_at)
		VALUES (gen_random_uuid()::text, NULLIF($1, ''), $2, $3, $4, $5, NOT EXISTS (SELECT 1 FROM llm_models WHERE is_default),
			$6, $7, $7)
		RETURNING `+modelColumns, m.Name, m.Provider, m.Model, m.BaseURL, m.APIKeySealed, m.TeamsMayUse, at))
	if err != nil {
		if isUniqueViolation(err) {
			return LLMModel{}, ErrNameTaken
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
// there is no such entry. The default entry is written to llm_settings
// again. A new key
// clears the entry's auth mark in the same transaction.
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
		return false, err
	}
	if keySealed != nil {
		if err := clearAuthMark(ctx, tx, EntryRef{SourceGlobal, id}); err != nil {
			return false, err
		}
	}
	if m.Default {
		if err := writeDefaultToSettings(ctx, tx, m, at); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// SetLLMModelMeta changes the name and the teams may use switch of an entry,
// each unless nil. found is false when there is no such entry. A name another
// entry reads as, ignoring case, is ErrNameTaken.
func (s *Store) SetLLMModelMeta(ctx context.Context, id string, name *string, teamsMayUse *bool, at time.Time) (found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return false, err
	}
	if name != nil {
		if taken, err := nameTaken(ctx, tx, *name, id); err != nil || taken {
			if taken {
				err = ErrNameTaken
			}
			return false, err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE llm_models SET name = COALESCE($2, name), teams_may_use = COALESCE($3, teams_may_use),
		updated_at = $4 WHERE id = $1`, id, name, teamsMayUse, at)
	if err != nil {
		if isUniqueViolation(err) {
			return false, ErrNameTaken
		}
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, tx.Commit(ctx)
}

// SetDefaultLLMModel makes an entry the default, in place of the one that is,
// and writes it to llm_settings. found is false, and the default stays, when
// there is no such entry. Once the global chain is saved, the chain and not
// the flag decides what reviews try first, so the entry also goes to
// position 1 of that chain: see moveToGlobalChainTop.
func (s *Store) SetDefaultLLMModel(ctx context.Context, id string, at time.Time) (found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockModelList(ctx, tx); err != nil {
		return false, err
	}
	if found, err := setDefault(ctx, tx, id, at); err != nil || !found {
		return false, err
	}
	if err := moveToGlobalChainTop(ctx, tx, EntryRef{SourceGlobal, id}, at); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// moveToGlobalChainTop puts ref at position 1 of the stored global chain, the
// other entries moved down in their order, when that chain has entries. With
// no stored chain (or an empty one) the default is the chain, so nothing is
// written. An entry that is not in a full chain is ErrChainTooLong: no entry
// is dropped to make room. tx holds the list lock, which SaveChain takes for
// the global chain too.
func moveToGlobalChainTop(ctx context.Context, tx pgx.Tx, ref EntryRef, at time.Time) error {
	chains, err := loadChains(ctx, tx, chainWhere, ScopeGlobal, int64(0), "", "")
	if err != nil || len(chains) == 0 || len(chains[0].Entries) == 0 {
		return err
	}
	c := chains[0]
	if c.Entries[0] == ref {
		return nil
	}
	entries := []EntryRef{ref}
	for _, e := range c.Entries {
		if e != ref {
			entries = append(entries, e)
		}
	}
	if len(entries) > MaxChain {
		return ErrChainTooLong
	}
	if err := writeChainEntries(ctx, tx, c.ID, entries); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE model_chains SET updated_at = $2 WHERE id = $1`, c.ID, at)
	return err
}

// setDefault is SetDefaultLLMModel inside tx, which holds the list lock.
func setDefault(ctx context.Context, tx pgx.Tx, id string, at time.Time) (found bool, err error) {
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
	return true, writeDefaultToSettings(ctx, tx, m, at)
}

// DeleteLLMModel removes an entry. The default is refused with
// ErrModelIsDefault, unless shadowed: the config file's llm: block is the
// default then and the flag is dormant, so the entry may go, and the oldest
// entry left
// takes the flag and is written to llm_settings; with none left there is no
// default. An entry chains or a repository's model_id name goes all the
// same: they keep naming it, and the resolver skips it as removed. A missing
// entry is ErrModelNotFound.
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
