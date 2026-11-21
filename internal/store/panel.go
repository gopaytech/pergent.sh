package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// A panel review is one run (Job P reports on it) and one panel_members row
// per attempt of member A or B. The members belong to the review's first run
// (run_id), so Job P's later attempts read the same members. While they run,
// the review's live run waits (state 'waiting', skip_reason WaitPanel): the
// previous release never claims it and ignores panel_members.
//
// Locks are taken in one order: the merge request's lock (lockMR), then the
// waiting run's row, then member rows. Nothing that holds a member row waits
// for a run row.

// MemberState is where a panel member is in its life: queued → running
// (claimed) → reported (a result or a failure) → done (it has a result) or
// failed; reported → fell_back when a quota, billing or auth error moves it to
// its next entry; any live state → superseded when its run is gone. Off is
// the one row of a review whose panel could not start; it never moves.
type MemberState string

const (
	MemberQueued     MemberState = "queued"
	MemberRunning    MemberState = "running"
	MemberReported   MemberState = "reported"
	MemberDone       MemberState = "done"
	MemberFailed     MemberState = "failed"
	MemberFellBack   MemberState = "fell_back"
	MemberSuperseded MemberState = "superseded"
	MemberOff        MemberState = "off"
)

// PanelMember is one attempt of member A or B of a panel review. RunID is the
// review's first run; AttemptOf is the role's first attempt (nil for it).
// FailureReason of an off row is why the panel could not start.
type PanelMember struct {
	ID, RunID, Role                                string
	AttemptOf                                      *string
	State                                          MemberState
	JobName                                        string
	TokenExpiresAt                                 time.Time
	Result                                         []byte
	FailureReason, FailureStep, FailureDetail      string
	PodName                                        string
	CheckoutSeen                                   int
	Provider, Model                                string
	Entry                                          RunEntry
	Attempts                                       int
	CreatedAt, DispatchedAt, ReportedAt, NotBefore time.Time
}

const memberColumns = `id::text, run_id::text, role, attempt_of::text, state, job_name, token_expires_at, result,
	failure_reason, failure_step, failure_detail, pod_name, checkout_seen, provider, model,
	model_source, model_entry_id, model_entry_name, COALESCE(model_team_id, 0), chain_position, chain_length,
	fallback_class, attempts, created_at, dispatched_at, reported_at, not_before`

func scanMember(row pgx.Row) (PanelMember, error) {
	var (
		m                                    PanelMember
		state                                string
		tokenExp, dispatched, rep, notBefore *time.Time
	)
	err := row.Scan(&m.ID, &m.RunID, &m.Role, &m.AttemptOf, &state, &m.JobName, &tokenExp, &m.Result,
		&m.FailureReason, &m.FailureStep, &m.FailureDetail, &m.PodName, &m.CheckoutSeen, &m.Provider, &m.Model,
		&m.Entry.Source, &m.Entry.ID, &m.Entry.Name, &m.Entry.TeamID, &m.Entry.Position, &m.Entry.Length,
		&m.Entry.FallbackClass, &m.Attempts, &m.CreatedAt, &dispatched, &rep, &notBefore)
	if err != nil {
		return PanelMember{}, err
	}
	m.State = MemberState(state)
	m.TokenExpiresAt = deref(tokenExp)
	m.DispatchedAt = deref(dispatched)
	m.ReportedAt = deref(rep)
	m.NotBefore = deref(notBefore)
	return m, nil
}

