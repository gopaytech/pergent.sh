package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxRuleSets is the most rule sets the library holds.
const MaxRuleSets = 200

var (
	// ErrRuleSetExists is returned when a rule set's name is taken.
	ErrRuleSetExists = errors.New("store: rule set name already in the library")
	// ErrRuleSetNotFound is returned when a named rule set is not in the
	// library.
	ErrRuleSetNotFound = errors.New("store: rule set not in the library")
	// ErrRuleSetVersion is returned by UpdateRuleSet when the set has moved
	// on since the version the caller read.
	ErrRuleSetVersion = errors.New("store: rule set changed since it was read")
	// ErrRuleSetsFull is returned when the library holds MaxRuleSets.
	ErrRuleSetsFull = errors.New("store: the rule library is full")
)

// ruleSetLockKey is the pg_advisory_xact_lock key that serialises rule-set
// creation, so the size limit holds under concurrent creates.
const ruleSetLockKey = 0x72756c65 // "rule"

// RuleSet is a named list of path rules: Content is the rule file's YAML,
// which the caller checks with pergent's rule.Load before it is stored.
type RuleSet struct {
	Name, Content        string
	Version              int
	CreatedAt, UpdatedAt time.Time
}

// MissingRuleSetError names a rule set a save names that is not in the
// library: the first one missing, in name order. It matches
// ErrRuleSetNotFound.
type MissingRuleSetError struct{ Name string }

func (e *MissingRuleSetError) Error() string {
	return fmt.Sprintf("store: rule set %q is not in the library", e.Name)
}

// Is makes errors.Is(err, ErrRuleSetNotFound) true.
func (e *MissingRuleSetError) Is(target error) bool { return target == ErrRuleSetNotFound }

// RuleSetInUseError is returned by DeleteRuleSet for a set a repository or
// the default list names.
type RuleSetInUseError struct {
	Repos      []RepoRef
	InDefaults bool
}

func (e *RuleSetInUseError) Error() string {
	return fmt.Sprintf("store: rule set is in use by %d repositories (default list: %t)", len(e.Repos), e.InDefaults)
}

// ResolvedRuleSets is what a list of rule-set names resolved to, read at one
// point in time: Names is the list (the default list for nil names), Sets the
// sets found and Missing the names that are not in the library, each once.
type ResolvedRuleSets struct {
	Names   []string
	Sets    map[string]RuleSet
	Missing []string
}

const ruleSetColumns = `name, content, version, created_at, updated_at`

func scanRuleSet(row pgx.Row) (RuleSet, error) {
	var r RuleSet
	err := row.Scan(&r.Name, &r.Content, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// RuleSets returns every rule set, ordered by name, with its content.
func (s *Store) RuleSets(ctx context.Context) ([]RuleSet, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ruleSetColumns+` FROM rule_sets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuleSet
	for rows.Next() {
		r, err := scanRuleSet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleSet returns the rule set called name, or ErrRuleSetNotFound.
func (s *Store) RuleSet(ctx context.Context, name string) (RuleSet, error) {
	r, err := scanRuleSet(s.pool.QueryRow(ctx, `SELECT `+ruleSetColumns+` FROM rule_sets WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return RuleSet{}, ErrRuleSetNotFound
	}
	return r, err
}

// CreateRuleSet adds a rule set at version 1. A taken name is
// ErrRuleSetExists; a library of MaxRuleSets is ErrRuleSetsFull.
func (s *Store) CreateRuleSet(ctx context.Context, name, content string, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ruleSetLockKey); err != nil {
		return err
	}
	var exists bool
	var count int
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rule_sets WHERE name = $1), count(*) FROM rule_sets`, name).
		Scan(&exists, &count); err != nil {
		return err
	}
	if exists {
		return ErrRuleSetExists
	}
	if count >= MaxRuleSets {
		return ErrRuleSetsFull
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rule_sets (name, content, version, created_at, updated_at)
		VALUES ($1, $2, 1, $3, $3)`, name, content, at); err != nil {
		if isUniqueViolation(err) {
			return ErrRuleSetExists
		}
		return err
	}
	return tx.Commit(ctx)
}

