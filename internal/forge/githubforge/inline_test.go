package githubforge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
)

var _ forge.Inline = (*Forge)(nil)

// findingBody is an inline comment body as render.InlineComment ends it.
func findingBody(text, fp string) string { return text + "\n\n" + render.InlineMarker(fp) }

func gqlThreads(nodes ...map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			"nodes":    nodes,
		}}}}}
}

// gqlThread is a thread whose first comment is by a Bot account.
func gqlThread(id string, resolved, outdated bool, path string, line, startLine any, comment int64, body, author string) map[string]any {
	return gqlThreadBy(id, resolved, outdated, path, line, startLine, comment, body, author, "Bot")
}

func gqlThreadBy(id string, resolved, outdated bool, path string, line, startLine any, comment int64, body, author, typename string) map[string]any {
	return map[string]any{
		"id": id, "isResolved": resolved, "isOutdated": outdated, "path": path, "line": line, "startLine": startLine,
		"comments": map[string]any{"nodes": []map[string]any{{"fullDatabaseId": strconv.FormatInt(comment, 10), "databaseId": int32(comment), "body": body, "author": map[string]any{"__typename": typename, "login": author}}}},
	}
}

func TestPostThreadsOneReview(t *testing.T) {
	f := newFake(t)
	f.reply("POST", prPath+"/reviews", 200, map[string]any{"id": 9})
	f.reply("POST", "/graphql", 200, gqlThreads(
		gqlThread("T-old", false, false, "a.go", 10, nil, 400, findingBody("old", "fp-other"), reviewSlug+"[bot]"),
		gqlThread("T-1", false, false, "a.go", 10, nil, 501, findingBody("one", "fp-1"), reviewSlug+"[bot]"),
		gqlThread("T-2", false, false, "b.go", 9, 4, 502, findingBody("two", "fp-2"), reviewSlug+"[bot]"),
	))
	ch := forge.Change{Number: 3, HeadSHA: sha('a')}
	refs, errs, err := f.forge(t).PostThreads(ctx, repoID, ch, []forge.NewThread{
		{Path: "a.go", Line: 10, Body: findingBody("one", "fp-1")},
		{Path: "b.go", Line: 4, EndLine: 9, Body: findingBody("two", "fp-2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(errs, []error{nil, nil}) {
		t.Fatalf("errs = %v", errs)
	}
	if want := []forge.ThreadRef{{ID: "T-1", CommentID: 501}, {ID: "T-2", CommentID: 502}}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
	reqs := f.requests("POST", prPath+"/reviews")
	if len(reqs) != 1 {
		t.Fatalf("%d reviews posted, want 1", len(reqs))
	}
	if reqs[0].Auth != "Bearer "+installToken {
		t.Errorf("posted with %q", reqs[0].Auth)
	}
	jsonEq(t, reqs[0].Body, `{"commit_id":"`+sha('a')+`","event":"COMMENT","body":"Findings on the changed lines, from the review of `+"`aaaaaaa`"+`.","comments":[
		{"path":"a.go","body":`+quote(findingBody("one", "fp-1"))+`,"line":10,"side":"RIGHT"},
		{"path":"b.go","body":`+quote(findingBody("two", "fp-2"))+`,"line":9,"side":"RIGHT","start_line":4,"start_side":"RIGHT"}]}`)
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestPostThreads422FallsBackPerComment(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var bodies []string
	f.handle("POST", prPath+"/reviews", func(w http.ResponseWriter, r *http.Request) {
		b := f.requests("POST", prPath+"/reviews")
		mu.Lock()
		bodies = append(bodies, b[len(b)-1].Body)
		n := len(bodies)
		mu.Unlock()
		// 1: the whole review, 2: the bad thread, 3: the good one.
		if n == 1 || strings.Contains(bodies[n-1], "bad.go") {
			reply(http.StatusUnprocessableEntity, map[string]string{"message": "Line could not be resolved"})(w, r)
			return
		}
		reply(http.StatusOK, map[string]any{"id": 9})(w, r)
	})
	f.reply("POST", "/graphql", 200, gqlThreads(
		gqlThread("T-good", false, false, "good.go", 3, nil, 601, findingBody("good", "fp-good"), reviewSlug+"[bot]"),
	))
	refs, errs, err := f.forge(t).PostThreads(ctx, repoID, forge.Change{Number: 3, HeadSHA: sha('a')}, []forge.NewThread{
		{Path: "bad.go", Line: 99, Body: findingBody("bad", "fp-bad")},
		{Path: "good.go", Line: 3, Body: findingBody("good", "fp-good")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 || errs[0] == nil || errs[1] != nil {
		t.Fatalf("errs = %v; want the bad one's error only", errs)
	}
	if want := []forge.ThreadRef{{}, {ID: "T-good", CommentID: 601}}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
	reqs := f.requests("POST", prPath+"/reviews")
	if len(reqs) != 3 {
		t.Fatalf("%d review requests, want 3 (all, then one each)", len(reqs))
	}
	for _, r := range reqs[1:] {
		var got struct{ Comments []json.RawMessage }
		if err := json.Unmarshal([]byte(r.Body), &got); err != nil || len(got.Comments) != 1 {
			t.Errorf("fallback review = %s", r.Body)
		}
	}
}

// A thread that was not attempted has its error in errs, so a cancelled
// context never leaves a nil error beside a thread that was not posted.
func TestPostThreadsCancelledMidFallback(t *testing.T) {
	f := newFake(t)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var n atomic.Int32
	f.handle("POST", prPath+"/reviews", func(w http.ResponseWriter, r *http.Request) {
		// 1: the whole review, 2: the first thread on its own.
		if n.Add(1) == 2 {
			cancel()
		}
		reply(http.StatusUnprocessableEntity, map[string]string{"message": "Line could not be resolved"})(w, r)
	})
	_, errs, err := f.forge(t).PostThreads(cctx, repoID, forge.Change{Number: 3, HeadSHA: sha('a')}, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: findingBody("a", "fp-a")},
		{Path: "b.go", Line: 1, Body: findingBody("b", "fp-b")},
		{Path: "c.go", Line: 1, Body: findingBody("c", "fp-c")},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(errs) != 3 || errs[0] == nil {
		t.Fatalf("errs = %v", errs)
	}
	for i := 1; i < 3; i++ {
		if !errors.Is(errs[i], context.Canceled) {
			t.Errorf("errs[%d] = %v, want context.Canceled", i, errs[i])
		}
	}
	if got := f.requests("POST", prPath+"/reviews"); len(got) != 2 {
		t.Errorf("%d review requests, want 2 (all, then the first)", len(got))
	}
}

func TestPostThreadsOtherErrorFailsTheCall(t *testing.T) {
	f := newFake(t)
	f.reply("POST", prPath+"/reviews", 403, map[string]string{"message": "Resource not accessible by integration"})
	_, _, err := f.forge(t).PostThreads(ctx, repoID, forge.Change{Number: 3, HeadSHA: sha('a')}, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: findingBody("x", "fp-x")},
		{Path: "b.go", Line: 1, Body: findingBody("y", "fp-y")},
	})
	if err == nil {
		t.Fatal("a 403 was not an error")
	}
	if n := len(f.requests("POST", prPath+"/reviews")); n != 1 {
		t.Errorf("%d review requests after a 403, want 1", n)
	}
}

func TestPostThreadsWithoutMarkerIsNotPosted(t *testing.T) {
	f := newFake(t)
	f.reply("POST", prPath+"/reviews", 200, map[string]any{"id": 9})
	f.reply("POST", "/graphql", 200, gqlThreads(
		gqlThread("T-1", false, false, "a.go", 1, nil, 501, findingBody("one", "fp-1"), reviewSlug),
	))
	refs, errs, err := f.forge(t).PostThreads(ctx, repoID, forge.Change{Number: 3, HeadSHA: sha('a')}, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: "no marker here"},
		{Path: "a.go", Line: 1, Body: findingBody("one", "fp-1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] == nil || errs[1] != nil || refs[0] != (forge.ThreadRef{}) || refs[1].ID != "T-1" {
		t.Fatalf("refs = %+v, errs = %v", refs, errs)
	}
	reqs := f.requests("POST", prPath+"/reviews")
	if len(reqs) != 1 || strings.Contains(reqs[0].Body, "no marker here") {
		t.Fatalf("review requests = %+v", reqs)
	}
}

func TestPostThreadsNotFoundAfterPosting(t *testing.T) {
	f := newFake(t)
	f.reply("POST", prPath+"/reviews", 200, map[string]any{"id": 9})
	// Same fingerprint, but a stranger's thread: not ours.
	f.reply("POST", "/graphql", 200, gqlThreads(
		gqlThread("T-x", false, false, "a.go", 1, nil, 501, findingBody("one", "fp-1"), "mallory"),
	))
	refs, errs, err := f.forge(t).PostThreads(ctx, repoID, forge.Change{Number: 3, HeadSHA: sha('a')}, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: findingBody("one", "fp-1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if refs[0] != (forge.ThreadRef{}) || errs[0] == nil {
		t.Fatalf("refs = %+v, errs = %v; want a zero ref and an error", refs, errs)
	}
}

func TestPostThreadsNothingToPost(t *testing.T) {
	f := newFake(t)
	refs, errs, err := f.forge(t).PostThreads(ctx, repoID, forge.Change{Number: 3, HeadSHA: sha('a')}, nil)
	if err != nil || len(refs) != 0 || len(errs) != 0 {
		t.Fatalf("PostThreads(nil) = %v, %v, %v", refs, errs, err)
	}
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("%d requests for nothing", n)
	}
}

func TestThreadsOnlyBotWithMarker(t *testing.T) {
	f := newFake(t)
	f.reply("POST", "/graphql", 200, gqlThreads(
		gqlThread("T-bot", false, false, "a.go", 12, 10, 1, findingBody("mine", "fp-1"), reviewSlug+"[bot]"),
		gqlThread("T-slug", true, false, "b.go", 3, nil, 2, findingBody("mine too", "fp-2"), "Acme-Review"), // GraphQL may drop [bot]; case differs
		gqlThread("T-forged", false, false, "c.go", 5, nil, 3, findingBody("pasted", "fp-3"), "mallory"),
		gqlThread("T-nomarker", false, false, "d.go", 5, nil, 4, "a plain review comment", reviewSlug+"[bot]"),
		gqlThread("T-other-bot", false, false, "e.go", 5, nil, 5, findingBody("other", "fp-5"), approveSlug+"[bot]"),
		// A person can register an account named like the App's slug.
		gqlThreadBy("T-user", false, false, "f.go", 5, nil, 6, findingBody("impostor", "fp-6"), reviewSlug, "User"),
		gqlThreadBy("T-user-bot", false, false, "g.go", 5, nil, 7, findingBody("impostor", "fp-7"), reviewSlug+"[bot]", "User"),
		gqlThreadBy("T-no-type", false, false, "h.go", 5, nil, 8, findingBody("impostor", "fp-8"), reviewSlug+"[bot]", ""),
	))
	got, err := f.forge(t).Threads(ctx, repoID, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Thread{
		{Ref: forge.ThreadRef{ID: "T-bot", CommentID: 1}, Body: findingBody("mine", "fp-1"), Path: "a.go", Line: 10, EndLine: 12},
		{Ref: forge.ThreadRef{ID: "T-slug", CommentID: 2}, Body: findingBody("mine too", "fp-2"), Path: "b.go", Line: 3, EndLine: 3, Resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Threads = %+v\nwant %+v", got, want)
	}
	reqs := f.requests("POST", "/graphql")
	if len(reqs) != 1 || reqs[0].Auth != "Bearer "+installToken {
		t.Fatalf("graphql requests = %+v", reqs)
	}
	var req struct{ Variables map[string]any }
	_ = json.Unmarshal([]byte(reqs[0].Body), &req)
	if req.Variables["owner"] != "acme" || req.Variables["name"] != "widgets" || req.Variables["number"] != float64(3) {
		t.Errorf("variables = %v", req.Variables)
	}
}

func TestThreadsOutdatedHasNoLines(t *testing.T) {
	f := newFake(t)
	f.reply("POST", "/graphql", 200, gqlThreads(
		gqlThread("T-1", false, true, "a.go", 12, 10, 1, findingBody("mine", "fp-1"), reviewSlug+"[bot]"),
		gqlThread("T-2", false, true, "b.go", nil, nil, 2, findingBody("mine", "fp-2"), reviewSlug+"[bot]"),
	))
	got, err := f.forge(t).Threads(ctx, repoID, 3)
	if err != nil || len(got) != 2 {
		t.Fatalf("Threads = %+v, %v", got, err)
	}
	for _, th := range got {
		if th.Line != 0 || th.EndLine != 0 {
			t.Errorf("outdated thread %s has lines %d-%d", th.Ref.ID, th.Line, th.EndLine)
		}
	}
}

func TestChangeDiff(t *testing.T) {
	f := newFake(t)
	f.reply("GET", prPath+"/files", 200, []map[string]any{
		{"filename": "a.go", "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b"},
		{"filename": "gone.go", "status": "removed", "patch": "@@ -1 +0,0 @@\n-x"},
	})
	got, err := f.forge(t).ChangeDiff(ctx, repoID, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.FileDiff{{Path: "a.go", Patch: "@@ -1 +1 @@\n-a\n+b"}, {Path: "gone.go", Deleted: true, Patch: "@@ -1 +0,0 @@\n-x"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangeDiff = %+v", got)
	}
}

func TestCompareDiff(t *testing.T) {
	f := newFake(t)
	f.reply("GET", "/repos/acme/widgets/compare/"+sha('1')+"..."+sha('2'), 200, map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": sha('1')}, "files": []map[string]any{
		{"filename": "a.go", "previous_filename": "old.go", "status": "renamed", "patch": "@@ -1 +1 @@\n-a\n+b"},
	}})
	got, err := f.forge(t).CompareDiff(ctx, repoID, sha('1'), sha('2'))
	if err != nil {
		t.Fatal(err)
	}
	if want := []forge.FileDiff{{Path: "a.go", OldPath: "old.go", Patch: "@@ -1 +1 @@\n-a\n+b"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CompareDiff = %+v", got)
	}
}

func TestReplyThread(t *testing.T) {
	f := newFake(t)
	f.reply("POST", prPath+"/comments/501/replies", 201, map[string]any{"id": 8})
	if err := f.forge(t).ReplyThread(ctx, repoID, 3, forge.ThreadRef{ID: "T-1", CommentID: 501}, "done"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests("POST", prPath+"/comments/501/replies")
	if len(reqs) != 1 {
		t.Fatalf("%d replies", len(reqs))
	}
	jsonEq(t, reqs[0].Body, `{"body":"done"}`)
}

func TestResolveThread(t *testing.T) {
	f := newFake(t)
	f.reply("POST", "/graphql", 200, map[string]any{"data": map[string]any{}})
	for _, resolved := range []bool{true, false} {
		if err := f.forge(t).ResolveThread(ctx, repoID, 3, forge.ThreadRef{ID: "T-1", CommentID: 501}, resolved); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.requests("POST", "/graphql")
	if len(reqs) != 2 || !strings.Contains(reqs[0].Body, "resolveReviewThread") || strings.Contains(reqs[0].Body, "unresolveReviewThread") ||
		!strings.Contains(reqs[1].Body, "unresolveReviewThread") {
		t.Fatalf("graphql requests = %+v", reqs)
	}
}

func TestReactThread(t *testing.T) {
	f := newFake(t)
	f.reply("POST", "/repos/acme/widgets/pulls/comments/502/reactions", 201, map[string]any{"id": 1})
	if err := f.forge(t).ReactThread(ctx, repoID, 3, 502, forge.ReactDone); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests("POST", "/repos/acme/widgets/pulls/comments/502/reactions")
	if len(reqs) != 1 {
		t.Fatalf("%d reactions", len(reqs))
	}
	jsonEq(t, reqs[0].Body, `{"content":"+1"}`)
	if err := f.forge(t).ReactThread(ctx, repoID, 3, 502, forge.Reaction("nope")); err == nil {
		t.Error("an unknown reaction was accepted")
	}
}

func TestThreadsBlockMerge(t *testing.T) {
	f := newFake(t)
	f.reply("GET", "/repos/acme/widgets/rules/branches/main", 200, []map[string]any{
		{"type": "pull_request", "parameters": map[string]any{"required_review_thread_resolution": true}},
	})
	got, err := f.forge(t).ThreadsBlockMerge(ctx, repoID, "main")
	if err != nil || !got {
		t.Fatalf("ThreadsBlockMerge = %v, %v", got, err)
	}
}
