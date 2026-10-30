// Package github is a small GitHub REST client: repositories, pull requests,
// compare, contents, issue comments, check runs and reviews, authenticated
// either as a GitHub App installation (see App) or with a plain token (see
// NewTokenClient).
//
// Errors never contain a token: it travels only in the Authorization header,
// which is never sent to another host on a redirect or a pagination link, and
// any copy of it in a response body is masked before it reaches an error.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIURL is GitHub.com's REST API.
	DefaultAPIURL = "https://api.github.com"

	apiVersion  = "2022-11-28"
	userAgent   = "pergent-sh"
	acceptJSON  = "application/vnd.github+json"
	acceptRaw   = "application/vnd.github.raw+json"
	expiryField = "GitHub-Authentication-Token-Expiration"

	maxRetries    = 3
	maxRetryAfter = 30 * time.Second
	maxResponse   = 32 << 20
	storedBody    = 4096
	perPage       = "100"
	maxPages      = 100
)

// Client calls the GitHub REST API with one credential: a fixed token
// (NewTokenClient) or an App installation's token (App.Client).
type Client struct {
	base  string
	hc    *http.Client
	token func(ctx context.Context) (string, error) // nil: no Authorization
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time

	mu        sync.Mutex
	expiry    time.Time
	hasExpiry bool
}

// NewTokenClient returns a client that authenticates with token, for example
// a bot user's fine-grained personal access token. An empty apiURL is
// GitHub.com. A nil hc gets a client with a 30 s timeout; the client uses a
// copy of hc that refuses redirects to another host.
func NewTokenClient(apiURL, token string, hc *http.Client) *Client {
	return newClient(apiURL, safeHTTPClient(hc), func(context.Context) (string, error) { return token, nil })
}

func newClient(apiURL string, hc *http.Client, token func(context.Context) (string, error)) *Client {
	return &Client{
		base:  normalizeBase(apiURL),
		hc:    hc,
		token: token,
		sleep: sleepCtx,
		now:   time.Now,
	}
}

func normalizeBase(apiURL string) string {
	if apiURL == "" {
		return DefaultAPIURL
	}
	return strings.TrimRight(apiURL, "/")
}

// TokenExpiry is the expiry GitHub last reported for this client's token in
// the GitHub-Authentication-Token-Expiration response header. ok is false
// until a response carried it (tokens without an expiry never do).
func (c *Client) TokenExpiry() (t time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiry, c.hasExpiry
}

// safeHTTPClient returns a copy of hc (nil: a 30 s timeout client) that
// follows a redirect only to the same scheme and host: Go forwards the
// Authorization header on a same-host redirect, and we refuse the rest.
func safeHTTPClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	own := *hc
	own.CheckRedirect = sameHostRedirects(hc.CheckRedirect)
	return &own
}

// errCrossHostRedirect is a redirect the client refused to follow.
var errCrossHostRedirect = errors.New("github: refused a redirect to another host or scheme")

func sameHostRedirects(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		first := via[0].URL
		if req.URL.Host != first.Host || req.URL.Scheme != first.Scheme {
			return errCrossHostRedirect
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("github: stopped after 10 redirects")
		}
		return nil
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var errBadSegment = errors.New("github: invalid path segment")

// segment escapes one URL path segment ("/" becomes %2F). A segment that
// would change the path ("", ".", "..") is refused.
func segment(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", errBadSegment
	}
	return url.PathEscape(s), nil
}

// repoPath is /repos/{owner}/{repo} followed by rest, which is already escaped.
func repoPath(owner, repo string, rest ...string) (string, error) {
	o, err := segment(owner)
	if err != nil {
		return "", err
	}
	r, err := segment(repo)
	if err != nil {
		return "", err
	}
	return "/repos/" + o + "/" + r + strings.Join(rest, ""), nil
}

// filePath escapes a repository path segment by segment; the slashes between
// segments stay literal, as GitHub's contents API expects. "" is the root.
func filePath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	parts := strings.Split(p, "/")
	for i, s := range parts {
		e, err := segment(s)
		if err != nil {
			return "", err
		}
		parts[i] = e
	}
	return "/" + strings.Join(parts, "/"), nil
}

