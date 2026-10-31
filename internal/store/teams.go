package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The roles a team gives its members, from most to least.
const (
	RoleAdmin      = "admin"
	RoleMaintainer = "maintainer"
	RoleViewer     = "viewer"
)

func validRole(role string) bool {
	return role == RoleAdmin || role == RoleMaintainer || role == RoleViewer
}

// ErrUnknownAccount is returned when a team change names a member that isn't
// an account (it was removed meanwhile).
var ErrUnknownAccount = errors.New("no such account")

// TeamRepo is a repository a team covers on an instance; an empty Project
// covers every repository of Instance.
type TeamRepo struct{ Instance, Project string }

// Team is a named group of accounts that has one role on its repositories.
// Members are account IDs, ascending; Repos are ordered by instance and
// project ignoring case.
type Team struct {
	ID        int64
	Name      string
	Role      string
	Members   []int64
	Repos     []TeamRepo
	CreatedAt time.Time
}

// CreateTeam stores a team with no members and no repositories. A name
// taken ignoring case is ErrExists.
func (s *Store) CreateTeam(ctx context.Context, name, role string, at time.Time) (Team, error) {
	if !validRole(role) {
		return Team{}, fmt.Errorf("store: unknown role %q", role)
	}
	t := Team{Name: name, Role: role}
	err := s.pool.QueryRow(ctx, `INSERT INTO teams (name, role, created_at) VALUES ($1, $2, $3) RETURNING id, created_at`,
		name, role, at).Scan(&t.ID, &t.CreatedAt)
	if isUniqueViolation(err) {
		return Team{}, ErrExists
	}
	if err != nil {
		return Team{}, err
	}
	return t, nil
}

// loadTeams reads the teams matching where (with args), by name ignoring
// case, with their members and repositories.
func loadTeams(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, where string, args ...any) ([]Team, error) {
	rows, err := q.Query(ctx, `SELECT id, name, role, created_at FROM teams t `+where+` ORDER BY lower(name), id`, args...)
	if err != nil {
		return nil, err
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) {
		var t Team
		err := row.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedAt)
		return t, err
	})
	if err != nil || len(teams) == 0 {
		return teams, err
	}
	ids := make([]int64, len(teams))
	byID := make(map[int64]*Team, len(teams))
	for i := range teams {
		ids[i] = teams[i].ID
		byID[teams[i].ID] = &teams[i]
	}
	rows, err = q.Query(ctx, `SELECT team_id, account_id FROM team_members WHERE team_id = ANY($1) ORDER BY account_id`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var teamID, accountID int64
		if err := rows.Scan(&teamID, &accountID); err != nil {
			rows.Close()
			return nil, err
		}
		byID[teamID].Members = append(byID[teamID].Members, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT team_id, instance, project FROM team_repos WHERE team_id = ANY($1)
		ORDER BY instance, lower(project)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var teamID int64
		var r TeamRepo
		if err := rows.Scan(&teamID, &r.Instance, &r.Project); err != nil {
			rows.Close()
			return nil, err
		}
		byID[teamID].Repos = append(byID[teamID].Repos, r)
	}
	return teams, rows.Err()
}

// Teams returns every team, by name ignoring case.
func (s *Store) Teams(ctx context.Context) ([]Team, error) {
	return loadTeams(ctx, s.pool, ``)
}

// Team returns the team with the given ID; found is false when there is none.
func (s *Store) Team(ctx context.Context, id int64) (Team, bool, error) {
	teams, err := loadTeams(ctx, s.pool, `WHERE t.id = $1`, id)
	if err != nil || len(teams) == 0 {
		return Team{}, false, err
	}
	return teams[0], true, nil
}

// AccountTeams returns the teams the account is a member of.
func (s *Store) AccountTeams(ctx context.Context, accountID int64) ([]Team, error) {
	return loadTeams(ctx, s.pool, `WHERE EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id AND m.account_id = $1)`, accountID)
}

// UpdateTeam reads the team under a lock, hands it to apply, and stores what
// apply left: its name, role, members and repositories replace the old ones
// (duplicates are one). When apply fails nothing changes and its error is
// returned. ok is false when there is no such team; a name taken ignoring
// case is ErrExists, a member that isn't an account ErrUnknownAccount.
func (s *Store) UpdateTeam(ctx context.Context, id int64, apply func(*Team) error) (ok bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var locked int64
	err = tx.QueryRow(ctx, `SELECT id FROM teams WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	teams, err := loadTeams(ctx, tx, `WHERE t.id = $1`, id)
	if err != nil {
		return false, err
	}
	t := teams[0]
	if err := apply(&t); err != nil {
		return false, err
	}
	if !validRole(t.Role) {
		return false, fmt.Errorf("store: unknown role %q", t.Role)
	}
	if _, err := tx.Exec(ctx, `UPDATE teams SET name = $2, role = $3 WHERE id = $1`, id, t.Name, t.Role); err != nil {
		if isUniqueViolation(err) {
			return false, ErrExists
		}
		return false, err
	}
	members := slices.Clone(t.Members)
	slices.Sort(members)
	members = slices.Compact(members)
	if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1`, id); err != nil {
		return false, err
	}
	if len(members) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, account_id) SELECT $1, unnest($2::bigint[])`, id, members); err != nil {
			if isForeignKeyViolation(err) {
				return false, ErrUnknownAccount
			}
			return false, err
		}
	}
	seen := map[TeamRepo]bool{}
	var instances, projects []string
	for _, r := range t.Repos {
		key := TeamRepo{r.Instance, strings.ToLower(r.Project)}
		if !seen[key] {
			seen[key] = true
			instances, projects = append(instances, r.Instance), append(projects, r.Project)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_repos WHERE team_id = $1`, id); err != nil {
		return false, err
	}
	if len(instances) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO team_repos (team_id, instance, project)
			SELECT $1, i, p FROM unnest($2::text[], $3::text[]) AS u(i, p)`, id, instances, projects); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteTeam removes a team with its members and repositories (not the
// accounts); ok is false when there is no such team. When check is not nil
// it is handed the team under the lock UpdateTeam takes, so what it decides
// on can't change before the team goes; an error from it keeps the team and
// is returned.
func (s *Store) DeleteTeam(ctx context.Context, id int64, check func(Team) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var locked int64
	err = tx.QueryRow(ctx, `SELECT id FROM teams WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if check != nil {
		teams, err := loadTeams(ctx, tx, `WHERE t.id = $1`, id)
		if err != nil {
			return false, err
		}
		if err := check(teams[0]); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
