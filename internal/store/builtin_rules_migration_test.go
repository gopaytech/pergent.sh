package store

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/rule"
)

// migration0050Names is the list 0050 gives a repository whose switch was
// on, read from the file.
func migration0050Names(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("migrations/0050_built_in_rule_names.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)ARRAY\[(.*?)\]`).FindSubmatch(b)
	if m == nil {
		t.Fatal("0050 has no ARRAY[...] of names")
	}
	var names []string
	for _, q := range regexp.MustCompile(`'([^']*)'`).FindAllSubmatch(m[1], -1) {
		names = append(names, string(q[1]))
	}
	return names
}

// 0050 gives a repository that had the switch on every built-in rule of
// pergent v0.11.0, the pinned release when it was written, in builtin.yaml
// order. A later pergent may add rules; it must keep these names, in this
// order, or a migrated repository loses one without saying.
func TestBuiltInRuleNamesMigrationList(t *testing.T) {
	var current []string
	for _, e := range rule.Builtin() {
		if !slices.Contains(current, e.Name) {
			current = append(current, e.Name)
		}
	}
	names := migration0050Names(t)
	if len(names) != 67 {
		t.Fatalf("0050 names %d built-in rules, want the 67 of pergent v0.11.0", len(names))
	}
	last := -1
	for _, n := range names {
		i := slices.Index(current, n)
		if i < 0 {
			t.Errorf("0050 names %q, which is no built-in rule now", n)
			continue
		}
		if i <= last {
			t.Errorf("0050 names %q out of builtin.yaml order, or twice", n)
		}
		last = i
	}
}

// 0050 only adds columns and fills the new list from the old switch: true
// gets every name, false and NULL none, and nothing else changes.
func TestBuiltInRuleNamesMigration(t *testing.T) {
	text, err := os.ReadFile("migrations/0050_built_in_rule_names.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(text), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`,
		`(?i)\bALTER\s+COLUMN\b`, `(?i)\bNOT\s+NULL\b`} {
		if regexp.MustCompile(bad).MatchString(sql.String()) {
			t.Errorf("migration 0050 matches %s; it may only add", bad)
		}
	}

	s := newStoreAt(t, 49)
	if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, built_in_rules) VALUES
		('main', 'acme/on', true), ('main', 'acme/off', false), ('main', 'acme/unset', NULL)`); err != nil {
		t.Fatal(err)
	}
	before := schemaColumns(t, s)
	applyUpTo(t, s, 50)
	after := schemaColumns(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0050 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for _, col := range []string{"dashboard_repos.built_in_rule_names", "runs.built_in_rules_note"} {
		if c, ok := after[col]; !ok || c.Nullable != "YES" {
			t.Errorf("column %s after 0050 = %+v (present %v); want a nullable column", col, c, ok)
		}
	}
	want := map[string][]string{"acme/on": migration0050Names(t), "acme/off": nil, "acme/unset": nil}
	for project, names := range want {
		var got []string
		var old *bool
		if err := s.pool.QueryRow(ctx, `SELECT built_in_rule_names, built_in_rules FROM dashboard_repos WHERE project = $1`, project).
			Scan(&got, &old); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, names) || (names == nil && got != nil) {
			t.Errorf("%s built_in_rule_names = %v, want %v", project, got, names)
		}
		if project == "acme/on" && (old == nil || !*old) {
			t.Errorf("%s built_in_rules = %v; the old switch must stay", project, old)
		}
	}
}

// The previous release writes the old switches and reads its own columns on
// a migrated database.
func TestBuiltInRuleNamesMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 49)
	applyUpTo(t, s, 50)
	for _, q := range []string{
		`INSERT INTO dashboard_repos (instance, project, created_by, rule_sets, built_in_rules) VALUES ('main', 'acme/api', 'alice', NULL, true)`,
		`UPDATE dashboard_repos SET built_in_rules = false WHERE project = 'acme/api'`,
		`INSERT INTO repo_defaults (id, built_in_rules, updated_at) VALUES (1, true, now())`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("0049-era %q: %v", q, err)
		}
	}
	var on, def bool
	if err := s.pool.QueryRow(ctx, `SELECT d.built_in_rules, r.built_in_rules FROM dashboard_repos d, repo_defaults r
		WHERE d.project = 'acme/api' AND r.id = 1`).Scan(&on, &def); err != nil || on || !def {
		t.Errorf("old switches = %v %v, %v; want false true", on, def, err)
	}
}