func id(n int64) string { return "/" + strconv.FormatInt(n, 10) }

// response is a successful (2xx) reply.
type response struct {
	header http.Header
	body   []byte
}

// do sends one request to path (already escaped, relative to the API URL),
// retrying as send does.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, accept string) (*response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("github: encoding request: %w", err)
		}
	}
	u, err := url.Parse(c.base + path)
	if err != nil {
		return nil, errors.New("github: invalid API URL or path")
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return c.send(ctx, method, u.String(), payload, accept)
}

// retry says whether a failed attempt may be retried and how long to wait
// (0: the exponential backoff).
type retry struct {
	ok   bool
	wait time.Duration
}

// send sends a request, retrying 429, 5xx, a rate-limited 403 and connection
// errors up to 3 times. A non-2xx final reply is *APIError.
func (c *Client) send(ctx context.Context, method, rawURL string, payload []byte, accept string) (*response, error) {
	delay := time.Second
	for attempt := 0; ; attempt++ {
		resp, r, err := c.once(ctx, method, rawURL, payload, accept)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || attempt >= maxRetries || !r.ok {
			return nil, err
		}
		wait := delay
		if r.wait > 0 {
			wait = min(r.wait, maxRetryAfter)
		}
		delay *= 2
		if serr := c.sleep(ctx, wait); serr != nil {
			return nil, err
		}
	}
}

// connError is a transport failure. It carries no URL query or headers.
type connError struct{ msg string }

func (e *connError) Error() string { return e.msg }

func (c *Client) once(ctx context.Context, method, rawURL string, payload []byte, accept string) (*response, retry, error) {
	var tok string
	if c.token != nil {
		var err error
		if tok, err = c.token(ctx); err != nil {
			return nil, retry{}, err
		}
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, retry{}, errors.New("github: building request")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if errors.Is(err, errCrossHostRedirect) {
		// Not a connection failure: retrying would only be refused again.
		return nil, retry{}, errCrossHostRedirect
	}
	if err != nil {
		var ue *url.Error
		reason := err.Error()
		if errors.As(err, &ue) {
			reason = ue.Err.Error()
		}
		return nil, retry{ok: true}, &connError{msg: fmt.Sprintf("github: %s request failed: %s", method, mask(tok, reason))}
	}
	defer res.Body.Close()
	c.noteExpiry(res.Header)
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return nil, retry{ok: true}, &connError{msg: fmt.Sprintf("github: reading response: %s", mask(tok, err.Error()))}
	}
	if len(data) > maxResponse {
		return nil, retry{}, errors.New("github: response too large")
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return &response{header: res.Header, body: data}, retry{}, nil
	}
	// Mask before cutting: a cut through the token would leave part of it.
	stored := mask(tok, string(data))
	if len(stored) > storedBody {
		stored = truncateUTF8(stored, storedBody)
	}
	return nil, c.retryFor(res), &APIError{Status: res.StatusCode, Body: stored}
}

// retryFor decides whether a non-2xx reply is retried: 429 and 5xx always, a
// 403 only when it is a rate limit (X-RateLimit-Remaining: 0, or a secondary
// limit's Retry-After). The wait is Retry-After, else the time until
// X-RateLimit-Reset.
func (c *Client) retryFor(res *http.Response) retry {
	limited := strings.TrimSpace(res.Header.Get("X-RateLimit-Remaining")) == "0"
	var wait time.Duration
	if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s > 0 {
		wait = time.Duration(s) * time.Second
	} else if limited {
		if reset, err := strconv.ParseInt(strings.TrimSpace(res.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			if d := time.Unix(reset, 0).Sub(c.now()); d > 0 {
				wait = d
			}
		}
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests, res.StatusCode >= 500:
		return retry{ok: true, wait: wait}
	case res.StatusCode == http.StatusForbidden && (limited || wait > 0):
		return retry{ok: true, wait: wait}
	}
	return retry{}
}

var expiryLayouts = []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339}

