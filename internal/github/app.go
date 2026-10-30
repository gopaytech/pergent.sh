package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	jwtBackdate  = 60 * time.Second
	jwtLifetime  = 9 * time.Minute
	tokenRefresh = 5 * time.Minute
)

// App is a GitHub App: it signs JWTs with the App's private key, looks up
// installations and mints installation tokens. Installation tokens are
// cached per installation and refreshed when fewer than 5 minutes remain;
// clone tokens are minted fresh every time.
type App struct {
	base  string
	appID int64
	key   *rsa.PrivateKey
	hc    *http.Client
	api   *Client // authenticated with the App's JWT
	anon  *Client // no credentials, for public lookups

	// Injectable for tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu     sync.Mutex
	tokens map[int64]*cachedToken
}

// cachedToken is one installation's token. Its mutex is held while minting,
// so concurrent callers for the same installation mint once.
type cachedToken struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewApp returns the App appID whose private key is privateKeyPEM (PKCS#1
// "RSA PRIVATE KEY" or PKCS#8 "PRIVATE KEY" holding an RSA key). An empty
// apiURL is GitHub.com; hc is handled as in NewTokenClient. Errors never
// contain any of the key's text.
func NewApp(apiURL string, appID int64, privateKeyPEM string, hc *http.Client) (*App, error) {
	if appID <= 0 {
		return nil, errors.New("github: app id must be positive")
	}
	key, err := parseKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	a := &App{
		base:   normalizeBase(apiURL),
		appID:  appID,
		key:    key,
		hc:     safeHTTPClient(hc),
		now:    time.Now,
		sleep:  sleepCtx,
		tokens: map[int64]*cachedToken{},
	}
	a.api = a.client(func(context.Context) (string, error) { return a.JWT(a.now()) })
	a.anon = a.client(nil)
	return a, nil
}

// parseKey reads an RSA private key from PEM. Its errors are fixed strings:
// parser errors could quote the key's bytes.
func parseKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("github: app private key is not PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("github: app private key: invalid PKCS#1 RSA key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("github: app private key: invalid PKCS#8 key")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github: app private key: PKCS#8 key is not RSA")
		}
		return rk, nil
	default:
		return nil, errors.New(`github: app private key: want PEM type "RSA PRIVATE KEY" or "PRIVATE KEY"`)
	}
}

// client returns a Client on the App's API URL and HTTP client whose clock
// and sleep are the App's.
func (a *App) client(token func(context.Context) (string, error)) *Client {
	c := newClient(a.base, a.hc, token)
	c.now = func() time.Time { return a.now() }
	c.sleep = func(ctx context.Context, d time.Duration) error { return a.sleep(ctx, d) }
	return c
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

// JWT returns the App's RS256 JSON Web Token for now: iat is 60 s earlier
// (clock drift), exp 9 minutes later, iss the App ID.
func (a *App) JWT(now time.Time) (string, error) {
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix(), strconv.FormatInt(a.appID, 10)})
	if err != nil {
		return "", errors.New("github: encoding the app JWT")
	}
	signing := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("github: signing the app JWT failed")
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Slug returns the App's slug (GET /app).
func (a *App) Slug(ctx context.Context) (string, error) {
	var out struct {
		Slug string `json:"slug"`
	}
	if err := a.api.getJSON(ctx, "/app", nil, &out); err != nil {
		return "", err
	}
	if out.Slug == "" {
		return "", errors.New("github: GET /app returned no slug")
	}
	return out.Slug, nil
}

// BotUserID returns the user ID of the App's bot user, {slug}[bot]. The
// lookup is unauthenticated: a JWT is accepted only by the /app endpoints.
func (a *App) BotUserID(ctx context.Context) (int64, error) {
	slug, err := a.Slug(ctx)
	if err != nil {
		return 0, err
	}
	s, err := segment(slug + "[bot]")
	if err != nil {
		return 0, err
	}
	var u User
	if err := a.anon.getJSON(ctx, "/users/"+s, nil, &u); err != nil {
		return 0, err
	}
	if u.ID == 0 {
		return 0, errors.New("github: bot user has no id")
	}
	return u.ID, nil
}

// Installation returns the ID of the App's installation that covers
// owner/repo. An App not installed there is ErrNotFound.
func (a *App) Installation(ctx context.Context, owner, repo string) (int64, error) {
	p, err := repoPath(owner, repo, "/installation")
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := a.api.getJSON(ctx, p, nil, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, errors.New("github: installation has no id")
	}
	return out.ID, nil
}

