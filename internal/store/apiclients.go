package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrAPIClientExists is returned when a client's name is taken.
	ErrAPIClientExists = errors.New("store: an API client with that name exists")
	// ErrAPIKeyExists is returned when a client already has a key with that
	// name, in any case.
	ErrAPIKeyExists = errors.New("store: the client has a key with that name")
	// ErrAPIPrefixTaken is returned when another key has the prefix.
	ErrAPIPrefixTaken = errors.New("store: an API key has that prefix")
)

const (
	// HardMaxRequestBytes and HardMaxFiles are the highest limits a client may
	// be given: they keep the files' Secret under 1 MiB.
	HardMaxRequestBytes = 819200
	HardMaxFiles        = 1000
)

// APILimits are the caps on one client's requests. A zero field on create
// takes the default.
type APILimits struct {
	MaxRequestBytes, MaxFiles, MaxInFlight, RequestsPerHour int
	MonthlyTokenBudget                                      int64
}

// DefaultAPILimits are the limits of a client created without its own.
var DefaultAPILimits = APILimits{524288, 200, 5, 600, 10_000_000}

// APIClient is an outside system that asks for reviews through the review API.
// ModelID is "" for the default model. The secrets are sealed with
// PERGENT_SH_SECRET_KEY; PreviousSecretUntil is the zero time when there is no
// previous secret.
type APIClient struct {
	ID                   int64
	Name                 string
	Skills               []string
	ModelID              string
	CallbackURL          string
	CallbackSecretSealed []byte
	PreviousSecretSealed []byte
	PreviousSecretUntil  time.Time
	Limits               APILimits
	CreatedAt, UpdatedAt time.Time
}

// APIKey is one of a client's keys. Hash is the SHA-256 of the whole key;
// Prefix finds the row.
type APIKey struct {
	ID, ClientID          int64
	Name, Prefix          string
	Hash                  []byte
	CreatedAt             time.Time
	ExpiresAt, LastUsedAt *time.Time
}

const apiClientColumns = `id, name, skills, model_id, callback_url, callback_secret_sealed, previous_secret_sealed,
	previous_secret_until, max_request_bytes, max_files, max_in_flight, requests_per_hour, monthly_token_budget,
	created_at, updated_at`

func scanAPIClient(row pgx.Row) (APIClient, error) {
	var (
		c       APIClient
		modelID *string
		until   *time.Time
	)
	err := row.Scan(&c.ID, &c.Name, &c.Skills, &modelID, &c.CallbackURL, &c.CallbackSecretSealed, &c.PreviousSecretSealed,
		&until, &c.Limits.MaxRequestBytes, &c.Limits.MaxFiles, &c.Limits.MaxInFlight, &c.Limits.RequestsPerHour,
		&c.Limits.MonthlyTokenBudget, &c.CreatedAt, &c.UpdatedAt)
	if modelID != nil {
		c.ModelID = *modelID
	}
	if until != nil {
		c.PreviousSecretUntil = *until
	}
	return c, err
}

// apiClientParams are the writable columns of a client, in the order the
// statements below number them from $1.
func apiClientParams(c APIClient) []any {
	var modelID *string
	if c.ModelID != "" {
		modelID = &c.ModelID
	}
	var until *time.Time
	if !c.PreviousSecretUntil.IsZero() {
		until = &c.PreviousSecretUntil
	}
	skills := c.Skills
	if skills == nil {
		skills = []string{}
	}
	return []any{skills, modelID, c.CallbackURL, c.CallbackSecretSealed, c.PreviousSecretSealed, until,
		c.Limits.MaxRequestBytes, c.Limits.MaxFiles, c.Limits.MaxInFlight, c.Limits.RequestsPerHour, c.Limits.MonthlyTokenBudget}
}

