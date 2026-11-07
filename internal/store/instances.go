package store

import (
	"context"
	"errors"
	"slices"
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
// dashboard repositories, its attention rows, and the dismissals, commands,
// inline threads and finding fates of its merge requests. The instance row and its
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
		`DELETE FROM dismissals WHERE instance = $1`,
		`DELETE FROM commands WHERE instance = $1`,
		`DELETE FROM webhook_deliveries WHERE instance = $1`,
		`DELETE FROM inline_threads WHERE instance = $1`,
		`DELETE FROM finding_fates WHERE instance = $1`,
		`DELETE FROM fate_jobs WHERE instance = $1`,
		`DELETE FROM fate_checks WHERE instance = $1`,
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
	Approve, ReviewDrafts, InlineFindings *bool
	FailureStatus                         *string
	JobTimeoutSeconds                     *int
	MaxTurns                              *int
	JobMemory                             *string
	// RerunLimitPerMR and RerunLimitPerRepo cap the re-runs asked for per
	// merge request and per repository and day.
	RerunLimitPerMR, RerunLimitPerRepo *int
	// Skills is the default skill list: library names, for a repository
	// whose own skills setting is null. Nil (an empty list is stored as nil)
	// is no skills.
	Skills    []string
	UpdatedAt time.Time
	// CommandsAuthorAndMaintainersOnly is whether only a change's author and
	// the repository's maintainers and owners can use pergent commands on it
	// (nil: the default, on).
	CommandsAuthorAndMaintainersOnly *bool
	// IncrementalReviews is whether a push reviews only the files changed
	// since the last review (nil: the default, on).
	IncrementalReviews *bool
	// LearnFromDismissals is whether later reviews are told what people with
	// write access dismissed, and why (nil: the default, on).
	LearnFromDismissals *bool
	// QuietReviews is whether a review says less (nil: the default, off).
	QuietReviews *bool
	// CommentLayout is the shape of the review comment, "block:state" entries
	// in order (nil: the default layout).
	CommentLayout []string
	// LinkedReposEnabled is whether the impact check across linked
	// repositories is on (nil: the default, off). Links are per repository
	// and have no default.
	LinkedReposEnabled *bool
}

// RepoDefaults returns the defaults; the zero value when none were put.
func (s *Store) RepoDefaults(ctx context.Context) (RepoDefaults, error) {
	var d RepoDefaults
	err := s.pool.QueryRow(ctx, `SELECT approve, review_drafts, failure_status, job_timeout_seconds, max_turns, job_memory,
			rerun_limit_per_mr, rerun_limit_per_repo, skills, updated_at, inline_findings,
			commands_author_and_maintainers_only, incremental_reviews, learn_from_dismissals, quiet_reviews, comment_layout,
			linked_repos_enabled
		FROM repo_defaults WHERE id = 1`).
		Scan(&d.Approve, &d.ReviewDrafts, &d.FailureStatus, &d.JobTimeoutSeconds, &d.MaxTurns, &d.JobMemory,
			&d.RerunLimitPerMR, &d.RerunLimitPerRepo, &d.Skills, &d.UpdatedAt, &d.InlineFindings,
			&d.CommandsAuthorAndMaintainersOnly, &d.IncrementalReviews, &d.LearnFromDismissals, &d.QuietReviews, &d.CommentLayout, &d.LinkedReposEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoDefaults{}, nil
	}
	if len(d.Skills) == 0 {
		d.Skills = nil
	}
	if len(d.CommentLayout) == 0 {
		d.CommentLayout = nil
	}
	return d, err
}

// AddDefaultSkills appends the library skills named to the default skill
// list, those not on it yet, in one locked UPDATE: no other default is
// read or written, so a change to approve, the caps or the rest made at the
// same time stays. Each named skill is locked until the change is stored; one
// that is not in the library is ErrSkillNotFound and nothing changes.
func (s *Store) AddDefaultSkills(ctx context.Context, names []string) error {
	var add []string // names once each, in the order given
	for _, n := range names {
		if !slices.Contains(add, n) {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := lockNamedSkills(ctx, tx, add); err != nil {
		return err
	}
	// The conflict branch locks the row, so two adds run one after the other
	// and each sees the other's names. The names keep their order.
	if _, err := tx.Exec(ctx, `INSERT INTO repo_defaults (id, skills, updated_at) VALUES (1, $1, now())
		ON CONFLICT (id) DO UPDATE SET updated_at = now(),
			skills = COALESCE(repo_defaults.skills, '{}') || ARRAY(
				SELECT n FROM unnest($1::text[]) AS t(n) WHERE n <> ALL (COALESCE(repo_defaults.skills, '{}')))`, add); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutRepoDefaults replaces the defaults with d, nil fields included. Its
// UpdatedAt is ignored: the database's now is stored. Each library skill d
// names is locked until the change is stored, so it can't be deleted
// meanwhile; one that is not in the library is ErrSkillNotFound.
func (s *Store) PutRepoDefaults(ctx context.Context, d RepoDefaults) error {
	skills := d.Skills
	if len(skills) == 0 {
		skills = nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := lockNamedSkills(ctx, tx, skills); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_defaults (id, approve, review_drafts, failure_status, job_timeout_seconds, max_turns, job_memory,
			rerun_limit_per_mr, rerun_limit_per_repo, skills, updated_at, inline_findings,
			commands_author_and_maintainers_only, incremental_reviews, learn_from_dismissals, quiet_reviews, comment_layout, linked_repos_enabled)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET approve = EXCLUDED.approve, review_drafts = EXCLUDED.review_drafts,
			failure_status = EXCLUDED.failure_status, job_timeout_seconds = EXCLUDED.job_timeout_seconds,
			max_turns = EXCLUDED.max_turns, job_memory = EXCLUDED.job_memory, rerun_limit_per_mr = EXCLUDED.rerun_limit_per_mr,
			rerun_limit_per_repo = EXCLUDED.rerun_limit_per_repo, skills = EXCLUDED.skills, updated_at = EXCLUDED.updated_at,
			inline_findings = EXCLUDED.inline_findings,
			commands_author_and_maintainers_only = EXCLUDED.commands_author_and_maintainers_only,
			incremental_reviews = EXCLUDED.incremental_reviews,
			learn_from_dismissals = EXCLUDED.learn_from_dismissals,
			quiet_reviews = EXCLUDED.quiet_reviews,
			comment_layout = EXCLUDED.comment_layout,
			linked_repos_enabled = EXCLUDED.linked_repos_enabled`,
		d.Approve, d.ReviewDrafts, d.FailureStatus, d.JobTimeoutSeconds, d.MaxTurns, d.JobMemory, d.RerunLimitPerMR, d.RerunLimitPerRepo, skills, d.InlineFindings,
		d.CommandsAuthorAndMaintainersOnly, d.IncrementalReviews, d.LearnFromDismissals, d.QuietReviews, nilIfEmpty(d.CommentLayout), d.LinkedReposEnabled); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
