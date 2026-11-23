package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A multi review is one run (the judge Job reports on it) and one
// review_members row per attempt of reviewer 1 to 5. The members belong to
// the review's first run (run_id), so the judge's later attempts read the
// same members. While they run, the review's live run waits (state
// 'waiting', skip_reason WaitReviewers): v0.18.0 never claims it and ignores
// review_members. Nothing here reads or writes panel_members.
//
// Locks are taken in one order: the merge request's lock (lockMR), then the
// waiting run's row, then member rows. Nothing that holds a member row waits
// for a run row.

// MemberState is where a review member is in its life: queued → running
// (claimed) → reported (a result or a failure) → done (it has a result) or
// failed; reported → fell_back when a quota, billing or auth error moves it to
// its next entry; any live state → superseded when its run is gone. Off is
// the one row of a review whose multi review could not start; it never
// moves. Skipped (MemberSkipped) is an extra reviewer that could not start.
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

// WaitReviewers: a multi review's run waits while its members review.
// v0.18.0 never claims it.
const WaitReviewers = "review_members"

// MemberSkipped is an extra reviewer that could not start at the split. It
// is final; failure_reason says why.
const MemberSkipped MemberState = "skipped"

// OutcomeJudgeFailed is the outcome reason of a judge run that crashed or
// gave no usable answer, closed as OutcomeFellBack so the next attempt runs
// with --no-judge (EnqueueJudgeRetry).
const OutcomeJudgeFailed = "judge_failed"

// maxReviewers is the most reviewers a multi review has.
const maxReviewers = 5

