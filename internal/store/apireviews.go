package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrIdempotencyMismatch is returned by EnqueueAPIReview when the client used
// the idempotency key before for another request.
var ErrIdempotencyMismatch = errors.New("store: idempotency key reused with another request")

// NewAPIReview is a review asked for through the review API. RequestHash is
// the hash of the request the IdempotencyKey ("" for none) was sent with, and
// InputsSealed the sealed inputs the Job gets.
type NewAPIReview struct {
	ClientID                                        int64
	Tenant, Subject, Title, KeyName, IdempotencyKey string
	RequestHash                                     []byte
	BaseSHA, HeadSHA                                string
	InputsSealed                                    []byte
	FileCount, FileBytes                            int
}

// APIReview is an API review: its run and what the caller sent. Response is
// the result as the API returns it, set when the review finished (nil
// before), at FinishedAt. HasInputs is true while the sealed inputs are kept.
type APIReview struct {
	Run                                         Run
	ClientID                                    int64
	ClientName, Tenant, Subject, Title, KeyName string
	Response                                    []byte
	FinishedAt                                  time.Time
	HasInputs                                   bool
	CreatedAt                                   time.Time
}

// apiReviewFrom joins each API review to its run. The review's columns are
// renamed where runs has a column of that name, so runColumns stays
// unqualified.
const apiReviewFrom = `runs JOIN (
		SELECT ar.run_id, ar.client_id, c.name AS client_name, ar.tenant, ar.subject, ar.title AS api_title,
			ar.key_name, ar.response, ar.finished_at, ar.inputs_sealed IS NOT NULL AS has_inputs,
			ar.created_at AS api_created_at
		FROM api_reviews ar JOIN api_clients c ON c.id = ar.client_id) a ON a.run_id = runs.id`

const apiReviewColumns = runColumns + `, a.client_id, a.client_name, a.tenant, a.subject, a.api_title,
	a.key_name, a.response, a.finished_at, a.has_inputs, a.api_created_at`

// scanAPIReview reads a row of apiReviewColumns. It reads the run's columns
// through scanRun, so the two never drift apart.
func scanAPIReview(row pgx.Row) (APIReview, error) {
	var (
		r        APIReview
		finished *time.Time
	)
	run, err := scanRun(tailScanner{row, []any{&r.ClientID, &r.ClientName, &r.Tenant, &r.Subject, &r.Title,
		&r.KeyName, &r.Response, &finished, &r.HasInputs, &r.CreatedAt}})
	if err != nil {
		return APIReview{}, err
	}
	r.Run = run
	r.FinishedAt = deref(finished)
	return r, nil
}

// tailScanner is a row whose Scan fills the given targets and then tail, for
// a row of runColumns followed by more columns.
type tailScanner struct {
	row  pgx.Row
	tail []any
}

func (t tailScanner) Scan(dest ...any) error {
	return t.row.Scan(append(dest, t.tail...)...)
}