func (s *Store) queryMembers(ctx context.Context, sql string, args ...any) ([]PanelMember, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PanelMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// memberUpdate runs an UPDATE that has to touch exactly one member.
func (s *Store) memberUpdate(ctx context.Context, id, sql string, args ...any) error {
	if !validUUID(id) {
		return fmt.Errorf("store: panel member %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: panel member %s not found or in the wrong state", id)
	}
	return nil
}

// panelWaitsSQL is the condition of a run waiting for the members of review
// $1 (the review's first run): the first run or a later attempt of it.
const panelWaitsSQL = `(id = $1::uuid OR attempt_of = $1::uuid) AND state = 'waiting' AND skip_reason = 'panel_members'`

// memberRunWaitsSQL is the same for the member row m.
const memberRunWaitsSQL = `EXISTS (SELECT 1 FROM runs r WHERE (r.id = m.run_id OR r.attempt_of = m.run_id)
	AND r.state = 'waiting' AND r.skip_reason = 'panel_members')`

// lockWaitingRun locks the run waiting for the members of review root, and
// returns it; found is false when no run waits for them. The lock keeps
// member dispatches of one review apart and holds off a supersede.
func lockWaitingRun(ctx context.Context, tx pgx.Tx, root, mode string) (id string, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT id::text FROM runs WHERE `+panelWaitsSQL+` FOR `+mode, root).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// reviewOf returns the merge request and the review's first run of run id;
// found is false for an unknown run.
func reviewOf(ctx context.Context, tx pgx.Tx, id string) (mr MRKey, root string, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT instance, project_id, mr_iid, COALESCE(attempt_of, id)::text FROM runs WHERE id = $1::uuid`,
		id).Scan(&mr.Instance, &mr.ProjectID, &mr.IID, &root)
	if errors.Is(err, pgx.ErrNoRows) {
		return MRKey{}, "", false, nil
	}
	return mr, root, err == nil, err
}

// PanelSplit is what a run records when it is split into a panel: the merge
// request's base and start SHAs, which both members review, and its inputs
// as SetRunInputs records them for a review, which their results are
// checked against.
type PanelSplit struct {
	BaseSHA, StartSHA string
	Inputs            RunInputs
}

// StartPanel makes the claimed, not dispatched run runID a panel review: it
// waits (WaitPanel) with dispatched_at now and what split holds, and one
// queued member per role is added, in one transaction. started is false when the run is no longer
// claimed (superseded meanwhile), or was dispatched before: a run released
// for Job P keeps its dispatched_at, so a review is split once. The members belong to the review's first
// run, and the review's off row, from an earlier attempt, is dropped: the
// panel did start.
func (s *Store) StartPanel(ctx context.Context, runID string, split PanelSplit, now time.Time) (started bool, err error) {
	if !validUUID(runID) {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	mr, root, found, err := reviewOf(ctx, tx, runID)
	if err != nil || !found {
		return false, err
	}
	if err := lockMR(ctx, tx, mr); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2, dispatched_at = $3,
		base_sha = $4, start_sha = $5
		WHERE id = $1::uuid AND state = 'running' AND job_name = '' AND dispatched_at IS NULL`, runID, WaitPanel, now, split.BaseSHA, split.StartSHA)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if err := setRunInputs(ctx, tx, runID, split.Inputs); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM panel_members WHERE run_id = $1::uuid AND state = 'off'`, root); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO panel_members (run_id, role) VALUES ($1::uuid, 'a'), ($1::uuid, 'b')`, root); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// RecordPanelOff keeps why the run's panel could not start (its one 'off' row,
// of the review's first run). A second call replaces the reason. An unknown
// run is an error. Call it only when the review has no live members: the
// off row is for a review that runs as one review. StartPanel removes an off
// row; nothing here removes members.
func (s *Store) RecordPanelOff(ctx context.Context, runID, reason string) error {
	if !validUUID(runID) {
		return fmt.Errorf("store: run %q not found", runID)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO panel_members (run_id, role, state, failure_reason)
		SELECT COALESCE(attempt_of, id), 'a', 'off', $2 FROM runs WHERE id = $1::uuid
		ON CONFLICT (run_id) WHERE state = 'off' DO UPDATE SET failure_reason = EXCLUDED.failure_reason`,
		runID, cutText(reason))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found", runID)
	}
	return nil
}

// PanelMembers returns the members of the review of run id (its first run or
// a later attempt), its off row too, oldest first.
func (s *Store) PanelMembers(ctx context.Context, id string) ([]PanelMember, error) {
	if !validUUID(id) {
		return nil, nil
	}
	return s.queryMembers(ctx, `SELECT `+memberColumns+` FROM panel_members
		WHERE run_id = (SELECT COALESCE(attempt_of, id) FROM runs WHERE id = $1::uuid)
		ORDER BY created_at, id`, id)
}

