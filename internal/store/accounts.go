package store

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The kinds of account.
const (
	AccountPassword = "password" // an operator who signs in with a password
	AccountPlatform = "platform" // a GitLab or GitHub user who signed in with OAuth; no longer used, and hidden
	AccountSSO      = "sso"      // a person who signs in through OIDC or a JWT, identified by email
)

// RoleUser is an account's other role besides RoleAdmin (teams.go). On a
// team a user is a viewer or a maintainer instead (see TeamMember).
const RoleUser = "user"

var (
	// ErrExists is returned when a name (a username, a team name) is taken.
	ErrExists = errors.New("already exists")
	// ErrLastPasswordAccount is returned when deleting the only password
	// account, which would leave nobody able to sign in with a password.
	ErrLastPasswordAccount = errors.New("the last local admin can't be removed")
	// ErrBadEmail is returned by NormalizeEmail, and by what calls it, for a
	// string that isn't exactly one email address.
	ErrBadEmail = errors.New("store: not an email address")
)

// Account is a sign-in identity. The fields of the other kind are zero.
type Account struct {
	ID   int64
	Kind string
	// Username is the name shown for the account; an SSO account's is its
	// email.
	Username string
	// Role is RoleAdmin or RoleUser. A password account is always an admin.
	Role string
	// SSO accounts: the lower-cased email, and whether an admin added it
	// before it first signed in (the flag stays set after the first
	// sign-in). The account is invited while LastSignInAt is nil.
	Email string
	Added bool
	// Platform accounts: the platform, the instance and the user's ID there.
	Platform, Instance string
	PlatformUserID     int64
	// Password accounts: the argon2id hash and whether the password must be
	// changed at the next sign-in.
	PasswordHash       string
	MustChangePassword bool
	// FailedAttempts counts the wrong passwords since the last good one;
	// LockedUntil, when set, is when the lock on the account ends.
	FailedAttempts int
	LockedUntil    *time.Time
	CreatedAt      time.Time
	LastSignInAt   *time.Time
}

const accountColumns = `id, kind, username, COALESCE(platform, ''), COALESCE(instance, ''), COALESCE(platform_user_id, 0),
	COALESCE(password_hash, ''), must_change_password, failed_attempts, locked_until, created_at, last_sign_in_at,
	COALESCE(email, ''), CASE WHEN kind = 'password' THEN 'admin' ELSE role END, added`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Kind, &a.Username, &a.Platform, &a.Instance, &a.PlatformUserID,
		&a.PasswordHash, &a.MustChangePassword, &a.FailedAttempts, &a.LockedUntil, &a.CreatedAt, &a.LastSignInAt,
		&a.Email, &a.Role, &a.Added)
	return a, err
}

// NormalizeEmail returns s as the one form the store keeps an email in. It
// lower-cases ASCII letters only, so two different addresses never fold into
// one (the Kelvin sign U+212A stays as it is, and is not a "k"). s must be
// exactly one address: no surrounding or inner spaces or control characters,
// no display name, one "@" with something on both sides, at most 254 bytes.
func NormalizeEmail(s string) (string, error) {
	if s == "" || len(s) > 254 || !utf8.ValidString(s) {
		return "", ErrBadEmail
	}
	if strings.Count(s, "@") != 1 || strings.HasPrefix(s, "@") || strings.HasSuffix(s, "@") {
		return "", ErrBadEmail
	}
	var out strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		case unicode.IsSpace(r), unicode.IsControl(r), unicode.Is(unicode.Z, r), unicode.Is(unicode.Cf, r),
			strings.ContainsRune(`<>,;:"\()[]`, r):
			return "", ErrBadEmail
		}
		out.WriteRune(r)
	}
	// One plain address, no display name.
	if a, err := mail.ParseAddress(out.String()); err != nil || a.Name != "" || a.Address != out.String() {
		return "", ErrBadEmail
	}
	return out.String(), nil
}

// isUniqueViolation reports whether err is PostgreSQL's unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports whether err is PostgreSQL's
// foreign_key_violation.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// CreatePasswordAccount stores a password account; hash is the argon2id
// hash of its password and must not be empty. A username taken ignoring case
// is ErrExists.
func (s *Store) CreatePasswordAccount(ctx context.Context, username, hash string, mustChange bool, at time.Time) (Account, error) {
	if hash == "" {
		return Account{}, errors.New("store: a password account needs a password hash")
	}
	a, err := scanAccount(s.pool.QueryRow(ctx, `INSERT INTO accounts (kind, username, password_hash, must_change_password, role, created_at)
		VALUES ('password', $1, $2, $3, 'admin', $4) RETURNING `+accountColumns, username, hash, mustChange, at))
	if isUniqueViolation(err) {
		return Account{}, ErrExists
	}
	return a, err
}

