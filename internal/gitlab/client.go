// Package gitlab is a small GitLab REST client: merge requests, repository
// files, notes, commit statuses, approvals, webhooks and deploy tokens.
//
// Errors never contain the token: it travels only in the PRIVATE-TOKEN header
// (a user's OAuth token: Authorization), which is never sent to another host
// on a redirect, and any copy of it in a response body is masked before it
// reaches an error.
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
	bearer  bool // token is an OAuth token: Authorization: Bearer
	hc      *http.Client
	sleep   func(ctx context.Context, d time.Duration) error // injectable for tests
}

// New returns a client for the GitLab instance at baseURL (for example
// https://gitlab.example.com). A nil hc gets a client with a 30 s timeout.
// The client uses a copy of hc that refuses redirects to another host: Go
// forwards the PRIVATE-TOKEN header on every redirect it follows.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	own := *hc
	own.CheckRedirect = sameHostRedirects(hc.CheckRedirect)
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      &own,
		sleep:   sleepCtx,
	}
}

// NewOAuth is New for a user's OAuth access token, which GitLab takes in
// the Authorization header as a Bearer token rather than in PRIVATE-TOKEN.
// The same rules hold: no redirect to another host, the token masked in
// errors.
func NewOAuth(baseURL, token string, hc *http.Client) *Client {
	c := New(baseURL, token, hc)
	c.bearer = true
	return c
}

// errCrossHostRedirect is a redirect the client refused to follow.
var errCrossHostRedirect = errors.New("gitlab: refused a redirect to another host or scheme")

// sameHostRedirects wraps next (nil means Go's default of 10 redirects) so a
// redirect is followed only to the same scheme and host, port included.
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
			return errors.New("gitlab: stopped after 10 redirects")
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
	return c.doMax(ctx, method, path, query, body, maxResponse)
}

// doMax is do with a reply limit: a reply of more than limit bytes is
// ErrTooLarge, and no more than limit+1 bytes of it are read.
func (c *Client) doMax(ctx context.Context, method, path string, query url.Values, body any, limit int) (*response, error) {
	return c.doTry(ctx, method, path, query, body, limit, true)
}

// doOnce is do for a request that must not be sent twice (a POST that
// creates something): a 429, a 5xx or a connection error is returned, not
// retried, since the first attempt may have been applied.
func (c *Client) doOnce(ctx context.Context, method, path string, query url.Values, body any) (*response, error) {
	return c.doTry(ctx, method, path, query, body, maxResponse, false)
}

