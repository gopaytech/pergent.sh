package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// How an installed skill was read (SkillSource.Kind): through the API of a
// connected instance, or from a public git URL.
const (
	SourceKindInstance = "instance"
	SourceKindURL      = "url"
)

// MaxLibrarySkills is the most skills the library holds.
const MaxLibrarySkills = 200

var (
	// ErrSkillExists is returned when a skill's name is taken.
	ErrSkillExists = errors.New("store: skill name already in the library")
	// ErrSkillNotFound is returned when a named skill (or version) is not in
	// the library.
	ErrSkillNotFound = errors.New("store: skill not in the library")
	// ErrStaleVersion is returned by UpdateLibrarySkill when the skill has
	// moved on since the version the caller read.
	ErrStaleVersion = errors.New("store: skill changed since it was read")
	// ErrSkillInstalled is returned when a skill installed from a source is
	// edited by hand.
	ErrSkillInstalled = errors.New("store: skill is installed from a source and is read-only")
	// ErrLibraryFull is returned when the library holds MaxLibrarySkills.
	ErrLibraryFull = errors.New("store: the skill library is full")
)

// MissingSkillError names a skill a save names that is not in the library:
// the first one missing, in name order. It matches ErrSkillNotFound.
type MissingSkillError struct{ Name string }

func (e *MissingSkillError) Error() string {
	return fmt.Sprintf("store: skill %q is not in the library", e.Name)
}

// Is makes errors.Is(err, ErrSkillNotFound) true.
func (e *MissingSkillError) Is(target error) bool { return target == ErrSkillNotFound }

// libraryLockKey is the pg_advisory_xact_lock key that serialises skill
// creation, so the size limit holds under concurrent creates.
const libraryLockKey = 0x736b696c // "skil"

// SkillSource is where an installed skill came from: Kind is SourceKindInstance
// (Instance and Repo are set) or SourceKindURL (URL is set); Ref is the branch
// or tag, Path the skill's own file at the source and Commit the commit read.
type SkillSource struct{ Kind, Instance, Repo, URL, Ref, Path, Commit string }

// LibrarySkill is a skill of the library. Size is the byte length of the
// content; lists leave Content empty.
type LibrarySkill struct {
	Name, Content, UpdatedBy string
	Version, Size            int
	UpdatedAt                time.Time
	Source                   *SkillSource // nil: written in the dashboard
}

// LibrarySkillVersion is one saved version of a skill. SHA256 is the hash of
// the content; Commit is the source commit of an installed version.
type LibrarySkillVersion struct {
	Name, Content, Actor, Commit string
	Version                      int
	SHA256                       []byte
	At                           time.Time
}

// NewSkill is a skill to add to the library: written (Source nil) or
// installed.
type NewSkill struct {
	Name, Content, Actor string
	Source               *SkillSource
	At                   time.Time
}

// RepoRef names a repository by its instance and project path.
type RepoRef struct{ Instance, Project string }

// SkillInUseError is returned by DeleteLibrarySkill for a skill a repository
// or the default list names.
type SkillInUseError struct {
	Repos      []RepoRef
	InDefaults bool
}

func (e *SkillInUseError) Error() string {
	return fmt.Sprintf("store: skill is in use by %d repositories (default list: %t)", len(e.Repos), e.InDefaults)
}

// ResolvedLibrary is the library skills a list of names resolved to, and the
// names that are not in the library, read at one point in time.
type ResolvedLibrary struct {
	Skills  map[string]LibrarySkill
	Missing []string
}

const sourceColumns = `source_kind, source_instance, source_repo, source_url, source_ref, source_path, source_commit`

func scanSource(kind *string, instance, repo, url, ref, path, commit string) *SkillSource {
	if kind == nil {
		return nil
	}
	return &SkillSource{Kind: *kind, Instance: instance, Repo: repo, URL: url, Ref: ref, Path: path, Commit: commit}
}

