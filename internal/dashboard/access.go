package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// accessWorkers is how many repositories are checked at once.
	accessWorkers = 8
	// retryAfterFailure spaces the checks of a session whose last check
	// failed, so a platform outage doesn't make every request wait on it.
	retryAfterFailure = time.Minute
	// tokenLeeway renews a token this long before it expires.
	tokenLeeway = time.Minute
)

// errRecentFailure is a check skipped because the last one failed less than
// retryAfterFailure ago.
var errRecentFailure = errors.New("the last access check failed less than a minute ago")

// accessResult is what a session may see: the repositories on its instance
// and whether its user, under their current username, is an admin there.
type accessResult struct {
	username string
	isAdmin  bool
	projects []string
}

// flight is an access check in progress; requests of the same session wait
// for it instead of starting their own.
type flight struct {
	done chan struct{}
	res  accessResult
	err  error
}

// access returns what the session may see at now. A check younger than
// AccessTTL is used as stored. An older one is made again; if that fails,
// the stored one is used while it is younger than two AccessTTLs, and after
// that the session sees nothing and is no admin (fail closed) until a check
// succeeds.
func (a *Auth) access(ctx context.Context, se store.Session, now time.Time) (accessResult, error) {
	if fresh(se.AccessCheckedAt, now, AccessTTL) {
		return a.stored(ctx, se)
	}
	res, err := a.refreshShared(ctx, se, now)
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, errRecentFailure) {
		a.log.Warn("dashboard: access check failed", "platform", se.Platform, "instance", se.Instance,
			"user", se.Username, "error", err.Error())
	}
	if fresh(se.AccessCheckedAt, now, 2*AccessTTL) {
		return a.stored(ctx, se)
	}
	if err := a.store.ClearSessionAccess(ctx, se.IDHash); err != nil {
		return accessResult{}, err
	}
	// Nothing to see and no admin, but still the user who signed in.
	return accessResult{username: se.Username}, nil
}

// fresh reports whether a check at checked is at most ttl old at now.
func fresh(checked *time.Time, now time.Time, ttl time.Duration) bool {
	return checked != nil && now.Sub(*checked) <= ttl
}

// stored is the session's access as last stored.
func (a *Auth) stored(ctx context.Context, se store.Session) (accessResult, error) {
	projects, err := a.store.SessionProjects(ctx, se.IDHash)
	if err != nil {
		return accessResult{}, err
	}
	return accessResult{username: se.Username, isAdmin: se.IsAdmin, projects: projects}, nil
}

// refreshShared runs one access check per session at a time: a request
// that finds one in progress waits for its result.
func (a *Auth) refreshShared(ctx context.Context, se store.Session, now time.Time) (accessResult, error) {
	key := string(se.IDHash)
	a.mu.Lock()
	if f, ok := a.flights[key]; ok {
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return accessResult{}, ctx.Err()
		}
	}
	if at, ok := a.failedAt[key]; ok && now.Sub(at) < retryAfterFailure {
		a.mu.Unlock()
		return accessResult{}, errRecentFailure
	}
	f := &flight{done: make(chan struct{})}
	a.flights[key] = f
	a.mu.Unlock()

	f.res, f.err = a.refresh(ctx, se, now)

	a.mu.Lock()
	delete(a.flights, key)
	if f.err != nil {
		// Drop old failures (sessions gone since) as new ones come.
		for k, at := range a.failedAt {
			if now.Sub(at) > 2*AccessTTL {
				delete(a.failedAt, k)
			}
		}
		a.failedAt[key] = now
	} else {
		delete(a.failedAt, key)
	}
	a.mu.Unlock()
	close(f.done)
	return f.res, f.err
}

// forget drops what Auth keeps in memory about a session that ended.
func (a *Auth) forget(hash []byte) {
	a.mu.Lock()
	delete(a.failedAt, string(hash))
	a.mu.Unlock()
}

// refresh renews the session's token if it is about to expire and checks
// its access again. It runs to its own deadline even if the request that
// started it goes away, as other requests may be waiting for it.
func (a *Auth) refresh(ctx context.Context, se store.Session, now time.Time) (accessResult, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signInTimeout)
	defer cancel()

	// Another request may have checked since se was read.
	cur, found, err := a.store.Session(ctx, se.IDHash)
	if err != nil {
		return accessResult{}, err
	}
	if !found {
		return accessResult{}, errors.New("the session ended")
	}
	if fresh(cur.AccessCheckedAt, now, AccessTTL) {
		return a.stored(ctx, cur)
	}
	tok, err := a.liveToken(ctx, cur, now)
	if err != nil {
		return accessResult{}, err
	}
	return a.checkAccess(ctx, cur, tok)
}