// lockAPI takes a transaction-scoped lock named key, as lockMR does for a
// merge request.
func lockAPI(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// EnqueueAPIReview queues an API review: a run of kind KindAPI with no merge
// request and its api_reviews row, in one transaction. A subject is its
// client, tenant and subject together. The subject's queued and running API
// reviews are superseded, as a push supersedes a merge request's, and each
// gets its callback (when the client has a callback URL); a reported one is
// not: it finishes, and its callback goes out then.
//
// With an IdempotencyKey the client used before, nothing is created or
// superseded: created is false and id is that review's, whatever its state,
// or ErrIdempotencyMismatch when it was sent with another RequestHash.
//
// It locks the subject, then the key: two requests for one subject never both
// leave a live review, and two retries with one key make one review. Every
// call takes the locks in that order, so they never deadlock.
func (s *Store) EnqueueAPIReview(ctx context.Context, n NewAPIReview) (id string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	client := strconv.FormatInt(n.ClientID, 10)
	if err := lockAPI(ctx, tx, "api/"+client+"/"+n.Tenant+"/"+n.Subject); err != nil {
		return "", false, err
	}
	var key *string
	if n.IdempotencyKey != "" {
		key = &n.IdempotencyKey
		if err := lockAPI(ctx, tx, "apikey/"+client+"/"+n.IdempotencyKey); err != nil {
			return "", false, err
		}
		var stored []byte
		err := tx.QueryRow(ctx, `SELECT run_id::text, request_hash FROM api_reviews
			WHERE client_id = $1 AND idempotency_key = $2`, n.ClientID, n.IdempotencyKey).Scan(&id, &stored)
		if err == nil {
			if !bytes.Equal(stored, n.RequestHash) {
				return "", false, ErrIdempotencyMismatch
			}
			if err := tx.Commit(ctx); err != nil {
				return "", false, err
			}
			return id, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", false, err
		}
	}

	// Each review superseded gets its callback, when the client has a URL.
	if _, err := tx.Exec(ctx, `WITH s AS (
			UPDATE runs SET state = 'superseded'
			WHERE id IN (SELECT run_id FROM api_reviews WHERE client_id = $1 AND tenant = $2 AND subject = $3)
			  AND state IN ('queued', 'running')
			RETURNING id)
		INSERT INTO api_callbacks (run_id, client_id)
		SELECT s.id, c.id FROM s JOIN api_clients c ON c.id = $1 WHERE c.callback_url <> ''
		ON CONFLICT (run_id) DO NOTHING`, n.ClientID, n.Tenant, n.Subject); err != nil {
		return "", false, err
	}
	// The quota is the client's own (its monthly token budget), so the
	// repository quota never holds the run.
	err = tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha, base_sha, kind, api_client_id, quota_checked)
		VALUES ('', 0, nextval('api_review_numbers'), $1, $2, 'api', $3, true) RETURNING id::text`,
		n.HeadSHA, n.BaseSHA, n.ClientID).Scan(&id)
	if err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_reviews (run_id, client_id, tenant, subject, title, key_name,
			idempotency_key, request_hash, inputs_sealed, file_count, file_bytes)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, n.ClientID, n.Tenant, n.Subject, n.Title, n.KeyName,
		key, n.RequestHash, n.InputsSealed, n.FileCount, n.FileBytes); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// APIReview returns the client's review id. Another client's review, a merge
// request's run, or a malformed or unknown id is not found.
func (s *Store) APIReview(ctx context.Context, clientID int64, id string) (APIReview, bool, error) {
	return s.oneAPIReview(ctx, `runs.id = $1::uuid AND a.client_id = $2`, id, clientID)
}

// APIReviewByKey returns the client's review that was asked for with
// idempotency key, and the hash of the request it was sent with. found is
// false when the client has not used the key (or the review is gone).
func (s *Store) APIReviewByKey(ctx context.Context, clientID int64, key string) (r APIReview, requestHash []byte, found bool, err error) {
	var id string
	err = s.pool.QueryRow(ctx, `SELECT run_id::text, request_hash FROM api_reviews
		WHERE client_id = $1 AND idempotency_key = $2`, clientID, key).Scan(&id, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIReview{}, nil, false, nil
	}
	if err != nil {
		return APIReview{}, nil, false, err
	}
	r, found, err = s.APIReview(ctx, clientID, id)
	if err != nil || !found {
		return APIReview{}, nil, false, err
	}
	return r, requestHash, true, nil
}

// APIReviewByID returns the review id, of any client, for an admin.
func (s *Store) APIReviewByID(ctx context.Context, id string) (APIReview, bool, error) {
	return s.oneAPIReview(ctx, `runs.id = $1::uuid`, id)
}

func (s *Store) oneAPIReview(ctx context.Context, where, id string, args ...any) (APIReview, bool, error) {
	if !validUUID(id) {
		return APIReview{}, false, nil
	}
	r, err := scanAPIReview(s.pool.QueryRow(ctx, `SELECT `+apiReviewColumns+` FROM `+apiReviewFrom+`
		WHERE `+where, append([]any{id}, args...)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIReview{}, false, nil
	}
	if err != nil {
		return APIReview{}, false, err
	}
	return r, true, nil
}

// APIReviewCursor is where a page of APIReviews ends: the last review's
// CreatedAt and run ID.
type APIReviewCursor struct {
	CreatedAt time.Time
	ID        string
}

// APIReviewFilter picks a client's reviews. An empty Tenant, Subject or Status
// matches any; Status is a v1 status. After starts the page after that
// review, and Limit caps it (0 or less: no cap). A ClientID of 0 matches no
// review.
type APIReviewFilter struct {
	ClientID                int64
	Tenant, Subject, Status string
	After                   *APIReviewCursor
	Limit                   int
}

// apiStatusSQL is each v1 status of an API review, from its run's state: the
// rules of reviewapi.StatusOf. A claimed run without a Job is still queued; a
// reported one is running until its result is out. Once published it is
// completed when the stored result's status is, and failed otherwise (also
// when there is no result, only a failure); publish_failed is failed.
var apiStatusSQL = map[string]string{
	"queued":     `(runs.state = 'queued' OR (runs.state = 'running' AND runs.job_name = ''))`,
	"running":    `((runs.state = 'running' AND runs.job_name <> '') OR runs.state = 'reported')`,
	"completed":  `(runs.state = 'published' AND runs.result->>'status' = 'completed')`,
	"failed":     `(runs.state = 'publish_failed' OR (runs.state = 'published' AND runs.result->>'status' IS DISTINCT FROM 'completed'))`,
	"superseded": `runs.state = 'superseded'`,
}

// APIReviews returns the reviews f picks, newest first (by CreatedAt, then
// by run ID). An unknown Status or a malformed cursor ID is an error.
func (s *Store) APIReviews(ctx context.Context, f APIReviewFilter) ([]APIReview, error) {
	status := "true"
	if f.Status != "" {
		var ok bool
		if status, ok = apiStatusSQL[f.Status]; !ok {
			return nil, fmt.Errorf("store: unknown review status %q", f.Status)
		}
	}
	var afterAt *time.Time
	var afterID *string
	if f.After != nil {
		if !validUUID(f.After.ID) {
			return nil, fmt.Errorf("store: cursor ID %q is not a run ID", f.After.ID)
		}
		afterAt, afterID = &f.After.CreatedAt, &f.After.ID
	}
	var limit *int
	if f.Limit > 0 {
		limit = &f.Limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+apiReviewColumns+` FROM `+apiReviewFrom+`
		WHERE a.client_id = $1 AND ($2 = '' OR a.tenant = $2) AND ($3 = '' OR a.subject = $3) AND `+status+`
		  AND ($4::timestamptz IS NULL OR (a.api_created_at, a.run_id) < ($4, $5::uuid))
		ORDER BY a.api_created_at DESC, a.run_id DESC LIMIT $6`,
		f.ClientID, f.Tenant, f.Subject, afterAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIReview
	for rows.Next() {
		r, err := scanAPIReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// APIClientStats is what a client did from some time on: its API reviews (of
// any status), how many of them failed (the v1 status), the tokens its
// reviews' skills used, and how many keys it has now.
type APIClientStats struct {
	Keys, Reviews, Failed, Tokens int64
}

// APIClientStats returns the stats of every client by ID, with the reviews
// created at or after since. A client with no review has zero counts; every
// client is there. It is one query, whatever the number of clients.
func (s *Store) APIClientStats(ctx context.Context, since time.Time) (map[int64]APIClientStats, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, (SELECT count(*) FROM api_keys k WHERE k.client_id = c.id),
			COALESCE(m.reviews, 0), COALESCE(m.failed, 0), COALESCE(m.tokens, 0)
		FROM api_clients c LEFT JOIN (
			SELECT ar.client_id, count(*) AS reviews, count(*) FILTER (WHERE `+apiStatusSQL["failed"]+`) AS failed,
				COALESCE(sum(u.tokens), 0)::bigint AS tokens
			FROM api_reviews ar JOIN runs ON runs.id = ar.run_id
			LEFT JOIN (SELECT run_id, sum(tokens_in + tokens_out) AS tokens FROM run_usage GROUP BY run_id) u
				ON u.run_id = ar.run_id
			WHERE ar.created_at >= $1
			GROUP BY ar.client_id) m ON m.client_id = c.id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]APIClientStats{}
	for rows.Next() {
		var (
			id int64
			st APIClientStats
		)
		if err := rows.Scan(&id, &st.Keys, &st.Reviews, &st.Failed, &st.Tokens); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// APIReviewInputs returns the sealed inputs of the API review runID. found is
// false once they are dropped, or for an unknown run.
func (s *Store) APIReviewInputs(ctx context.Context, runID string) ([]byte, bool, error) {
	if !validUUID(runID) {
		return nil, false, nil
	}
	var in []byte
	err := s.pool.QueryRow(ctx, `SELECT inputs_sealed FROM api_reviews
		WHERE run_id = $1::uuid AND inputs_sealed IS NOT NULL`, runID).Scan(&in)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return in, true, nil
}

// DropAPIInputs clears the sealed inputs of the API review runID, once its Job
// has them. Inputs already dropped, or an unknown run, are not an error.
func (s *Store) DropAPIInputs(ctx context.Context, runID string) error {
	if !validUUID(runID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE api_reviews SET inputs_sealed = NULL WHERE run_id = $1::uuid`, runID)
	return err
}

