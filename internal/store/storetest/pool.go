package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The names of the databases of this package. Each ends in random hex.
const (
	prefixFree  = "pergent_f_" // + template hash + "_": free copies
	prefixUsed  = "pergent_u_" // + owner id + "_": copies a binary uses
	prefixDirty = "pergent_d_" // copies to drop
)

// maxFree is the most free copies the template keeps. A copy over it is
// dropped. It is a little above the most copies a full run of all packages
// uses at one time, so the pool holds a few hundred MB at most.
const maxFree = 24

// template is a migrated template database.
type template struct {
	name        string
	fingerprint string   // a fresh copy has this fingerprint
	keep        []string // the tables the template holds rows in
}

func (t template) hash() string { return strings.TrimPrefix(t.name, "pergent_tmpl_") }

// String is the comment the template keeps its fingerprint in.
func (t template) String() string {
	return strings.Join(append([]string{"storetest " + recipe, t.fingerprint}, t.keep...), "\n")
}

func parseTemplate(name, comment string) (template, error) {
	lines := strings.Split(comment, "\n")
	if len(lines) < 2 || lines[0] != "storetest "+recipe {
		return template{}, fmt.Errorf("template %s has an unknown comment", name)
	}
	return template{name: name, fingerprint: lines[1], keep: lines[2:]}, nil
}

// claim takes a free copy of the template, or makes a new one, and returns
// its name. A free copy is taken by renaming it: when two binaries rename
// one copy, one of them fails and tries the next.
func (st *state) claim(ctx context.Context) (string, error) {
	used := fmt.Sprintf("%s%08x_%s", prefixUsed, st.owner, hex.EncodeToString(randomBytes(8)))
	free := prefixFree + st.tmpl.hash() + "_"
	for range 5 {
		var name string
		err := st.admin.QueryRow(ctx, `SELECT datname FROM pg_database WHERE starts_with(datname, $1)
			ORDER BY random() LIMIT 1`, free).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return "", err
		}
		if _, err := st.admin.Exec(ctx, "ALTER DATABASE "+ident(name)+" RENAME TO "+ident(used)); err == nil {
			return used, nil
		}
	}
	if _, err := st.admin.Exec(ctx, "CREATE DATABASE "+ident(used)+" TEMPLATE "+ident(st.tmpl.name)); err != nil {
		return "", fmt.Errorf("create database: %w", err)
	}
	return used, nil
}