// withDefaultLimits fills each zero limit with its default.
func withDefaultLimits(l APILimits) APILimits {
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = DefaultAPILimits.MaxRequestBytes
	}
	if l.MaxFiles == 0 {
		l.MaxFiles = DefaultAPILimits.MaxFiles
	}
	if l.MaxInFlight == 0 {
		l.MaxInFlight = DefaultAPILimits.MaxInFlight
	}
	if l.RequestsPerHour == 0 {
		l.RequestsPerHour = DefaultAPILimits.RequestsPerHour
	}
	if l.MonthlyTokenBudget == 0 {
		l.MonthlyTokenBudget = DefaultAPILimits.MonthlyTokenBudget
	}
	return l
}

// violatedConstraint returns the name of the unique constraint err breaks, or
// "" when it is not a unique violation.
func violatedConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

// CreateAPIClient adds c (its ID and times are ignored) and returns it as
// stored. A zero limit takes the default. A name that is taken is
// ErrAPIClientExists.
func (s *Store) CreateAPIClient(ctx context.Context, c APIClient) (APIClient, error) {
	c.Limits = withDefaultLimits(c.Limits)
	args := append([]any{c.Name}, apiClientParams(c)...)
	out, err := scanAPIClient(s.pool.QueryRow(ctx, `INSERT INTO api_clients
		(name, skills, model_id, callback_url, callback_secret_sealed, previous_secret_sealed, previous_secret_until,
		 max_request_bytes, max_files, max_in_flight, requests_per_hour, monthly_token_budget)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+apiClientColumns, args...))
	if violatedConstraint(err) == "api_clients_name_key" {
		return APIClient{}, ErrAPIClientExists
	}
	return out, err
}

// APIClients returns every client, by name.
func (s *Store) APIClients(ctx context.Context) ([]APIClient, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiClientColumns+` FROM api_clients ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIClient
	for rows.Next() {
		c, err := scanAPIClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// APIClientByName returns the client with that name.
func (s *Store) APIClientByName(ctx context.Context, name string) (APIClient, bool, error) {
	c, err := scanAPIClient(s.pool.QueryRow(ctx, `SELECT `+apiClientColumns+` FROM api_clients WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIClient{}, false, nil
	}
	return c, err == nil, err
}

// APIClientByID returns the client with that ID.
func (s *Store) APIClientByID(ctx context.Context, id int64) (APIClient, bool, error) {
	c, err := scanAPIClient(s.pool.QueryRow(ctx, `SELECT `+apiClientColumns+` FROM api_clients WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIClient{}, false, nil
	}
	return c, err == nil, err
}

// UpdateAPIClient reads the client under a row lock, lets apply change it and
// saves it, so two updates never lose each other's change. ID, Name and
// CreatedAt are not writable: whatever apply sets there is ignored. found is
// false when there is no such client.
func (s *Store) UpdateAPIClient(ctx context.Context, name string, apply func(*APIClient)) (APIClient, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return APIClient{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	c, err := scanAPIClient(tx.QueryRow(ctx, `SELECT `+apiClientColumns+` FROM api_clients WHERE name = $1 FOR UPDATE`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIClient{}, false, nil
	}
	if err != nil {
		return APIClient{}, false, err
	}
	id, keptName, created := c.ID, c.Name, c.CreatedAt
	apply(&c)
	c.ID, c.Name, c.CreatedAt = id, keptName, created

	args := append([]any{id}, apiClientParams(c)...)
	out, err := scanAPIClient(tx.QueryRow(ctx, `UPDATE api_clients SET skills = $2, model_id = $3, callback_url = $4,
		callback_secret_sealed = $5, previous_secret_sealed = $6, previous_secret_until = $7, max_request_bytes = $8,
		max_files = $9, max_in_flight = $10, requests_per_hour = $11, monthly_token_budget = $12, updated_at = clock_timestamp()
		WHERE id = $1 RETURNING `+apiClientColumns, args...))
	if err != nil {
		return APIClient{}, false, err
	}
	return out, true, tx.Commit(ctx)
}

// DeleteAPIClient removes a client and, with it, its keys. found is false when
// there is no such client. jobs is nil until a client's reviews are stored.
func (s *Store) DeleteAPIClient(ctx context.Context, name string) (jobs []string, found bool, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_clients WHERE name = $1`, name)
	if err != nil {
		return nil, false, err
	}
	return nil, tag.RowsAffected() > 0, nil
}

const apiKeyColumns = `id, client_id, name, prefix, key_hash, created_at, expires_at, last_used_at`

func scanAPIKey(row pgx.Row) (APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.ClientID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt)
	return k, err
}

// CreateAPIKey adds k (its ID and CreatedAt are ignored) and returns it as
// stored. A name the client already has, in any case, is ErrAPIKeyExists; a
// prefix another key has is ErrAPIPrefixTaken.
func (s *Store) CreateAPIKey(ctx context.Context, k APIKey) (APIKey, error) {
	out, err := scanAPIKey(s.pool.QueryRow(ctx, `INSERT INTO api_keys (client_id, name, prefix, key_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+apiKeyColumns, k.ClientID, k.Name, k.Prefix, k.Hash, k.ExpiresAt))
	switch violatedConstraint(err) {
	case "api_keys_name_idx":
		return APIKey{}, ErrAPIKeyExists
	case "api_keys_prefix_key":
		return APIKey{}, ErrAPIPrefixTaken
	}
	return out, err
}

// APIKeys returns a client's keys, newest first.
func (s *Store) APIKeys(ctx context.Context, clientID int64) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE client_id = $1 ORDER BY created_at DESC, id DESC`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteAPIKey removes one of a client's keys and returns it. found is false
// when the client has no such key, which a key of another client is.
func (s *Store) DeleteAPIKey(ctx context.Context, clientID, id int64) (APIKey, bool, error) {
	k, err := scanAPIKey(s.pool.QueryRow(ctx, `DELETE FROM api_keys WHERE id = $1 AND client_id = $2 RETURNING `+apiKeyColumns, id, clientID))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, false, nil
	}
	return k, err == nil, err
}

// APIKeyByPrefix returns the key with that prefix and its client. The caller
// compares the hash and checks the expiry.
func (s *Store) APIKeyByPrefix(ctx context.Context, prefix string) (APIKey, APIClient, bool, error) {
	var (
		k       APIKey
		c       APIClient
		modelID *string
		until   *time.Time
	)
	err := s.pool.QueryRow(ctx, `SELECT k.id, k.client_id, k.name, k.prefix, k.key_hash, k.created_at, k.expires_at, k.last_used_at,
		c.id, c.name, c.skills, c.model_id, c.callback_url, c.callback_secret_sealed, c.previous_secret_sealed,
		c.previous_secret_until, c.max_request_bytes, c.max_files, c.max_in_flight, c.requests_per_hour,
		c.monthly_token_budget, c.created_at, c.updated_at
		FROM api_keys k JOIN api_clients c ON c.id = k.client_id WHERE k.prefix = $1`, prefix).Scan(
		&k.ID, &k.ClientID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt,
		&c.ID, &c.Name, &c.Skills, &modelID, &c.CallbackURL, &c.CallbackSecretSealed, &c.PreviousSecretSealed,
		&until, &c.Limits.MaxRequestBytes, &c.Limits.MaxFiles, &c.Limits.MaxInFlight, &c.Limits.RequestsPerHour,
		&c.Limits.MonthlyTokenBudget, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, APIClient{}, false, nil
	}
	if err != nil {
		return APIKey{}, APIClient{}, false, err
	}
	if modelID != nil {
		c.ModelID = *modelID
	}
	if until != nil {
		c.PreviousSecretUntil = *until
	}
	return k, c, true, nil
}

// TouchAPIKey records a use of a key at now. It writes only when the key has
// no last use or the last use is over a minute old, so a busy client does not
// write on every request.
func (s *Store) TouchAPIKey(ctx context.Context, id int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, id, now)
	return err
}