// UpdateRuleSet saves content as the next version of a rule set and returns
// it. expect is the version the caller read: another one is
// ErrRuleSetVersion. A missing set is ErrRuleSetNotFound.
func (s *Store) UpdateRuleSet(ctx context.Context, name, content string, expect int, at time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var current int
	err = tx.QueryRow(ctx, `SELECT version FROM rule_sets WHERE name = $1 FOR UPDATE`, name).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRuleSetNotFound
	}
	if err != nil {
		return 0, err
	}
	if current != expect {
		return 0, ErrRuleSetVersion
	}
	if _, err := tx.Exec(ctx, `UPDATE rule_sets SET content = $2, version = $3, updated_at = $4 WHERE name = $1`,
		name, content, current+1, at); err != nil {
		return 0, err
	}
	return current + 1, tx.Commit(ctx)
}

// DeleteRuleSet removes a rule set. It is refused with a *RuleSetInUseError
// while a dashboard repository or the default list names it, or configRepos
// (the config-file repositories that do, which the caller knows) is not
// empty. The set's row is locked first, and a save of a list naming it locks
// the row too, so a delete and such a save cannot both succeed. A missing set
// is ErrRuleSetNotFound.
func (s *Store) DeleteRuleSet(ctx context.Context, name string, configRepos []RepoRef) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var found string
	err = tx.QueryRow(ctx, `SELECT name FROM rule_sets WHERE name = $1 FOR UPDATE`, name).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRuleSetNotFound
	}
	if err != nil {
		return err
	}
	inUse := &RuleSetInUseError{}
	rows, err := tx.Query(ctx, `SELECT instance, project FROM dashboard_repos WHERE $1 = ANY (rule_sets)
		ORDER BY instance, lower(project)`, name)
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
	inUse.Repos = append(inUse.Repos, configRepos...)
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repo_defaults WHERE $1 = ANY (rule_sets))`, name).Scan(&inUse.InDefaults); err != nil {
		return err
	}
	if len(inUse.Repos) > 0 || inUse.InDefaults {
		return inUse
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rule_sets WHERE name = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockNamedRuleSets locks each named rule set FOR KEY SHARE, which holds off
// a concurrent delete until the transaction ends, and returns a
// *MissingRuleSetError naming the first that is not there. The rows are
// locked in name order, so two savers never deadlock.
func lockNamedRuleSets(ctx context.Context, tx pgx.Tx, names []string) error {
	if len(names) == 0 {
		return nil
	}
	want := slices.Clone(names)
	slices.Sort(want)
	want = slices.Compact(want)
	rows, err := tx.Query(ctx, `SELECT name FROM rule_sets WHERE name = ANY ($1) ORDER BY name FOR KEY SHARE`, want)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range want {
		if !have[name] {
			return &MissingRuleSetError{Name: name}
		}
	}
	return nil
}

// ResolveRuleSets reads the named rule sets, with their content, in one
// repeatable-read transaction, so a concurrent edit gives the old sets or the
// new ones and never a mix. nil names are the default list.
func (s *Store) ResolveRuleSets(ctx context.Context, names []string) (ResolvedRuleSets, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ResolvedRuleSets{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if names == nil {
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT rule_sets FROM repo_defaults WHERE id = 1), '{}')`).Scan(&names); err != nil {
			return ResolvedRuleSets{}, err
		}
	}
	out := ResolvedRuleSets{Names: names, Sets: map[string]RuleSet{}}
	rows, err := tx.Query(ctx, `SELECT `+ruleSetColumns+` FROM rule_sets WHERE name = ANY ($1)`, names)
	if err != nil {
		return ResolvedRuleSets{}, err
	}
	for rows.Next() {
		r, err := scanRuleSet(rows)
		if err != nil {
			rows.Close()
			return ResolvedRuleSets{}, err
		}
		out.Sets[r.Name] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ResolvedRuleSets{}, err
	}
	seen := map[string]bool{}
	for _, n := range names {
		if _, ok := out.Sets[n]; !ok && !seen[n] {
			out.Missing = append(out.Missing, n)
		}
		seen[n] = true
	}
	return out, nil
}
