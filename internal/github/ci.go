package github

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// CheckRun is one check run on a commit. Title, Summary and Text are the
// run's output; an Actions run has none worth reading.
type CheckRun struct {
	ID                 int64
	Name               string
	Status, Conclusion string
	AppID              int64
	AppSlug            string
	Title, Summary     string
	Text               string
}

// CheckSuite is one check suite on a commit. RunCount is its number of
// latest check runs.
type CheckSuite struct {
	ID       int64
	Status   string
	RunCount int
	AppID    int64
}

const (
	// maxArtifactZip is the biggest artifact zip that is read into memory.
	maxArtifactZip = 20 << 20
	// maxArtifactFile is the most that is read from the one file.
	maxArtifactFile = 2 << 20
)

type checkRunJSON struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
	} `json:"output"`
}

func (j checkRunJSON) run() CheckRun {
	return CheckRun{ID: j.ID, Name: j.Name, Status: j.Status, Conclusion: j.Conclusion,
		AppID: j.App.ID, AppSlug: j.App.Slug, Title: j.Output.Title, Summary: j.Output.Summary, Text: j.Output.Text}
}

func checkRunsPath(owner, repo, sha string, q url.Values) (string, error) {
	s, err := segment(sha)
	if err != nil {
		return "", err
	}
	p, err := repoPath(owner, repo, "/commits/", s, "/check-runs")
	if err != nil {
		return "", err
	}
	return p + "?" + q.Encode(), nil
}

// CheckRuns returns the latest check runs on sha named exactly name. The name
// is query-encoded for GitHub, and matched again here: GitHub's check_name is
// exact, but a reader that kept a near match could pass a job it never saw.
func (c *Client) CheckRuns(ctx context.Context, owner, repo, sha, name string) ([]CheckRun, error) {
	p, err := checkRunsPath(owner, repo, sha, url.Values{"check_name": {name}, "filter": {"latest"}})
	if err != nil {
		return nil, err
	}
	var out []CheckRun
	err = c.paginate(ctx, p, func(body []byte) error {
		var page struct {
			CheckRuns []checkRunJSON `json:"check_runs"`
		}
		if err := decode(body, &page); err != nil {
			return err
		}
		for _, r := range page.CheckRuns {
			if r.Name == name {
				out = append(out, r.run())
			}
		}
		return nil
	})
	return out, err
}

// CheckSuites returns the check suites on sha.
func (c *Client) CheckSuites(ctx context.Context, owner, repo, sha string) ([]CheckSuite, error) {
	s, err := segment(sha)
	if err != nil {
		return nil, err
	}
	p, err := repoPath(owner, repo, "/commits/", s, "/check-suites")
	if err != nil {
		return nil, err
	}
	var out []CheckSuite
	err = c.paginate(ctx, p, func(body []byte) error {
		var page struct {
			Suites []struct {
				ID       int64  `json:"id"`
				Status   string `json:"status"`
				RunCount int    `json:"latest_check_runs_count"`
				App      struct {
					ID int64 `json:"id"`
				} `json:"app"`
			} `json:"check_suites"`
		}
		if err := decode(body, &page); err != nil {
			return err
		}
		for _, s := range page.Suites {
			out = append(out, CheckSuite{ID: s.ID, Status: s.Status, RunCount: s.RunCount, AppID: s.App.ID})
		}
		return nil
	})
	return out, err
}

// CheckRunAnnotations returns up to max annotations of a check run, one line
// each: "path:line level: title: message".
func (c *Client) CheckRunAnnotations(ctx context.Context, owner, repo string, id int64, max int) ([]string, error) {
	p, err := repoPath(owner, repo, "/check-runs", "/"+strconv.FormatInt(id, 10), "/annotations")
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		return nil, nil
	}
	var page []struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		Level     string `json:"annotation_level"`
		Title     string `json:"title"`
		Message   string `json:"message"`
	}
	if err := c.getJSON(ctx, p, url.Values{"per_page": {strconv.Itoa(min(max, 100))}}, &page); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range page {
		if len(out) >= max {
			break
		}
		line := fmt.Sprintf("%s:%d %s:", a.Path, a.StartLine, a.Level)
		if a.Title != "" {
			line += " " + a.Title + ":"
		}
		out = append(out, line+" "+a.Message)
	}
	return out, nil
}

// ActionsJobRunID returns the workflow run an Actions job belongs to.
func (c *Client) ActionsJobRunID(ctx context.Context, owner, repo string, jobID int64) (int64, error) {
	p, err := repoPath(owner, repo, "/actions/jobs", id(jobID))
	if err != nil {
		return 0, err
	}
	var out struct {
		RunID int64 `json:"run_id"`
	}
	if err := c.getJSON(ctx, p, nil, &out); err != nil {
		return 0, err
	}
	if out.RunID == 0 {
		return 0, errors.New("github: job has no run id")
	}
	return out.RunID, nil
}

// stream GETs path and follows a redirect by hand: GitHub answers a log or an
// artifact with a 302 to a storage host, which must not get our token. A
// non-2xx final reply is *APIError. The caller closes the body.
func (c *Client) stream(ctx context.Context, path string) (io.ReadCloser, error) {
	return c.open(ctx, path, false)
}