// DropSpentAPIInputs clears the sealed inputs no Job will need again: those
// of a review with a Job (which has them) and of one that is past dispatch
// without one (superseded, or failed before its Job). A queued review, or one
// claimed and not yet dispatched, keeps them. It returns how many it cleared.
func (s *Store) DropSpentAPIInputs(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE api_reviews ar SET inputs_sealed = NULL
		FROM runs r
		WHERE r.id = ar.run_id AND ar.inputs_sealed IS NOT NULL
		  AND (r.job_name <> '' OR r.state NOT IN ('queued', 'running', 'waiting'))`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// FinishAPIReview records that a reported API review's result is out: the
// run is marked published with dec, as MarkPublished does, and the review
// keeps response, the result as the API returns it, finished now, and its
// callback is queued when the client has a callback URL, in one transaction.
// A run that is not reported, or a response that is not JSON
// (ErrInvalidResult), is an error and changes nothing.
func (s *Store) FinishAPIReview(ctx context.Context, runID string, response []byte, dec RunDecision) error {
	if !json.Valid(response) {
		return ErrInvalidResult
	}
	if !validUUID(runID) {
		return fmt.Errorf("store: run %q not found", runID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, markPublishedSQL, runID, dec.Outcome, dec.Reason, dec.Status, dec.Approved)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found or in the wrong state", runID)
	}
	tag, err = tx.Exec(ctx, `UPDATE api_reviews SET response = $2::jsonb, finished_at = now()
		WHERE run_id = $1::uuid`, runID, string(response))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s is not an API review", runID)
	}
	if _, err := tx.Exec(ctx, insertCallbackSQL, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
