// Package storetest gives each test its own migrated database, fast.
//
// The first test in a test binary makes sure a template database exists: a
// database with every migration applied once, in the schema Schema. A test
// then gets a database of its own, a copy of that template, and is the only
// user of it until the test ends.
//
// Copies are used again. In PostgreSQL 16 DROP DATABASE forces a checkpoint,
// which takes seconds on a busy server, so dropping a copy after each test
// is slow. When a test ends, the copy is reset instead: the tables the test
// wrote to are truncated and the sequences restarted. A fingerprint of the
// copy (its schema, the size of each relation, the planner statistics and
// the rows of the tables the template holds rows in) must then equal the
// fingerprint of the template. Only then does the copy go back to the pool of
// free copies. A copy whose fingerprint differs (a test changed the schema,
// say) is dropped in the background.
//
// The pool is shared by all test binaries, also across runs: a copy is
// claimed by renaming it, and only one session can rename a database. A
// binary holds a session advisory lock while it runs, so a later binary can
// tell a copy that a stopped binary left behind and drop it.
//
// The template name holds a hash of the migration files, so a changed or new
// migration builds a new template, with a pool of its own.
//
// The package does not import package store, so the tests of package store
// can use it too. A caller passes its open function, for example
// store.OpenInSchema.
package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL names the variable that holds the URL of the test database. Tests
// skip when it is empty.
const EnvURL = "PERGENT_SH_TEST_DATABASE_URL"

// Schema is the schema the template is migrated in, and so the schema every
// copy holds its tables in.
const Schema = "test_store"

// recipe changes when the way a template is built or checked changes, so a
// template from an older recipe is not used.
const recipe = "v1"

// lockClass is the first key of the two-key session advisory locks of this
// package. The second key is 0 for the template build, or the id of a
// running binary.
const lockClass = 0x70657274 // "pert"

// Store is what a template build needs from a store.
type Store interface {
	Migrate(ctx context.Context) error
	Close()
}

// OpenFunc opens a store bound to schema in the database at url, creating
// the schema if needed. store.OpenInSchema is one.
type OpenFunc[S Store] func(ctx context.Context, url, schema string) (S, error)

// state is what one test binary knows about the database.
type state struct {
	base  string        // the URL in EnvURL
	admin *pgxpool.Pool // on the database of base
	owner int32         // the id of this binary
	tmpl  template
	drops chan string // copies to drop in the background
}

var (
	mu       sync.Mutex
	shared   *state
	setupErr error
)

// Open returns a store on a database of its own, with every migration
// applied. It closes the store when the test ends. A test may close the
// store itself. Open skips the test when EnvURL is not set.
func Open[S Store](t testing.TB, open OpenFunc[S]) S {
	t.Helper()
	dbURL := URL(t, open)
	s, err := open(context.Background(), PoolURL(dbURL), Schema)
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// URL returns the URL of a database of its own, with every migration applied
// in the schema Schema. The database goes back to the pool when the test
// ends, after the cleanups the test adds later, so the caller closes its own
// connections in a later cleanup. Connections still open then are ended.
// URL skips the test when EnvURL is not set.
func URL[S Store](t testing.TB, open OpenFunc[S]) string {
	t.Helper()
	base := os.Getenv(EnvURL)
	if base == "" {
		t.Skipf("%s is not set", EnvURL)
	}
	ctx := context.Background()
	st, err := setup(ctx, base, open)
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	name, err := st.claim(ctx)
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	t.Cleanup(func() { st.release(name) })
	u, err := withDatabase(base, name)
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	return u
}

// setup connects, takes this binary's owner lock and makes sure the
// template is there, once per test binary.
func setup[S Store](ctx context.Context, base string, open OpenFunc[S]) (*state, error) {
	mu.Lock()
	defer mu.Unlock()
	if shared != nil || setupErr != nil {
		return shared, setupErr
	}
	st, err := newState(ctx, base, open)
	if err != nil {
		setupErr = err
		return nil, err
	}
	shared = st
	go st.reap()
	return st, nil
}

func newState[S Store](ctx context.Context, base string, open OpenFunc[S]) (*state, error) {
	cfg, err := pgxpool.ParseConfig(base)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvURL, err)
	}
	cfg.MaxConns = 8
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	st := &state{base: base, admin: admin, drops: make(chan string, 1024)}
	// The owner lock lives as long as this connection, which is never
	// released: it ends when the binary exits.
	conn, err := admin.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	for {
		st.owner = int32(binary.BigEndian.Uint32(randomBytes(4)) &^ (1 << 31))
		if st.owner == 0 {
			continue
		}
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, lockClass, st.owner).Scan(&ok); err != nil {
			conn.Release()
			return nil, err
		}
		if ok {
			break
		}
	}
	hash, err := migrationsHash()
	if err != nil {
		return nil, err
	}
	if st.tmpl, err = loadTemplate(ctx, st, "pergent_tmpl_"+hash, open); err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}
	return st, nil
}

