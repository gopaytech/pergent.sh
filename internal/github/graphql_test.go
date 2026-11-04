package github

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGraphQLURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.github.com":             "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3":     "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/api/v3/":    "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/prefix/api": "https://ghe.example.com/prefix/api/graphql",
	} {
		if got := graphqlURL(normalizeBase(base)); got != want {
			t.Errorf("graphqlURL(%q) = %q, want %q", base, got, want)
		}
	}
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func decodeGQL(t *testing.T, body string) gqlRequest {
	t.Helper()
	var r gqlRequest
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return r
}

func threadsPage(next string, nodes ...string) string {
	page := `{"hasNextPage":false,"endCursor":null}`
	if next != "" {
		page = `{"hasNextPage":true,"endCursor":"` + next + `"}`
	}
	return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":` + page + `,"nodes":[` + strings.Join(nodes, ",") + `]}}}}}`
}

func TestReviewThreadsPaginates(t *testing.T) {
	var calls atomic.Int32
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, threadsPage("c1", `{"id":"T1","isResolved":false,"isOutdated":false,"path":"a.go","line":12,"startLine":10,
				"comments":{"nodes":[{"fullDatabaseId":"501","databaseId":501,"body":"first\n<!-- pergent:finding f1 -->","author":{"__typename":"Bot","login":"pergent-review[bot]"}}]}}`))
			return
		}
		_, _ = io.WriteString(w, threadsPage("", `{"id":"T2","isResolved":true,"isOutdated":true,"path":"b.go","line":null,"startLine":null,
			"comments":{"nodes":[{"fullDatabaseId":"502","databaseId":502,"body":"second","author":null}]}}`, `{"id":"T3","isResolved":false,"isOutdated":false,"path":"c.go","line":3,"startLine":null,"comments":{"nodes":[]}}`))
	})
	got, err := c.ReviewThreads(ctx, "acme", "widgets", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewThread{
		{ID: "T1", Path: "a.go", Line: 12, StartLine: 10, CommentID: 501, Body: "first\n<!-- pergent:finding f1 -->", Author: "pergent-review[bot]", AuthorType: "Bot"},
		{ID: "T2", Resolved: true, Outdated: true, Path: "b.go", CommentID: 502, Body: "second"},
		{ID: "T3", Path: "c.go", Line: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReviewThreads = %+v", got)
	}
	all := rec.all()
	if len(all) != 2 {
		t.Fatalf("requests = %d", len(all))
	}
	expect(t, all[0], "POST", "/graphql", "")
	if all[0].Header.Get("Authorization") != "Bearer "+testToken {
		t.Error("the query was sent without the token")
	}
	r0, r1 := decodeGQL(t, all[0].Body), decodeGQL(t, all[1].Body)
	for _, f := range []string{"reviewThreads(first: 100, after: $cursor)", "pageInfo", "hasNextPage", "endCursor", "isResolved", "isOutdated", "path", "startLine", "comments(first: 1)", "fullDatabaseId", "databaseId", "author { __typename login }"} {
		if !strings.Contains(r0.Query, f) {
			t.Errorf("query lacks %q:\n%s", f, r0.Query)
		}
	}
	if r0.Variables["owner"] != "acme" || r0.Variables["name"] != "widgets" || r0.Variables["number"] != float64(3) {
		t.Errorf("variables = %v", r0.Variables)
	}
	if r0.Variables["cursor"] != nil || r1.Variables["cursor"] != "c1" {
		t.Errorf("cursors = %v, %v", r0.Variables["cursor"], r1.Variables["cursor"])
	}
}

// GitHub deprecated databaseId (a 32-bit Int) for fullDatabaseId (a BigInt
// sent as a string): the latter wins when it is valid.
func TestReviewThreadsCommentIDs(t *testing.T) {
	node := func(comment string) string {
		return `{"id":"T","isResolved":false,"isOutdated":false,"path":"a.go","line":1,"startLine":null,"comments":{"nodes":[{` + comment + `,"body":"x","author":null}]}}`
	}
	for name, tc := range map[string]struct {
		comment string
		want    int64
	}{
		"full id above 2^32":         {`"fullDatabaseId":"4294967301","databaseId":null`, 4294967301},
		"full id null, legacy set":   {`"fullDatabaseId":null,"databaseId":501`, 501},
		"full id absent, legacy set": {`"databaseId":502`, 502},
		"both set":                   {`"fullDatabaseId":"9000000001","databaseId":5`, 9000000001},
		"full id invalid":            {`"fullDatabaseId":"abc","databaseId":503`, 503},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, threadsPage("", node(tc.comment))) })
			got, err := c.ReviewThreads(ctx, "acme", "widgets", 3)
			if err != nil || len(got) != 1 {
				t.Fatalf("got=%+v err=%v", got, err)
			}
			if got[0].CommentID != tc.want {
				t.Errorf("CommentID = %d, want %d", got[0].CommentID, tc.want)
			}
		})
	}
}

func TestReviewThreadsStopsAt30Pages(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, threadsPage("more"))
	})
	if _, err := c.ReviewThreads(ctx, "acme", "widgets", 3); err == nil || !strings.Contains(err.Error(), "30 pages") {
		t.Fatalf("err = %v", err)
	}
	if n := len(rec.all()); n != 30 {
		t.Errorf("requests = %d, want 30", n)
	}
}

func TestReviewThreadsMissingPullRequest(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
	})
	if _, err := c.ReviewThreads(ctx, "acme", "widgets", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGraphQLErrorsAreAnError(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"Could not resolve to a PullRequest"},{"message":"second"}]}`)
	})
	_, err := c.ReviewThreads(ctx, "acme", "widgets", 3)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve to a PullRequest") {
		t.Fatalf("err = %v", err)
	}
}

func TestGraphQLHTTPErrorIsAPIError(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"nope"}`, 403) })
	if err := c.ResolveReviewThread(ctx, "T1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveAndUnresolveReviewThread(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":true}}}}`)
	})
	if err := c.ResolveReviewThread(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	if err := c.UnresolveReviewThread(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	for i, mutation := range []string{"resolveReviewThread(input: {threadId: $id})", "unresolveReviewThread(input: {threadId: $id})"} {
		got := rec.get(i)
		expect(t, got, "POST", "/graphql", "")
		req := decodeGQL(t, got.Body)
		if !strings.HasPrefix(req.Query, "mutation") || !strings.Contains(req.Query, mutation) {
			t.Errorf("query %d = %s", i, req.Query)
		}
		if !reflect.DeepEqual(req.Variables, map[string]any{"id": "T1"}) {
			t.Errorf("variables %d = %v", i, req.Variables)
		}
	}
}

func TestGraphQLOnEnterpriseURL(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{}}`)
	})
	c.base += "/api/v3"
	if err := c.ResolveReviewThread(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	expect(t, rec.get(0), "POST", "/api/graphql", "")
}
