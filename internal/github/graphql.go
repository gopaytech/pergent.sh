package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// maxThreadPages bounds ReviewThreads: 30 pages of 100 threads.
const maxThreadPages = 30

// graphqlURL is the GraphQL endpoint for a REST base URL: GitHub Enterprise
// Server serves REST at <host>/api/v3 and GraphQL at <host>/api/graphql,
// GitHub.com serves them at api.github.com and api.github.com/graphql.
func graphqlURL(base string) string {
	base = strings.TrimRight(base, "/")
	if rest, ok := strings.CutSuffix(base, "/v3"); ok {
		return rest + "/graphql"
	}
	return base + "/graphql"
}

// graphql runs one query or mutation and decodes its "data" into out (nil:
// ignored). It is sent like a REST request (same token, retries and limits).
// A non-2xx reply is *APIError; a reply with a non-empty "errors" array is an
// error carrying the messages.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("github: encoding request: %w", err)
	}
	resp, err := c.send(ctx, http.MethodPost, graphqlURL(c.base), payload, acceptJSON, maxResponse)
	if err != nil {
		return err
	}
	var reply struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := decode(resp.body, &reply); err != nil {
		return err
	}
	if len(reply.Errors) > 0 {
		msgs := make([]string, len(reply.Errors))
		for i, e := range reply.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("github: graphql: %s", truncateUTF8(strings.Join(msgs, "; "), maxErrorBody))
	}
	if out == nil || len(reply.Data) == 0 {
		return nil
	}
	return decode(reply.Data, out)
}

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          startLine
          comments(first: 1) { nodes { databaseId body author { __typename login } } }
        }
      }
    }
  }
}`

// ReviewThreads lists the review threads of a pull request, each with its
// first comment (a thread GitHub shows without comments has CommentID 0 and
// no Body). More than 30 pages of 100 threads is an error. A pull request
// that isn't there is ErrNotFound.
func (c *Client) ReviewThreads(ctx context.Context, owner, repo string, number int64) ([]ReviewThread, error) {
	var (
		out    []ReviewThread
		cursor any
	)
	for page := 0; ; page++ {
		if page >= maxThreadPages {
			return nil, fmt.Errorf("github: more than %d pages of review threads", maxThreadPages)
		}
		var data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							IsOutdated bool   `json:"isOutdated"`
							Path       string `json:"path"`
							Line       *int   `json:"line"`
							StartLine  *int   `json:"startLine"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64  `json:"databaseId"`
									Body       string `json:"body"`
									Author     *struct {
										Typename string `json:"__typename"`
										Login    string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": repo, "number": number, "cursor": cursor}
		if err := c.graphql(ctx, reviewThreadsQuery, vars, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, &APIError{Status: http.StatusNotFound, Body: "pull request not found"}
		}
		threads := data.Repository.PullRequest.ReviewThreads
		for _, n := range threads.Nodes {
			t := ReviewThread{ID: n.ID, Resolved: n.IsResolved, Outdated: n.IsOutdated, Path: n.Path}
			if n.Line != nil {
				t.Line = *n.Line
			}
			if n.StartLine != nil {
				t.StartLine = *n.StartLine
			}
			if len(n.Comments.Nodes) > 0 {
				first := n.Comments.Nodes[0]
				t.CommentID, t.Body = first.DatabaseID, first.Body
				if first.Author != nil {
					t.Author, t.AuthorType = first.Author.Login, first.Author.Typename
				}
			}
			out = append(out, t)
		}
		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == "" {
			return out, nil
		}
		cursor = threads.PageInfo.EndCursor
	}
}

const (
	resolveThreadMutation = `mutation($id: ID!) {
  resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}`
	unresolveThreadMutation = `mutation($id: ID!) {
  unresolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}`
)

// ResolveReviewThread resolves the review thread with the given node ID.
func (c *Client) ResolveReviewThread(ctx context.Context, threadID string) error {
	return c.graphql(ctx, resolveThreadMutation, map[string]any{"id": threadID}, nil)
}

// UnresolveReviewThread reopens the review thread with the given node ID.
func (c *Client) UnresolveReviewThread(ctx context.Context, threadID string) error {
	return c.graphql(ctx, unresolveThreadMutation, map[string]any{"id": threadID}, nil)
}