// noteExpiry records the token expiry GitHub reports for a fine-grained
// personal access token.
func (c *Client) noteExpiry(h http.Header) {
	v := strings.TrimSpace(h.Get(expiryField))
	if v == "" {
		return
	}
	for _, layout := range expiryLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			c.mu.Lock()
			c.expiry, c.hasExpiry = t.UTC(), true
			c.mu.Unlock()
			return
		}
	}
}

// mask removes tok from text that may reach an error.
func mask(tok, s string) string {
	if tok == "" {
		return s
	}
	return strings.ReplaceAll(s, tok, "***")
}

func decode(data []byte, out any) error {
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("github: decoding response: invalid JSON")
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, query, nil, acceptJSON)
	if err != nil {
		return err
	}
	return decode(resp.body, out)
}

// paginate calls fn with each page's body, following the Link header's
// rel="next" URL, which must be on the API's own scheme and host.
func (c *Client) paginate(ctx context.Context, path string, fn func(body []byte) error) error {
	return c.paginateTimed(ctx, path, 0, fn)
}

// paginateTimed is paginate with each page's request (retries included)
// bounded by pageTimeout; 0 means only ctx bounds it.
func (c *Client) paginateTimed(ctx context.Context, path string, pageTimeout time.Duration, fn func(body []byte) error) error {
	base, err := url.Parse(c.base)
	if err != nil {
		return errors.New("github: invalid API URL")
	}
	u, err := url.Parse(c.base + path)
	if err != nil {
		return errors.New("github: invalid API URL or path")
	}
	u.RawQuery = url.Values{"per_page": {perPage}}.Encode()
	next := u.String()
	for page := 0; ; page++ {
		if page >= maxPages {
			return fmt.Errorf("github: more than %d pages", maxPages)
		}
		pctx, cancel := ctx, context.CancelFunc(func() {})
		if pageTimeout > 0 {
			pctx, cancel = context.WithTimeout(ctx, pageTimeout)
		}
		resp, err := c.send(pctx, http.MethodGet, next, nil, acceptJSON)
		cancel()
		if err != nil {
			return err
		}
		if err := fn(resp.body); err != nil {
			return err
		}
		link := nextLink(resp.header.Get("Link"))
		if link == "" {
			return nil
		}
		nu, err := url.Parse(link)
		if err != nil || nu.Scheme != base.Scheme || nu.Host != base.Host {
			return errors.New("github: refused a pagination link to another host or scheme")
		}
		next = nu.String()
	}
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		end := strings.Index(part, ">")
		if !strings.HasPrefix(part, "<") || end < 0 {
			continue
		}
		for _, p := range strings.Split(part[end+1:], ";") {
			p = strings.TrimSpace(p)
			if !strings.HasPrefix(p, "rel=") {
				continue
			}
			for _, rel := range strings.Fields(strings.Trim(p[len("rel="):], `"`)) {
				if rel == "next" {
					return part[1:end]
				}
			}
		}
	}
	return ""
}

func refQuery(ref string) url.Values {
	if ref == "" {
		return nil
	}
	return url.Values{"ref": {ref}}
}

