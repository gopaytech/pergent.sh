package gitlab

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Pipeline is a pipeline as listed.
type Pipeline struct {
	ID     int64  `json:"id"`
	SHA    string `json:"sha"`
	Ref    string `json:"ref"`
	Status string `json:"status"`
}

// CIJob is a job or a bridge (a trigger job) of a pipeline.
type CIJob struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Bridge is set on a trigger job, which has a status and no log.
	Bridge bool `json:"-"`
	// Downstream is a bridge's child pipeline; nil until it is created.
	Downstream *Downstream `json:"downstream_pipeline"`
}

// Downstream is the child pipeline a trigger job created.
type Downstream struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func (c *Client) pipelines(ctx context.Context, path string, query url.Values, limit int) ([]Pipeline, error) {
	var out []Pipeline
	err := c.paginate(ctx, path, query, func(body []byte) (bool, error) {
		var page []Pipeline
		if err := decode(body, &page); err != nil {
			return false, err
		}
		out = append(out, page...)
		return limit > 0 && len(out) >= limit, nil
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

// PipelinesForSHA lists the project's pipelines on one commit.
func (c *Client) PipelinesForSHA(ctx context.Context, projectID int64, sha string) ([]Pipeline, error) {
	return c.pipelines(ctx, projPath(projectID)+"/pipelines", url.Values{"sha": {sha}}, 0)
}

// MRPipelines lists the pipelines of a merge request, merged-results ones included.
func (c *Client) MRPipelines(ctx context.Context, projectID, iid int64) ([]Pipeline, error) {
	return c.pipelines(ctx, mrPath(projectID, iid)+"/pipelines", nil, 0)
}

// RecentPipelines lists the project's n newest pipelines.
func (c *Client) RecentPipelines(ctx context.Context, projectID int64, n int) ([]Pipeline, error) {
	return c.pipelines(ctx, projPath(projectID)+"/pipelines", url.Values{"order_by": {"id"}, "sort": {"desc"}}, n)
}

// CommitParents returns the parent SHAs of a commit.
func (c *Client) CommitParents(ctx context.Context, projectID int64, sha string) ([]string, error) {
	var out struct {
		ParentIDs []string `json:"parent_ids"`
	}
	if _, err := c.getJSON(ctx, projPath(projectID)+"/repository/commits/"+seg(sha), nil, &out); err != nil {
		return nil, err
	}
	return out.ParentIDs, nil
}

// PipelineJobs lists a pipeline's jobs and its bridges.
func (c *Client) PipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]CIJob, error) {
	var out []CIJob
	for _, kind := range []string{"jobs", "bridges"} {
		path := projPath(projectID) + "/pipelines/" + strconv.FormatInt(pipelineID, 10) + "/" + kind
		err := c.paginate(ctx, path, nil, func(body []byte) (bool, error) {
			var page []CIJob
			if err := decode(body, &page); err != nil {
				return false, err
			}
			for i := range page {
				page[i].Bridge = kind == "bridges"
			}
			out = append(out, page...)
			return false, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// JobTrace opens a job's log. The caller closes it.
func (c *Client) JobTrace(ctx context.Context, projectID, jobID int64) (io.ReadCloser, error) {
	return c.stream(ctx, projPath(projectID)+"/jobs/"+strconv.FormatInt(jobID, 10)+"/trace", nil)
}

// JobArtifact opens one file of a job's artifacts. The caller closes it.
func (c *Client) JobArtifact(ctx context.Context, projectID, jobID int64, path string) (io.ReadCloser, error) {
	parts := strings.Split(strings.TrimLeft(path, "/"), "/")
	for i, p := range parts {
		parts[i] = seg(p)
	}
	return c.stream(ctx, projPath(projectID)+"/jobs/"+strconv.FormatInt(jobID, 10)+"/artifacts/"+strings.Join(parts, "/"), nil)
}