// ClaimQueuedMembers moves up to limit queued members that are due at now,
// and whose run still waits for them, to running and returns them, oldest
// first. Concurrent callers never claim the same member. The members of runs
// of the instances in skip stay queued.
func (s *Store) ClaimQueuedMembers(ctx context.Context, limit int, now time.Time, skip ...string) ([]PanelMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	if skip == nil {
		skip = []string{} // an empty array, not NULL: `<> ALL(NULL)` is never true
	}
	// The rows are picked in a CTE, which runs once. As a subquery of the
	// UPDATE it can be scanned again, and SKIP LOCKED then skips a row this
	// statement already took, so LIMIT picks one more.
	return s.queryMembers(ctx, `WITH picked AS (
			SELECT m.id AS picked_id FROM panel_members m
			WHERE m.state = 'queued' AND (m.not_before IS NULL OR m.not_before <= $2)
			  AND EXISTS (SELECT 1 FROM runs r WHERE (r.id = m.run_id OR r.attempt_of = m.run_id)
				AND r.state = 'waiting' AND r.skip_reason = 'panel_members' AND r.instance <> ALL($3::text[]))
			ORDER BY m.created_at, m.id LIMIT $1 FOR UPDATE OF m SKIP LOCKED)
		UPDATE panel_members SET state = 'running', claimed_at = $2
		FROM picked WHERE id = picked_id AND state = 'queued'
		RETURNING `+memberColumns, limit, now, skip)
}

// PanelRun returns the run waiting for the members of review root (the
// review's first run, a member's RunID): root itself or a later attempt of
// it. found is false when no run waits for them.
func (s *Store) PanelRun(ctx context.Context, root string) (Run, bool, error) {
	if !validUUID(root) {
		return Run{}, false, nil
	}
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE `+panelWaitsSQL, root))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// SetMemberEntry records the entry, provider and model of a member being
// dispatched (queued or running), before its Job starts. Any other member is
// an error.
func (s *Store) SetMemberEntry(ctx context.Context, id string, e RunEntry, provider, model string) error {
	return s.memberUpdate(ctx, id, `UPDATE panel_members SET model_source = $2, model_entry_id = $3, model_entry_name = $4,
			model_team_id = NULLIF($5::bigint, 0), chain_position = $6, chain_length = $7, fallback_class = $8,
			provider = $9, model = $10
		WHERE id = $1::uuid AND state IN ('queued', 'running')`,
		id, e.Source, e.ID, cutText(e.Name), e.TeamID, e.Position, e.Length, e.FallbackClass, cutText(provider), cutText(model))
}

// MarkMemberDispatched records the member's Job and the SHA-256 of its result
// token, valid until expires; first is true when no other member of its
// review had a Job yet. ErrNotRunning when the member is not running or its
// run no longer waits for it.
func (s *Store) MarkMemberDispatched(ctx context.Context, id, jobName string, tokenHash []byte, expires, now time.Time) (first bool, err error) {
	if !validUUID(id) {
		return false, ErrNotRunning
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var root string
	err = tx.QueryRow(ctx, `SELECT run_id::text FROM panel_members WHERE id = $1::uuid`, id).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotRunning
	}
	if err != nil {
		return false, err
	}
	// The run's lock makes the 2 members' dispatches one after the other, so
	// only one of them is first.
	if _, found, err := lockWaitingRun(ctx, tx, root, "NO KEY UPDATE"); err != nil {
		return false, err
	} else if !found {
		return false, ErrNotRunning
	}
	tag, err := tx.Exec(ctx, `UPDATE panel_members SET job_name = $2, token_hash = $3, token_expires_at = $4, dispatched_at = $5
		WHERE id = $1::uuid AND state = 'running'`, id, jobName, tokenHash, expires, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrNotRunning
	}
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM panel_members
		WHERE run_id = $1::uuid AND id <> $2::uuid AND job_name <> '')`, root, id).Scan(&first); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return first, nil
}