// LibrarySkills returns every skill, ordered by name, without its content;
// Size is set.
func (s *Store) LibrarySkills(ctx context.Context) ([]LibrarySkill, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, version, octet_length(content), updated_at, updated_by, `+sourceColumns+`
		FROM library_skills ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LibrarySkill
	for rows.Next() {
		var (
			k                              LibrarySkill
			kind                           *string
			inst, repo, url, ref, pth, cmt string
		)
		if err := rows.Scan(&k.Name, &k.Version, &k.Size, &k.UpdatedAt, &k.UpdatedBy,
			&kind, &inst, &repo, &url, &ref, &pth, &cmt); err != nil {
			return nil, err
		}
		k.Source = scanSource(kind, inst, repo, url, ref, pth, cmt)
		out = append(out, k)
	}
	return out, rows.Err()
}

// LibrarySkill returns a skill with its content, and its versions, newest
// first, without their content. A missing skill is ErrSkillNotFound.
func (s *Store) LibrarySkill(ctx context.Context, name string) (LibrarySkill, []LibrarySkillVersion, error) {
	var (
		k                              LibrarySkill
		kind                           *string
		inst, repo, url, ref, pth, cmt string
	)
	err := s.pool.QueryRow(ctx, `SELECT name, version, content, octet_length(content), updated_at, updated_by, `+sourceColumns+`
		FROM library_skills WHERE name = $1`, name).
		Scan(&k.Name, &k.Version, &k.Content, &k.Size, &k.UpdatedAt, &k.UpdatedBy, &kind, &inst, &repo, &url, &ref, &pth, &cmt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LibrarySkill{}, nil, ErrSkillNotFound
	}
	if err != nil {
		return LibrarySkill{}, nil, err
	}
	k.Source = scanSource(kind, inst, repo, url, ref, pth, cmt)
	rows, err := s.pool.Query(ctx, `SELECT name, version, sha256, source_commit, at, actor
		FROM library_skill_versions WHERE name = $1 ORDER BY version DESC`, name)
	if err != nil {
		return LibrarySkill{}, nil, err
	}
	defer rows.Close()
	var versions []LibrarySkillVersion
	for rows.Next() {
		var v LibrarySkillVersion
		if err := rows.Scan(&v.Name, &v.Version, &v.SHA256, &v.Commit, &v.At, &v.Actor); err != nil {
			return LibrarySkill{}, nil, err
		}
		versions = append(versions, v)
	}
	return k, versions, rows.Err()
}

// LibrarySkillVersion returns one version of a skill, with its content. The
// history outlives a deleted skill. A missing one is ErrSkillNotFound.
func (s *Store) LibrarySkillVersion(ctx context.Context, name string, version int) (LibrarySkillVersion, error) {
	var v LibrarySkillVersion
	err := s.pool.QueryRow(ctx, `SELECT name, version, content, sha256, source_commit, at, actor
		FROM library_skill_versions WHERE name = $1 AND version = $2`, name, version).
		Scan(&v.Name, &v.Version, &v.Content, &v.SHA256, &v.Commit, &v.At, &v.Actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return LibrarySkillVersion{}, ErrSkillNotFound
	}
	return v, err
}

// nextVersion is one more than the highest version the name ever had, so a
// re-created name continues its numbering.
func nextVersion(ctx context.Context, tx pgx.Tx, name string) (int, error) {
	var v int
	err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM library_skill_versions WHERE name = $1`, name).Scan(&v)
	return v, err
}

func addVersion(ctx context.Context, tx pgx.Tx, name string, version int, content, commit, actor string, at time.Time) error {
	sum := sha256.Sum256([]byte(content))
	_, err := tx.Exec(ctx, `INSERT INTO library_skill_versions (name, version, content, sha256, source_commit, at, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, name, version, content, sum[:], commit, at, actor)
	return err
}

