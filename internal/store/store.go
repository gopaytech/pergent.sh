// Package store is the PostgreSQL store for runs, merge-request state and
// projects. It imports no other internal package.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is the pg_advisory_xact_lock key that serialises Migrate
// across replicas.
const migrationLockKey = 0x70657267 // "perg"

// Store is the handle on the database. It is safe for concurrent use.
type Store struct {
	pool   *pgxpool.Pool
	schema string // set only by OpenInSchema
}

// Open connects to the database at url and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	return open(ctx, url, "")
}

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// OpenInSchema is like Open but binds every connection to the given schema,
// creating it if needed. It exists so tests can isolate themselves; DropSchema
// removes the schema again.
func OpenInSchema(ctx context.Context, url, schema string) (*Store, error) {
	if !schemaName.MatchString(schema) {
		return nil, fmt.Errorf("store: invalid schema name %q", schema)
	}
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	_, err = admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize())
	admin.Close()
	if err != nil {
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	return open(ctx, url, schema)
}

func open(ctx context.Context, url, schema string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool, schema: schema}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// DropSchema drops the schema a store was opened with by OpenInSchema, and
// everything in it. It refuses on a store opened with Open.
func (s *Store) DropSchema(ctx context.Context) error {
	if s.schema == "" {
		return errors.New("store: not opened with OpenInSchema")
	}
	_, err := s.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s.schema}.Sanitize()+" CASCADE")
	return err
}

// Migrate applies the embedded migrations that have not run yet, in name
// order, each in its own transaction under an advisory lock, so concurrent
// callers (several replicas, a rollout) apply each one once.
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		sql, err := migrationFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return err
		}
		if err := s.applyMigration(ctx, version, name, string(sql)); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	prefix, _, _ := strings.Cut(name, "_")
	v, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("store: migration %q has no numeric prefix", name)
	}
	return v, nil
}

func (s *Store) applyMigration(ctx context.Context, version int, name, sql string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY)`); err != nil {
		return err
	}
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return nil
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