// release resets the copy name and puts it back in the pool, or drops it
// when the reset cannot make it equal to a fresh copy.
func (st *state) release(name string) {
	ctx := context.Background()
	// A session the test left open could hold a lock the reset waits for.
	st.endSessions(ctx, name)
	clean := st.reset(ctx, name) == nil
	target := prefixDirty + hex.EncodeToString(randomBytes(8))
	if clean {
		var n int
		err := st.admin.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE starts_with(datname, $1)`,
			prefixFree+st.tmpl.hash()+"_").Scan(&n)
		if err == nil && n < maxFree {
			target = prefixFree + st.tmpl.hash() + "_" + hex.EncodeToString(randomBytes(8))
		}
	}
	if err := st.rename(ctx, name, target); err != nil {
		st.dropLater(name)
		return
	}
	if strings.HasPrefix(target, prefixDirty) {
		st.dropLater(target)
	}
}

// rename renames a database. It first ends the sessions still connected to
// it, as a rename needs none.
func (st *state) rename(ctx context.Context, from, to string) error {
	var err error
	for range 50 {
		st.endSessions(ctx, from)
		if _, err = st.admin.Exec(ctx, "ALTER DATABASE "+ident(from)+" RENAME TO "+ident(to)); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// endSessions ends the sessions connected to the database name.
func (st *state) endSessions(ctx context.Context, name string) {
	_, _ = st.admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
}

// dropLater queues a database for the background worker to drop. When the
// queue is full, or the binary exits first, a later binary drops it.
func (st *state) dropLater(name string) {
	select {
	case st.drops <- name:
	default:
	}
}

// reap runs in the background for the life of the binary. It drops the
// dirty copies, the ones there now and then the ones tests leave.
func (st *state) reap() {
	ctx := context.Background()
	rows, err := st.admin.Query(ctx, `SELECT datname FROM pg_database WHERE starts_with(datname, $1)`, prefixDirty)
	if err == nil {
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err == nil {
			for _, name := range names {
				st.drop(ctx, name)
			}
		}
	}
	for name := range st.drops {
		st.drop(ctx, name)
	}
}

// retire marks as dirty what must not stay on the server: the templates,
// builds and free copies of other migration hashes than hash, the free copies
// of hash over maxFree, and the copies of binaries that are not running any
// more (their owner lock is free). A copy whose owner lock is held stays. It
// returns the new dirty names, for the caller to drop. The caller holds the
// build lock on conn, so no build runs meanwhile.
func (st *state) retire(ctx context.Context, conn *pgxpool.Conn, hash string) ([]string, error) {
	rows, err := conn.Query(ctx, `SELECT datname FROM pg_database
		WHERE starts_with(datname, 'pergent_tmpl_') OR starts_with(datname, 'pergent_build_')
			OR starts_with(datname, $1) OR starts_with(datname, $2)
		ORDER BY datname`, prefixFree, prefixUsed)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var stale []string
	free := 0
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, "pergent_tmpl_"):
			if strings.TrimPrefix(name, "pergent_tmpl_") != hash {
				stale = append(stale, name)
			}
		case strings.HasPrefix(name, "pergent_build_"):
			if strings.TrimPrefix(name, "pergent_build_") != hash {
				stale = append(stale, name)
			}
		case strings.HasPrefix(name, prefixFree):
			h, _, _ := strings.Cut(strings.TrimPrefix(name, prefixFree), "_")
			if h != hash {
				stale = append(stale, name)
			} else if free++; free > maxFree {
				stale = append(stale, name)
			}
		default:
			dead, err := st.deadOwner(ctx, conn, name)
			if err != nil {
				return nil, err
			}
			if dead {
				stale = append(stale, name)
			}
		}
	}
	var out []string
	for _, name := range stale {
		dirty := prefixDirty + hex.EncodeToString(randomBytes(8))
		st.endSessions(ctx, name)
		// A rename fails when another binary claimed the copy first: it
		// stays theirs.
		if _, err := conn.Exec(ctx, "ALTER DATABASE "+ident(name)+" RENAME TO "+ident(dirty)); err == nil {
			out = append(out, dirty)
		}
	}
	return out, nil
}

// deadOwner reports whether the copy name, in use by a binary, has an owner
// that is not running any more: its owner lock is free.
func (st *state) deadOwner(ctx context.Context, conn *pgxpool.Conn, name string) (bool, error) {
	id, _, _ := strings.Cut(strings.TrimPrefix(name, prefixUsed), "_")
	owner, err := strconv.ParseInt(id, 16, 32)
	if err != nil || int32(owner) == st.owner {
		return false, nil
	}
	var free bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, lockClass, int32(owner)).Scan(&free); err != nil {
		return false, err
	}
	if free {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, lockClass, int32(owner)); err != nil {
			return false, err
		}
	}
	return free, nil
}

// drop drops the database name. A retired template must stop being a
// template first.
func (st *state) drop(ctx context.Context, name string) {
	_, _ = st.admin.Exec(ctx, "ALTER DATABASE "+ident(name)+" WITH IS_TEMPLATE false")
	_, _ = st.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(name)+" WITH (FORCE)")
}

// reset makes the copy name equal to a fresh copy of the template, and
// fails when it cannot: the copy is then dropped. It truncates the tables
// the test wrote to, restarts the sequences it used and clears the
// statistics counters. Then the fingerprint of the copy must equal the
// fingerprint of the template.
func (st *state) reset(ctx context.Context, name string) error {
	dbURL, err := withDatabase(st.base, name)
	if err != nil {
		return err
	}
	conn, err := connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	// A table that was never written to has no pages; nor have a table and
	// its indexes after TRUNCATE. The fingerprint holds the size of each
	// relation, so a table this misses makes the copy dirty, not wrong.
	rows, err := conn.Query(ctx, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE `+userSchema+` AND c.relkind = 'r' AND pg_relation_size(c.oid) > 0`)
	if err != nil {
		return err
	}
	written, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	written = slices.DeleteFunc(written, func(t string) bool { return slices.Contains(st.tmpl.keep, t) })
	rows, err = conn.Query(ctx, `SELECT format('%I.%I', schemaname, sequencename) FROM pg_sequences
		WHERE last_value IS NOT NULL`)
	if err != nil {
		return err
	}
	used, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var q []string
	if len(written) > 0 {
		// CASCADE also truncates the empty tables that point at these.
		q = append(q, "TRUNCATE "+strings.Join(written, ", ")+" RESTART IDENTITY CASCADE")
	}
	for _, s := range used {
		q = append(q, "ALTER SEQUENCE "+s+" RESTART")
	}
	q = append(q, "SELECT pg_stat_reset()")
	if _, err := conn.Exec(ctx, strings.Join(q, "; ")); err != nil {
		return err
	}
	fp, err := fingerprintConn(ctx, conn, st.tmpl.keep)
	if err != nil {
		return err
	}
	if fp != st.tmpl.fingerprint {
		return errors.New("the copy differs from the template")
	}
	return nil
}

// connect opens one connection with a search path that holds only
// pg_catalog, so every name the fingerprint prints has its schema.
func connect(ctx context.Context, dbURL string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	// A reset that waits on a lock fails, and the copy is dropped.
	cfg.RuntimeParams["lock_timeout"] = "10s"
	return pgx.ConnectConfig(ctx, cfg)
}

// userSchema matches the schemas of pg_namespace n that are not the system's.
const userSchema = `n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'`

// tablesWithRows lists the tables of the database at dbURL that hold rows.
func tablesWithRows(ctx context.Context, dbURL string) ([]string, error) {
	conn, err := connect(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE `+userSchema+` AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tables {
		var has bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+t+")").Scan(&has); err != nil {
			return nil, err
		}
		if has {
			out = append(out, t)
		}
	}
	return out, nil
}