// CreateLibrarySkill adds a skill, written or installed, and returns its
// version: one more than the highest the name ever had. A taken name is
// ErrSkillExists; a library of MaxLibrarySkills is ErrLibraryFull.
func (s *Store) CreateLibrarySkill(ctx context.Context, n NewSkill) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, libraryLockKey); err != nil {
		return 0, err
	}
	var exists bool
	var count int
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM library_skills WHERE name = $1), count(*) FROM library_skills`, n.Name).
		Scan(&exists, &count); err != nil {
		return 0, err
	}
	if exists {
		return 0, ErrSkillExists
	}
	if count >= MaxLibrarySkills {
		return 0, ErrLibraryFull
	}
	version, err := nextVersion(ctx, tx, n.Name)
	if err != nil {
		return 0, err
	}
	var src SkillSource
	var kind *string
	if n.Source != nil {
		src, kind = *n.Source, &n.Source.Kind
	}
	if _, err := tx.Exec(ctx, `INSERT INTO library_skills (name, version, content, updated_at, updated_by, `+sourceColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		n.Name, version, n.Content, n.At, n.Actor, kind, src.Instance, src.Repo, src.URL, src.Ref, src.Path, src.Commit); err != nil {
		if isUniqueViolation(err) {
			return 0, ErrSkillExists
		}
		return 0, err
	}
	if err := addVersion(ctx, tx, n.Name, version, n.Content, src.Commit, n.Actor, n.At); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

// lockSkillForUpdate reads a skill's version and whether it is installed,
// holding its row until the transaction ends.
func lockSkillForUpdate(ctx context.Context, tx pgx.Tx, name string) (installed bool, err error) {
	var kind *string
	err = tx.QueryRow(ctx, `SELECT source_kind FROM library_skills WHERE name = $1 FOR UPDATE`, name).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrSkillNotFound
	}
	return kind != nil, err
}

// UpdateLibrarySkill saves content as the next version of a skill written in
// the dashboard and returns it. expect is the version the caller read: an
// older one is ErrStaleVersion. An installed skill is ErrSkillInstalled.
func (s *Store) UpdateLibrarySkill(ctx context.Context, name, content, actor string, expect int, at time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var kind *string
	var current int
	err = tx.QueryRow(ctx, `SELECT version, source_kind FROM library_skills WHERE name = $1 FOR UPDATE`, name).Scan(&current, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrSkillNotFound
	}
	if err != nil {
		return 0, err
	}
	if kind != nil {
		return 0, ErrSkillInstalled
	}
	if current != expect {
		return 0, ErrStaleVersion
	}
	version, err := nextVersion(ctx, tx, name)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE library_skills SET version = $2, content = $3, updated_at = $4, updated_by = $5 WHERE name = $1`,
		name, version, content, at, actor); err != nil {
		return 0, err
	}
	if err := addVersion(ctx, tx, name, version, content, "", actor, at); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

// UpdateInstalledSkill saves content read from the skill's source as the
// next version and records src, the source and commit it came from (a new
// ref included). It returns the version. A missing skill is
// ErrSkillNotFound; a skill written in the dashboard is refused.
func (s *Store) UpdateInstalledSkill(ctx context.Context, name, content, actor string, src SkillSource, at time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	installed, err := lockSkillForUpdate(ctx, tx, name)
	if err != nil {
		return 0, err
	}
	if !installed {
		return 0, fmt.Errorf("store: skill %q was written in the dashboard, not installed", name)
	}
	version, err := nextVersion(ctx, tx, name)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE library_skills SET version = $2, content = $3, updated_at = $4, updated_by = $5,
			source_kind = $6, source_instance = $7, source_repo = $8, source_url = $9, source_ref = $10,
			source_path = $11, source_commit = $12
		WHERE name = $1`,
		name, version, content, at, actor, src.Kind, src.Instance, src.Repo, src.URL, src.Ref, src.Path, src.Commit); err != nil {
		return 0, err
	}
	if err := addVersion(ctx, tx, name, version, content, src.Commit, actor, at); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

// DeleteLibrarySkill removes a skill; its versions stay. It is refused with a
// *SkillInUseError while a dashboard repository or the default list names it,
// or configRepos (the config-file repositories that do, which the caller
// knows) is not empty. The skill's row is locked first, and a save of a list
// naming it locks the row too, so a delete and such a save cannot both
// succeed. A missing skill is ErrSkillNotFound.
func (s *Store) DeleteLibrarySkill(ctx context.Context, name string, configRepos []RepoRef) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := lockSkillForUpdate(ctx, tx, name); err != nil {
		return err
	}
	inUse := &SkillInUseError{}
	rows, err := tx.Query(ctx, `SELECT instance, project FROM dashboard_repos WHERE $1 = ANY (skills)
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
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repo_defaults WHERE $1 = ANY (skills))`, name).Scan(&inUse.InDefaults); err != nil {
		return err
	}
	if len(inUse.Repos) > 0 || inUse.InDefaults {
		return inUse
	}
	if _, err := tx.Exec(ctx, `DELETE FROM library_skills WHERE name = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockNamedSkills locks each named library skill FOR KEY SHARE, which holds