// open is stream. With archive, only ctx bounds the request (an archive can
// take longer than the client's timeout), and the body is asked for as it is
// stored: Go's transport un-gzips a reply marked Content-Encoding: gzip.
func (c *Client) open(ctx context.Context, path string, archive bool) (io.ReadCloser, error) {
	var tok string
	if c.token != nil {
		var err error
		if tok, err = c.token(ctx); err != nil {
			return nil, err
		}
	}
	hc := *c.hc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if archive {
		hc.Timeout = 0
	}
	get := func(rawURL, auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, errors.New("github: building request")
		}
		req.Header.Set("Accept", acceptJSON)
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", userAgent)
		if archive {
			req.Header.Set("Accept-Encoding", "identity")
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		res, err := hc.Do(req)
		if err != nil {
			reason := err.Error()
			var ue *url.Error
			if errors.As(err, &ue) {
				reason = ue.Err.Error()
			}
			return nil, &connError{msg: "github: GET request failed: " + mask(tok, reason)}
		}
		return res, nil
	}
	res, err := get(c.base+path, tok)
	if err != nil {
		return nil, err
	}
	for hop := 0; res.StatusCode >= 300 && res.StatusCode < 400; hop++ {
		loc, lerr := res.Location()
		res.Body.Close()
		if lerr != nil || hop >= 3 {
			return nil, errors.New("github: bad redirect")
		}
		// The next host gets no credentials.
		if res, err = get(loc.String(), ""); err != nil {
			return nil, err
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, storedBody))
		return nil, &APIError{Status: res.StatusCode, Body: mask(tok, string(data)),
			RateLimited: res.StatusCode == http.StatusForbidden && c.retryFor(res).ok}
	}
	return res.Body, nil
}

// JobLogs opens an Actions job's log, following GitHub's redirect to the
// storage host without sending the token there. The caller closes it.
func (c *Client) JobLogs(ctx context.Context, owner, repo string, jobID int64) (io.ReadCloser, error) {
	p, err := repoPath(owner, repo, "/actions/jobs", id(jobID), "/logs")
	if err != nil {
		return nil, err
	}
	return c.stream(ctx, p)
}

// RunArtifactFile reads one file out of a workflow run's artifact zip. The zip
// is read into memory up to 20 MiB (more is an error) and the file up to 2
// MiB. An artifact or file that isn't there, or a path that is a directory, is
// ErrNotFound. Of the live artifacts with the name (any name when it is
// empty), the newest, with the highest id, is read.
func (c *Client) RunArtifactFile(ctx context.Context, owner, repo string, runID int64, artifact, path string) (string, error) {
	p, err := repoPath(owner, repo, "/actions/runs", id(runID), "/artifacts")
	if err != nil {
		return "", err
	}
	q := url.Values{}
	if artifact != "" {
		q.Set("name", artifact)
	}
	var list struct {
		Artifacts []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Expired bool   `json:"expired"`
		} `json:"artifacts"`
	}
	if err := c.getJSON(ctx, p, q, &list); err != nil {
		return "", err
	}
	// A re-run's attempts each upload theirs to the same run: the newest
	// (highest id) wins.
	var artID int64
	for _, a := range list.Artifacts {
		if !a.Expired && (artifact == "" || a.Name == artifact) && a.ID > artID {
			artID = a.ID
		}
	}
	if artID == 0 {
		return "", fmt.Errorf("artifact %q: %w", artifact, ErrNotFound)
	}
	zp, err := repoPath(owner, repo, "/actions/artifacts", id(artID), "/zip")
	if err != nil {
		return "", err
	}
	rc, err := c.stream(ctx, zp)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxArtifactZip+1))
	if err != nil {
		return "", &connError{msg: "github: reading artifact: " + err.Error()}
	}
	if len(data) > maxArtifactZip {
		return "", ErrTooLarge
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", errors.New("github: artifact is not a zip")
	}
	want := strings.TrimLeft(path, "/")
	for _, f := range zr.File {
		if strings.TrimSuffix(f.Name, "/") != strings.TrimSuffix(want, "/") {
			continue
		}
		if f.FileInfo().IsDir() {
			break
		}
		fr, err := f.Open()
		if err != nil {
			return "", errors.New("github: reading the artifact file")
		}
		defer fr.Close()
		b, err := io.ReadAll(io.LimitReader(fr, maxArtifactFile))
		if err != nil {
			return "", errors.New("github: reading the artifact file")
		}
		return string(b), nil
	}
	return "", fmt.Errorf("artifact file %q: %w", path, ErrNotFound)
}

// RecentCheckRunNames returns the sorted, distinct names of the latest check
// runs on each of shas.
func (c *Client) RecentCheckRunNames(ctx context.Context, owner, repo string, shas []string) ([]string, error) {
	set := map[string]bool{}
	for _, sha := range shas {
		p, err := checkRunsPath(owner, repo, sha, url.Values{"filter": {"latest"}})
		if err != nil {
			return nil, err
		}
		err = c.paginate(ctx, p, func(body []byte) error {
			var page struct {
				CheckRuns []checkRunJSON `json:"check_runs"`
			}
			if err := decode(body, &page); err != nil {
				return err
			}
			for _, r := range page.CheckRuns {
				set[r.Name] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
