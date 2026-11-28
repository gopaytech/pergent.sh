package github

import (
	"context"
	"errors"
	"io"
	"net/url"
)

// Tarball opens the .tar.gz of owner/repo at commit sha. GitHub answers with
// a redirect to its download host, which gets no token. The bytes are given
// as that host sends them, and only ctx bounds the download. The caller
// closes it.
func (c *Client) Tarball(ctx context.Context, owner, repo, sha string) (io.ReadCloser, error) {
	if sha == "" {
		return nil, errors.New("github: tarball of no commit")
	}
	p, err := repoPath(owner, repo, "/tarball/", url.PathEscape(sha))
	if err != nil {
		return nil, err
	}
	return c.open(ctx, p, true)
}