// off a concurrent delete until the transaction ends, and returns a
// *MissingSkillError (an ErrSkillNotFound) naming the first that is not
// there. The rows are locked in name order, so two savers never deadlock.
func lockNamedSkills(ctx context.Context, tx pgx.Tx, names []string) error {
	if len(names) == 0 {
		return nil
	}
	want := slices.Clone(names)
	slices.Sort(want)
	want = slices.Compact(want)
	rows, err := tx.Query(ctx, `SELECT name FROM library_skills WHERE name = ANY ($1) ORDER BY name FOR KEY SHARE`, want)
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
			return &MissingSkillError{Name: name}
		}
	}
	return nil
}

// ResolveLibrary reads the named skills, with their content, in one
// repeatable-read transaction, so a concurrent edit gives the old set or the
// new set and never a mix. Names that are not in the library are Missing,
// each once, in the order given. nil names are the default list.
func (s *Store) ResolveLibrary(ctx context.Context, names []string) (ResolvedLibrary, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ResolvedLibrary{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if names == nil {
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT skills FROM repo_defaults WHERE id = 1), '{}')`).Scan(&names); err != nil {
			return ResolvedLibrary{}, err
		}
	}
	out := ResolvedLibrary{Skills: map[string]LibrarySkill{}}
	rows, err := tx.Query(ctx, `SELECT name, version, content, octet_length(content), updated_at, updated_by, `+sourceColumns+`
		FROM library_skills WHERE name = ANY ($1)`, names)
	if err != nil {
		return ResolvedLibrary{}, err
	}
	for rows.Next() {
		var (
			k                              LibrarySkill
			kind                           *string
			inst, repo, url, ref, pth, cmt string
		)
		if err := rows.Scan(&k.Name, &k.Version, &k.Content, &k.Size, &k.UpdatedAt, &k.UpdatedBy,
			&kind, &inst, &repo, &url, &ref, &pth, &cmt); err != nil {
			rows.Close()
			return ResolvedLibrary{}, err
		}
		k.Source = scanSource(kind, inst, repo, url, ref, pth, cmt)
		out.Skills[k.Name] = k
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ResolvedLibrary{}, err
	}
	seen := map[string]bool{}
	for _, n := range names {
		if _, ok := out.Skills[n]; !ok && !seen[n] {
			out.Missing = append(out.Missing, n)
		}
		seen[n] = true
	}
	return out, nil
}

// LibrarySize is the total bytes of the named skills' text; a name that is
// not in the library counts for nothing. nil names are the default list.
func (s *Store) LibrarySize(ctx context.Context, names []string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(content)), 0)::int FROM library_skills
		WHERE name = ANY (CASE WHEN $1::text[] IS NULL
			THEN COALESCE((SELECT skills FROM repo_defaults WHERE id = 1), '{}') ELSE $1::text[] END)`, names).Scan(&n)
	return n, err
}
