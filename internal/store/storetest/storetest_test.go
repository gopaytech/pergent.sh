package storetest

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent.sh/internal/store"
)

var ctx = context.Background()

// open returns a copy and a connection to it, closed before the copy goes
// back to the pool.
func open(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	dbURL := URL(t, store.OpenInSchema)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/"), conn
}

func exec(t *testing.T, conn *pgx.Conn, q string) {
	t.Helper()
	if _, err := conn.Exec(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, conn *pgx.Conn, q string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// A copy holds every migration and no other rows.
func TestCopyIsMigrated(t *testing.T) {
	_, conn := open(t)
	files, err := filepath.Glob("../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations = %v, %v", files, err)
	}
	if n := count(t, conn, `SELECT count(*) FROM `+Schema+`.schema_migrations`); n != len(files) {
		t.Errorf("schema_migrations rows = %d, want %d", n, len(files))
	}
	if n := count(t, conn, `SELECT count(*) FROM `+Schema+`.accounts`); n != 0 {
		t.Errorf("accounts rows = %d, want 0", n)
	}
}

// Two copies share nothing: a row in one is not in the other.
func TestCopiesAreIsolated(t *testing.T) {
	a, connA := open(t)
	b, connB := open(t)
	if a == b {
		t.Fatalf("both copies are %s", a)
	}
	exec(t, connA, `INSERT INTO `+Schema+`.teams (name, role, created_at) VALUES ('devs', 'maintainer', now())`)
	if n := count(t, connB, `SELECT count(*) FROM `+Schema+`.teams`); n != 0 {
		t.Errorf("teams rows in b = %d, want 0", n)
	}
}

// After rows and sequences are used, a reset gives a fresh copy back.
func TestResetCleansData(t *testing.T) {
	name, conn := open(t)
	exec(t, conn, `INSERT INTO `+Schema+`.teams (name, role, created_at) VALUES ('devs', 'maintainer', now())`)
	exec(t, conn, `SELECT nextval('`+Schema+`.audit_log_id_seq')`)
	if err := shared.reset(ctx, name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n := count(t, conn, `SELECT count(*) FROM `+Schema+`.teams`); n != 0 {
		t.Errorf("teams rows after reset = %d, want 0", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM pg_sequences WHERE last_value IS NOT NULL`); n != 0 {
		t.Errorf("used sequences after reset = %d, want 0", n)
	}
	// The identity starts again, as in a fresh copy.
	var id int64
	if err := conn.QueryRow(ctx, `INSERT INTO `+Schema+`.teams (name, role, created_at)
		VALUES ('ops', 'maintainer', now()) RETURNING id`).Scan(&id); err != nil || id != 1 {
		t.Errorf("first team id after reset = %d, %v; want 1", id, err)
	}
}

// A copy a test changed in a way a reset cannot undo is never reused.
func TestResetRefusesChangedCopy(t *testing.T) {
	for _, change := range []string{
		`ALTER TABLE ` + Schema + `.teams ADD COLUMN x int`,
		`ALTER TABLE ` + Schema + `.teams DROP COLUMN role`,
		`CREATE INDEX teams_x ON ` + Schema + `.teams (created_at)`,
		`CREATE TABLE public.extra (id int)`,
		`CREATE FUNCTION ` + Schema + `.f() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`DELETE FROM ` + Schema + `.schema_migrations WHERE version = 1`,
		`INSERT INTO ` + Schema + `.teams (name, role, created_at) SELECT 't' || g, 'maintainer', now()
			FROM generate_series(1, 200) g; ANALYZE ` + Schema + `.teams`,
	} {
		t.Run(change, func(t *testing.T) {
			name, conn := open(t)
			exec(t, conn, change)
			if err := shared.reset(ctx, name); err == nil {
				t.Errorf("reset after %q = nil, want an error", change)
			}
		})
	}
}

func TestWithDatabase(t *testing.T) {
	for _, c := range []struct{ base, want string }{
		{"postgres://u:p@h:5432/db?sslmode=disable", "postgres://u:p@h:5432/x?sslmode=disable"},
		{"postgresql://h/db", "postgresql://h/x"},
		{"host=h dbname=db", "host=h dbname=db dbname=x"},
	} {
		got, err := withDatabase(c.base, "x")
		if err != nil || got != c.want {
			t.Errorf("withDatabase(%q) = %q, %v; want %q", c.base, got, err, c.want)
		}
	}
}