// PasswordAccount returns the password account named username, ignoring
// case; found is false when there is none.
func (s *Store) PasswordAccount(ctx context.Context, username string) (Account, bool, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts
		WHERE kind = 'password' AND lower(username) = lower($1)`, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// CountPasswordAccounts returns how many password accounts there are.
func (s *Store) CountPasswordAccounts(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE kind = 'password'`).Scan(&n)
	return n, err
}

// CountWaitingAccounts returns how many SSO accounts are waiting: users (not
// admins) who have signed in and are on no team, until an admin places them.
// An invited account that never signed in does not wait.
func (s *Store) CountWaitingAccounts(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts a
		WHERE a.kind = 'sso' AND a.role = 'user' AND a.last_sign_in_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM team_members m WHERE m.account_id = a.id)`).Scan(&n)
	return n, err
}

// UpsertPlatformAccount returns the account of user userID on instance,
// making it when there is none, and records the user's current username and
// the sign-in time at.
func (s *Store) UpsertPlatformAccount(ctx context.Context, platform, instance string, userID int64, username string, at time.Time) (Account, error) {
	return scanAccount(s.pool.QueryRow(ctx, `INSERT INTO accounts (kind, username, platform, instance, platform_user_id, created_at, last_sign_in_at)
		VALUES ('platform', $1, $2, $3, $4, $5, $5)
		ON CONFLICT (instance, platform_user_id) WHERE kind = 'platform'
		DO UPDATE SET username = EXCLUDED.username, last_sign_in_at = EXCLUDED.last_sign_in_at
		RETURNING `+accountColumns, username, platform, instance, userID, at))
}

// Account returns the account with the given ID; found is false when there
// is none.
func (s *Store) Account(ctx context.Context, id int64) (Account, bool, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// Accounts returns every account that can sign in: the local admins and the
// SSO accounts, by username ignoring case. Platform accounts are left out:
// no sign-in reaches them any more.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountColumns+` FROM accounts
		WHERE kind <> 'platform' ORDER BY lower(username), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddSSOAccount makes an admin's invitation: the SSO account of email, with
// Added set, which lets that email sign in whatever the allowlist says. The
// account has not signed in yet (LastSignInAt is nil) unless it already had;
// one that exists keeps its role and its sign-in time and gets Added set.
// email is normalised as NormalizeEmail does; one that isn't an address is
// an error.
func (s *Store) AddSSOAccount(ctx context.Context, email string, at time.Time) (Account, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Account{}, err
	}
	return s.upsertSSO(ctx, `INSERT INTO accounts (kind, username, email, added, created_at)
		VALUES ('sso', $1, $1, true, $2)
		ON CONFLICT (email) DO UPDATE SET added = true
		RETURNING `+accountColumns, email, at)
}

// upsertSSO runs an INSERT ... ON CONFLICT (email) of an SSO account. Two
// statements for one address that run at the same time can both pass the
// conflict check on email and then meet at the lower(email) index, and the
// second gets a unique violation. Run again, the statement sees the first
// one's row and updates it. A violation that comes back is a true fold: a
// different address that lower() reads as the same (ErrExists).
func (s *Store) upsertSSO(ctx context.Context, query, email string, at time.Time) (Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, query, email, at))
	if isUniqueViolation(err) {
		a, err = scanAccount(s.pool.QueryRow(ctx, query, email, at))
		if isUniqueViolation(err) {
			return Account{}, ErrExists
		}
	}
	return a, err
}

// SignInSSOAccount records a sign-in of the person with email at time at:
// it makes the SSO account when there is none (a plain user, Added false),
// and otherwise sets its last sign-in time, keeping its role and Added. The
// caller has already decided that email may sign in. email is normalised as
// NormalizeEmail does.
func (s *Store) SignInSSOAccount(ctx context.Context, email string, at time.Time) (Account, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Account{}, err
	}
	return s.upsertSSO(ctx, `INSERT INTO accounts (kind, username, email, created_at, last_sign_in_at)
		VALUES ('sso', $1, $1, $2, $2)
		ON CONFLICT (email) DO UPDATE SET last_sign_in_at = EXCLUDED.last_sign_in_at
		RETURNING `+accountColumns, email, at)
}

// SSOAccountByEmail returns the SSO account of email (normalised as
// NormalizeEmail does); found is false when there is none.
func (s *Store) SSOAccountByEmail(ctx context.Context, email string) (Account, bool, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Account{}, false, err
	}
	a, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts
		WHERE kind = 'sso' AND email = $1`, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// SetAccountRole sets the role (RoleAdmin or RoleUser) of an SSO account.
// A password account is always an admin, and an ID that isn't an SSO
// account is an error.
func (s *Store) SetAccountRole(ctx context.Context, id int64, role string) error {
	if role != RoleAdmin && role != RoleUser {
		return fmt.Errorf("store: unknown account role %q", role)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE accounts SET role = $2 WHERE id = $1 AND kind = 'sso'`, id, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: no SSO account %d", id)
	}
	return nil
}