func (c *Client) postID(ctx context.Context, method, path string, body any) (int64, error) {
	resp, err := c.do(ctx, method, path, nil, body, acceptJSON)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := decode(resp.body, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// CurrentUser returns the user the token belongs to (GET /user). An App
// installation token has no user; this is for NewTokenClient.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	err := c.getJSON(ctx, "/user", nil, &u)
	return u, err
}

// Repo returns a repository. One the credential can't see is ErrNotFound.
func (c *Client) Repo(ctx context.Context, owner, repo string) (Repository, error) {
	var r Repository
	p, err := repoPath(owner, repo)
	if err != nil {
		return r, err
	}
	err = c.getJSON(ctx, p, nil, &r)
	return r, err
}

// RepoAccess is what a user token may do in a repository.
type RepoAccess struct {
	// FullName is the repository's owner/name as GitHub spells it.
	FullName string
	// Private is true for a private or internal repository.
	Private bool
	// Pull is the user's read access (permissions.pull).
	Pull bool
}

// RepoAccess returns the token user's access to a repository, from GET
// /repos/{owner}/{repo}. One the user can't see is ErrNotFound. A token
// without a user (an App installation's) gets no permissions, so Pull is
// false.
func (c *Client) RepoAccess(ctx context.Context, owner, repo string) (RepoAccess, error) {
	p, err := repoPath(owner, repo)
	if err != nil {
		return RepoAccess{}, err
	}
	var r struct {
		FullName    string `json:"full_name"`
		Private     bool   `json:"private"`
		Permissions struct {
			Pull bool `json:"pull"`
		} `json:"permissions"`
	}
	if err := c.getJSON(ctx, p, nil, &r); err != nil {
		return RepoAccess{}, err
	}
	return RepoAccess{FullName: r.FullName, Private: r.Private, Pull: r.Permissions.Pull}, nil
}

// TeamMembershipActive reports whether username is an active member of the
// team org/slug (GET /orgs/{org}/teams/{slug}/memberships/{username}). A
// pending invitation is false, so is not a member (404); any other failure
// is an error.
func (c *Client) TeamMembershipActive(ctx context.Context, org, slug, username string) (bool, error) {
	var parts [3]string
	for i, s := range []string{org, slug, username} {
		e, err := segment(s)
		if err != nil {
			return false, err
		}
		parts[i] = e
	}
	var m struct {
		State string `json:"state"`
	}
	err := c.getJSON(ctx, "/orgs/"+parts[0]+"/teams/"+parts[1]+"/memberships/"+parts[2], nil, &m)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.State == "active", nil
}

// RulesetApprovals is the highest required_approving_review_count of the
// pull_request rules the repository's active rulesets apply to branch
// (readable with the metadata permission); found is false when none does.
func (c *Client) RulesetApprovals(ctx context.Context, owner, repo, branch string) (n int, found bool, err error) {
	b, err := filePath(branch)
	if err != nil || b == "" {
		return 0, false, errBadSegment
	}
	p, err := repoPath(owner, repo, "/rules/branches", b)
	if err != nil {
		return 0, false, err
	}
	err = c.paginate(ctx, p, func(body []byte) error {
		var rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				RequiredApprovingReviewCount int `json:"required_approving_review_count"`
			} `json:"parameters"`
		}
		if err := decode(body, &rules); err != nil {
			return err
		}
		for _, r := range rules {
			if r.Type == "pull_request" {
				n, found = max(n, r.Parameters.RequiredApprovingReviewCount), true
			}
		}
		return nil
	})
	return n, found, err
}

// ProtectionApprovals is the required_approving_review_count of branch's
// classic protection rule. Reading it needs the Administration permission;
// GitHub answers 404 when the branch has no such rule, or the caller can't
// see it.
func (c *Client) ProtectionApprovals(ctx context.Context, owner, repo, branch string) (int, error) {
	b, err := filePath(branch)
	if err != nil || b == "" {
		return 0, errBadSegment
	}
	p, err := repoPath(owner, repo, "/branches", b, "/protection/required_pull_request_reviews")
	if err != nil {
		return 0, err
	}
	var r struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	}
	err = c.getJSON(ctx, p, nil, &r)
	return r.RequiredApprovingReviewCount, err
}

// PullRequest returns one pull request.
func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int64) (PullRequest, error) {
	var pr PullRequest
	p, err := repoPath(owner, repo, "/pulls", id(number))
	if err != nil {
		return pr, err
	}
	err = c.getJSON(ctx, p, nil, &pr)
	return pr, err
}

// Compare returns the merge base of base and head
// (GET compare/{base}...{head}, merge_base_commit.sha).
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (string, error) {
	b, err := segment(base)
	if err != nil {
		return "", err
	}
	h, err := segment(head)
	if err != nil {
		return "", err
	}
	p, err := repoPath(owner, repo, "/compare/", b, "...", h)
	if err != nil {
		return "", err
	}
	var out struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := c.getJSON(ctx, p, nil, &out); err != nil {
		return "", err
	}
	return out.MergeBaseCommit.SHA, nil
}