func fingerprint(ctx context.Context, dbURL string, keep []string) (string, error) {
	conn, err := connect(ctx, dbURL)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	return fingerprintConn(ctx, conn, keep)
}

// fingerprintConn hashes what a test could change in a database: the
// schemas and every object in them, the size of each relation, the state of
// each sequence, the planner statistics, the extensions, the large objects,
// and the rows of the tables in keep, in their physical order.
func fingerprintConn(ctx context.Context, conn *pgx.Conn, keep []string) (string, error) {
	var catalog string
	if err := conn.QueryRow(ctx, fingerprintSQL).Scan(&catalog); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(catalog))
	for _, t := range keep {
		var rows string
		if err := conn.QueryRow(ctx, `SELECT coalesce(string_agg(t.ctid::text || ' ' || t::text, E'\n' ORDER BY t.ctid), '')
			FROM `+t+` t`).Scan(&rows); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "\x00%s\x00%s", t, rows)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const fingerprintSQL = `
WITH ns AS (
	SELECT n.oid, n.nspname FROM pg_namespace n WHERE ` + userSchema + `
), rel AS (
	SELECT c.oid, ns.nspname, c.relname, c.relkind, c.relpersistence, c.reloptions, c.relacl, c.reltoastrelid
	FROM pg_class c JOIN ns ON ns.oid = c.relnamespace
), item(x) AS (
	SELECT 'n ' || nspname FROM ns
	UNION ALL
	SELECT format('r %s.%s %s %s %s %s', nspname, relname, relkind, relpersistence, reloptions, relacl) FROM rel
	UNION ALL
	SELECT format('z %s.%s %s', nspname, relname, pg_relation_size(oid)) FROM rel WHERE relkind IN ('r', 'i', 'S', 'm')
	UNION ALL
	SELECT format('z toast of %s.%s %s', nspname, relname, pg_relation_size(reltoastrelid)) FROM rel WHERE reltoastrelid <> 0
	UNION ALL
	SELECT format('a %s.%s %s %s %s %s %s %s %s %s', r.nspname, r.relname, a.attnum, a.attname,
		format_type(a.atttypid, a.atttypmod), a.attnotnull, a.attisdropped, a.attidentity, a.attgenerated,
		pg_get_expr(d.adbin, d.adrelid))
	FROM pg_attribute a JOIN rel r ON r.oid = a.attrelid
	LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	WHERE a.attnum > 0
	UNION ALL
	SELECT format('c %s.%s %s %s %s', ns.nspname, co.conname, co.conrelid::regclass, co.contypid::regtype,
		pg_get_constraintdef(co.oid))
	FROM pg_constraint co JOIN ns ON ns.oid = co.connamespace
	UNION ALL
	SELECT 'i ' || pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN rel r ON r.oid = i.indexrelid
	UNION ALL
	SELECT 't ' || pg_get_triggerdef(tg.oid) FROM pg_trigger tg JOIN rel r ON r.oid = tg.tgrelid
	WHERE NOT tg.tgisinternal
	UNION ALL
	SELECT format('w %s %s', ru.rulename, ru.ev_class::regclass) FROM pg_rewrite ru JOIN rel r ON r.oid = ru.ev_class
	WHERE ru.rulename <> '_RETURN'
	UNION ALL
	SELECT format('v %s.%s %s', nspname, relname, pg_get_viewdef(oid)) FROM rel WHERE relkind IN ('v', 'm')
	UNION ALL
	SELECT format('f %s.%s(%s) %s %s %s', ns.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
		p.prokind, pg_get_function_result(p.oid), md5(coalesce(p.prosrc, '')))
	FROM pg_proc p JOIN ns ON ns.oid = p.pronamespace
	UNION ALL
	SELECT format('y %s.%s %s %s %s %s', ns.nspname, ty.typname, ty.typtype,
		format_type(ty.typbasetype, ty.typtypmod), ty.typdefault,
		(SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = ty.oid))
	FROM pg_type ty JOIN ns ON ns.oid = ty.typnamespace
	WHERE ty.typtype IN ('e', 'd', 'r', 'm')
	UNION ALL
	SELECT format('s %s.%s %s %s %s %s %s %s %s', schemaname, sequencename, data_type, start_value, min_value,
		max_value, increment_by, cycle, last_value)
	FROM pg_sequences
	UNION ALL
	SELECT format('p %s %s %s', pol.polname, pol.polrelid::regclass, pg_get_expr(pol.polqual, pol.polrelid))
	FROM pg_policy pol
	UNION ALL
	SELECT format('x %s.%s %s', r.nspname, r.relname, count(*)) FROM pg_statistic s JOIN rel r ON r.oid = s.starelid
	GROUP BY r.nspname, r.relname
	UNION ALL
	SELECT 'X ' || count(*) FROM pg_statistic_ext
	UNION ALL
	SELECT format('e %s %s', extname, extversion) FROM pg_extension
	UNION ALL
	SELECT 'l ' || count(*) FROM pg_largeobject_metadata
)
SELECT md5(string_agg(x, E'\n' ORDER BY x COLLATE "C")) FROM item`
