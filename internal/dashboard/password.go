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
// either way.
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
	if err := st.SetPassword(ctx, acc.ID, hash, true); err != nil {
		return "", err
	}
	if err := st.DeleteAccountSessions(ctx, acc.ID, nil); err != nil {
		return "", err
	}
	if err := st.Audit(ctx, store.AuditEntry{At: now, Actor: "cli", Action: "password_reset", Detail: acc.Username}); err != nil {
		return "", err
	}
	return pw, nil
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
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts, try again later")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	acc, found, err := a.store.PasswordAccount(ctx, in.Username)
	if err != nil {
		a.log.Error("dashboard password sign-in: loading the account failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "sign-in failed, please try again")
		return
	}
	if !found {
		password.Verify(dummyHash, in.Password)
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}
	log := a.log.With("user", acc.Username)
	locked := acc.LockedUntil != nil && acc.LockedUntil.After(now)
	if !password.Verify(acc.PasswordHash, in.Password) || locked {
		a.failedSignIn(ctx, acc, locked, now, log)
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
	if err := a.store.RecordSignIn(ctx, acc.ID, now); err != nil {
		log.Error("dashboard password sign-in: recording it failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "sign-in failed, please try again")
		return
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

// failedSignIn records a refused sign-in of acc: a wrong password counts
// towards the lock (wasLocked: the account was locked already, so the
// right password was refused too and nothing more is counted).
func (a *Auth) failedSignIn(ctx context.Context, acc store.Account, wasLocked bool, now time.Time, log *slog.Logger) {
	detail := "wrong password"
	if wasLocked {
		detail = "account locked"
	}
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
// {current, new}. It runs behind Require, which checks the CSRF token. The
// account's other sessions end, and a change asked for at sign-in is done.
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
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts, try again later")
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
	if !password.Verify(acc.PasswordHash, in.Current) {
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	if subtle.ConstantTimeCompare([]byte(in.New), []byte(in.Current)) == 1 {
		writeError(w, http.StatusBadRequest, "the new password must differ from the current one")
		return
	}
	hash, err := password.Hash(in.New)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	log := a.log.With("user", acc.Username)
	if err := a.store.SetPassword(ctx, acc.ID, hash, false); err != nil {
		log.Error("dashboard: storing a new password failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not change the password")
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