// DeleteAccount removes an account with its sessions and team memberships;
// ok is false when there is no such account. The last password account is
// kept: ErrLastPasswordAccount.
func (s *Store) DeleteAccount(ctx context.Context, id int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var kind string
	err = tx.QueryRow(ctx, `SELECT kind FROM accounts WHERE id = $1 FOR UPDATE`, id).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if kind == AccountPassword {
		// Lock every password account, so two deletions can't each see the
		// other's account still there.
		rows, err := tx.Query(ctx, `SELECT id FROM accounts WHERE kind = 'password' FOR UPDATE`)
		if err != nil {
			return false, err
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		if n <= 1 {
			return false, ErrLastPasswordAccount
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// SetPassword replaces a password account's hash, and clears its failed
// attempts and lock. An ID that isn't a password account is an error.
func (s *Store) SetPassword(ctx context.Context, id int64, hash string, mustChange bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE accounts SET password_hash = $2, must_change_password = $3,
			failed_attempts = 0, locked_until = NULL
		WHERE id = $1 AND kind = 'password'`, id, hash, mustChange)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: no password account %d", id)
	}
	return nil
}

// RecordFailedSignIn counts a wrong password at time now and, at max
// failures in a row, locks the account for lock; setLock says whether this
// call set the lock, so of failures that land at once only one does.
// Failures from before a lock that has ended start over, and a failure
// while locked leaves the lock where it is.
func (s *Store) RecordFailedSignIn(ctx context.Context, id int64, now time.Time, max int, lock time.Duration) (setLock bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var attempts int
	var until *time.Time
	err = tx.QueryRow(ctx, `SELECT failed_attempts, locked_until FROM accounts WHERE id = $1 FOR UPDATE`, id).Scan(&attempts, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if until != nil && !until.After(now) {
		attempts, until = 0, nil // the earlier lock has ended
	}
	attempts++
	if attempts >= max && until == nil {
		t := now.Add(lock)
		until, setLock = &t, true
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET failed_attempts = $2, locked_until = $3 WHERE id = $1`, id, attempts, until); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return setLock, nil
}

// RecordSignIn records a good sign-in at time at and clears the account's
// failed attempts and lock.
func (s *Store) RecordSignIn(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE accounts SET last_sign_in_at = $2, failed_attempts = 0, locked_until = NULL WHERE id = $1`, id, at)
	return err
}

// DeleteAccountSessions ends the account's sessions except the one whose ID
// hash is keep (nil keeps none).
func (s *Store) DeleteAccountSessions(ctx context.Context, id int64, keep []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE account_id = $1 AND ($2::bytea IS NULL OR id_hash <> $2)`, id, keep)
	return err
}

// RecordSignInUnlessLocked is RecordSignIn for a password sign-in: in the
// same statement it checks that the password account isn't locked at at,
// so a lock set by a concurrent failure since the account was read is never
// cleared by it. ok is false, and nothing is changed, when it is locked (or
// isn't a password account).
func (s *Store) RecordSignInUnlessLocked(ctx context.Context, id int64, at time.Time) (ok bool, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE accounts SET last_sign_in_at = $2, failed_attempts = 0, locked_until = NULL
		WHERE id = $1 AND kind = 'password' AND (locked_until IS NULL OR locked_until <= $2)`, id, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ChangePasswordUnlessLocked replaces a password account's hash, clears a
// change asked for, its failed attempts and its lock, all only when it
// isn't locked at now; ok is false, and nothing is changed, when it is
// locked (or isn't a password account). hash must not be empty.
func (s *Store) ChangePasswordUnlessLocked(ctx context.Context, id int64, hash string, now time.Time) (ok bool, err error) {
	if hash == "" {
		return false, errors.New("store: a password account needs a password hash")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE accounts SET password_hash = $2, must_change_password = false,
			failed_attempts = 0, locked_until = NULL
		WHERE id = $1 AND kind = 'password' AND (locked_until IS NULL OR locked_until <= $3)`, id, hash, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ResetPassword, in one transaction, replaces a password account's hash
// with one that must be changed at the next sign-in (clearing its failed
// attempts and lock), ends all its sessions and records audit; on an error
// none of it is done. An ID that isn't a password account is an error.
func (s *Store) ResetPassword(ctx context.Context, id int64, hash string, audit AuditEntry) error {
	if hash == "" {
		return errors.New("store: a password account needs a password hash")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `UPDATE accounts SET password_hash = $2, must_change_password = true,
			failed_attempts = 0, locked_until = NULL
		WHERE id = $1 AND kind = 'password'`, id, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: no password account %d", id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE account_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (at, actor_id, actor, action, instance, project, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, audit.At, audit.ActorID, audit.Actor, audit.Action, audit.Instance,
		audit.Project, audit.Detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
