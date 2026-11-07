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

// TeamMember is an account on a team with its role there: RoleViewer or
// RoleMaintainer. An empty Role is stored as RoleViewer.
type TeamMember struct {
	AccountID int64
	Role      string
}

// ErrUnknownAccount is returned when a team change names a member that isn't
// an account (it was removed meanwhile).
var ErrUnknownAccount = errors.New("no such account")

// TeamRepo is a repository a team covers on an instance; an empty Project
// covers every repository of Instance.
type TeamRepo struct{ Instance, Project string }

// Team is a named group of accounts. Members are ordered by account ID and
// each has a role on the team; platform accounts (no longer used) are never
// listed. Repos are ordered by instance and project ignoring case. A team has
// no role of its own: what it gives comes from its members' roles. (The
// teams.role column stays, written "viewer", until a later release drops it.)
type Team struct {
	ID        int64
	Name      string
	Members   []TeamMember
	Repos     []TeamRepo
	CreatedAt time.Time
}

// CreateTeam stores a team with no members and no repositories. A name
// taken ignoring case is ErrExists. The unused teams.role column, which is
// NOT NULL, is written "viewer".
func (s *Store) CreateTeam(ctx context.Context, name string, at time.Time) (Team, error) {
	t := Team{Name: name}
	err := s.pool.QueryRow(ctx, `INSERT INTO teams (name, role, created_at) VALUES ($1, 'viewer', $2) RETURNING id, created_at`,
		name, at).Scan(&t.ID, &t.CreatedAt)
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
	rows, err := q.Query(ctx, `SELECT id, name, created_at FROM teams t `+where+` ORDER BY lower(name), id`, args...)
	if err != nil {
		return nil, err
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) {
		var t Team
		err := row.Scan(&t.ID, &t.Name, &t.CreatedAt)
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
	rows, err = q.Query(ctx, `SELECT m.team_id, m.account_id, m.role FROM team_members m
		JOIN accounts a ON a.id = m.account_id
		WHERE m.team_id = ANY($1) AND a.kind <> 'platform' ORDER BY m.account_id`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var teamID int64
		var m TeamMember
		if err := rows.Scan(&teamID, &m.AccountID, &m.Role); err != nil {
			rows.Close()
			return nil, err
		}
		byID[teamID].Members = append(byID[teamID].Members, m)
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
// apply left: its name, members and repositories replace the old ones
// (an account named twice is one member, with the higher role). A member
// role must be RoleViewer, RoleMaintainer or empty (viewer). When apply
// fails nothing changes and its error is returned. ok is false when there is no such team; a name taken ignoring
// case is ErrExists, a member that isn't an account (or is a platform
// account) ErrUnknownAccount. The memberships of platform accounts are left
// as they are.
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
	if _, err := tx.Exec(ctx, `UPDATE teams SET name = $2 WHERE id = $1`, id, t.Name); err != nil {
		if isUniqueViolation(err) {
			return false, ErrExists
		}
		return false, err
	}
	byAccount := map[int64]string{}
	for _, m := range t.Members {
		role := m.Role
		if role == "" {
			role = RoleViewer
		}
		if role != RoleViewer && role != RoleMaintainer {
			return false, fmt.Errorf("store: unknown member role %q", m.Role)
		}
		if role == RoleMaintainer || byAccount[m.AccountID] == "" {
			byAccount[m.AccountID] = role
		}
	}
	accountIDs := make([]int64, 0, len(byAccount))
	for accountID := range byAccount {
		accountIDs = append(accountIDs, accountID)
	}
	slices.Sort(accountIDs)
	roles := make([]string, len(accountIDs))
	for i, accountID := range accountIDs {
		roles[i] = byAccount[accountID]
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_members m WHERE m.team_id = $1
		AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = m.account_id AND a.kind <> 'platform')`, id); err != nil {
		return false, err
	}
	if len(accountIDs) > 0 {
		tag, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, account_id, role)
			SELECT $1::bigint, u.id, u.role FROM unnest($2::bigint[], $3::text[]) AS u(id, role)
			JOIN accounts a ON a.id = u.id AND a.kind <> 'platform'`, id, accountIDs, roles)
		if err != nil {
			if isForeignKeyViolation(err) {
				return false, ErrUnknownAccount
			}
			return false, err
		}
		if tag.RowsAffected() != int64(len(accountIDs)) {
			return false, ErrUnknownAccount
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
