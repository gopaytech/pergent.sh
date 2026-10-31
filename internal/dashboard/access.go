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

// accessResult is what the platform lets a session see, of the
// repositories checked: those on its instance, and its user's current
// username.
type accessResult struct {
	username string
	projects []store.VisibleProject
	// closed is set when no check has vouched for the session for two
	// AccessTTLs: it sees nothing and has no role, whatever its teams.
	closed bool
}

// flight is an access check in progress; requests of the same session wait
// for it instead of starting their own.
type flight struct {
	done chan struct{}
	res  accessResult
	err  error
}

// access returns what the platform lets the session see at now, of what
// cov covers. A check younger than AccessTTL made for cov's scope is used
// as stored. Otherwise one is made now; if that fails, the stored one is
// used while it is younger than two AccessTTLs (the caller keeps only what
// cov covers), and after that, or at once for a session never checked, the
// session sees nothing and has no role (fail closed, closed is set) until a
// check succeeds.
func (a *Auth) access(ctx context.Context, se store.Session, now time.Time, cov coverage) (accessResult, error) {
	if se.AccessScope == cov.scope && fresh(se.AccessCheckedAt, now, AccessTTL) {
		return a.stored(ctx, se)
	}
	res, err := a.refreshShared(ctx, se, now, cov)
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
	// Nothing to see and no role, but still the user who signed in.
	return accessResult{username: se.Username, closed: true}, nil
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
	return accessResult{username: se.Username, projects: projects}, nil
}

// refreshShared runs one access check per session at a time: a request
// that finds one in progress waits for its result, which may be for another
// coverage (a team changed meanwhile); the caller keeps only what its own
// covers, and the next request checks again.
func (a *Auth) refreshShared(ctx context.Context, se store.Session, now time.Time, cov coverage) (accessResult, error) {
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

	f.res, f.err = a.refresh(ctx, se, now, cov)

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
// its access to what cov covers again. It runs to its own deadline even if
// the request that started it goes away, as other requests may be waiting
// for it.
func (a *Auth) refresh(ctx context.Context, se store.Session, now time.Time, cov coverage) (accessResult, error) {
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
	if cur.AccessScope == cov.scope && fresh(cur.AccessCheckedAt, now, AccessTTL) {
		return a.stored(ctx, cur)
	}
	tok, err := a.liveToken(ctx, cur, now)
	if err != nil {
		return accessResult{}, err
	}
	return a.checkAccess(ctx, cur, tok, cov)
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
	p, ok := a.providers.Get(cur.Instance)
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
	p, ok := a.providers.Get(se.Instance)
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

// coverage is what the account's teams cover on instance now.
func (a *Auth) coverage(ctx context.Context, accountID int64, instance string) (coverage, error) {
	teams, err := a.store.AccountTeams(ctx, accountID)
	if err != nil {
		return coverage{}, err
	}
	repos, err := a.repos.List(ctx)
	if err != nil {
		return coverage{}, err
	}
	return cover(teams, repos, instance), nil
}

// checkAccess asks the platform, with the user's token, who the user is now
// (a GitHub login can change hands after a rename) and which of the
// repositories cov covers they can see, and stores the answer with cov's
// scope. Only those repositories are asked about. Any call that fails fails
// the whole check: nothing is stored.
func (a *Auth) checkAccess(ctx context.Context, se store.Session, tok oauthToken, cov coverage) (accessResult, error) {
	p, ok := a.providers.Get(se.Instance)
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
	visible, err := checkProjects(ctx, p, tok.AccessToken, cov.projects)
	if err != nil {
		return accessResult{}, err
	}
	for i := range visible {
		visible[i].Instance = se.Instance
	}
	if err := a.store.SetSessionAccess(ctx, se.IDHash, username, cov.scope, visible, a.now()); err != nil {
		return accessResult{}, err
	}
	return accessResult{username: username, projects: visible}, nil
}

// checkProjects returns the projects the token's user can see, each with
// its platform ID, checking accessWorkers at a time. The first failure
// stops the rest and is the result.
func checkProjects(ctx context.Context, p provider, token string, projects []string) ([]store.VisibleProject, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	seen := make([]int64, len(projects))
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
			id, ok, err := p.canSee(ctx, token, project)
			if err == nil && ok && id <= 0 {
				err = errors.New("the platform gave no ID")
			}
			if err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("checking %s: %w", project, err)
					cancel()
				})
				return
			}
			if ok {
				seen[i] = id
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []store.VisibleProject
	for i, id := range seen {
		if id > 0 {
			out = append(out, store.VisibleProject{Path: projects[i], ID: id})
		}
	}
	return out, nil
}
