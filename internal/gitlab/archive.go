package gitlab

import (
	"context"
	"io"
	"net/url"
)

// Archive opens the .tar.gz of the project's files at commit sha, as GitLab
// sends it. Only ctx bounds the download. The caller closes it.
func (c *Client) Archive(ctx context.Context, projectID int64, sha string) (io.ReadCloser, error) {
	return c.open(ctx, projPath(projectID)+"/repository/archive.tar.gz", url.Values{"sha": {sha}}, true)
}
