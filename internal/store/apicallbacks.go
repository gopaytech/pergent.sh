package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AttentionCallbackFailing is the kind of the "Needs attention" item of a
// client whose callbacks keep failing: instance "", the client's name as the
// project.
const AttentionCallbackFailing = "callback_failing"

// The states of an API review's callback (APICallbackAttempts).
const (
	CallbackNone      = "none"
	CallbackPending   = "pending"
	CallbackDelivered = "delivered"
	CallbackGaveUp    = "gave_up"
)

// APICallback is a callback due to be sent: the review's run, and its
// client's callback URL and secrets as they are now (sealed; PreviousUntil is
// the zero time with no previous secret). ID is the X-Pergent-Delivery of
// every try. Attempts counts the tries so far, all failed.
type APICallback struct {
	ID, RunID                    string
	ClientID                     int64
	ClientName, URL              string
	SecretSealed, PreviousSealed []byte
	PreviousUntil, CreatedAt     time.Time
	Attempts                     int
}

// CallbackAttempt is one try of a callback: when, the HTTP status (0 for no
// answer), the error (already redacted, "" for none) and how long it took.
type CallbackAttempt struct {
	At         time.Time
	StatusCode int
	Error      string
	Duration   time.Duration
}

// insertCallbackSQL queues the callback of the API review $1 for its client
// when the client has a callback URL. A review that has one keeps it.
const insertCallbackSQL = `INSERT INTO api_callbacks (run_id, client_id)
	SELECT ar.run_id, ar.client_id FROM api_reviews ar JOIN api_clients c ON c.id = ar.client_id
	WHERE ar.run_id = $1::uuid AND c.callback_url <> ''
	ON CONFLICT (run_id) DO NOTHING`

// EnqueueAPICallback queues the callback of the API review runID, as the
// publisher does when it gives up on it. It is a no-op when the review has
// one, its client has no callback URL, or runID is not an API review.
func (s *Store) EnqueueAPICallback(ctx context.Context, runID string) error {
	if !validUUID(runID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, insertCallbackSQL, runID)
	return err
}

// DueAPICallbacks returns at most limit callbacks not delivered and not given
// up whose next try is at or before now, the longest waiting first.
func (s *Store) DueAPICallbacks(ctx context.Context, now time.Time, limit int) ([]APICallback, error) {
	rows, err := s.pool.Query(ctx, `SELECT cb.id::text, cb.run_id::text, cb.client_id, c.name, c.callback_url,
			c.callback_secret_sealed, c.previous_secret_sealed, c.previous_secret_until, cb.created_at, cb.attempts
		FROM api_callbacks cb JOIN api_clients c ON c.id = cb.client_id
		WHERE cb.delivered_at IS NULL AND cb.gave_up_at IS NULL AND cb.next_at <= $1
		ORDER BY cb.next_at, cb.id LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APICallback
	for rows.Next() {
		var (
			cb    APICallback
			until *time.Time
		)
		if err := rows.Scan(&cb.ID, &cb.RunID, &cb.ClientID, &cb.ClientName, &cb.URL,
			&cb.SecretSealed, &cb.PreviousSealed, &until, &cb.CreatedAt, &cb.Attempts); err != nil {
			return nil, err
		}
		cb.PreviousUntil = deref(until)
		out = append(out, cb)
	}
	return out, rows.Err()
}

// RecordAPICallbackAttempt records attempt a of callback id: delivered, given
// up, or else to be tried again at next. A callback that is gone (its client
// was removed) or already delivered or given up records nothing, and that is
// not an error.
func (s *Store) RecordAPICallbackAttempt(ctx context.Context, id string, a CallbackAttempt, delivered bool, next time.Time, gaveUp bool) error {
	if !validUUID(id) {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `UPDATE api_callbacks SET attempts = attempts + 1,
			delivered_at = CASE WHEN $3 THEN $2::timestamptz END,
			gave_up_at = CASE WHEN NOT $3 AND $5 THEN $2::timestamptz END,
			next_at = CASE WHEN $3 OR $5 THEN next_at ELSE $4::timestamptz END
		WHERE id = $1::uuid AND delivered_at IS NULL AND gave_up_at IS NULL`, id, a.At, delivered, next, gaveUp)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_callback_attempts (callback_id, at, status_code, error, duration_ms)
		VALUES ($1::uuid, $2, $3, $4, $5)`, id, a.At, a.StatusCode, a.Error, a.Duration.Milliseconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// APICallbackAttempts returns the state of the API review runID's callback
// (CallbackNone when it has none) and its attempts, oldest first.
func (s *Store) APICallbackAttempts(ctx context.Context, runID string) (state string, attempts []CallbackAttempt, err error) {
	if !validUUID(runID) {
		return CallbackNone, nil, nil
	}
	var (
		id                string
		delivered, gaveUp bool
	)
	err = s.pool.QueryRow(ctx, `SELECT id::text, delivered_at IS NOT NULL, gave_up_at IS NOT NULL
		FROM api_callbacks WHERE run_id = $1::uuid`, runID).Scan(&id, &delivered, &gaveUp)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallbackNone, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	switch {
	case delivered:
		state = CallbackDelivered
	case gaveUp:
		state = CallbackGaveUp
	default:
		state = CallbackPending
	}
	rows, err := s.pool.Query(ctx, `SELECT at, status_code, error, duration_ms FROM api_callback_attempts
		WHERE callback_id = $1::uuid ORDER BY at, id`, id)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			a  CallbackAttempt
			ms int64
		)
		if err := rows.Scan(&a.At, &a.StatusCode, &a.Error, &ms); err != nil {
			return "", nil, err
		}
		a.Duration = time.Duration(ms) * time.Millisecond
		attempts = append(attempts, a)
	}
	return state, attempts, rows.Err()
}

// CountUndeliveredAPICallbacks counts the client's callbacks made at or after
// since that are not delivered and had at least one failed attempt, given up
// or not.
func (s *Store) CountUndeliveredAPICallbacks(ctx context.Context, clientID int64, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_callbacks
		WHERE client_id = $1 AND created_at >= $2 AND delivered_at IS NULL AND attempts > 0`, clientID, since).Scan(&n)
	return n, err
}