// Installations returns the IDs of the App's installations (GET
// /app/installations, with the App's JWT).
func (a *App) Installations(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := a.api.paginate(ctx, "/app/installations", func(body []byte) error {
		var page []struct {
			ID int64 `json:"id"`
		}
		if err := decode(body, &page); err != nil {
			return err
		}
		for _, in := range page {
			if in.ID != 0 {
				ids = append(ids, in.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// Client returns a client authenticated as installation installationID. Its
// token comes from the App's cache.
func (a *App) Client(installationID int64) *Client {
	return a.client(func(ctx context.Context) (string, error) { return a.installationToken(ctx, installationID) })
}

func (a *App) installationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	ct, ok := a.tokens[installationID]
	if !ok {
		ct = &cachedToken{}
		a.tokens[installationID] = ct
	}
	a.mu.Unlock()

	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.token != "" && ct.expires.Sub(a.now()) >= tokenRefresh {
		return ct.token, nil
	}
	tok, err := a.mint(ctx, installationID, nil)
	if err != nil {
		return "", installationError("github: installation token", err)
	}
	ct.token, ct.expires = tok.Token, tok.ExpiresAt
	return ct.token, nil
}

// ErrInstallation is matched (errors.Is) by the error of an installation
// token GitHub didn't mint: the installation token behind App.Client's
// requests, or a clone token. It never matches ErrNotFound, whatever GitHub
// answered: a 404 there means the installation is gone, not the repository,
// file or pull request the request was for. InstallationGone tells a gone
// installation apart.
var ErrInstallation = errors.New("github: installation token not minted")

// InstallationError is a failed installation token mint. Status is GitHub's
// HTTP status, 0 when there was no reply. It doesn't unwrap to the
// underlying error, so the mint's *APIError is never mistaken for the
// caller's own.
type InstallationError struct {
	Status int
	msg    string
}

func (e *InstallationError) Error() string { return e.msg }

// Is makes errors.Is(err, ErrInstallation) match.
func (e *InstallationError) Is(target error) bool { return target == ErrInstallation }

func installationError(prefix string, err error) error {
	ie := &InstallationError{msg: prefix + ": " + err.Error()}
	var ae *APIError
	if errors.As(err, &ae) {
		ie.Status = ae.Status
	}
	return ie
}

// InstallationGone reports whether err is an installation token GitHub
// refused with 404: the App isn't installed under that ID any more.
func InstallationGone(err error) bool {
	var ie *InstallationError
	return errors.As(err, &ie) && ie.Status == 404
}

type mintedToken struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// mint creates an installation access token, scoped by body when non-nil.
func (a *App) mint(ctx context.Context, installationID int64, body any) (mintedToken, error) {
	var t mintedToken
	resp, err := a.api.do(ctx, http.MethodPost, "/app/installations"+id(installationID)+"/access_tokens", nil, body, acceptJSON)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(resp.body, &t); err != nil {
		return mintedToken{}, errors.New("github: decoding the access token response")
	}
	if t.Token == "" || t.ExpiresAt.IsZero() {
		return mintedToken{}, errors.New("github: access token response without a token or expiry")
	}
	return t, nil
}

type cloneScope struct {
	RepositoryIDs []int64         `json:"repository_ids"`
	Permissions   cloneScopePerms `json:"permissions"`
}

type cloneScopePerms struct {
	Contents string `json:"contents"`
}

// CloneToken mints a token that can only read the contents of repository
// repoID, for one review Job. It is never cached. A token GitHub grants with
// anything beyond read access is refused.
func (a *App) CloneToken(ctx context.Context, installationID, repoID int64) (string, time.Time, error) {
	t, err := a.mint(ctx, installationID, cloneScope{
		RepositoryIDs: []int64{repoID},
		Permissions:   cloneScopePerms{Contents: "read"},
	})
	if err != nil {
		return "", time.Time{}, installationError("github: clone token", err)
	}
	if t.Permissions["contents"] != "read" {
		return "", time.Time{}, errors.New("github: clone token: contents access is not read")
	}
	for _, level := range t.Permissions {
		if level != "read" {
			return "", time.Time{}, errors.New("github: clone token: granted more than read access")
		}
	}
	return t.Token, t.ExpiresAt, nil
}