// ReviewMember is one attempt of reviewer 1 to 5 of a multi review. RunID is
// the review's first run; AttemptOf is the reviewer's first attempt (nil for
// it). Chain is the reviewer's chain as it was at the split (empty for an
// off row). FailureReason of an off row is why the multi review did not
// start, of a skipped row why the reviewer could not start.
type ReviewMember struct {
	ID, RunID                                      string
	Reviewer                                       int
	AttemptOf                                      *string
	State                                          MemberState
	Chain                                          FrozenChain
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

const reviewMemberColumns = `id::text, run_id::text, reviewer, attempt_of::text, state, chain, job_name, token_expires_at,
	result, failure_reason, failure_step, failure_detail, pod_name, checkout_seen, provider, model,
	model_source, model_entry_id, model_entry_name, COALESCE(model_team_id, 0), chain_position, chain_length,
	fallback_class, attempts, created_at, dispatched_at, reported_at, not_before`

func scanReviewMember(row pgx.Row) (ReviewMember, error) {
	var (
		m                                    ReviewMember
		state                                string
		chain                                []byte
		tokenExp, dispatched, rep, notBefore *time.Time
	)
	err := row.Scan(&m.ID, &m.RunID, &m.Reviewer, &m.AttemptOf, &state, &chain, &m.JobName, &tokenExp,
		&m.Result, &m.FailureReason, &m.FailureStep, &m.FailureDetail, &m.PodName, &m.CheckoutSeen, &m.Provider, &m.Model,
		&m.Entry.Source, &m.Entry.ID, &m.Entry.Name, &m.Entry.TeamID, &m.Entry.Position, &m.Entry.Length,
		&m.Entry.FallbackClass, &m.Attempts, &m.CreatedAt, &dispatched, &rep, &notBefore)
	if err != nil {
		return ReviewMember{}, err
	}
	if chain != nil {
		if err := json.Unmarshal(chain, &m.Chain); err != nil {
			return ReviewMember{}, fmt.Errorf("store: review member %s chain: %w", m.ID, err)
		}
	}
	m.State = MemberState(state)
	m.TokenExpiresAt = deref(tokenExp)
	m.DispatchedAt = deref(dispatched)
	m.ReportedAt = deref(rep)
	m.NotBefore = deref(notBefore)
	return m, nil
}

// queryReviewMembers returns the members sql reads, oldest first, and by
// reviewer number among those created together.
func (s *Store) queryReviewMembers(ctx context.Context, sql string, args ...any) ([]ReviewMember, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewMember
	for rows.Next() {
		m, err := scanReviewMember(rows)
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
		if out[i].Reviewer != out[j].Reviewer {
			return out[i].Reviewer < out[j].Reviewer
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// reviewMemberUpdate runs an UPDATE that has to touch exactly one member.
func (s *Store) reviewMemberUpdate(ctx context.Context, id, sql string, args ...any) error {
	if !validUUID(id) {
		return fmt.Errorf("store: review member %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: review member %s not found or in the wrong state", id)
	}
	return nil
}

// multiReviewWaitsSQL is the condition of a run waiting for the members of
// review $1 (the review's first run): the first run or a later attempt of it.
const multiReviewWaitsSQL = `(id = $1::uuid OR attempt_of = $1::uuid) AND state = 'waiting' AND skip_reason = 'review_members'`

// reviewMemberRunWaitsSQL is the same for the member row m.
const reviewMemberRunWaitsSQL = `EXISTS (SELECT 1 FROM runs r WHERE (r.id = m.run_id OR r.attempt_of = m.run_id)
	AND r.state = 'waiting' AND r.skip_reason = 'review_members')`

// lockMultiReviewRun locks the run waiting for the members of review root,
// and returns it; found is false when no run waits for them. The lock keeps
// member dispatches of one review apart and holds off a supersede.
func lockMultiReviewRun(ctx context.Context, tx pgx.Tx, root, mode string) (id string, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT id::text FROM runs WHERE `+multiReviewWaitsSQL+` FOR `+mode, root).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// SplitReviewer is one reviewer at the split: its frozen chain, and why it
// can't start ("" queues it; reviewer 1 is always queued).
type SplitReviewer struct {
	Number  int
	Chain   FrozenChain
	Skipped string
}

// MultiReviewSplit is what StartMultiReview records: the merge request's
// base and start SHAs, which every reviewer reviews; the run's inputs as
// SetRunInputs records them, which their results are checked against; every
// reviewer (1 first); and the frozen judge chain.
type MultiReviewSplit struct {
	BaseSHA, StartSHA string
	Inputs            RunInputs
	Reviewers         []SplitReviewer
	Judge             FrozenChain
}

// check reports why split can't be recorded: the reviewers must be 1 to at
// most 5, in number order, and reviewer 1 must be queued.
func (split MultiReviewSplit) check() error {
	if len(split.Reviewers) == 0 || len(split.Reviewers) > maxReviewers {
		return fmt.Errorf("store: a multi review has 1 to %d reviewers, not %d", maxReviewers, len(split.Reviewers))
	}
	for i, r := range split.Reviewers {
		if r.Number != i+1 {
			return fmt.Errorf("store: reviewer %d of the split is number %d", i+1, r.Number)
		}
	}
	if split.Reviewers[0].Skipped != "" {
		return errors.New("store: reviewer 1 is never skipped")
	}
	return nil
}

// StartMultiReview makes the claimed, not dispatched run runID a multi
// review, in one transaction under the merge request's lock: it waits
// (WaitReviewers) with dispatched_at now and what split holds, one member
// per reviewer is added (queued, or skipped with its reason), each with its
// frozen chain, and the judge chain is stored. started is false when the run
// is no longer claimed (superseded meanwhile), or was dispatched before: a
// run released for the judge keeps its dispatched_at, so a review is split
// once. The members belong to the review's first run, and the review's off
// row, from an earlier attempt, is dropped: the multi review did start. A
// split that check refuses is an error and writes nothing.
func (s *Store) StartMultiReview(ctx context.Context, runID string, split MultiReviewSplit, now time.Time) (started bool, err error) {
	if !validUUID(runID) {
		return false, nil
	}
	if err := split.check(); err != nil {
		return false, err
	}
	numbers := make([]int32, len(split.Reviewers))
	chains := make([]string, len(split.Reviewers))
	skips := make([]string, len(split.Reviewers))
	for i, r := range split.Reviewers {
		b, err := json.Marshal(r.Chain)
		if err != nil {
			return false, err
		}
		numbers[i], chains[i], skips[i] = int32(r.Number), string(b), cutText(r.Skipped)
	}
	judge, err := json.Marshal(split.Judge)
	if err != nil {
		return false, err
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
		WHERE id = $1::uuid AND state = 'running' AND job_name = '' AND dispatched_at IS NULL`,
		runID, WaitReviewers, now, split.BaseSHA, split.StartSHA)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if err := setRunInputs(ctx, tx, runID, split.Inputs); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM review_members WHERE run_id = $1::uuid AND state = 'off'`, root); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO review_members (run_id, reviewer, state, chain, failure_reason)
		SELECT $1::uuid, u.n, CASE WHEN u.skip = '' THEN 'queued' ELSE 'skipped' END, u.chain::jsonb, u.skip
		FROM unnest($2::int[], $3::text[], $4::text[]) AS u(n, chain, skip)`, root, numbers, chains, skips); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO review_judge_chains (run_id, chain) VALUES ($1::uuid, $2::jsonb)
		ON CONFLICT (run_id) DO UPDATE SET chain = EXCLUDED.chain`, root, string(judge)); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// JudgeChainOf is the frozen judge chain of run id's review. found is false
// when the review was never split (or id is unknown).
func (s *Store) JudgeChainOf(ctx context.Context, id string) (FrozenChain, bool, error) {
	if !validUUID(id) {
		return FrozenChain{}, false, nil
	}
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT c.chain FROM runs r JOIN review_judge_chains c ON c.run_id = COALESCE(r.attempt_of, r.id)
		WHERE r.id = $1::uuid`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return FrozenChain{}, false, nil
	}
	if err != nil {
		return FrozenChain{}, false, err
	}
	var c FrozenChain
	if err := json.Unmarshal(b, &c); err != nil {
		return FrozenChain{}, false, fmt.Errorf("store: judge chain of %s: %w", id, err)
	}
	return c, true, nil
}

// RecordMultiReviewOff keeps why the run's multi review could not start (its
// one 'off' row, of the review's first run, reviewer 1, no chain). A second
// call replaces the reason. An unknown run is an error. Call it only when
// the review has no live members: the off row is for a review that runs as
// one review. StartMultiReview removes an off row; nothing here removes
// members.
func (s *Store) RecordMultiReviewOff(ctx context.Context, runID, reason string) error {
	if !validUUID(runID) {
		return fmt.Errorf("store: run %q not found", runID)
	}
	// claimed_at is the claim of the run that recorded it:
	// MultiReviewOffReason tells this dispatch's row from one an earlier
	// claim left.
	tag, err := s.pool.Exec(ctx, `INSERT INTO review_members (run_id, reviewer, state, failure_reason, claimed_at)
		SELECT COALESCE(attempt_of, id), 1, 'off', $2, claimed_at FROM runs WHERE id = $1::uuid
		ON CONFLICT (run_id) WHERE state = 'off' DO UPDATE SET failure_reason = EXCLUDED.failure_reason,
			claimed_at = EXCLUDED.claimed_at`,
		runID, cutText(reason))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found", runID)
	}
	return nil
}