func (c *Client) doTry(ctx context.Context, method, path string, query url.Values, body any, limit int, retry bool) (*response, error) {
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
		resp, retryAfter, err := c.once(ctx, method, u.String(), payload, limit)
		if err == nil {
			return resp, nil
		}
		if !retry || ctx.Err() != nil || attempt >= maxRetries || !retryable(err) {
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

func (c *Client) once(ctx context.Context, method, rawURL string, payload []byte, limit int) (*response, time.Duration, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, 0, errors.New("gitlab: building request")
	}
	if c.bearer {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.Header.Set("PRIVATE-TOKEN", c.token)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if errors.Is(err, errCrossHostRedirect) {
		// Not a connection failure: retrying would only be refused again.
		return nil, 0, errCrossHostRedirect
	}
	if err != nil {
		var ue *url.Error
		reason := err.Error()
		if errors.As(err, &ue) {
			reason = ue.Err.Error()
		}
		return nil, 0, &connError{msg: fmt.Sprintf("gitlab: %s request failed: %s", method, c.mask(reason))}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	if err != nil {
		return nil, 0, &connError{msg: fmt.Sprintf("gitlab: reading response: %s", c.mask(err.Error()))}
	}
	if len(data) > limit {
		return nil, 0, ErrTooLarge
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return &response{status: res.StatusCode, header: res.Header, body: data}, 0, nil
	}
	// Mask before cutting: a cut through the token would leave part of it.
	stored := c.mask(string(data))
	if len(stored) > storedBody {
		stored = truncateUTF8(stored, storedBody)
	}
	ae := &APIError{Status: res.StatusCode, Body: stored}
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
	return c.paginateTimed(ctx, path, query, 0, fn)
}

// paginateTimed is paginate with each page's request (retries included)
// bounded by pageTimeout; 0 means only ctx bounds it.
func (c *Client) paginateTimed(ctx context.Context, path string, query url.Values, pageTimeout time.Duration, fn func(body []byte) (bool, error)) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", "100")
	for {
		pctx, cancel := ctx, context.CancelFunc(func() {})
		if pageTimeout > 0 {
			pctx, cancel = context.WithTimeout(ctx, pageTimeout)
		}
		resp, err := c.do(pctx, http.MethodGet, path, q, nil)
		cancel()
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

// HasMemberAccess reports whether the token's user has at least minLevel
// on project id however GitLab grants it: as a member of the project or
// its group, or through a group the project or its group is shared with.
// GET /projects/:path's permissions leave shares out; the project listing
// (GET /projects?min_access_level=, narrowed to the one ID) counts them.
func (c *Client) HasMemberAccess(ctx context.Context, id int64, minLevel int) (bool, error) {
	q := url.Values{
		"min_access_level": {strconv.Itoa(minLevel)}, "simple": {"true"},
		"id_after": {strconv.FormatInt(id-1, 10)}, "id_before": {strconv.FormatInt(id+1, 10)},
	}
	var page []struct {
		ID int64 `json:"id"`
	}
	if _, err := c.getJSON(ctx, "/projects", q, &page); err != nil {
		return false, err
	}
	for _, p := range page {
		if p.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// ApprovalRules lists the project's approval rules.
func (c *Client) ApprovalRules(ctx context.Context, projectID int64) ([]ApprovalRule, error) {
	var all []ApprovalRule
	err := c.paginate(ctx, projPath(projectID)+"/approval_rules", nil, func(body []byte) (bool, error) {
		var page []ApprovalRule
		if err := decode(body, &page); err != nil {
			return false, err
		}
		all = append(all, page...)
		return false, nil
	})
	return all, err
}

// ApprovalsBeforeMerge is the project's own required approval count (the
// setting from before approval rules).
func (c *Client) ApprovalsBeforeMerge(ctx context.Context, projectID int64) (int, error) {
	var a struct {
		ApprovalsBeforeMerge int `json:"approvals_before_merge"`
	}
	_, err := c.getJSON(ctx, projPath(projectID)+"/approvals", nil, &a)
	return a.ApprovalsBeforeMerge, err
}

// ProtectedBranches lists the project's protected branch rules, wildcard
// rules ("release/*") included.
func (c *Client) ProtectedBranches(ctx context.Context, projectID int64) ([]ProtectedBranch, error) {
	var all []ProtectedBranch
	err := c.paginate(ctx, projPath(projectID)+"/protected_branches", nil, func(body []byte) (bool, error) {
		var page []ProtectedBranch
		if err := decode(body, &page); err != nil {
			return false, err
		}
		all = append(all, page...)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// GetMergeRequest returns one merge request. Draft is true when GitLab reports
// either draft or work_in_progress.
func (c *Client) GetMergeRequest(ctx context.Context, projectID, iid int64) (MergeRequest, error) {
	var mr MergeRequest
	_, err := c.getJSON(ctx, mrPath(projectID, iid), nil, &mr)
	mr.Draft = mr.Draft || mr.WorkInProgress
	return mr, err
}

// MergeRequestDiffs returns the merge request's files at its head (GET
// .../merge_requests/:iid/diffs, GitLab 15.7 and later). A file GitLab
// collapsed or found too large stays in the list with an empty Diff.
//
// GitLab stops listing files past its diff limits without saying so, so the
// list is checked against the merge request's changes_count: a count that
// ends in "+" (GitLab stopped counting) or differs from the number of files
// is ErrPartialDiff. An older GitLab answers 404 for /diffs, and the
// deprecated .../changes, which is not paginated, is read instead; its
// overflow flag is ErrPartialDiff. A merge request that doesn't exist is
// ErrNotFound.
func (c *Client) MergeRequestDiffs(ctx context.Context, projectID, iid int64) ([]Diff, error) {
	var out []Diff
	err := c.paginate(ctx, mrPath(projectID, iid)+"/diffs", nil, func(body []byte) (bool, error) {
		var page []Diff
		if err := decode(body, &page); err != nil {
			return false, err
		}
		out = append(out, page...)
		return false, nil
	})
	if isStatus(err, http.StatusNotFound) {
		var ch struct {
			Changes  []Diff `json:"changes"`
			Overflow bool   `json:"overflow"`
		}
		if _, err := c.getJSON(ctx, mrPath(projectID, iid)+"/changes", nil, &ch); err != nil {
			return nil, err
		}
		if ch.Overflow {
			return nil, fmt.Errorf("%w: GitLab cut the merge request's changes short (overflow)", ErrPartialDiff)
		}
		return ch.Changes, nil
	}
	if err != nil {
		return out, err
	}
	mr, err := c.GetMergeRequest(ctx, projectID, iid)
	if err != nil {
		return nil, err
	}
	count := string(mr.ChangesCount)
	if strings.HasSuffix(count, "+") {
		return nil, fmt.Errorf("%w: the merge request changes %s files, more than GitLab lists", ErrPartialDiff, count)
	}
	// An empty count is GitLab not having counted yet: only an empty list
	// matches it.
	n := 0
	if count == "" {
		if len(out) != 0 {
			return nil, fmt.Errorf("%w: the merge request's changes_count isn't available yet", ErrPartialDiff)
		}
	} else if n, err = strconv.Atoi(count); err != nil {
		return nil, errors.New("gitlab: decoding response: bad changes_count")
	}
	if n != len(out) {
		return nil, fmt.Errorf("%w: GitLab lists %d files but the merge request changes %d", ErrPartialDiff, len(out), n)
	}
	return out, nil
}

// Compare returns the files that differ between two commits, straight from
// one to the other (GET .../repository/compare?straight=true). A comparison
// that timed out or hit a size limit (compare_timeout, a collapsed or too
// large file) may not show every change: ErrPartialDiff.
func (c *Client) Compare(ctx context.Context, projectID int64, from, to string) ([]Diff, error) {
	var out struct {
		Diffs          *[]Diff `json:"diffs"`
		CompareTimeout bool    `json:"compare_timeout"`
	}
	q := url.Values{"from": {from}, "to": {to}, "straight": {"true"}}
	if _, err := c.getJSON(ctx, projPath(projectID)+"/repository/compare", q, &out); err != nil {
		return nil, err
	}
	if out.CompareTimeout {
		return nil, fmt.Errorf("%w: the comparison timed out", ErrPartialDiff)
	}
	// No list at all is not an empty one: nothing says no file changed.
	if out.Diffs == nil {
		return nil, fmt.Errorf("%w: the comparison has no diff list", ErrPartialDiff)
	}
	for _, d := range *out.Diffs {
		if d.Collapsed || d.TooLarge {
			return nil, fmt.Errorf("%w: the comparison leaves out the diff of a file", ErrPartialDiff)
		}
	}
	return *out.Diffs, nil
}

// MergeBase returns the ID of the best common ancestor of two commits
// (GET .../repository/merge_base). Commits with no common ancestor are
// ErrNotFound.
func (c *Client) MergeBase(ctx context.Context, projectID int64, a, b string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	q := url.Values{"refs[]": {a, b}}
	if _, err := c.getJSON(ctx, projPath(projectID)+"/repository/merge_base", q, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Discussions lists the merge request's discussions, every note of each.
func (c *Client) Discussions(ctx context.Context, projectID, iid int64) ([]Discussion, error) {
	var out []Discussion
	err := c.paginate(ctx, mrPath(projectID, iid)+"/discussions", nil, func(body []byte) (bool, error) {
		var page []Discussion
		if err := decode(body, &page); err != nil {
			return false, err
		}
		out = append(out, page...)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateDiffDiscussion starts a discussion with body at pos and returns its
// ID and its first note's ID. The position goes in the JSON body as a nested
// "position" object, as GitLab's form fields position[...] would carry it.
func (c *Client) CreateDiffDiscussion(ctx context.Context, projectID, iid int64, body string, pos Position) (string, int64, error) {
	position := map[string]any{
		"position_type": "text",
		"base_sha":      pos.BaseSHA,
		"start_sha":     pos.StartSHA,
		"head_sha":      pos.HeadSHA,
		"old_path":      pos.OldPath,
		"new_path":      pos.NewPath,
		"new_line":      pos.NewLine,
	}
	if pos.OldLine > 0 {
		position["old_line"] = pos.OldLine
	}
	// Sent once: a retried POST that GitLab had already applied would start a
	// second thread.
	resp, err := c.doOnce(ctx, http.MethodPost, mrPath(projectID, iid)+"/discussions", nil,
		map[string]any{"body": body, "position": position})
	if err != nil {
		return "", 0, err
	}
	var d Discussion
	if err := decode(resp.body, &d); err != nil {
		return "", 0, err
	}
	if d.ID == "" {
		return "", 0, errors.New("gitlab: discussion has no id")
	}
	var noteID int64
	if len(d.Notes) > 0 {
		noteID = d.Notes[0].ID
	}
	return d.ID, noteID, nil
}

// ReplyDiscussion adds a note to a discussion. A missing discussion is
// ErrNotFound.
func (c *Client) ReplyDiscussion(ctx context.Context, projectID, iid int64, discussionID, body string) error {
	_, err := c.do(ctx, http.MethodPost, mrPath(projectID, iid)+"/discussions/"+seg(discussionID)+"/notes", nil,
		map[string]string{"body": body})
	return err
}

// ResolveDiscussion resolves a discussion, or reopens it when resolved is
// false. A missing discussion is ErrNotFound.
func (c *Client) ResolveDiscussion(ctx context.Context, projectID, iid int64, discussionID string, resolved bool) error {
	_, err := c.do(ctx, http.MethodPut, mrPath(projectID, iid)+"/discussions/"+seg(discussionID),
		url.Values{"resolved": {strconv.FormatBool(resolved)}}, nil)
	return err
}

// CommitSHA returns the commit a branch, tag or commit ref points at (GET
// /projects/:id/repository/commits/:ref). A ref that doesn't exist is
// ErrNotFound.
func (c *Client) CommitSHA(ctx context.Context, projectID int64, ref string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if _, err := c.getJSON(ctx, projPath(projectID)+"/repository/commits/"+seg(ref), nil, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("gitlab: commit has no id")
	}
	return out.ID, nil
}

// GetFile returns a file's raw content at ref. A missing file is nil, false, nil.
func (c *Client) GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error) {
	return c.GetFileMax(ctx, projectID, path, ref, maxResponse)
}

// GetFileMax is GetFile for a file that may not be trusted to be small: one
// of more than max bytes is nil, false, ErrTooLarge, and no more than max+1
// bytes of it are read.
func (c *Client) GetFileMax(ctx context.Context, projectID int64, path, ref string, max int) ([]byte, bool, error) {
	resp, err := c.doMax(ctx, http.MethodGet, projPath(projectID)+"/repository/files/"+seg(path)+"/raw", url.Values{"ref": {ref}}, nil, max)
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

// Note is a comment on a merge request.
type Note struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	System bool   `json:"system"`
	Author User   `json:"author"`
}

// GetNote returns one comment on a merge request.
func (c *Client) GetNote(ctx context.Context, projectID, iid, noteID int64) (Note, error) {
	var n Note
	_, err := c.getJSON(ctx, mrPath(projectID, iid)+"/notes/"+strconv.FormatInt(noteID, 10), nil, &n)
	return n, err
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

// AwardNoteEmoji adds the emoji name (eyes, thumbsup, ...) to a note as this
// user. A missing note is ErrNotFound. An emoji the user already gave is
// success: GitLab answers 404 "has already been taken", which is not a
// deleted note.
func (c *Client) AwardNoteEmoji(ctx context.Context, projectID, iid, noteID int64, name string) error {
	_, err := c.do(ctx, http.MethodPost, mrPath(projectID, iid)+"/notes/"+strconv.FormatInt(noteID, 10)+"/award_emoji",
		nil, map[string]string{"name": name})
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && strings.Contains(ae.Body, "already been taken") {
		return nil
	}
	return err
}

// MemberAccessLevel returns a user's access level in a project, inherited
// from its groups included (GitLab's 10 Guest, 20 Reporter, 30 Developer,
// 40 Maintainer, 50 Owner). Only an active membership counts: a user who
// isn't a member, isn't active (blocked, deactivated), is awaiting admission
// (membership_state) or whose membership has expired is 0, not an error. A
// membership state or expiry that can't be read is 0 too.
func (c *Client) MemberAccessLevel(ctx context.Context, projectID, userID int64) (int, error) {
	var m struct {
		AccessLevel     int    `json:"access_level"`
		State           string `json:"state"`
		MembershipState string `json:"membership_state"`
		ExpiresAt       string `json:"expires_at"`
	}
	_, err := c.getJSON(ctx, projPath(projectID)+"/members/all/"+strconv.FormatInt(userID, 10), nil, &m)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if m.State != "active" || (m.MembershipState != "" && m.MembershipState != "active") {
		return 0, nil
	}
	if m.ExpiresAt != "" {
		// A date (the membership ends when that day starts, UTC) or, to be
		// safe, a timestamp.
		exp, err := time.Parse("2006-01-02", m.ExpiresAt)
		if err != nil {
			if exp, err = time.Parse(time.RFC3339, m.ExpiresAt); err != nil {
				return 0, nil
			}
		}
		if !exp.After(time.Now()) {
			return 0, nil
		}
	}
	return m.AccessLevel, nil
}

// UserIsBot reports whether GitLab marks the user as a bot (its `bot`
// attribute: project and group access token users, service accounts and the
// like). A user GitLab doesn't know is ErrNotFound.
func (c *Client) UserIsBot(ctx context.Context, userID int64) (bool, error) {
	var u struct {
		Bot bool `json:"bot"`
	}
	if _, err := c.getJSON(ctx, "/users/"+strconv.FormatInt(userID, 10), nil, &u); err != nil {
		return false, err
	}
	return u.Bot, nil
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

// ApprovedBy returns the ids of the users who approve the merge request now.
func (c *Client) ApprovedBy(ctx context.Context, projectID, iid int64) ([]int64, error) {
	var a struct {
		ApprovedBy []struct {
			User User `json:"user"`
		} `json:"approved_by"`
	}
	if _, err := c.getJSON(ctx, mrPath(projectID, iid)+"/approvals", nil, &a); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(a.ApprovedBy))
	for _, u := range a.ApprovedBy {
		ids = append(ids, u.User.ID)
	}
	return ids, nil
}

// Unapprove withdraws this user's approval. Not approved (404) is not an error.
func (c *Client) Unapprove(ctx context.Context, projectID, iid int64) error {
	_, err := c.do(ctx, http.MethodPost, mrPath(projectID, iid)+"/unapprove", nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// EnsureHook makes sure the project has a webhook for url with the given
// secret token, for merge request and note (comment) events: it edits the hook
// with the same URL, which also switches note events on for a hook made
// before commands existed, or creates one.
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
		"note_events":             true,
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

// ProjectListOptions selects the projects ProjectPaths lists.
type ProjectListOptions struct {
	// MinAccessLevel is the least member access level (Reporter is 20,
	// Maintainer 40).
	MinAccessLevel int
	// Search, when not "", keeps the projects whose path, name or
	// namespace holds it (GitLab's search with search_namespaces).
	Search string
	// Limit is the most paths returned.
	Limit int
	// CallTimeout bounds each page's request; 0 means only ctx bounds it.
	CallTimeout time.Duration
}

// ErrSearchIncomplete is ListsProject's error when its page budget ended
// before the search did: it can't tell.
var ErrSearchIncomplete = errors.New("gitlab: the project search ran out of pages before it found an answer")

// ProjectLookup bounds ListsProject.
type ProjectLookup struct {
	// MinAccessLevel is the least member access level, as in
	// ProjectListOptions.
	MinAccessLevel int
	// MaxPages is the most pages (100 projects each) read.
	MaxPages int
	// CallTimeout bounds each page's request; 0 means only ctx bounds it.
	CallTimeout time.Duration
}

// ListsProject finds the project at path in the listing ProjectPaths reads
// with the same MinAccessLevel, and returns the path as GitLab spells it: GitLab
// paths are unique whatever the case, so the match ignores it. It asks the way
// the picker does, GET /projects?min_access_level=, searching the project's
// path with search_namespaces and paging until the path turns up.
// GET /projects/:path would not do: its permissions leave out access gained
// through an ancestor group or a share. found is false when the listing has
// no such project. When o.MaxPages end with more pages to read, the answer is
// ErrSearchIncomplete, not "not found".
func (c *Client) ListsProject(ctx context.Context, path string, o ProjectLookup) (listed string, found bool, err error) {
	if path == "" || strings.HasSuffix(path, "/") {
		return "", false, nil
	}
	// The whole path is searched, not the name: GitLab matches a search under
	// 3 characters exactly (a project called ab would be compared with
	// acme/ab and missed), and a path is always longer and matches far fewer
	// projects than a common name would.
	q := url.Values{"min_access_level": {strconv.Itoa(o.MinAccessLevel)}, "simple": {"true"},
		"search": {path}, "search_namespaces": {"true"}}
	q.Set("per_page", "100")
	for pages := 1; ; pages++ {
		pctx, cancel := ctx, context.CancelFunc(func() {})
		if o.CallTimeout > 0 {
			pctx, cancel = context.WithTimeout(ctx, o.CallTimeout)
		}
		resp, err := c.do(pctx, http.MethodGet, "/projects", q, nil)
		cancel()
		if err != nil {
			return "", false, err
		}
		var page []struct {
			Path string `json:"path_with_namespace"`
		}
		if err := decode(resp.body, &page); err != nil {
			return "", false, err
		}
		for _, p := range page {
			if strings.EqualFold(p.Path, path) {
				return p.Path, true, nil
			}
		}
		next := strings.TrimSpace(resp.header.Get("X-Next-Page"))
		if next == "" {
			return "", false, nil
		}
		if pages >= o.MaxPages {
			return "", false, ErrSearchIncomplete
		}
		q.Set("page", next)
	}
}

// ProjectPageOptions selects the one page ProjectPage reads.
type ProjectPageOptions struct {
	// MinAccessLevel is the least member access level, as in
	// ProjectListOptions.
	MinAccessLevel int
	// Search, when not "", is searched as in ProjectListOptions.
	Search string
	// Page is the page to read (1-based) and PerPage its size (GitLab
	// allows 100 at most).
	Page, PerPage int
	// CallTimeout bounds the request; 0 means only ctx bounds it.
	CallTimeout time.Duration
}

// ProjectPage returns the paths on one page of the projects the token's user
// is a member of with at least o.MinAccessLevel, ordered by ID (unique, so
// pages don't move between requests and no project falls between two):
// GET /projects?order_by=id&sort=asc&page=&per_page=.
// hasMore is true when GitLab names a later page (X-Next-Page).
func (c *Client) ProjectPage(ctx context.Context, o ProjectPageOptions) (paths []string, hasMore bool, err error) {
	q := url.Values{"min_access_level": {strconv.Itoa(o.MinAccessLevel)}, "simple": {"true"},
		"order_by": {"id"}, "sort": {"asc"},
		"page": {strconv.Itoa(o.Page)}, "per_page": {strconv.Itoa(o.PerPage)}}
	if o.Search != "" {
		q.Set("search", o.Search)
		q.Set("search_namespaces", "true")
	}
	pctx, cancel := ctx, context.CancelFunc(func() {})
	if o.CallTimeout > 0 {
		pctx, cancel = context.WithTimeout(ctx, o.CallTimeout)
	}
	defer cancel()
	resp, err := c.do(pctx, http.MethodGet, "/projects", q, nil)
	if err != nil {
		return nil, false, err
	}
	var page []struct {
		Path string `json:"path_with_namespace"`
	}
	if err := decode(resp.body, &page); err != nil {
		return nil, false, err
	}
	for _, p := range page {
		if p.Path != "" {
			paths = append(paths, p.Path)
		}
	}
	return paths, strings.TrimSpace(resp.header.Get("X-Next-Page")) != "", nil
}

// ProjectPaths returns the paths of the projects the token's user is a
// member of with at least o.MinAccessLevel, directly or through a group:
// GET /projects?min_access_level=&simple=true. It stops after o.Limit
// paths; truncated is true when there were more. A public project the user
// isn't a member of is not listed. On an error it also returns the paths
// read before it.
func (c *Client) ProjectPaths(ctx context.Context, o ProjectListOptions) (paths []string, truncated bool, err error) {
	q := url.Values{"min_access_level": {strconv.Itoa(o.MinAccessLevel)}, "simple": {"true"}}
	if o.Search != "" {
		q.Set("search", o.Search)
		q.Set("search_namespaces", "true")
	}
	err = c.paginateTimed(ctx, "/projects", q, o.CallTimeout, func(body []byte) (bool, error) {
		var page []struct {
			Path string `json:"path_with_namespace"`
		}
		if err := decode(body, &page); err != nil {
			return true, err
		}
		for _, p := range page {
			if p.Path == "" {
				continue
			}
			if len(paths) == o.Limit {
				truncated = true
				return true, nil
			}
			paths = append(paths, p.Path)
		}
		return false, nil
	})
	return paths, truncated, err
}
