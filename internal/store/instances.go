package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// InstanceRow is an instance added in the browser, a row of instances;
// instances in the config file have none. Platform is "gitlab" or "github".
// URL is a GitLab instance's address; APIURL and the App IDs are a GitHub
// instance's (ApproveAppID 0 approves with a token). An empty OAuthClientID
// means nobody signs in through this instance. Removing is set while the
// instance is being taken out.
type InstanceRow struct {
	Name, Platform string
	URL            string
	APIURL         string
	ReviewAppID    int64
	ApproveAppID   int64
	OAuthClientID  string
	Removing       bool
	CreatedAt      time.Time
}

// ErrRemoving is returned by UpdateInstance for an instance being removed.
var ErrRemoving = errors.New("the instance is being removed")

// SealedSecret is a secret sealed with PERGENT_SH_SECRET_KEY, as stored.
type SealedSecret struct {
	Value     []byte
	UpdatedAt time.Time
}

const instanceColumns = `name, platform, url, api_url, review_app_id, approve_app_id, oauth_client_id, removing, created_at`

func scanInstance(row pgx.Row) (InstanceRow, error) {
	var r InstanceRow
	err := row.Scan(&r.Name, &r.Platform, &r.URL, &r.APIURL, &r.ReviewAppID, &r.ApproveAppID, &r.OAuthClientID, &r.Removing, &r.CreatedAt)
	return r, err
}