// RequeueMember puts a claimed member back in the queue, not to be claimed
// before notBefore, and counts the attempt. A member that is not queued or
// running is an error.
func (s *Store) RequeueMember(ctx context.Context, id string, notBefore time.Time) error {
	return s.memberUpdate(ctx, id, `UPDATE panel_members SET state = 'queued', not_before = $2, attempts = attempts + 1
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
}

// PostponeMember puts a claimed member back in the queue, not to be claimed
// before notBefore, without counting an attempt. A member in another state,
// or an unknown id, is left alone and is not an error.
func (s *Store) PostponeMember(ctx context.Context, id string, notBefore time.Time) error {
	if !validUUID(id) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE panel_members SET state = 'queued', not_before = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
	return err
}

// StoreMemberResult accepts the result a member's Job posted on the route of
// run runID (the review's first run). The token is checked as StoreResult
// checks it: against the stored SHA-256 in constant time, not expired, and
// single use by the member's state. It is checked first, in the same one
// read for a member of another run, an unknown member and a known one: each
// refusal hashes the token and compares it. The outcome, in order:
//   - ResultUnauthorized: unknown member, a member of another run, no token,
//     wrong token or expired;
//   - ResultDiscarded: the member is superseded, or its run no longer waits
//     for it (superseded, or released for Job P: a late second post after
//     the release is discarded, not a conflict); the result is dropped;
//   - ResultConflict: the member already has a result or failure, or is not
//     running;
//   - ErrInvalidResult: body is not valid JSON; nothing is stored;
//   - ResultStored: the member is now reported.
func (s *Store) StoreMemberResult(ctx context.Context, runID, id, token string, body []byte, now time.Time) (ResultOutcome, error) {
	presented := sha256.Sum256([]byte(token))
	if !validUUID(id) || !validUUID(runID) {
		subtle.ConstantTimeCompare(presented[:], noToken[:])
		return ResultUnauthorized, nil
	}
	if ok, err := memberTokenOK(ctx, s.pool, runID, id, presented[:], now, ""); err != nil || !ok {
		return ResultUnauthorized, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	_, waits, err := lockWaitingRun(ctx, tx, runID, "SHARE")
	if err != nil {
		return 0, err
	}
	var (
		state   string
		hasRes  bool
		failure string
	)
	// Read again under the member's lock, token included: it was good above.
	if ok, err := memberTokenOK(ctx, tx, runID, id, presented[:], now, " FOR UPDATE"); err != nil || !ok {
		return ResultUnauthorized, err
	}
	err = tx.QueryRow(ctx, `SELECT state, result IS NOT NULL, failure_reason
		FROM panel_members WHERE id = $1::uuid`, id).Scan(&state, &hasRes, &failure)
	if errors.Is(err, pgx.ErrNoRows) { // its run was deleted meanwhile
		return ResultUnauthorized, nil
	}
	if err != nil {
		return 0, err
	}

	switch {
	case MemberState(state) == MemberSuperseded || !waits:
		return ResultDiscarded, nil
	case hasRes || failure != "" || MemberState(state) != MemberRunning:
		return ResultConflict, nil
	}
	if !json.Valid(body) {
		return 0, ErrInvalidResult
	}
	if _, err := tx.Exec(ctx, `UPDATE panel_members SET result = $2::jsonb, state = 'reported', reported_at = $3
		WHERE id = $1::uuid`, id, string(body), now); err != nil {
		return 0, fmt.Errorf("store: save member result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return ResultStored, nil
}

// rowQuerier reads one row: the pool, or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// noToken is compared against when there is no stored token, so a refusal
// takes the same steps whether or not the member exists.
var noToken [sha256.Size]byte

// memberTokenOK reports whether presented is the SHA-256 of member id's
// live token, the member being one of run runID's. lock is appended to the
// read (" FOR UPDATE" in a transaction). A member that is not there is
// compared against noToken, as one with a wrong token is.
func memberTokenOK(ctx context.Context, q rowQuerier, runID, id string, presented []byte, now time.Time, lock string) (bool, error) {
	var (
		stored  []byte
		expires *time.Time
	)
	err := q.QueryRow(ctx, `SELECT token_hash, token_expires_at FROM panel_members
		WHERE id = $1::uuid AND run_id = $2::uuid`+lock, id, runID).Scan(&stored, &expires)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	want := stored
	if len(want) != sha256.Size {
		want = noToken[:]
	}
	same := subtle.ConstantTimeCompare(presented, want) == 1
	return same && stored != nil && expires != nil && !now.After(*expires), nil
}

// ReportMemberFailure records that a queued or running member will never
// produce a result, so it is reported with the failure f. It reports whether
// it changed the member; one that already has a result or a failure is left
// alone. It never touches the run.
func (s *Store) ReportMemberFailure(ctx context.Context, id string, f Failure, now time.Time) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	if f.Reason == "" {
		f.Reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE panel_members SET state = 'reported', failure_reason = $2,
		failure_step = $4, failure_detail = $5, reported_at = $3
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND result IS NULL AND failure_reason = ''`,
		id, f.Reason, now, f.Step, CleanText(f.Detail))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceMemberWithFailure turns the stored result of a reported member into
// the failure f. It returns ErrNotReported when the member is not reported
// with a result.
func (s *Store) ReplaceMemberWithFailure(ctx context.Context, id string, f Failure) error {
	if !validUUID(id) {
		return ErrNotReported
	}
	if f.Reason == "" {
		f.Reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE panel_members SET result = NULL, failure_reason = $2,
		failure_step = $3, failure_detail = $4
		WHERE id = $1::uuid AND state = 'reported' AND result IS NOT NULL`,
		id, f.Reason, f.Step, CleanText(f.Detail))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReported
	}
	return nil
}

// MembersToSettle returns the reported members whose run still waits for
// them, oldest report first. Those of a run that is gone are left to
// SupersedeOrphanMembers.
func (s *Store) MembersToSettle(ctx context.Context) ([]PanelMember, error) {
	ms, err := s.queryMembers(ctx, `SELECT `+memberColumns+` FROM panel_members m
		WHERE state = 'reported' AND `+memberRunWaitsSQL)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].ReportedAt.Before(ms[j].ReportedAt) })
	return ms, nil
}

// PanelsToRelease returns the reviews (their first runs) whose run still
// waits for its members while none of them is live any more: ReleasePanel
// decides them. A settle pass that stopped between a member's settle and the
// release finds its review here on the next pass.
func (s *Store) PanelsToRelease(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT COALESCE(r.attempt_of, r.id)::text FROM runs r
		WHERE r.state = 'waiting' AND r.skip_reason = 'panel_members'
		  AND NOT EXISTS (SELECT 1 FROM panel_members m WHERE m.run_id = COALESCE(r.attempt_of, r.id)
			AND m.state IN ('queued', 'running', 'reported'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

// SettleMember moves a reported member to done (it has a result) or failed,
// and returns the new state. A member that is not reported, or an unknown id,
// is left alone: the state is "".
func (s *Store) SettleMember(ctx context.Context, id string) (MemberState, error) {
	if !validUUID(id) {
		return "", nil
	}
	var state string
	err := s.pool.QueryRow(ctx, `UPDATE panel_members
		SET state = CASE WHEN result IS NOT NULL THEN 'done' ELSE 'failed' END
		WHERE id = $1::uuid AND state = 'reported' RETURNING state`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return MemberState(state), nil
}

// EnqueueMemberAttempt drops member from for the next entry of its chain,
// under the merge request's lock, in one transaction: it stores m (the entry
// the provider refused), moves from to fell_back and queues the role's next
// attempt, which points at the role's first attempt. That is only done while
// from is reported and its run still waits; otherwise created is false and
// nothing changes.
func (s *Store) EnqueueMemberAttempt(ctx context.Context, from PanelMember, m Mark) (id string, created bool, err error) {
	if !validUUID(from.ID) {
		return "", false, fmt.Errorf("store: panel member %q not found", from.ID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var root string
	err = tx.QueryRow(ctx, `SELECT run_id::text FROM panel_members WHERE id = $1::uuid`, from.ID).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	mr, _, found, err := reviewOf(ctx, tx, root)
	if err != nil || !found {
		return "", false, err
	}
	if err := lockMR(ctx, tx, mr); err != nil {
		return "", false, err
	}
	if _, found, err := lockWaitingRun(ctx, tx, root, "NO KEY UPDATE"); err != nil || !found {
		return "", false, err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM panel_members WHERE id = $1::uuid FOR UPDATE`, from.ID).Scan(&state); err != nil {
		return "", false, err
	}
	if MemberState(state) != MemberReported {
		return "", false, nil
	}
	if err := upsertMark(ctx, tx, m); err != nil {
		return "", false, err
	}
	// Fell back first: one live member per run and role.
	if _, err := tx.Exec(ctx, `UPDATE panel_members SET state = 'fell_back' WHERE id = $1::uuid`, from.ID); err != nil {
		return "", false, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO panel_members (run_id, role, attempt_of)
		SELECT run_id, role, COALESCE(attempt_of, id) FROM panel_members WHERE id = $1::uuid
		RETURNING id::text`, from.ID).Scan(&id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// ReleasePanel acts when every role of the review of run root (its first run
// or a later attempt) has a done or failed member and none is live: with at least one done, the
// waiting run is queued again (no skip reason, not_before NULL); with both
// failed it is reported with both reasons. Otherwise nothing changes.
func (s *Store) ReleasePanel(ctx context.Context, root string, now time.Time) (released, failed bool, err error) {
	if !validUUID(root) {
		return false, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	mr, root, found, err := reviewOf(ctx, tx, root)
	if err != nil || !found {
		return false, false, err
	}
	if err := lockMR(ctx, tx, mr); err != nil {
		return false, false, err
	}
	runID, found, err := lockWaitingRun(ctx, tx, root, "NO KEY UPDATE")
	if err != nil || !found {
		return false, false, err
	}
	type roleState struct {
		live, done, failed bool
		reason             string
	}
	roles := map[string]*roleState{"a": {}, "b": {}}
	rows, err := tx.Query(ctx, `SELECT role, state, failure_reason FROM panel_members
		WHERE run_id = $1::uuid AND state <> 'off' ORDER BY created_at, id`, root)
	if err != nil {
		return false, false, err
	}
	for rows.Next() {
		var role, state, reason string
		if err := rows.Scan(&role, &state, &reason); err != nil {
			rows.Close()
			return false, false, err
		}
		r := roles[role]
		switch MemberState(state) {
		case MemberQueued, MemberRunning, MemberReported:
			r.live = true
		case MemberDone:
			r.done = true
		case MemberFailed:
			r.failed, r.reason = true, reason
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	a, b := roles["a"], roles["b"]
	for _, r := range []*roleState{a, b} {
		if r.live || !(r.done || r.failed) {
			return false, false, nil
		}
	}
	if a.done || b.done {
		if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'queued', skip_reason = '', not_before = NULL
			WHERE id = $1::uuid`, runID); err != nil {
			return false, false, err
		}
		return true, false, tx.Commit(ctx)
	}
	reason := "both review panel members failed: A: " + cutText(a.reason) + "; B: " + cutText(b.reason)
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'reported', skip_reason = '', failure_reason = $2,
			reported_at = $3, not_before = $3, attempts = 0
		WHERE id = $1::uuid`, runID, reason, now); err != nil {
		return false, false, err
	}
	return false, true, tx.Commit(ctx)
}

// SupersedePanel supersedes a waiting panel run and its live members, setting
// the run's outcome reason to reason unless it is "". Any other run, or an
// unknown id, is left alone and is not an error.
func (s *Store) SupersedePanel(ctx context.Context, runID, reason string) error {
	if !validUUID(runID) {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	mr, root, found, err := reviewOf(ctx, tx, runID)
	if err != nil || !found {
		return err
	}
	if err := lockMR(ctx, tx, mr); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded', outcome_reason = COALESCE(NULLIF($2, ''), outcome_reason)
		WHERE id = $1::uuid AND state = 'waiting' AND skip_reason = 'panel_members'`, runID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE panel_members SET state = 'superseded'
		WHERE run_id = $1::uuid AND state IN ('queued', 'running', 'reported')`, root); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SupersedeOrphanMembers supersedes live members whose review has no run
// waiting for them (a newer head, a close or a supersede took it), and
// returns how many it changed.
func (s *Store) SupersedeOrphanMembers(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE panel_members m SET state = 'superseded'
		WHERE state IN ('queued', 'running', 'reported') AND NOT `+memberRunWaitsSQL)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// MembersToCleanUp returns superseded members whose Job has not been cleaned up.