// File returns a file's raw content at ref. A missing file is nil, false, nil,
// and so is a directory: GitHub answers a directory with its JSON listing
// whatever media type was asked for, which is not a file (as on GitLab).
func (c *Client) File(ctx context.Context, owner, repo, path, ref string) ([]byte, bool, error) {
	fp, err := filePath(path)
	if err != nil {
		return nil, false, err
	}
	p, err := repoPath(owner, repo, "/contents", fp)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.do(ctx, http.MethodGet, p, refQuery(ref), nil, acceptRaw)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if isListing(resp) {
		return nil, false, nil
	}
	return resp.body, true, nil
}

// isListing reports whether a contents response is a directory listing: JSON
// (a raw file never is) whose body is an array.
func isListing(resp *response) bool {
	mt, _, _ := mime.ParseMediaType(resp.header.Get("Content-Type"))
	return mt == "application/json" && bytes.HasPrefix(bytes.TrimSpace(resp.body), []byte("["))
}

// Tree lists one directory at ref (non-recursive) through the contents API.
// A missing directory, or a path that isn't one, is nil, nil.
func (c *Client) Tree(ctx context.Context, owner, repo, path, ref string) ([]TreeEntry, error) {
	fp, err := filePath(path)
	if err != nil {
		return nil, err
	}
	p, err := repoPath(owner, repo, "/contents", fp)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, p, refQuery(ref), nil, acceptJSON)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(resp.body), []byte("[")) {
		return nil, nil // a file, symlink or submodule: an object, not a listing
	}
	var raw []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := decode(resp.body, &raw); err != nil {
		return nil, err
	}
	out := make([]TreeEntry, 0, len(raw))
	for _, e := range raw {
		t := e.Type
		switch t {
		case "file":
			t = "blob"
		case "dir":
			t = "tree"
		case "submodule":
			t = "commit"
		}
		out = append(out, TreeEntry{Name: e.Name, Path: e.Path, Type: t})
	}
	return out, nil
}

