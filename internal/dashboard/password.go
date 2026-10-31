package dashboard

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/password"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// loginCSRFCookieName holds the sign-in form's CSRF token, which
	// POST /auth/password must send in X-CSRF-Token.
	loginCSRFCookieName = "__Host-pergent_login_csrf"
	loginCSRFTTL        = 10 * time.Minute

	// maxFailures wrong passwords in a row lock a password account for
	// lockFor.
	maxFailures = 5
	lockFor     = 15 * time.Minute

	// firstAdmin is the username of the password account Bootstrap makes.
	firstAdmin = "admin"

	wrongCredentials = "wrong username or password"
	tooManyAttempts  = "too many sign-in attempts, try again later"

	// hashSlots argon2id hashes or checks (19 MiB each) run at once; a
	// request waits at most hashSlotWait for one, then gets 503.
	hashSlots    = 4
	hashSlotWait = 5 * time.Second
)

// ErrNotPasswordAccount is ResetPassword's answer for a username that is
// not a password account.
var ErrNotPasswordAccount = errors.New("not a password account")

// dummyHash is checked against for a username with no account, so the
// answer takes as long as for one with an account.
var dummyHash = func() string {
	h, err := password.Hash(password.Generate())
	if err != nil {
		panic("dashboard: hashing the dummy password: " + err.Error())
	}
	return h
}()

// Bootstrap creates the "admin" password account when none exists: the
// password is os.Getenv(cfg.Dashboard.AdminPasswordEnv) when that is set and
// not empty, else password.Generate(), logged once at Warn as "initial admin
// password" with attribute password=<value>. MustChangePassword is true
// either way. A password from the environment shorter than
// password.MinLength is an error naming the variable.
func Bootstrap(ctx context.Context, st *store.Store, cfg *config.Config, now time.Time, log *slog.Logger) error {
	n, err := st.CountPasswordAccounts(ctx)
	if err != nil {
		return fmt.Errorf("dashboard: counting password accounts: %w", err)
	}
	if n > 0 {
		return nil
	}
	pw, generated := "", false
	if name := cfg.Dashboard.AdminPasswordEnv; name != "" {
		pw = os.Getenv(name)
		if pw != "" && utf8.RuneCountInString(pw) < password.MinLength {
			return fmt.Errorf("dashboard.admin_password_env: the password in %s must have at least %d characters",
				name, password.MinLength)
		}
	}
	if pw == "" {
		pw, generated = password.Generate(), true
	}
	hash, err := password.Hash(pw)
	if err != nil {
		return fmt.Errorf("dashboard: hashing the first admin password: %w", err)
	}
	_, err = st.CreatePasswordAccount(ctx, firstAdmin, hash, true, now)
	if errors.Is(err, store.ErrExists) {
		return nil // another start made it first
	}
	if err != nil {
		return fmt.Errorf("dashboard: creating the first admin: %w", err)
	}
	if generated {
		// The one secret ever logged: nobody else knows it yet.
		log.Warn("initial admin password", "username", firstAdmin, "password", pw)
	}
	return nil
}

// ResetPassword sets a new generated password for a password account,
// forces a change, clears the lock and deletes its sessions; returns the new
// password. ErrNotPasswordAccount when username is not a password account.
// The change, the sessions' end and the audit entry are one transaction: on
// an error none of them happened.
func ResetPassword(ctx context.Context, st *store.Store, username string, now time.Time) (string, error) {
	acc, found, err := st.PasswordAccount(ctx, username)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotPasswordAccount
	}
	pw := password.Generate()
	hash, err := password.Hash(pw)
	if err != nil {
		return "", err
	}
	if err := st.ResetPassword(ctx, acc.ID, hash,
		store.AuditEntry{At: now, Actor: "cli", Action: "password_reset", Detail: acc.Username}); err != nil {
		return "", err
	}
	return pw, nil
}

