package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// provider is one instance's sign-in and access checks. Every method that
// takes a token uses it only in a request to that instance.
type provider interface {
	platform() string
	// authorizeURL is where the browser goes to sign in.
	authorizeURL(state, challenge string) string
	// exchange trades the callback's code for a token.
	exchange(ctx context.Context, code, verifier string) (oauthToken, error)
	// refresh trades a refresh token for a new token.
	refresh(ctx context.Context, refreshToken string) (oauthToken, error)
	// user is the token's user.
	user(ctx context.Context, accessToken string) (id int64, username string, err error)
	// canSee reports whether the user can read the project's code and
	// merge requests, and the project's platform ID (above 0 when ok).
	// Not found or forbidden is false; a failure to tell is an error.
	canSee(ctx context.Context, accessToken, project string) (id int64, ok bool, err error)
	// isAdmin reports whether the user is in admin's group or team. A
	// team the platform refuses to show is errAdminUnreadable.
	isAdmin(ctx context.Context, accessToken string, userID int64, username string, admin config.DashboardAdmin) (bool, error)
	// memberRepos lists, up to limit, the repositories the user is a
	// member of and can read (GitLab: Reporter or above; GitHub: pull)
	// that hold search ("" for all), for the "add repository" picker. A
	// public repository the user isn't a member of is not listed. Each
	// request has its own 10 s; on an error it also returns the
	// repositories read before it.
	memberRepos(ctx context.Context, accessToken, search string, limit int) (repos []string, truncated bool, err error)
	// memberRepo reports whether the user is such a member of project,
	// and its path as the platform spells it and its platform ID. Not
	// found or forbidden is false; a failure to tell is an error.
	memberRepo(ctx context.Context, accessToken, project string) (repo store.VisibleProject, ok bool, err error)
}

// oauthToken is a user's token, stored sealed. Expiry is zero for a token
// that does not expire.
type oauthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// expiresBy reports whether the token is expired at t.
func (t oauthToken) expiresBy(at time.Time) bool {
	return !t.Expiry.IsZero() && !at.Before(t.Expiry)
}

// callContext bounds one call to the platform by d (platformTimeout when d
// is 0), whatever HTTP client makes it.
func callContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = platformTimeout
	}
	return context.WithTimeout(ctx, d)
}

// randomString is n random bytes, base64url without padding.
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the platform is broken; never fall back.
		panic("dashboard: reading random bytes: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceChallenge is the S256 challenge of a PKCE verifier.
func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// errorCode keeps an OAuth error code only if it looks like one, so a
// provider can't put anything else in our errors and logs.
var errorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// tokenClient is hc refusing every redirect: the request carries the client
// secret, and a token endpoint never redirects.
func tokenClient(hc *http.Client) *http.Client {
	own := *hc
	own.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &own
}

// postToken sends an OAuth token request (form-encoded, JSON answer) and
// reads the token. GitHub answers errors with 200 and an "error" field, so
// that field is checked whatever the status. Errors hold neither the form
// (it has the client secret and the code) nor the response body.
func postToken(ctx context.Context, hc *http.Client, endpoint string, form url.Values, now time.Time) (oauthToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthToken{}, errors.New("oauth: building token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := tokenClient(hc).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return oauthToken{}, fmt.Errorf("oauth: token request failed: %s", maskForm(err.Error(), form))
	}
	defer res.Body.Close()
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return oauthToken{}, errors.New("oauth: reading token response failed")
	}
	if json.Unmarshal(data, &body) != nil {
		body.Error = ""
		if res.StatusCode/100 == 2 {
			return oauthToken{}, fmt.Errorf("oauth: token endpoint answered %d with no JSON", res.StatusCode)
		}
	}
	if res.StatusCode/100 != 2 || body.Error != "" {
		code := "unknown"
		if errorCode.MatchString(body.Error) {
			code = body.Error
		}
		return oauthToken{}, fmt.Errorf("oauth: token endpoint answered %d, error %s", res.StatusCode, code)
	}
	if body.AccessToken == "" {
		return oauthToken{}, errors.New("oauth: token endpoint answered without an access token")
	}
	tok := oauthToken{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken}
	if body.ExpiresIn > 0 {
		tok.Expiry = now.Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// maskForm removes the form's secret values from a transport error, which
// may quote the request.
func maskForm(s string, form url.Values) string {
	for _, k := range []string{"client_secret", "code", "code_verifier", "refresh_token"} {
		if v := form.Get(k); v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}