func (s *Store) MembersToCleanUp(ctx context.Context) ([]PanelMember, error) {
	return s.queryMembers(ctx, `SELECT `+memberColumns+` FROM panel_members
		WHERE state = 'superseded' AND NOT cleaned_up ORDER BY created_at, id`)
}

// MarkMemberCleanedUp records that a superseded member's Job is gone.
func (s *Store) MarkMemberCleanedUp(ctx context.Context, id string) error {
	return s.memberUpdate(ctx, id, `UPDATE panel_members SET cleaned_up = true WHERE id = $1::uuid`, id)
}

// RunningMembers returns every member in state running.
func (s *Store) RunningMembers(ctx context.Context) ([]PanelMember, error) {
	return s.queryMembers(ctx, `SELECT `+memberColumns+` FROM panel_members WHERE state = 'running' ORDER BY created_at, id`)
}

// RecoverUnlaunchedMembers puts running members that never got a Job back in
// the queue, due now and without counting an attempt: those claimed before
// olderThan (or with no claim recorded). It returns how many it moved.
func (s *Store) RecoverUnlaunchedMembers(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE panel_members SET state = 'queued', not_before = now()
		WHERE state = 'running' AND job_name = '' AND (claimed_at IS NULL OR claimed_at < $1)`, olderThan)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// MembersToLog returns the members that have a result or a failure reported
// since the given time and a Job, but no stored log, oldest report first, as
// RunsToLog does for runs. Members created more than runLogLookback before
// since are left out. A superseded member's log is taken by its cleanup.
func (s *Store) MembersToLog(ctx context.Context, since time.Time) ([]PanelMember, error) {
	ms, err := s.queryMembers(ctx, `SELECT `+memberColumns+` FROM panel_members m
		WHERE state IN ('reported', 'done', 'failed', 'fell_back') AND job_name <> ''
		  AND created_at >= $2 AND reported_at >= $1
		  AND NOT EXISTS (SELECT 1 FROM panel_member_logs l WHERE l.member_id = m.id)`,
		since, since.Add(-runLogLookback))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].ReportedAt.Before(ms[j].ReportedAt) })
	return ms, nil
}

// SetMemberPod records the name of the pod that runs a member's Job.
func (s *Store) SetMemberPod(ctx context.Context, id, pod string) error {
	if !validUUID(id) {
		return errors.New("store: malformed panel member id")
	}
	_, err := s.pool.Exec(ctx, `UPDATE panel_members SET pod_name = $2 WHERE id = $1::uuid`, id, pod)
	return err
}

// AdvanceMemberCheckoutSeen records how far a member's checkout was seen to
// get: 1 running, 2 done. It only raises the value.
func (s *Store) AdvanceMemberCheckoutSeen(ctx context.Context, id string, seen int) error {
	if !validUUID(id) {
		return errors.New("store: malformed panel member id")
	}
	_, err := s.pool.Exec(ctx, `UPDATE panel_members SET checkout_seen = $2
		WHERE id = $1::uuid AND checkout_seen < $2`, id, seen)
	return err
}

// PanelMemberResult is the result of the role's done member of run runID's
// review, when token is run runID's live result token: the run is running and
// the token, checked against the stored SHA-256 in constant time, has not
// expired. The token is not spent. Every refusal (a bad token, an unknown
// run, a role with no done member) is (nil, false, nil).
func (s *Store) PanelMemberResult(ctx context.Context, runID, token, role string) ([]byte, bool, error) {
	return s.PanelMemberResultAt(ctx, runID, token, role, time.Now())
}

// PanelMemberResultAt is PanelMemberResult with the token's expiry checked at now.
func (s *Store) PanelMemberResultAt(ctx context.Context, runID, token, role string, now time.Time) ([]byte, bool, error) {
	if !validUUID(runID) || (role != "a" && role != "b") {
		return nil, false, nil
	}
	var (
		stored  []byte
		expires *time.Time
		state   string
		root    string
	)
	err := s.pool.QueryRow(ctx, `SELECT token_hash, token_expires_at, state, COALESCE(attempt_of, id)::text
		FROM runs WHERE id = $1::uuid`, runID).Scan(&stored, &expires, &state, &root)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	presented := sha256.Sum256([]byte(token))
	if stored == nil || subtle.ConstantTimeCompare(presented[:], stored) != 1 {
		return nil, false, nil
	}
	if expires == nil || now.After(*expires) || RunState(state) != Running {
		return nil, false, nil
	}
	var body []byte
	err = s.pool.QueryRow(ctx, `SELECT result FROM panel_members
		WHERE run_id = $1::uuid AND role = $2 AND state = 'done' AND result IS NOT NULL
		ORDER BY created_at DESC, id DESC LIMIT 1`, root, role).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// PutMemberLog stores the redacted log of a member, replacing an earlier one,
// made fit for a text column with CleanText.
func (s *Store) PutMemberLog(ctx context.Context, id, text string) error {
	if !validUUID(id) {
		return errors.New("store: malformed panel member id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO panel_member_logs (member_id, text) VALUES ($1::uuid, $2)
		ON CONFLICT (member_id) DO UPDATE SET text = EXCLUDED.text, created_at = now()`, id, CleanText(text))
	return err
}