// MultiReviewOffReason is why run id ran as one review, its multi review
// off: the reason of its review's off row, when that row was recorded by run
// id's current claim (the dispatch that launched it). found is false
// otherwise: no off row, or one an earlier claim left.
func (s *Store) MultiReviewOffReason(ctx context.Context, id string) (reason string, found bool, err error) {
	if !validUUID(id) {
		return "", false, nil
	}
	err = s.pool.QueryRow(ctx, `SELECT m.failure_reason FROM runs r
		JOIN review_members m ON m.run_id = COALESCE(r.attempt_of, r.id) AND m.state = 'off'
		WHERE r.id = $1::uuid AND r.claimed_at IS NOT NULL AND m.claimed_at = r.claimed_at`, id).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return reason, err == nil, err
}

// IsJudgeRun reports whether run id is a multi review's judge run: its
// review has a member that is not off.
func (s *Store) IsJudgeRun(ctx context.Context, id string) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	var judge bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs r
		JOIN review_members m ON m.run_id = COALESCE(r.attempt_of, r.id) AND m.state <> 'off'
		WHERE r.id = $1::uuid)`, id).Scan(&judge)
	return judge, err
}

// ReviewMembers returns the members of the review of run id (its first run
// or a later attempt), its off row too, oldest first.
func (s *Store) ReviewMembers(ctx context.Context, id string) ([]ReviewMember, error) {
	if !validUUID(id) {
		return nil, nil
	}
	return s.queryReviewMembers(ctx, `SELECT `+reviewMemberColumns+` FROM review_members
		WHERE run_id = (SELECT COALESCE(attempt_of, id) FROM runs WHERE id = $1::uuid)
		ORDER BY created_at, reviewer, id`, id)
}

// ClaimQueuedReviewMembers moves up to limit queued members that are due at
// now, and whose run still waits for them, to running and returns them,
// oldest first. Concurrent callers never claim the same member. The members
// of runs of the instances in skip stay queued. A skipped member is never
// claimed.
func (s *Store) ClaimQueuedReviewMembers(ctx context.Context, limit int, now time.Time, skip ...string) ([]ReviewMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	if skip == nil {
		skip = []string{} // an empty array, not NULL: `<> ALL(NULL)` is never true
	}
	// The rows are picked in a CTE, which runs once. As a subquery of the
	// UPDATE it can be scanned again, and SKIP LOCKED then skips a row this
	// statement already took, so LIMIT picks one more. The lock is NO KEY
	// UPDATE, as ClaimQueued's: a row that only holds the member's key does
	// not make the claim skip it.
	return s.queryReviewMembers(ctx, `WITH picked AS (
			SELECT m.id AS picked_id FROM review_members m
			WHERE m.state = 'queued' AND (m.not_before IS NULL OR m.not_before <= $2)
			  AND EXISTS (SELECT 1 FROM runs r WHERE (r.id = m.run_id OR r.attempt_of = m.run_id)
				AND r.state = 'waiting' AND r.skip_reason = 'review_members' AND r.instance <> ALL($3::text[]))
			ORDER BY m.created_at, m.reviewer, m.id LIMIT $1 FOR NO KEY UPDATE OF m SKIP LOCKED)
		UPDATE review_members SET state = 'running', claimed_at = $2
		FROM picked WHERE id = picked_id AND state = 'queued'
		RETURNING `+reviewMemberColumns, limit, now, skip)
}

// MultiReviewRun returns the run waiting for the members of review root (the
// review's first run, a member's RunID): root itself or a later attempt of
// it. found is false when no run waits for them.
func (s *Store) MultiReviewRun(ctx context.Context, root string) (Run, bool, error) {
	if !validUUID(root) {
		return Run{}, false, nil
	}
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE `+multiReviewWaitsSQL, root))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// SetReviewMemberEntry records the entry, provider and model of a member
// being dispatched (queued or running), before its Job starts. Any other
// member is an error.
func (s *Store) SetReviewMemberEntry(ctx context.Context, id string, e RunEntry, provider, model string) error {
	return s.reviewMemberUpdate(ctx, id, `UPDATE review_members SET model_source = $2, model_entry_id = $3, model_entry_name = $4,
			model_team_id = NULLIF($5::bigint, 0), chain_position = $6, chain_length = $7, fallback_class = $8,
			provider = $9, model = $10
		WHERE id = $1::uuid AND state IN ('queued', 'running')`,
		id, e.Source, e.ID, cutText(e.Name), e.TeamID, e.Position, e.Length, e.FallbackClass, cutText(provider), cutText(model))
}

// MarkReviewMemberDispatched records the member's Job and the SHA-256 of its
// result token, valid until expires; first is true when no other member of
// its review had a Job yet. ErrNotRunning when the member is not running or
// its run no longer waits for it.
func (s *Store) MarkReviewMemberDispatched(ctx context.Context, id, jobName string, tokenHash []byte, expires, now time.Time) (first bool, err error) {
	if !validUUID(id) {
		return false, ErrNotRunning
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var root string
	err = tx.QueryRow(ctx, `SELECT run_id::text FROM review_members WHERE id = $1::uuid`, id).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotRunning
	}
	if err != nil {
		return false, err
	}
	// The run's lock makes the members' dispatches one after the other, so
	// only one of them is first.
	if _, found, err := lockMultiReviewRun(ctx, tx, root, "NO KEY UPDATE"); err != nil {
		return false, err
	} else if !found {
		return false, ErrNotRunning
	}
	tag, err := tx.Exec(ctx, `UPDATE review_members SET job_name = $2, token_hash = $3, token_expires_at = $4, dispatched_at = $5
		WHERE id = $1::uuid AND state = 'running'`, id, jobName, tokenHash, expires, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrNotRunning
	}
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM review_members
		WHERE run_id = $1::uuid AND id <> $2::uuid AND job_name <> '')`, root, id).Scan(&first); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return first, nil
}

// RequeueReviewMember puts a claimed member back in the queue, not to be
// claimed before notBefore, and counts the attempt. A member that is not
// queued or running is an error.
func (s *Store) RequeueReviewMember(ctx context.Context, id string, notBefore time.Time) error {
	return s.reviewMemberUpdate(ctx, id, `UPDATE review_members SET state = 'queued', not_before = $2, attempts = attempts + 1
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
}