// liveToken returns the session's OAuth token, renewed first when it expires
// within tokenLeeway of now. Renewals are made one at a time, each on the
// token stored when it starts, so a refresh token is never spent twice.
func (a *Auth) liveToken(ctx context.Context, cur store.Session, now time.Time) (oauthToken, error) {
	tok, err := a.open(cur.TokenSealed)
	if err != nil {
		return oauthToken{}, err
	}
	if !tok.expiresBy(now.Add(tokenLeeway)) {
		return tok, nil
	}
	a.renewMu.Lock()
	defer a.renewMu.Unlock()
	// Another request may have renewed it meanwhile.
	cur, found, err := a.store.Session(ctx, cur.IDHash)
	if err != nil {
		return oauthToken{}, err
	}
	if !found {
		return oauthToken{}, errors.New("the session ended")
	}
	if tok, err = a.open(cur.TokenSealed); err != nil {
		return oauthToken{}, err
	}
	if !tok.expiresBy(now.Add(tokenLeeway)) {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return oauthToken{}, errors.New("the sign-in token expired and can't be renewed")
	}
	p, ok := a.providers[cur.Instance]
	if !ok {
		return oauthToken{}, fmt.Errorf("instance %q has no sign-in", cur.Instance)
	}
	renewed, err := p.refresh(ctx, tok.RefreshToken)
	if err != nil {
		return oauthToken{}, fmt.Errorf("renewing the sign-in token: %w", err)
	}
	if renewed.RefreshToken == "" {
		renewed.RefreshToken = tok.RefreshToken
	}
	sealed, err := a.seal(renewed)
	if err != nil {
		return oauthToken{}, err
	}
	// A renewed refresh token replaces the old one at the provider: store
	// it before anything else can fail.
	if err := a.store.SetSessionToken(ctx, cur.IDHash, sealed); err != nil {
		return oauthToken{}, err
	}
	return renewed, nil
}

// userToken returns the provider of the session's instance and its user's
// OAuth token, renewed if needed, for a call made with it. A renewal runs
// to its own deadline even if the request goes away: a refresh token spent
// with its successor unstored would end the session. The caller waits for
// it only while ctx lasts.
func (a *Auth) userToken(ctx context.Context, se Session) (provider, string, error) {
	p, ok := a.providers[se.Instance]
	if !ok || p.platform() != se.Platform {
		return nil, "", fmt.Errorf("instance %q has no %s sign-in", se.Instance, se.Platform)
	}
	type answer struct {
		token string
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signInTimeout)
		defer cancel()
		cur, found, err := a.store.Session(ctx, se.idHash)
		if err == nil && !found {
			err = errors.New("the session ended")
		}
		if err != nil {
			done <- answer{err: err}
			return
		}
		tok, err := a.liveToken(ctx, cur, a.now())
		done <- answer{token: tok.AccessToken, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return nil, "", got.err
		}
		return p, got.token, nil
	case <-ctx.Done():
		return nil, "", context.Cause(ctx)
	}
}

// checkAccess asks the platform, with the user's token, who the user is now
// (a GitHub login can change hands after a rename), which repositories
// switched on for the session's instance they can see and whether they are
// in one of the instance's admin groups or teams, and stores the answer.
// Any call that fails fails the whole check: nothing is stored.
func (a *Auth) checkAccess(ctx context.Context, se store.Session, tok oauthToken) (accessResult, error) {
	p, ok := a.providers[se.Instance]
	if !ok || p.platform() != se.Platform {
		return accessResult{}, fmt.Errorf("instance %q has no %s sign-in", se.Instance, se.Platform)
	}
	userID, username, err := p.user(ctx, tok.AccessToken)
	if err != nil {
		return accessResult{}, fmt.Errorf("reading the user: %w", err)
	}
	if userID != se.UserID {
		return accessResult{}, fmt.Errorf("the token now belongs to user %d, not %d", userID, se.UserID)
	}
	repos, err := a.repos.List(ctx)
	if err != nil {
		return accessResult{}, err
	}
	var candidates []string
	for _, r := range repos {
		if r.Instance == se.Instance && r.Platform == se.Platform {
			candidates = append(candidates, r.Project)
		}
	}
	visible, err := checkProjects(ctx, p, tok.AccessToken, candidates)
	if err != nil {
		return accessResult{}, err
	}
	isAdmin := false
	for _, admin := range a.cfg.Dashboard.Admins {
		if admin.Instance != se.Instance {
			continue
		}
		ok, err := p.isAdmin(ctx, tok.AccessToken, userID, username, admin)
		if err != nil {
			return accessResult{}, fmt.Errorf("admin check: %w", err)
		}
		if ok {
			isAdmin = true
			break
		}
	}
	if err := a.store.SetSessionAccess(ctx, se.IDHash, username, isAdmin, visible, a.now()); err != nil {
		return accessResult{}, err
	}
	return accessResult{username: username, isAdmin: isAdmin, projects: visible}, nil
}

// checkProjects returns the projects the token's user can see, checking
// accessWorkers at a time. The first failure stops the rest and is the
// result.
func checkProjects(ctx context.Context, p provider, token string, projects []string) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	seen := make([]bool, len(projects))
	sem := make(chan struct{}, accessWorkers)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i, project := range projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			ok, err := p.canSee(ctx, token, project)
			if err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("checking %s: %w", project, err)
					cancel()
				})
				return
			}
			seen[i] = ok
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []string
	for i, ok := range seen {
		if ok {
			out = append(out, projects[i])
		}
	}
	return out, nil
}