// Instances returns every instance added in the browser, ordered by name.
func (s *Store) Instances(ctx context.Context) ([]InstanceRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+instanceColumns+` FROM instances ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (InstanceRow, error) { return scanInstance(row) })
}

// InstanceRow returns the instance called name; found is false when there is
// none.
func (s *Store) InstanceRow(ctx context.Context, name string) (InstanceRow, bool, error) {
	r, err := scanInstance(s.pool.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceRow{}, false, nil
	}
	if err != nil {
		return InstanceRow{}, false, err
	}
	return r, true, nil
}

// putSecrets stores the sealed secrets of instance, replacing the fields
// already there and leaving the others.
func putSecrets(ctx context.Context, tx pgx.Tx, instance string, secrets map[string][]byte, at time.Time) error {
	for field, sealed := range secrets {
		if _, err := tx.Exec(ctx, `INSERT INTO instance_secrets (instance, field, sealed, updated_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (instance, field) DO UPDATE SET sealed = EXCLUDED.sealed, updated_at = EXCLUDED.updated_at`,
			instance, field, sealed, at); err != nil {
			return err
		}
	}
	return nil
}

// CreateInstance stores row (its CreatedAt and Removing are ignored: the row
// is created at at, not removing) with its sealed secrets, keyed by field,
// in one transaction. A name already taken is ErrExists.
func (s *Store) CreateInstance(ctx context.Context, row InstanceRow, secrets map[string][]byte, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	_, err = tx.Exec(ctx, `INSERT INTO instances (name, platform, url, api_url, review_app_id, approve_app_id, oauth_client_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.Name, row.Platform, row.URL, row.APIURL, row.ReviewAppID, row.ApproveAppID, row.OAuthClientID, at)
	if isUniqueViolation(err) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	if err := putSecrets(ctx, tx, row.Name, secrets, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateInstance sets the OAuth client ID (nil keeps it) and replaces the
// sealed secrets whose field is a key of secrets; every other secret stays.
// found is false when there is no such instance; an instance being removed
// is ErrRemoving, and nothing changes.
func (s *Store) UpdateInstance(ctx context.Context, name string, oauthClientID *string, secrets map[string][]byte, at time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `UPDATE instances SET oauth_client_id = COALESCE($2, oauth_client_id)
		WHERE name = $1 AND NOT removing`, name, oauthClientID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var removing bool
		err := tx.QueryRow(ctx, `SELECT removing FROM instances WHERE name = $1`, name).Scan(&removing)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return false, ErrRemoving
	}
	if err := putSecrets(ctx, tx, name, secrets, at); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// InstanceSecrets returns the sealed secrets of instance by field; empty
// when it has none or doesn't exist.
func (s *Store) InstanceSecrets(ctx context.Context, name string) (map[string]SealedSecret, error) {
	rows, err := s.pool.Query(ctx, `SELECT field, sealed, updated_at FROM instance_secrets WHERE instance = $1`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SealedSecret{}
	for rows.Next() {
		var field string
		var sec SealedSecret
		if err := rows.Scan(&field, &sec.Value, &sec.UpdatedAt); err != nil {
			return nil, err
		}
		out[field] = sec
	}
	return out, rows.Err()
}

// MarkInstanceRemoving flags the instance as being removed; found is false
// when there is no such instance.
func (s *Store) MarkInstanceRemoving(ctx context.Context, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE instances SET removing = true WHERE name = $1`, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteInstanceData deletes the instance's platform accounts (and with
// them their sessions and team memberships), its team repositories, its
// dashboard repositories and its attention rows. The instance row and its
// secrets stay: it is still Removing. Accounts and rows of other instances
// are untouched.
func (s *Store) DeleteInstanceData(ctx context.Context, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	for _, q := range []string{
		`DELETE FROM accounts WHERE kind = 'platform' AND instance = $1`,
		`DELETE FROM team_repos WHERE instance = $1`,
		`DELETE FROM dashboard_repos WHERE instance = $1`,
		`DELETE FROM repo_attention WHERE instance = $1`,
	} {
		if _, err := tx.Exec(ctx, q, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeleteInstance deletes the instance row and its secrets. An instance that
// isn't there is not an error.
func (s *Store) DeleteInstance(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM instances WHERE name = $1`, name)
	return err
}

// LLMRow is the LLM provider set in the browser. APIKeySealed is the API key
// sealed with PERGENT_SH_SECRET_KEY; nil when none is set.
type LLMRow struct {
	Provider, Model, BaseURL string
	APIKeySealed             []byte
	UpdatedAt                time.Time
}

// LLM returns the LLM row; found is false when none was ever put.
func (s *Store) LLM(ctx context.Context) (LLMRow, bool, error) {
	var r LLMRow
	err := s.pool.QueryRow(ctx, `SELECT provider, model, base_url, api_key_sealed, updated_at FROM llm_settings WHERE id = 1`).
		Scan(&r.Provider, &r.Model, &r.BaseURL, &r.APIKeySealed, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLMRow{}, false, nil
	}
	if err != nil {
		return LLMRow{}, false, err
	}
	return r, true, nil
}

// PutLLM stores the LLM row. A nil keySealed keeps the key already stored.
func (s *Store) PutLLM(ctx context.Context, provider, model, baseURL string, keySealed []byte, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO llm_settings (id, provider, model, base_url, api_key_sealed, updated_at)
		VALUES (1, $1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET provider = EXCLUDED.provider, model = EXCLUDED.model,
			base_url = EXCLUDED.base_url, updated_at = EXCLUDED.updated_at,
			api_key_sealed = COALESCE(EXCLUDED.api_key_sealed, llm_settings.api_key_sealed)`,
		provider, model, baseURL, keySealed, at)
	return err
}

// RepoDefaults are the settings a repository switched on in the browser gets
// where it sets none itself. A nil field is the built-in default; the zero
// value is "nothing set". FailureStatus is "failed" or "neutral"; JobMemory
// is a Kubernetes quantity.
type RepoDefaults struct {
	Approve, ReviewDrafts *bool
	FailureStatus         *string
	JobTimeoutSeconds     *int
	JobMemory             *string
	UpdatedAt             time.Time
}

// RepoDefaults returns the defaults; the zero value when none were put.
func (s *Store) RepoDefaults(ctx context.Context) (RepoDefaults, error) {
	var d RepoDefaults
	err := s.pool.QueryRow(ctx, `SELECT approve, review_drafts, failure_status, job_timeout_seconds, job_memory, updated_at
		FROM repo_defaults WHERE id = 1`).
		Scan(&d.Approve, &d.ReviewDrafts, &d.FailureStatus, &d.JobTimeoutSeconds, &d.JobMemory, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoDefaults{}, nil
	}
	return d, err
}

// PutRepoDefaults replaces the defaults with d, nil fields included. Its
// UpdatedAt is ignored: the database's now is stored.
func (s *Store) PutRepoDefaults(ctx context.Context, d RepoDefaults) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO repo_defaults (id, approve, review_drafts, failure_status, job_timeout_seconds, job_memory, updated_at)
		VALUES (1, $1, $2, $3, $4, $5, now())
		ON CONFLICT (id) DO UPDATE SET approve = EXCLUDED.approve, review_drafts = EXCLUDED.review_drafts,
			failure_status = EXCLUDED.failure_status, job_timeout_seconds = EXCLUDED.job_timeout_seconds,
			job_memory = EXCLUDED.job_memory, updated_at = EXCLUDED.updated_at`,
		d.Approve, d.ReviewDrafts, d.FailureStatus, d.JobTimeoutSeconds, d.JobMemory)
	return err
}
