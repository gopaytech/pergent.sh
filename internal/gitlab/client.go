// Package gitlab is a small GitLab REST client: merge requests, repository
// files, notes, commit statuses, approvals, webhooks and deploy tokens.
//
// Errors never contain the token: it travels only in the PRIVATE-TOKEN header,
// and any copy of it in a response body is masked before it reaches an error.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	statusName     = "pergent"
	maxRetries     = 3
	maxRetryAfter  = 30 * time.Second
	maxDescription = 255
	maxResponse    = 32 << 20
	storedBody     = 4096
)

// Client talks to one GitLab instance as one user (one token).
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
	sleep   func(ctx context.Context, d time.Duration) error // injectable for tests
}

// New returns a client for the GitLab instance at baseURL (for example
// https://gitlab.example.com). A nil hc gets a client with a 30 s timeout.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      hc,
		sleep:   sleepCtx,
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

// seg escapes one path segment; "/" becomes %2F.
func seg(s string) string { return url.PathEscape(s) }

func projPath(projectID int64) string { return "/projects/" + strconv.FormatInt(projectID, 10) }

func mrPath(projectID, iid int64) string {
	return projPath(projectID) + "/merge_requests/" + strconv.FormatInt(iid, 10)
}

// response is a successful (2xx) reply.
type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends one request, retrying 429, 5xx and connection errors. path is
// already escaped and relative to /api/v4. A non-2xx final reply is *APIError.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("gitlab: encoding request: %w", err)
		}
	}
	u, err := url.Parse(c.baseURL + "/api/v4" + path)
	if err != nil {
		return nil, errors.New("gitlab: invalid base URL or path")
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	delay := time.Second
	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.once(ctx, method, u.String(), payload)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || attempt >= maxRetries || !retryable(err) {
			return nil, err
		}
		wait := delay
		if retryAfter > 0 {
			wait = retryAfter
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
		}
		delay *= 2
		if serr := c.sleep(ctx, wait); serr != nil {
			return nil, err
		}
	}
}

// retryable reports whether err is a 429, a 5xx or a connection failure.
func retryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	var ce *connError
	return errors.As(err, &ce)
}

// connError is a transport failure. It carries no URL query or headers.
type connError struct{ msg string }

func (e *connError) Error() string { return e.msg }

func (c *Client) once(ctx context.Context, method, rawURL string, payload []byte) (*response, time.Duration, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, 0, errors.New("gitlab: building request")
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		reason := err.Error()
		if errors.As(err, &ue) {
			reason = ue.Err.Error()
		}
		return nil, 0, &connError{msg: fmt.Sprintf("gitlab: %s request failed: %s", method, c.mask(reason))}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return nil, 0, &connError{msg: fmt.Sprintf("gitlab: reading response: %s", c.mask(err.Error()))}
	}
	if len(data) > maxResponse {
		return nil, 0, errors.New("gitlab: response too large")
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return &response{status: res.StatusCode, header: res.Header, body: data}, 0, nil
	}
	stored := data
	if len(stored) > storedBody {
		stored = stored[:storedBody]
	}
	ae := &APIError{Status: res.StatusCode, Body: c.mask(string(stored))}
	var ra time.Duration
	if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s > 0 {
		ra = time.Duration(s) * time.Second
	}
	return nil, ra, ae
}

// mask removes the token from text that may reach an error.
func (c *Client) mask(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "***")
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) (http.Header, error) {
	resp, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	if err := decode(resp.body, out); err != nil {
		return nil, err
	}
	return resp.header, nil
}

func decode(data []byte, out any) error {
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("gitlab: decoding response: invalid JSON")
	}
	return nil
}

// paginate calls fn for each page, following X-Next-Page. fn returns true to stop.
func (c *Client) paginate(ctx context.Context, path string, query url.Values, fn func(body []byte) (bool, error)) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", "100")
	for {
		resp, err := c.do(ctx, http.MethodGet, path, q, nil)
		if err != nil {
			return err
		}
		stop, err := fn(resp.body)
		if err != nil || stop {
			return err
		}
		next := strings.TrimSpace(resp.header.Get("X-Next-Page"))
		if next == "" {
			return nil
		}
		q.Set("page", next)
	}
}

func isStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// CurrentUser returns the user the token belongs to.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	_, err := c.getJSON(ctx, "/user", nil, &u)
	return u, err
}

// GetProject looks a project up by its full path, for example acme/widgets.
func (c *Client) GetProject(ctx context.Context, path string) (Project, error) {
	var p Project
	_, err := c.getJSON(ctx, "/projects/"+seg(path), nil, &p)
	return p, err
}

// GetMergeRequest returns one merge request.
func (c *Client) GetMergeRequest(ctx context.Context, projectID, iid int64) (MergeRequest, error) {
	var mr MergeRequest
	_, err := c.getJSON(ctx, mrPath(projectID, iid), nil, &mr)
	return mr, err
}

// GetFile returns a file's raw content at ref. A missing file is nil, false, nil.
func (c *Client) GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, projPath(projectID)+"/repository/files/"+seg(path)+"/raw", url.Values{"ref": {ref}}, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return resp.body, true, nil
}