// MemberLog returns a member's stored log. A member without one, or a
// malformed id, is reported as not found.
func (s *Store) MemberLog(ctx context.Context, id string) (string, bool, error) {
	if !validUUID(id) {
		return "", false, nil
	}
	var text string
	err := s.pool.QueryRow(ctx, `SELECT text FROM panel_member_logs WHERE member_id = $1::uuid`, id).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// PutMemberDiagnostics stores a member's diagnostics, replacing earlier ones,
// made fit for a text column with CleanText.
func (s *Store) PutMemberDiagnostics(ctx context.Context, id string, d Diagnostics) error {
	if !validUUID(id) {
		return errors.New("store: malformed panel member id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO panel_member_diagnostics (member_id, clone_log, clone_log_masked, pod_state)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (member_id) DO UPDATE SET clone_log = EXCLUDED.clone_log,
			clone_log_masked = EXCLUDED.clone_log_masked, pod_state = EXCLUDED.pod_state, created_at = now()`,
		id, CleanText(d.CloneLog), CleanText(d.CloneLogMasked), CleanText(d.PodState))
	return err
}

// MemberDiagnostics returns a member's stored diagnostics. A member without
// any, or a malformed id, is reported as not found.
func (s *Store) MemberDiagnostics(ctx context.Context, id string) (Diagnostics, bool, error) {
	if !validUUID(id) {
		return Diagnostics{}, false, nil
	}
	var d Diagnostics
	err := s.pool.QueryRow(ctx, `SELECT clone_log, clone_log_masked, pod_state
		FROM panel_member_diagnostics WHERE member_id = $1::uuid`, id).Scan(&d.CloneLog, &d.CloneLogMasked, &d.PodState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Diagnostics{}, false, nil
	}
	if err != nil {
		return Diagnostics{}, false, err
	}
	return d, true, nil
}

// SetPanelJudge records the entry, provider and model role judges with in
// the Job P run runID being dispatched (queued or running), replacing an
// earlier one. Any other run is an error.
func (s *Store) SetPanelJudge(ctx context.Context, runID, role string, e RunEntry, provider, model string) error {
	if !validUUID(runID) {
		return fmt.Errorf("store: run %q not found", runID)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO panel_judges (run_id, role, provider, model, model_source, model_entry_id,
			model_entry_name, model_team_id, chain_length)
		SELECT id, $2, $3, $4, $5, $6, $7, NULLIF($8::bigint, 0), $9 FROM runs
		WHERE id = $1::uuid AND state IN ('queued', 'running')
		ON CONFLICT (run_id, role) DO UPDATE SET provider = EXCLUDED.provider, model = EXCLUDED.model,
			model_source = EXCLUDED.model_source, model_entry_id = EXCLUDED.model_entry_id,
			model_entry_name = EXCLUDED.model_entry_name, model_team_id = EXCLUDED.model_team_id,
			chain_length = EXCLUDED.chain_length`,
		runID, role, cutText(provider), cutText(model), e.Source, e.ID, cutText(e.Name), e.TeamID, e.Length)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found or in the wrong state", runID)
	}
	return nil
}

// PanelJudges returns the entry each role of the Job P run runID judged with,
// by role: its source, ID, name, team and chain length.
func (s *Store) PanelJudges(ctx context.Context, runID string) (map[string]RunEntry, error) {
	out := map[string]RunEntry{}
	if !validUUID(runID) {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT role, model_source, model_entry_id, model_entry_name, COALESCE(model_team_id, 0),
		chain_length FROM panel_judges WHERE run_id = $1::uuid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		var e RunEntry
		if err := rows.Scan(&role, &e.Source, &e.ID, &e.Name, &e.TeamID, &e.Length); err != nil {
			return nil, err
		}
		out[role] = e
	}
	return out, rows.Err()
}
