package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The review panel's code is gone, but a main build may have left its rows
// (WaitPanel runs and panel_members). These helpers write such rows with
// plain SQL, as that build did, so the legacy recovery and the code that
// must leave them alone can be tested.

// sqlState is the Postgres error code of err, or "".
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// heldRun gives a queued run of head on mr held for reason.
func heldRun(t *testing.T, s *Store, mr MRKey, head, reason string) string {
	t.Helper()
	id := enqueue(t, s, mr, head)
	if ok, err := s.HoldRun(ctx, id, reason); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	return id
}

// legacyMember is one panel_members row.
type legacyMember struct{ ID, Role, State string }

// legacyPanelRun gives a claimed run of head on mr made a legacy panel
// review, as a main build left it: the run waits (WaitPanel), dispatched at
// t0, and members a and b are queued.
func legacyPanelRun(t *testing.T, s *Store, mr MRKey, head string) string {
	t.Helper()
	id := enqueue(t, s, mr, head)
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2, dispatched_at = $3,
		base_sha = 'base-sha', start_sha = 'start-sha'
		WHERE id = $1::uuid AND state = 'running'`, id, WaitPanel, t0)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("make run %s wait for a panel: %v, %d rows", id, err, tag.RowsAffected())
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_members (run_id, role) VALUES ($1::uuid, 'a'), ($1::uuid, 'b')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// legacyMembers is every panel member of the review whose first run is
// root, oldest first.
func legacyMembers(t *testing.T, s *Store, root string) []legacyMember {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT id::text, role, state FROM panel_members WHERE run_id = $1::uuid
		ORDER BY created_at, id`, root)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []legacyMember
	for rows.Next() {
		var m legacyMember
		if err := rows.Scan(&m.ID, &m.Role, &m.State); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// legacyMemberOf is the newest member of role of the review whose first run
// is root.
func legacyMemberOf(t *testing.T, s *Store, root, role string) legacyMember {
	t.Helper()
	var out legacyMember
	for _, m := range legacyMembers(t, s, root) {
		if m.Role == role && m.State != "off" {
			out = m
		}
	}
	if out.ID == "" {
		t.Fatalf("no legacy member %s of %s", role, root)
	}
	return out
}

// runLegacyMember gives member id a running Job with the token, valid for an
// hour after t0, as that build's dispatch did.
func runLegacyMember(t *testing.T, s *Store, id, token string) {
	t.Helper()
	tag, err := s.pool.Exec(ctx, `UPDATE panel_members SET state = 'running', claimed_at = $2, job_name = $3,
		token_hash = $4, token_expires_at = $5, dispatched_at = $2 WHERE id = $1::uuid`,
		id, t0, "job-"+token, hash(token), t0.Add(time.Hour))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("run legacy member %s: %v", id, err)
	}
}

// doneLegacyMember gives member id a stored result, as a settled member had.
func doneLegacyMember(t *testing.T, s *Store, id, body string) {
	t.Helper()
	tag, err := s.pool.Exec(ctx, `UPDATE panel_members SET state = 'done', result = $2::jsonb, reported_at = $3
		WHERE id = $1::uuid`, id, body, t0.Add(time.Minute))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("finish legacy member %s: %v", id, err)
	}
}

// legacyPanelOff gives the review of run id the off row a main build wrote
// when its panel could not start.
func legacyPanelOff(t *testing.T, s *Store, id, reason string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_members (run_id, role, state, failure_reason, claimed_at)
		SELECT COALESCE(attempt_of, id), 'a', 'off', $2, claimed_at FROM runs WHERE id = $1::uuid`, id, reason); err != nil {
		t.Fatal(err)
	}
}