// IssueComments returns every conversation comment on a pull request.
func (c *Client) IssueComments(ctx context.Context, owner, repo string, number int64) ([]Comment, error) {
	p, err := repoPath(owner, repo, "/issues", id(number), "/comments")
	if err != nil {
		return nil, err
	}
	var out []Comment
	err = c.paginate(ctx, p, func(body []byte) error {
		var page []Comment
		if err := decode(body, &page); err != nil {
			return err
		}
		out = append(out, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateIssueComment adds a conversation comment to a pull request and
// returns its ID.
func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int64, body string) (int64, error) {
	p, err := repoPath(owner, repo, "/issues", id(number), "/comments")
	if err != nil {
		return 0, err
	}
	return c.postID(ctx, http.MethodPost, p, map[string]string{"body": body})
}

// UpdateIssueComment replaces a comment's body. A missing comment is
// ErrNotFound.
func (c *Client) UpdateIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	p, err := repoPath(owner, repo, "/issues/comments", id(commentID))
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch, p, nil, map[string]string{"body": body}, acceptJSON)
	return err
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

type checkRun struct {
	Name       string         `json:"name"`
	HeadSHA    string         `json:"head_sha"`
	Status     string         `json:"status"`
	Conclusion string         `json:"conclusion,omitempty"`
	Output     checkRunOutput `json:"output"`
}

// CreateCheckRun creates a check run on headSHA and returns its ID. The
// conclusion is sent only when status is "completed".
func (c *Client) CreateCheckRun(ctx context.Context, owner, repo, name, headSHA, status, conclusion, title, summary string) (int64, error) {
	p, err := repoPath(owner, repo, "/check-runs")
	if err != nil {
		return 0, err
	}
	run := checkRun{Name: name, HeadSHA: headSHA, Status: status, Output: checkRunOutput{Title: title, Summary: summary}}
	if status == "completed" {
		run.Conclusion = conclusion
	}
	return c.postID(ctx, http.MethodPost, p, run)
}

// CreateReview submits a review with event (for example "APPROVE") on
// commitID and returns its ID.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int64, commitID, event string) (int64, error) {
	p, err := repoPath(owner, repo, "/pulls", id(number), "/reviews")
	if err != nil {
		return 0, err
	}
	return c.postID(ctx, http.MethodPost, p, map[string]string{"commit_id": commitID, "event": event})
}

// ListReviews returns every review on a pull request, oldest first.
func (c *Client) ListReviews(ctx context.Context, owner, repo string, number int64) ([]Review, error) {
	p, err := repoPath(owner, repo, "/pulls", id(number), "/reviews")
	if err != nil {
		return nil, err
	}
	var out []Review
	err = c.paginate(ctx, p, func(body []byte) error {
		var page []Review
		if err := decode(body, &page); err != nil {
			return err
		}
		out = append(out, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DismissReview dismisses a review with message. A missing review is
// ErrNotFound; the caller decides whether that counts as done.
func (c *Client) DismissReview(ctx context.Context, owner, repo string, number, reviewID int64, message string) error {
	p, err := repoPath(owner, repo, "/pulls", id(number), "/reviews", id(reviewID), "/dismissals")
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, p, nil, map[string]string{"message": message, "event": "DISMISS"}, acceptJSON)
	return err
}

// errStopPaging ends a paginate early; it is not an error to the caller.
var errStopPaging = errors.New("github: stop paging")

// ListOptions selects the repositories UserRepos and InstallationRepos
// list.
type ListOptions struct {
	// Match, when not "", keeps the repositories whose full name holds it,
	// ignoring case (GitHub has no search on these lists: it is filtered
	// here, before Limit).
	Match string
	// Limit is the most names returned.
	Limit int
	// CallTimeout bounds each page's request; 0 means only ctx bounds it.
	CallTimeout time.Duration
}

// collectNames pages through path, adding the names page finds in each
// page that hold o.Match until o.Limit; truncated is true when there were
// more. On an error it also returns the names read before it.
func (c *Client) collectNames(ctx context.Context, path string, o ListOptions, page func(body []byte) ([]string, error)) (names []string, truncated bool, err error) {
	match := strings.ToLower(o.Match)
	err = c.paginateTimed(ctx, path, o.CallTimeout, func(body []byte) error {
		got, err := page(body)
		if err != nil {
			return err
		}
		for _, n := range got {
			if match != "" && !strings.Contains(strings.ToLower(n), match) {
				continue
			}
			if len(names) == o.Limit {
				truncated = true
				return errStopPaging
			}
			names = append(names, n)
		}
		return nil
	})
	if errors.Is(err, errStopPaging) {
		err = nil
	}
	return names, truncated, err
}

// UserRepos returns the full names of the repositories the token's user is
// the owner, a collaborator or an organization member of and can read
// (permissions.pull), from GET /user/repos, as o selects; truncated is true
// when there were more. For a GitHub App's user token GitHub lists only the
// repositories the App's installations reach too.
func (c *Client) UserRepos(ctx context.Context, o ListOptions) ([]string, bool, error) {
	return c.collectNames(ctx, "/user/repos", o, func(body []byte) ([]string, error) {
		var page []struct {
			FullName    string `json:"full_name"`
			Permissions struct {
				Pull bool `json:"pull"`
			} `json:"permissions"`
		}
		if err := decode(body, &page); err != nil {
			return nil, err
		}
		var out []string
		for _, r := range page {
			if r.FullName != "" && r.Permissions.Pull {
				out = append(out, r.FullName)
			}
		}
		return out, nil
	})
}

// InstallationRepos returns the full names of the repositories the client's
// App installation reaches (GET /installation/repositories), as o selects;
// truncated is true when there were more. The client must be App.Client's.
func (c *Client) InstallationRepos(ctx context.Context, o ListOptions) ([]string, bool, error) {
	return c.collectNames(ctx, "/installation/repositories", o, func(body []byte) ([]string, error) {
		var page struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := decode(body, &page); err != nil {
			return nil, err
		}
		var out []string
		for _, r := range page.Repositories {
			if r.FullName != "" {
				out = append(out, r.FullName)
			}
		}
		return out, nil
	})
}
