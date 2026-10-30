package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Project is a repository the service reviews, with the hook and deploy token
// it created there.
type Project struct {
	Instance              string
	ProjectID             int64
	Path, HTTPURLToRepo   string
	HookID, DeployTokenID int64
	DeployTokenUsername   string
	DeployTokenSealed     []byte
	DeployTokenExpiresAt  time.Time
	// CloneSeconds is the last measured checkout time, 0 when unknown.
	CloneSeconds int
}

const projectColumns = `instance, project_id, path, http_url_to_repo, hook_id, deploy_token_id,
	deploy_token_username, deploy_token_sealed, deploy_token_expires_at, clone_seconds`

func scanProject(row pgx.Row) (Project, error) {
	var (
		p       Project
		expires *time.Time
	)
	err := row.Scan(&p.Instance, &p.ProjectID, &p.Path, &p.HTTPURLToRepo, &p.HookID, &p.DeployTokenID,
		&p.DeployTokenUsername, &p.DeployTokenSealed, &expires, &p.CloneSeconds)
	p.DeployTokenExpiresAt = deref(expires)
	return p, err
}

// Project returns one project; ok is false when it is not stored.
func (s *Store) Project(ctx context.Context, instance string, projectID int64) (Project, bool, error) {
	p, err := scanProject(s.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects
		WHERE instance = $1 AND project_id = $2`, instance, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	return p, true, nil
}

// Projects returns every stored project ordered by instance and id.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY instance, project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertProject inserts or updates a project. An update keeps the stored
// CloneSeconds, which only SetCloneSeconds changes.
func (s *Store) UpsertProject(ctx context.Context, p Project) error {
	var expires *time.Time
	if !p.DeployTokenExpiresAt.IsZero() {
		expires = &p.DeployTokenExpiresAt
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO projects (`+projectColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (instance, project_id) DO UPDATE SET
			path = EXCLUDED.path, http_url_to_repo = EXCLUDED.http_url_to_repo, hook_id = EXCLUDED.hook_id,
			deploy_token_id = EXCLUDED.deploy_token_id, deploy_token_username = EXCLUDED.deploy_token_username,
			deploy_token_sealed = EXCLUDED.deploy_token_sealed, deploy_token_expires_at = EXCLUDED.deploy_token_expires_at`,
		p.Instance, p.ProjectID, p.Path, p.HTTPURLToRepo, p.HookID, p.DeployTokenID,
		p.DeployTokenUsername, p.DeployTokenSealed, expires, p.CloneSeconds)
	return err
}

// DeleteProject removes a project; deleting one that is not there is not an error.
func (s *Store) DeleteProject(ctx context.Context, instance string, projectID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM projects WHERE instance = $1 AND project_id = $2`, instance, projectID)
	return err
}

// SetCloneSeconds records the last measured checkout time of a project.
func (s *Store) SetCloneSeconds(ctx context.Context, instance string, projectID int64, seconds int) error {
	_, err := s.pool.Exec(ctx, `UPDATE projects SET clone_seconds = $3 WHERE instance = $1 AND project_id = $2`,
		instance, projectID, seconds)
	return err
}