// ListTree lists one directory at ref (non-recursive). A missing directory is nil, nil.
func (c *Client) ListTree(ctx context.Context, projectID int64, path, ref string) ([]TreeEntry, error) {
	var out []TreeEntry
	err := c.paginate(ctx, projPath(projectID)+"/repository/tree", url.Values{"path": {path}, "ref": {ref}}, func(body []byte) (bool, error) {
		var page []TreeEntry
		if err := decode(body, &page); err != nil {
			return false, err
		}
		out = append(out, page...)
		return false, nil
	})
	if isStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindNote returns the ID of the first note written by authorID whose body
// contains marker.
func (c *Client) FindNote(ctx context.Context, projectID, iid int64, authorID int64, marker string) (int64, bool, error) {
	var found int64
	err := c.paginate(ctx, mrPath(projectID, iid)+"/notes", nil, func(body []byte) (bool, error) {
		var page []struct {
			ID     int64  `json:"id"`
			Body   string `json:"body"`
			Author struct {
				ID int64 `json:"id"`
			} `json:"author"`
		}
		if err := decode(body, &page); err != nil {
			return false, err
		}
		for _, n := range page {
			if n.Author.ID == authorID && strings.Contains(n.Body, marker) {
				found = n.ID
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return 0, false, err
	}
	return found, found != 0, nil
}

// CreateNote adds a comment to a merge request and returns its ID.
func (c *Client) CreateNote(ctx context.Context, projectID, iid int64, body string) (int64, error) {
	resp, err := c.do(ctx, http.MethodPost, mrPath(projectID, iid)+"/notes", nil, map[string]string{"body": body})
	if err != nil {
		return 0, err
	}
	var n struct {
		ID int64 `json:"id"`
	}
	if err := decode(resp.body, &n); err != nil {
		return 0, err
	}
	return n.ID, nil
}

// UpdateNote replaces a comment's body. A missing note is ErrNotFound.
func (c *Client) UpdateNote(ctx context.Context, projectID, iid, noteID int64, body string) error {
	_, err := c.do(ctx, http.MethodPut, mrPath(projectID, iid)+"/notes/"+strconv.FormatInt(noteID, 10), nil, map[string]string{"body": body})
	return err
}

// SetCommitStatus sets the "pergent" status on sha. The description is cut to
// 255 bytes on a UTF-8 boundary.
func (c *Client) SetCommitStatus(ctx context.Context, projectID int64, sha string, state StatusState, description string) error {
	_, err := c.do(ctx, http.MethodPost, projPath(projectID)+"/statuses/"+seg(sha), nil, map[string]string{
		"state":       string(state),
		"name":        statusName,
		"description": truncateUTF8(description, maxDescription),
	})
	return err
}

// Approve approves the merge request at sha. If the head is no longer sha,
// GitLab answers 409 and the result is ErrHeadMoved.
func (c *Client) Approve(ctx context.Context, projectID, iid int64, sha string) error {
	_, err := c.do(ctx, http.MethodPost, mrPath(projectID, iid)+"/approve", nil, map[string]string{"sha": sha})
	if isStatus(err, http.StatusConflict) {
		return ErrHeadMoved
	}
	return err
}

// Unapprove withdraws this user's approval. Not approved (404) is not an error.
func (c *Client) Unapprove(ctx context.Context, projectID, iid int64) error {
	_, err := c.do(ctx, http.MethodPost, mrPath(projectID, iid)+"/unapprove", nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// EnsureHook makes sure the project has a merge-request webhook for url with
// the given secret token: it edits the hook with the same URL or creates one.
func (c *Client) EnsureHook(ctx context.Context, projectID int64, hookURL, secret string) (int64, error) {
	var existing int64
	err := c.paginate(ctx, projPath(projectID)+"/hooks", nil, func(body []byte) (bool, error) {
		var page []Hook
		if err := decode(body, &page); err != nil {
			return false, err
		}
		for _, h := range page {
			if h.URL == hookURL {
				existing = h.ID
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return 0, err
	}
	cfg := map[string]any{
		"url":                     hookURL,
		"token":                   secret,
		"merge_requests_events":   true,
		"push_events":             false,
		"enable_ssl_verification": true,
	}
	if existing != 0 {
		if _, err := c.do(ctx, http.MethodPut, projPath(projectID)+"/hooks/"+strconv.FormatInt(existing, 10), nil, cfg); err != nil {
			return 0, err
		}
		return existing, nil
	}
	resp, err := c.do(ctx, http.MethodPost, projPath(projectID)+"/hooks", nil, cfg)
	if err != nil {
		return 0, err
	}
	var h Hook
	if err := decode(resp.body, &h); err != nil {
		return 0, err
	}
	return h.ID, nil
}

// DeleteHook removes a webhook. A missing hook is not an error.
func (c *Client) DeleteHook(ctx context.Context, projectID, hookID int64) error {
	_, err := c.do(ctx, http.MethodDelete, projPath(projectID)+"/hooks/"+strconv.FormatInt(hookID, 10), nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// CreateDeployToken creates a read_repository deploy token that expires at expires.
func (c *Client) CreateDeployToken(ctx context.Context, projectID int64, name string, expires time.Time) (DeployToken, error) {
	resp, err := c.do(ctx, http.MethodPost, projPath(projectID)+"/deploy_tokens", nil, map[string]any{
		"name":       name,
		"scopes":     []string{"read_repository"},
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return DeployToken{}, err
	}
	var raw struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := decode(resp.body, &raw); err != nil {
		return DeployToken{}, err
	}
	dt := DeployToken{ID: raw.ID, Username: raw.Username, Token: raw.Token}
	if raw.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, raw.ExpiresAt)
		if err != nil {
			t, err = time.Parse("2006-01-02", raw.ExpiresAt)
		}
		if err != nil {
			return DeployToken{}, errors.New("gitlab: decoding response: bad expires_at")
		}
		dt.ExpiresAt = t
	}
	return dt, nil
}

// DeleteDeployToken revokes a deploy token. A missing token is not an error.
func (c *Client) DeleteDeployToken(ctx context.Context, projectID, tokenID int64) error {
	_, err := c.do(ctx, http.MethodDelete, projPath(projectID)+"/deploy_tokens/"+strconv.FormatInt(tokenID, 10), nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