// loadTemplate returns the template, building it first if it is not there.
// It holds a session advisory lock, so one build runs at a time across
// binaries.
func loadTemplate[S Store](ctx context.Context, st *state, name string, open OpenFunc[S]) (template, error) {
	tmpl := template{name: name}
	conn, err := st.admin.Acquire(ctx)
	if err != nil {
		return tmpl, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, 0)`, lockClass); err != nil {
		return tmpl, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, 0)`, lockClass) //nolint:errcheck // the session ends at exit

	tmpl, err = findOrBuild(ctx, st, conn, name, open)
	if err != nil {
		return tmpl, err
	}
	// The disk of the test server is small: what older migrations left
	// goes now, under the same lock.
	retired, err := st.retire(ctx, conn, tmpl.hash())
	if err != nil {
		return tmpl, fmt.Errorf("retire: %w", err)
	}
	for _, d := range retired {
		st.dropLater(d)
	}
	return tmpl, nil
}

// findOrBuild returns the template name, and builds it first when it is not
// there. The caller holds the build lock on conn.
func findOrBuild[S Store](ctx context.Context, st *state, conn *pgxpool.Conn, name string, open OpenFunc[S]) (template, error) {
	tmpl := template{name: name}
	var comment *string
	err := conn.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, name).Scan(&comment)
	if err == nil {
		if comment == nil {
			return tmpl, fmt.Errorf("template %s has no fingerprint", name)
		}
		return parseTemplate(name, *comment)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return tmpl, err
	}
	// Build under another name and rename at the end, so a build that was
	// stopped half way leaves no template that looks ready.
	tmp := "pergent_build_" + strings.TrimPrefix(name, "pergent_tmpl_")
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(tmp)+" WITH (FORCE)"); err != nil {
		return tmpl, err
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident(tmp)); err != nil {
		return tmpl, err
	}
	tmpURL, err := withDatabase(st.base, tmp)
	if err != nil {
		return tmpl, err
	}
	if err := migrate(ctx, tmpURL, open); err != nil {
		return tmpl, fmt.Errorf("migrate: %w", err)
	}
	if tmpl.keep, err = tablesWithRows(ctx, tmpURL); err != nil {
		return tmpl, err
	}
	if tmpl.fingerprint, err = fingerprint(ctx, tmpURL, tmpl.keep); err != nil {
		return tmpl, err
	}
	// Nobody may connect to a template: CREATE DATABASE fails while another
	// session is connected to its template.
	for _, q := range []string{
		"ALTER DATABASE " + ident(tmp) + " RENAME TO " + ident(name),
		"ALTER DATABASE " + ident(name) + " WITH IS_TEMPLATE true ALLOW_CONNECTIONS false",
		"COMMENT ON DATABASE " + ident(name) + " IS " + quote(tmpl.String()),
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			return tmpl, err
		}
	}
	return tmpl, nil
}

// migrate opens a store with open, migrates it and closes it.
func migrate[S Store](ctx context.Context, dbURL string, open OpenFunc[S]) error {
	s, err := open(ctx, dbURL, Schema)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Migrate(ctx)
}

// migrationsHash hashes the names and contents of the migration files, so a
// changed migration gets a new template.
func migrationsHash() (string, error) {
	dir, err := migrationsDir()
	if err != nil {
		return "", err
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no migrations in %s", dir)
	}
	sort.Strings(names)
	h := sha256.New()
	h.Write([]byte(recipe + "\x00" + Schema + "\x00"))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.Base(n), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// migrationsDir finds internal/store/migrations: next to this file, or else
// under the module root above the working directory (a build with -trimpath
// has no real path for this file).
func migrationsDir() (string, error) {
	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Join(filepath.Dir(file), "..", "migrations")
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			dir := filepath.Join(d, "internal", "store", "migrations")
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				return dir, nil
			}
			return "", fmt.Errorf("no %s", dir)
		}
		if filepath.Dir(d) == d {
			return "", errors.New("no go.mod above the working directory")
		}
	}
}

// PoolMaxConns is the most connections a test's store pool opens. Tests run
// in parallel, and several packages at once, so pools stay small to keep the
// server under its max_connections. It is what pgxpool picks on a 4-CPU CI
// runner anyway.
const PoolMaxConns = 4

// PoolURL is dbURL with the pool size set to PoolMaxConns. Only pgxpool reads
// that setting: give pgx.Connect the URL from URL as it is.
func PoolURL(dbURL string) string {
	param := "pool_max_conns=" + strconv.Itoa(PoolMaxConns)
	switch {
	case !strings.HasPrefix(dbURL, "postgres://") && !strings.HasPrefix(dbURL, "postgresql://"):
		return dbURL + " " + param
	case strings.Contains(dbURL, "?"):
		return dbURL + "&" + param
	default:
		return dbURL + "?" + param
	}
}

// withDatabase returns base with its database set to name. base is a URL
// or a keyword/value string.
func withDatabase(base, name string) (string, error) {
	if strings.HasPrefix(base, "postgres://") || strings.HasPrefix(base, "postgresql://") {
		u, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", EnvURL, err)
		}
		u.Path, u.RawPath = "/"+name, ""
		return u.String(), nil
	}
	// In a keyword/value string the last value of a keyword wins.
	return base + " dbname=" + name, nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