// PostponeReviewMember puts a claimed member back in the queue, not to be
// claimed before notBefore, without counting an attempt. A member in another
// state, or an unknown id, is left alone and is not an error.
func (s *Store) PostponeReviewMember(ctx context.Context, id string, notBefore time.Time) error {
	if !validUUID(id) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE review_members SET state = 'queued', not_before = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
	return err
}

// StoreReviewMemberResult accepts the result a member's Job posted on the
// route of run runID (the review's first run). The token is checked as
// StoreResult checks it: against the stored SHA-256 in constant time, not
// expired, and single use by the member's state. It is checked first, in
// the same one read for a member of another run, an unknown member and a
// known one: each refusal hashes the token and compares it. The outcome, in
// order:
//   - ResultUnauthorized: unknown member, a member of another run, no token,
//     wrong token or expired;
//   - ResultDiscarded: the member is superseded, or its run no longer waits
//     for it (superseded, or released for the judge: a late second post
//     after the release is discarded, not a conflict); the result is dropped;
//   - ResultConflict: the member already has a result or failure, or is not
//     running;
//   - ErrInvalidResult: body is not valid JSON; nothing is stored;
//   - ResultStored: the member is now reported.
func (s *Store) StoreReviewMemberResult(ctx context.Context, runID, id, token string, body []byte, now time.Time) (ResultOutcome, error) {
	presented := sha256.Sum256([]byte(token))
	if !validUUID(id) || !validUUID(runID) {
		subtle.ConstantTimeCompare(presented[:], noToken[:])
		return ResultUnauthorized, nil
	}
	if ok, err := reviewMemberTokenOK(ctx, s.pool, runID, id, presented[:], now, ""); err != nil || !ok {
		return ResultUnauthorized, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	_, waits, err := lockMultiReviewRun(ctx, tx, runID, "SHARE")
	if err != nil {
		return 0, err
	}
	var (
		state   string
		hasRes  bool
		failure string
	)
	// Read again under the member's lock, token included: it was good above.
	if ok, err := reviewMemberTokenOK(ctx, tx, runID, id, presented[:], now, " FOR UPDATE"); err != nil || !ok {
		return ResultUnauthorized, err
	}
	err = tx.QueryRow(ctx, `SELECT state, result IS NOT NULL, failure_reason
		FROM review_members WHERE id = $1::uuid`, id).Scan(&state, &hasRes, &failure)
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
	if _, err := tx.Exec(ctx, `UPDATE review_members SET result = $2::jsonb, state = 'reported', reported_at = $3
		WHERE id = $1::uuid`, id, string(body), now); err != nil {
		return 0, fmt.Errorf("store: save review member result: %w", err)
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

// reviewMemberTokenOK reports whether presented is the SHA-256 of member
// id's live token, the member being one of run runID's. lock is appended to
// the read (" FOR UPDATE" in a transaction). A member that is not there is
// compared against noToken, as one with a wrong token is.
func reviewMemberTokenOK(ctx context.Context, q rowQuerier, runID, id string, presented []byte, now time.Time, lock string) (bool, error) {
	var (
		stored  []byte
		expires *time.Time
	)
	err := q.QueryRow(ctx, `SELECT token_hash, token_expires_at FROM review_members
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

// ReportReviewMemberFailure records that a queued or running member will
// never produce a result, so it is reported with the failure f. It reports
// whether it changed the member; one that already has a result or a failure
// is left alone. It never touches the run.
func (s *Store) ReportReviewMemberFailure(ctx context.Context, id string, f Failure, now time.Time) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	if f.Reason == "" {
		f.Reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE review_members SET state = 'reported', failure_reason = $2,
		failure_step = $4, failure_detail = $5, reported_at = $3
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND result IS NULL AND failure_reason = ''`,
		id, f.Reason, now, f.Step, CleanText(f.Detail))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceReviewMemberWithFailure turns the stored result of a reported
// member into the failure f. It returns ErrNotReported when the member is
// not reported with a result, or its run no longer waits for it (the sweep
// takes it).
func (s *Store) ReplaceReviewMemberWithFailure(ctx context.Context, id string, f Failure) error {
	if !validUUID(id) {
		return ErrNotReported
	}
	if f.Reason == "" {
		f.Reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE review_members m SET result = NULL, failure_reason = $2,
		failure_step = $3, failure_detail = $4
		WHERE m.id = $1::uuid AND m.state = 'reported' AND m.result IS NOT NULL AND `+reviewMemberRunWaitsSQL,
		id, f.Reason, f.Step, CleanText(f.Detail))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReported
	}
	return nil
}

// FailReviewMemberKeepingResult moves a reported member to failed with
// reason and keeps its stored result: a provider refused its entry and its
// frozen chain has no entry left (M3.6). The judge still gets the result.
// false when the member is not reported or its run no longer waits.
func (s *Store) FailReviewMemberKeepingResult(ctx context.Context, id, reason string) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	if reason == "" {
		reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE review_members m SET state = 'failed', failure_reason = $2
		WHERE m.id = $1::uuid AND m.state = 'reported' AND `+reviewMemberRunWaitsSQL, id, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReviewMembersToSettle returns up to limit reported members whose run still
// waits for them, oldest report first. Those of a run that is gone are left
// to SupersedeOrphanReviewMembers.
func (s *Store) ReviewMembersToSettle(ctx context.Context, limit int) ([]ReviewMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	ms, err := s.queryReviewMembers(ctx, `SELECT `+reviewMemberColumns+` FROM review_members m
		WHERE state = 'reported' AND `+reviewMemberRunWaitsSQL+`
		ORDER BY reported_at, created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].ReportedAt.Before(ms[j].ReportedAt) })
	return ms, nil
}

// MultiReviewsToRelease returns the reviews (their first runs) whose run
// still waits for its members while none of them is live any more:
// ReleaseMultiReview decides them. A settle pass that stopped between a
// member's settle and the release finds its review here on the next pass.
func (s *Store) MultiReviewsToRelease(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT COALESCE(r.attempt_of, r.id)::text FROM runs r
		WHERE r.state = 'waiting' AND r.skip_reason = 'review_members'
		  AND NOT EXISTS (SELECT 1 FROM review_members m WHERE m.run_id = COALESCE(r.attempt_of, r.id)
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

// SettleReviewMember moves a reported member to done (it has a result) or
// failed, and returns the new state. A member that is not reported, one
// whose run no longer waits for it (the sweep takes it), or an unknown id,
// is left alone: the state is "".
func (s *Store) SettleReviewMember(ctx context.Context, id string) (MemberState, error) {
	if !validUUID(id) {
		return "", nil
	}
	var state string
	err := s.pool.QueryRow(ctx, `UPDATE review_members m
		SET state = CASE WHEN m.result IS NOT NULL THEN 'done' ELSE 'failed' END
		WHERE m.id = $1::uuid AND m.state = 'reported' AND `+reviewMemberRunWaitsSQL+` RETURNING m.state`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return MemberState(state), nil
}

// EnqueueReviewMemberAttempt drops member from for the next entry of its
// frozen chain, under the merge request's lock, in one transaction: it
// stores m (the entry the provider refused), moves from to fell_back and
// queues the reviewer's next attempt, which points at the reviewer's first
// attempt and carries the same frozen chain. That is only done while from is
// reported and its run still waits; otherwise created is false and nothing
// changes.
func (s *Store) EnqueueReviewMemberAttempt(ctx context.Context, from ReviewMember, m Mark) (id string, created bool, err error) {
	if !validUUID(from.ID) {
		return "", false, fmt.Errorf("store: review member %q not found", from.ID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var root string
	err = tx.QueryRow(ctx, `SELECT run_id::text FROM review_members WHERE id = $1::uuid`, from.ID).Scan(&root)
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
	if _, found, err := lockMultiReviewRun(ctx, tx, root, "NO KEY UPDATE"); err != nil || !found {
		return "", false, err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM review_members WHERE id = $1::uuid FOR UPDATE`, from.ID).Scan(&state); err != nil {
		return "", false, err
	}
	if MemberState(state) != MemberReported {
		return "", false, nil
	}
	if err := upsertMark(ctx, tx, m); err != nil {
		return "", false, err
	}
	// Fell back first: one live member per run and reviewer.
	if _, err := tx.Exec(ctx, `UPDATE review_members SET state = 'fell_back' WHERE id = $1::uuid`, from.ID); err != nil {
		return "", false, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO review_members (run_id, reviewer, attempt_of, chain)
		SELECT run_id, reviewer, COALESCE(attempt_of, id), chain FROM review_members WHERE id = $1::uuid
		RETURNING id::text`, from.ID).Scan(&id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// reviewerFinal is where one reviewer number of a review ended: the state
// and reason of its newest final attempt, and the name of the entry that
// attempt ran on.
type reviewerFinal struct {
	live   bool
	state  MemberState
	reason string
	name   string
}

// ReleaseMultiReview acts when the newest attempt of every reviewer number
// of the review of run root (its first run or a later attempt) is final
// (done, failed or skipped) and none is live: with one done or more, the
// waiting run is queued again for the judge (no skip reason, not_before
// NULL); with none it is reported, failed with every reviewer's reason in
// number order. A failed reviewer that kept a partial result is not done:
// every reviewer failed is never a pass. Otherwise nothing changes.
func (s *Store) ReleaseMultiReview(ctx context.Context, root string, now time.Time) (released, failed bool, err error) {
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
	runID, found, err := lockMultiReviewRun(ctx, tx, root, "NO KEY UPDATE")
	if err != nil || !found {
		return false, false, err
	}
	reviewers := map[int]*reviewerFinal{}
	rows, err := tx.Query(ctx, `SELECT reviewer, state, failure_reason, chain, chain_position, model_entry_name
		FROM review_members WHERE run_id = $1::uuid AND state <> 'off' ORDER BY created_at, id`, root)
	if err != nil {
		return false, false, err
	}
	for rows.Next() {
		var (
			n, position         int
			state, reason, name string
			chain               []byte
		)
		if err := rows.Scan(&n, &state, &reason, &chain, &position, &name); err != nil {
			rows.Close()
			return false, false, err
		}
		r := reviewers[n]
		if r == nil {
			r = &reviewerFinal{}
			reviewers[n] = r
		}
		switch MemberState(state) {
		case MemberQueued, MemberRunning, MemberReported:
			r.live = true
		case MemberDone, MemberFailed, MemberSkipped:
			r.state, r.reason, r.name = MemberState(state), reason, frozenEntryName(chain, position, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	if len(reviewers) == 0 {
		return false, false, nil
	}
	numbers := make([]int, 0, len(reviewers))
	done := false
	for n, r := range reviewers {
		if r.live || r.state == "" {
			return false, false, nil
		}
		done = done || r.state == MemberDone
		numbers = append(numbers, n)
	}
	if done {
		if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'queued', skip_reason = '', not_before = NULL
			WHERE id = $1::uuid`, runID); err != nil {
			return false, false, err
		}
		return true, false, tx.Commit(ctx)
	}
	sort.Ints(numbers)
	parts := make([]string, len(numbers))
	for i, n := range numbers {
		r := reviewers[n]
		who := "reviewer " + strconv.Itoa(n)
		if r.name != "" {
			who += " (" + cutText(r.name) + ")"
		}
		parts[i] = who + ": " + cutText(r.reason)
	}
	reason := "every reviewer failed: " + strings.Join(parts, "; ")
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'reported', skip_reason = '', failure_reason = $2,
			reported_at = $3, not_before = $3, attempts = 0
		WHERE id = $1::uuid`, runID, reason, now); err != nil {
		return false, false, err
	}
	return false, true, tx.Commit(ctx)
}

// frozenEntryName is the name, in the frozen chain, of the entry an attempt
// ran on (position is 1-based; 0 when it never started, so the chain's first
// entry). The name its dispatch recorded is the fallback, for an entry with
// no frozen name.
func frozenEntryName(chain []byte, position int, recorded string) string {
	var c FrozenChain
	if chain != nil && json.Unmarshal(chain, &c) == nil && len(c.Entries) > 0 {
		i := 0
		if position >= 1 && position <= len(c.Entries) {
			i = position - 1
		}
		if c.Entries[i].Name != "" {
			return c.Entries[i].Name
		}
	}
	return recorded
}

// WaitForReviewMembers puts the claimed run id, which has no Job yet, back
// to wait for its members, so ReleaseMultiReview decides it. It reports
// whether it changed the run; any other run, or an unknown id, is left
// alone.
func (s *Store) WaitForReviewMembers(ctx context.Context, id string) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = 'review_members'
		WHERE id = $1::uuid AND state = 'running' AND job_name = ''`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SupersedeMultiReview supersedes a run waiting for its members and its live
// members, setting the run's outcome reason to reason unless it is "". Any
// other run, or an unknown id, is left alone and is not an error.
func (s *Store) SupersedeMultiReview(ctx context.Context, runID, reason string) error {
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
		WHERE id = $1::uuid AND state = 'waiting' AND skip_reason = 'review_members'`, runID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE review_members SET state = 'superseded'
		WHERE run_id = $1::uuid AND state IN ('queued', 'running', 'reported')`, root); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SupersedeOrphanReviewMembers supersedes live members whose review has no
// run waiting for them (a newer head, a close or a supersede took it), and
// returns how many it changed.
func (s *Store) SupersedeOrphanReviewMembers(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE review_members m SET state = 'superseded'
		WHERE state IN ('queued', 'running', 'reported') AND NOT `+reviewMemberRunWaitsSQL)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ReviewMembersToCleanUp returns superseded members whose Job has not been
// cleaned up.
func (s *Store) ReviewMembersToCleanUp(ctx context.Context) ([]ReviewMember, error) {
	return s.queryReviewMembers(ctx, `SELECT `+reviewMemberColumns+` FROM review_members
		WHERE state = 'superseded' AND NOT cleaned_up ORDER BY created_at, id`)
}

// MarkReviewMemberCleanedUp records that a superseded member's Job is gone.
func (s *Store) MarkReviewMemberCleanedUp(ctx context.Context, id string) error {
	return s.reviewMemberUpdate(ctx, id, `UPDATE review_members SET cleaned_up = true WHERE id = $1::uuid`, id)
}

// RunningReviewMembers returns every member in state running.
func (s *Store) RunningReviewMembers(ctx context.Context) ([]ReviewMember, error) {
	return s.queryReviewMembers(ctx, `SELECT `+reviewMemberColumns+` FROM review_members WHERE state = 'running' ORDER BY created_at, id`)
}

// RecoverUnlaunchedReviewMembers puts running members that never got a Job
// back in the queue, due now and without counting an attempt: those claimed
// before olderThan (or with no claim recorded). It returns how many it
// moved.
func (s *Store) RecoverUnlaunchedReviewMembers(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE review_members SET state = 'queued', not_before = now()
		WHERE state = 'running' AND job_name = '' AND (claimed_at IS NULL OR claimed_at < $1)`, olderThan)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ReviewMembersToLog returns the members that have a result or a failure
// reported since the given time and a Job, but no stored log, oldest report
// first, as RunsToLog does for runs. Members created more than
// runLogLookback before since are left out. A superseded member's log is
// taken by its cleanup.
func (s *Store) ReviewMembersToLog(ctx context.Context, since time.Time) ([]ReviewMember, error) {
	ms, err := s.queryReviewMembers(ctx, `SELECT `+reviewMemberColumns+` FROM review_members m
		WHERE state IN ('reported', 'done', 'failed', 'fell_back') AND job_name <> ''
		  AND created_at >= $2 AND reported_at >= $1
		  AND NOT EXISTS (SELECT 1 FROM review_member_logs l WHERE l.member_id = m.id)`,
		since, since.Add(-runLogLookback))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].ReportedAt.Before(ms[j].ReportedAt) })
	return ms, nil
}

// SetReviewMemberPod records the name of the pod that runs a member's Job.
func (s *Store) SetReviewMemberPod(ctx context.Context, id, pod string) error {
	if !validUUID(id) {
		return errors.New("store: malformed review member id")
	}
	_, err := s.pool.Exec(ctx, `UPDATE review_members SET pod_name = $2 WHERE id = $1::uuid`, id, pod)
	return err
}

// AdvanceReviewMemberCheckoutSeen records how far a member's checkout was
// seen to get: 1 running, 2 done. It only raises the value.
func (s *Store) AdvanceReviewMemberCheckoutSeen(ctx context.Context, id string, seen int) error {
	if !validUUID(id) {
		return errors.New("store: malformed review member id")
	}
	_, err := s.pool.Exec(ctx, `UPDATE review_members SET checkout_seen = $2
		WHERE id = $1::uuid AND checkout_seen < $2`, id, seen)
	return err
}

// ReviewerResult is the result reviewer n (1 to 5) gives the judge run runID:
// that of the newest attempt of reviewer n of runID's review that is done,
// or failed with its result kept (never fell_back), when token is run
// runID's live result token: the run is running and the token, checked
// against the stored SHA-256 in constant time, has not expired. The token is
// not spent. Every refusal (a bad token, an unknown run, a reviewer with no
// such attempt) is (nil, false, nil).
func (s *Store) ReviewerResult(ctx context.Context, runID, token string, reviewer int) ([]byte, bool, error) {
	return s.ReviewerResultAt(ctx, runID, token, reviewer, time.Now())
}

// ReviewerResultAt is ReviewerResult with the token's expiry checked at now.
func (s *Store) ReviewerResultAt(ctx context.Context, runID, token string, reviewer int, now time.Time) ([]byte, bool, error) {
	body, _, found, err := s.ReviewerResultAuthAt(ctx, runID, token, reviewer, now)
	return body, found, err
}

// ReviewerResultAuth is ReviewerResult that also tells a refused token from
// a reviewer with no result. authed is false for a bad or missing token, a
// run with no stored token yet, an expired token, a run that is not
// running, an unknown run, and a reviewer number outside 1 to 5. found is
// true only when authed is true and the reviewer has a result.
func (s *Store) ReviewerResultAuth(ctx context.Context, runID, token string, reviewer int) (body []byte, authed, found bool, err error) {
	return s.ReviewerResultAuthAt(ctx, runID, token, reviewer, time.Now())
}

// ReviewerResultAuthAt is ReviewerResultAuth with the token's expiry checked at now.
func (s *Store) ReviewerResultAuthAt(ctx context.Context, runID, token string, reviewer int, now time.Time) (body []byte, authed, found bool, err error) {
	presented := sha256.Sum256([]byte(token))
	if !validUUID(runID) {
		subtle.ConstantTimeCompare(presented[:], noToken[:])
		return nil, false, false, nil
	}
	var (
		stored  []byte
		expires *time.Time
		state   string
		root    string
	)
	err = s.pool.QueryRow(ctx, `SELECT token_hash, token_expires_at, state, COALESCE(attempt_of, id)::text
		FROM runs WHERE id = $1::uuid`, runID).Scan(&stored, &expires, &state, &root)
	if errors.Is(err, pgx.ErrNoRows) {
		subtle.ConstantTimeCompare(presented[:], noToken[:])
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	want := stored
	if len(want) != sha256.Size {
		want = noToken[:]
	}
	same := subtle.ConstantTimeCompare(presented[:], want) == 1
	if !same || stored == nil || expires == nil || now.After(*expires) || RunState(state) != Running {
		return nil, false, false, nil
	}
	if reviewer < 1 || reviewer > maxReviewers {
		return nil, true, false, nil
	}
	err = s.pool.QueryRow(ctx, `SELECT result FROM review_members
		WHERE run_id = $1::uuid AND reviewer = $2 AND state IN ('done', 'failed') AND result IS NOT NULL
		ORDER BY created_at DESC, id DESC LIMIT 1`, root, reviewer).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	return body, true, true, nil
}

// PutReviewMemberLog stores the redacted log of a member, replacing an
// earlier one, made fit for a text column with CleanText.
func (s *Store) PutReviewMemberLog(ctx context.Context, id, text string) error {
	if !validUUID(id) {
		return errors.New("store: malformed review member id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO review_member_logs (member_id, text) VALUES ($1::uuid, $2)
		ON CONFLICT (member_id) DO UPDATE SET text = EXCLUDED.text, created_at = now()`, id, CleanText(text))
	return err
}

// ReviewMemberLog returns a member's stored log. A member without one, or a
// malformed id, is reported as not found.
func (s *Store) ReviewMemberLog(ctx context.Context, id string) (string, bool, error) {
	if !validUUID(id) {
		return "", false, nil
	}
	var text string
	err := s.pool.QueryRow(ctx, `SELECT text FROM review_member_logs WHERE member_id = $1::uuid`, id).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// PutReviewMemberDiagnostics stores a member's diagnostics, replacing
// earlier ones, made fit for a text column with CleanText.
func (s *Store) PutReviewMemberDiagnostics(ctx context.Context, id string, d Diagnostics) error {
	if !validUUID(id) {
		return errors.New("store: malformed review member id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO review_member_diagnostics (member_id, clone_log, clone_log_masked, pod_state)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (member_id) DO UPDATE SET clone_log = EXCLUDED.clone_log,
			clone_log_masked = EXCLUDED.clone_log_masked, pod_state = EXCLUDED.pod_state, created_at = now()`,
		id, CleanText(d.CloneLog), CleanText(d.CloneLogMasked), CleanText(d.PodState))
	return err
}

// ReviewMemberDiagnostics returns a member's stored diagnostics. A member
// without any, or a malformed id, is reported as not found.
func (s *Store) ReviewMemberDiagnostics(ctx context.Context, id string) (Diagnostics, bool, error) {
	if !validUUID(id) {
		return Diagnostics{}, false, nil
	}
	var d Diagnostics
	err := s.pool.QueryRow(ctx, `SELECT clone_log, clone_log_masked, pod_state
		FROM review_member_diagnostics WHERE member_id = $1::uuid`, id).Scan(&d.CloneLog, &d.CloneLogMasked, &d.PodState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Diagnostics{}, false, nil
	}
	if err != nil {
		return Diagnostics{}, false, err
	}
	return d, true, nil
}

// JudgeRow is what a judge Job run judged with (review_judges): every
// reviewer of the frozen set in --reviewer order, and either its entry,
// provider and model, or NoJudge and why. Entry.FallbackClass is not stored.
type JudgeRow struct {
	Reviewers       []int
	NoJudge         bool
	NoJudgeReason   string
	Provider, Model string
	Entry           RunEntry
}

// SetJudge records what the judge run runID being dispatched (queued or
// running) judges with, before its Job starts, replacing an earlier row. Any
// other run is an error.
func (s *Store) SetJudge(ctx context.Context, runID string, j JudgeRow) error {
	if !validUUID(runID) {
		return fmt.Errorf("store: run %q not found", runID)
	}
	reviewers := make([]int32, len(j.Reviewers))
	for i, n := range j.Reviewers {
		reviewers[i] = int32(n)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO review_judges (run_id, reviewers, no_judge, no_judge_reason, provider, model,
			model_source, model_entry_id, model_entry_name, model_team_id, chain_position, chain_length)
		SELECT id, $2::int[], $3, $4, $5, $6, $7, $8, $9, NULLIF($10::bigint, 0), $11, $12 FROM runs
		WHERE id = $1::uuid AND state IN ('queued', 'running')
		ON CONFLICT (run_id) DO UPDATE SET reviewers = EXCLUDED.reviewers, no_judge = EXCLUDED.no_judge,
			no_judge_reason = EXCLUDED.no_judge_reason, provider = EXCLUDED.provider, model = EXCLUDED.model,
			model_source = EXCLUDED.model_source, model_entry_id = EXCLUDED.model_entry_id,
			model_entry_name = EXCLUDED.model_entry_name, model_team_id = EXCLUDED.model_team_id,
			chain_position = EXCLUDED.chain_position, chain_length = EXCLUDED.chain_length`,
		runID, reviewers, j.NoJudge, cutText(j.NoJudgeReason), cutText(j.Provider), cutText(j.Model),
		j.Entry.Source, j.Entry.ID, cutText(j.Entry.Name), j.Entry.TeamID, j.Entry.Position, j.Entry.Length)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found or in the wrong state", runID)
	}
	return nil
}

// JudgeOf returns what the judge run runID judged with; found is false when
// it recorded nothing.
func (s *Store) JudgeOf(ctx context.Context, runID string) (JudgeRow, bool, error) {
	if !validUUID(runID) {
		return JudgeRow{}, false, nil
	}
	var (
		j         JudgeRow
		reviewers []int32
	)
	err := s.pool.QueryRow(ctx, `SELECT reviewers, no_judge, no_judge_reason, provider, model, model_source, model_entry_id,
			model_entry_name, COALESCE(model_team_id, 0), chain_position, chain_length
		FROM review_judges WHERE run_id = $1::uuid`, runID).Scan(&reviewers, &j.NoJudge, &j.NoJudgeReason,
		&j.Provider, &j.Model, &j.Entry.Source, &j.Entry.ID, &j.Entry.Name, &j.Entry.TeamID, &j.Entry.Position, &j.Entry.Length)
	if errors.Is(err, pgx.ErrNoRows) {
		return JudgeRow{}, false, nil
	}
	if err != nil {
		return JudgeRow{}, false, err
	}
	j.Reviewers = make([]int, len(reviewers))
	for i, n := range reviewers {
		j.Reviewers[i] = int(n)
	}
	return j, true, nil
}

// EnqueueJudgeRetry closes the reported judge run as fell_back with reason
// OutcomeJudgeFailed and queues the review's next attempt, as EnqueueAttempt
// does, with no mark. created is false when the run is not reported.
func (s *Store) EnqueueJudgeRetry(ctx context.Context, from Run) (id string, created bool, err error) {
	return s.enqueueNextAttempt(ctx, from, OutcomeJudgeFailed, nil)
}

// InheritMultiReviewSplit gives the running later attempt id of a judge run,
// not yet dispatched, what the run it fell back from carries: that run is
// the one that was split, or an earlier judge attempt that took it from
// there, so these are what the split recorded and its dispatch: the base and
// start SHAs, the model and entry, the skills, the step limit, the rules
// hash and built-in note, whether it is quiet or incremental, the layout,
// the links and the change's details. It is never the review's first run as
// such: a review can fall back on reviewer 1's chain before it is split, and
// those attempts carry older values. The judge Job writes none of them itself, and its
// result is checked against them. Any other run is an error.
func (s *Store) InheritMultiReviewSplit(ctx context.Context, id string) error {
	return s.mustUpdate(ctx, id, `UPDATE runs a SET base_sha = r.base_sha, start_sha = r.start_sha,
			provider = r.provider, model = r.model, max_turns = r.max_turns,
			skill_sources = r.skill_sources, skill_versions = r.skill_versions, skill_hashes = r.skill_hashes,
			rules_hash = r.rules_hash, built_in_rules_note = r.built_in_rules_note,
			quiet_reviews = r.quiet_reviews, comment_layout = r.comment_layout,
			incremental = r.incremental, base_run_id = r.base_run_id, linked_repos = r.linked_repos,
			model_source = r.model_source, model_entry_id = r.model_entry_id, model_entry_name = r.model_entry_name,
			model_team_id = r.model_team_id, chain_position = r.chain_position, chain_length = r.chain_length,
			fallback_class = r.fallback_class,
			title = r.title, web_url = r.web_url, source_branch = r.source_branch, author = r.author
		FROM runs r WHERE a.id = $1::uuid AND a.state = 'running' AND a.dispatched_at IS NULL AND a.attempt_of IS NOT NULL
		  AND r.id = (SELECT x.id FROM runs x
			WHERE (x.id = a.attempt_of OR x.attempt_of = a.attempt_of) AND x.id <> a.id
			  AND (x.created_at, x.id) < (a.created_at, a.id)
			ORDER BY x.created_at DESC, x.id DESC LIMIT 1)`, id)
}