// hashSlot takes one of the hashSlots argon2id slots, waiting until one is
// free, ctx ends or slotWait passes; ok is false when none came, and
// release gives the slot back otherwise. It bounds the memory the
// unauthenticated sign-in can make the process use.
func (a *Auth) hashSlot(ctx context.Context) (release func(), ok bool) {
	timer := time.NewTimer(a.slotWait)
	defer timer.Stop()
	select {
	case a.slots <- struct{}{}:
		return func() { <-a.slots }, true
	case <-timer.C:
	case <-ctx.Done():
	}
	return nil, false
}

// loginCSRF hands out the sign-in form's CSRF token: in a cookie and in the
// body, {"csrfToken": "..."}.
func (a *Auth) loginCSRF(w http.ResponseWriter, _ *http.Request) {
	token := randomString(32)
	http.SetCookie(w, &http.Cookie{
		Name: loginCSRFCookieName, Value: token, Path: "/", MaxAge: int(loginCSRFTTL / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"csrfToken": token})
}

// loginCSRFOK compares the X-CSRF-Token header with the login cookie in
// constant time.
func loginCSRFOK(r *http.Request) bool {
	c, err := r.Cookie(loginCSRFCookieName)
	if err != nil {
		return false
	}
	return csrfOK(r, c.Value)
}

// passwordSignIn signs a password account in: JSON {username, password}.
// Every failure, an unknown username included, is the same 401 after the
// same work (an argon2id check); 5 wrong passwords in a row lock the
// account for 15 minutes, and each client IP has 20 attempts per 5 minutes.
// With no argon2id slot free within 5 s the answer is 503.
func (a *Auth) passwordSignIn(w http.ResponseWriter, r *http.Request) {
	if !loginCSRFOK(r) {
		writeCSRFError(w)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be {\"username\": ..., \"password\": ...}")
		return
	}
	now := a.now()
	if !a.limit.allow(clientIP(r), now) {
		writeError(w, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	acc, found, err := a.store.PasswordAccount(ctx, in.Username)
	if err != nil {
		a.log.Error("dashboard password sign-in: loading the account failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "sign-in failed, please try again")
		return
	}
	release, ok := a.hashSlot(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, tooManyAttempts)
		return
	}
	hash := dummyHash
	if found {
		hash = acc.PasswordHash
	}
	good := a.verify(hash, in.Password)
	release()
	if !found {
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}
	log := a.log.With("user", acc.Username)
	locked := acc.LockedUntil != nil && acc.LockedUntil.After(now)
	if !good || locked {
		a.failedSignIn(ctx, acc, locked, refusedDetail(locked, "wrong password"), now, log)
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}
	if a.afterVerify != nil {
		a.afterVerify()
	}
	// The lock is checked again in the same statement that records the
	// sign-in: failures since the account was read may have set one.
	recorded, err := a.store.RecordSignInUnlessLocked(ctx, acc.ID, now)
	if err != nil {
		log.Error("dashboard password sign-in: recording it failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "sign-in failed, please try again")
		return
	}
	if !recorded {
		a.failedSignIn(ctx, acc, true, refusedDetail(true, ""), now, log)
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}

	// Never adopt the session the browser arrived with: end it.
	if hash, ok := sessionHash(r); ok {
		if err := a.store.DeleteSession(ctx, hash); err != nil {
			log.Error("dashboard password sign-in: ending the previous session failed", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "sign-in failed, please try again")
			return
		}
		a.forget(hash)
	}
	id := randomString(sessionIDBytes)
	if err := a.store.CreateSession(ctx, store.Session{
		IDHash: hashID(id), AccountID: acc.ID, Username: acc.Username, CSRF: randomString(32),
		LastSeenAt: now, CreatedAt: now,
	}); err != nil {
		log.Error("dashboard password sign-in: storing the session failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "sign-in failed, please try again")
		return
	}
	a.audit(ctx, acc, "sign_in", "", now)
	clearCookie(w, loginCSRFCookieName, "/")
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: id, Path: "/", MaxAge: int(MaxSessionAge / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	log.Info("dashboard password sign-in")
	w.WriteHeader(http.StatusNoContent)
}

// refusedDetail is the audit detail of a refused attempt: "account locked",
// or wrong when it wasn't locked.
func refusedDetail(locked bool, wrong string) string {
	if locked {
		return "account locked"
	}
	return wrong
}

// failedSignIn records a refused attempt at acc's password (a sign-in, or
// the current password at a change), with detail in the audit entry. Every
// refused attempt counts as a failure, whatever the password: one during a
// lock (wasLocked) adds to the count but leaves the lock where it is
// (store.RecordFailedSignIn). The attempt that sets the lock also records
// "locked".
func (a *Auth) failedSignIn(ctx context.Context, acc store.Account, wasLocked bool, detail string, now time.Time, log *slog.Logger) {
	locked, err := a.store.RecordFailedSignIn(ctx, acc.ID, now, maxFailures, lockFor)
	if err != nil {
		log.Error("dashboard password sign-in: recording a failure failed", "error", err.Error())
	}
	a.audit(ctx, acc, "sign_in_failed", detail, now)
	if locked && !wasLocked {
		log.Warn("dashboard password account locked after 5 wrong passwords", "for", lockFor.String())
		a.audit(ctx, acc, "locked", fmt.Sprintf("%d wrong passwords, locked for %s", maxFailures, lockFor), now)
	}
}

// audit records what acc did; a failure to is logged, not returned.
func (a *Auth) audit(ctx context.Context, acc store.Account, action, detail string, now time.Time) {
	id := acc.ID
	if err := a.store.Audit(ctx, store.AuditEntry{At: now, ActorID: &id, Actor: acc.Username, Action: action, Detail: detail}); err != nil {
		a.log.Error("dashboard: recording an audit entry failed", "action", action, "error", err.Error())
	}
}

// changePassword changes the signed-in password account's password: JSON
// {current, new}. It runs behind Require, which checks the CSRF token. A
// wrong current password counts towards the account's lock like a wrong
// sign-in, and while the account is locked every change is refused (401).
// The account's other sessions end, and a change asked for at sign-in is
// done.
func (a *Auth) changePassword(w http.ResponseWriter, r *http.Request) {
	se, _ := SessionFrom(r.Context())
	if se.Kind != store.AccountPassword {
		writeError(w, http.StatusForbidden, "only password accounts have a password to change")
		return
	}
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be {\"current\": ..., \"new\": ...}")
		return
	}
	now := a.now()
	if !a.limit.allow(clientIP(r), now) {
		writeError(w, http.StatusTooManyRequests, tooManyAttempts)
		return
	}
	if utf8.RuneCountInString(in.New) < password.MinLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the new password must have at least %d characters", password.MinLength))
		return
	}
	ctx := context.WithoutCancel(r.Context())
	acc, found, err := a.store.Account(ctx, se.AccountID)
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	log := a.log.With("user", acc.Username)
	release, ok := a.hashSlot(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, tooManyAttempts)
		return
	}
	good := a.verify(acc.PasswordHash, in.Current)
	same := subtle.ConstantTimeCompare([]byte(in.New), []byte(in.Current)) == 1
	var hash string
	if good && !same {
		hash, err = password.Hash(in.New)
	}
	release()
	locked := acc.LockedUntil != nil && acc.LockedUntil.After(now)
	if !good || locked {
		a.failedSignIn(ctx, acc, locked, refusedDetail(locked, "wrong current password at a password change"), now, log)
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	if same {
		writeError(w, http.StatusBadRequest, "the new password must differ from the current one")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	// As at sign-in, the lock is checked again in the write itself.
	changed, err := a.store.ChangePasswordUnlessLocked(ctx, acc.ID, hash, now)
	if err != nil {
		log.Error("dashboard: storing a new password failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	if !changed {
		a.failedSignIn(ctx, acc, true, refusedDetail(true, ""), now, log)
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	if err := a.store.DeleteAccountSessions(ctx, acc.ID, se.idHash); err != nil {
		log.Error("dashboard: ending the account's other sessions failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the password was changed, but other sessions could not be ended")
		return
	}
	a.audit(ctx, acc, "password_changed", "", now)
	log.Info("dashboard password changed")
	w.WriteHeader(http.StatusNoContent)
}
