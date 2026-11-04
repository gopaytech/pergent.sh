package gitlabforge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

var _ forge.Inline = (*Forge)(nil)

func marked(text, fp string) string { return text + "\n\n" + render.InlineMarker(fp) }

func TestChangeDiff(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, mr+"/diffs", http.StatusOK, []map[string]any{
		{"old_path": "old.go", "new_path": "new.go", "diff": "@@ -1 +1 @@\n-a\n+b\n"},
		{"old_path": "gone.go", "new_path": "gone.go", "deleted_file": true, "diff": "@@ -1 +0,0 @@\n-x\n"},
		{"old_path": "img.png", "new_path": "img.png", "diff": ""},
	})
	f.on(http.MethodGet, mr, http.StatusOK, map[string]any{"iid": 3, "changes_count": "3"})
	got, err := f.forge().ChangeDiff(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.FileDiff{
		{Path: "new.go", OldPath: "old.go", Patch: "@@ -1 +1 @@\n-a\n+b\n"},
		{Path: "gone.go", OldPath: "gone.go", Deleted: true, Patch: "@@ -1 +0,0 @@\n-x\n"},
		{Path: "img.png", OldPath: "img.png"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangeDiff = %+v, want %+v", got, want)
	}
}

// A diff GitLab cut short is an error, never the whole diff.
func TestChangeDiffPartial(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, mr+"/diffs", http.StatusOK, []map[string]any{{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-a\n+b\n"}})
	f.on(http.MethodGet, mr, http.StatusOK, map[string]any{"iid": 3, "changes_count": "1000+"})
	got, err := f.forge().ChangeDiff(context.Background(), 7, 3)
	if !errors.Is(err, gitlab.ErrPartialDiff) || got != nil {
		t.Errorf("got=%+v err=%v, want gitlab.ErrPartialDiff", got, err)
	}
}

func TestCompareDiffPartial(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/7/repository/compare", http.StatusOK, map[string]any{
		"compare_timeout": true, "diffs": []map[string]any{{"old_path": "a.go", "new_path": "a.go"}}})
	got, err := f.forge().CompareDiff(context.Background(), 7, "aaa", "bbb")
	if !errors.Is(err, gitlab.ErrPartialDiff) || got != nil {
		t.Errorf("got=%+v err=%v, want gitlab.ErrPartialDiff", got, err)
	}
}

func TestChangeDiffMissingChange(t *testing.T) {
	f := newFake(t) // every route is 404: /diffs and /changes
	_, err := f.forge().ChangeDiff(context.Background(), 7, 3)
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want forge.ErrNotFound", err)
	}
}

func TestCompareDiff(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/7/repository/compare", http.StatusOK, map[string]any{"diffs": []map[string]any{
		{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-a\n+b\n"},
	}})
	got, err := f.forge().CompareDiff(context.Background(), 7, "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if want := []forge.FileDiff{{Path: "a.go", OldPath: "a.go", Patch: "@@ -1 +1 @@\n-a\n+b\n"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("CompareDiff = %+v, want %+v", got, want)
	}
	if r := f.only(); r.Query != "from=aaa&straight=true&to=bbb" {
		t.Errorf("query = %q", r.Query)
	}
}

func TestThreadsOnlyBotWithMarker(t *testing.T) {
	pos := func(path string, line int) map[string]any { return map[string]any{"new_path": path, "new_line": line} }
	note := func(id, author int64, body string, position map[string]any, resolved bool) map[string]any {
		n := map[string]any{"id": id, "body": body, "author": map[string]any{"id": author}, "resolvable": position != nil, "resolved": resolved}
		if position != nil {
			n["position"] = position
		}
		return n
	}
	f := newFake(t)
	f.on(http.MethodGet, mr+"/discussions", http.StatusOK, []map[string]any{
		// The bot's, open, a reply from a person after it.
		{"id": "d-bot", "notes": []map[string]any{
			note(10, reviewUserID, marked("finding", "fp1"), pos("a.go", 12), false),
			note(11, 9, "will fix", nil, false),
		}},
		// The bot's, resolved, over a range.
		{"id": "d-range", "notes": []map[string]any{
			note(20, reviewUserID, marked("range", "fp2"), map[string]any{"new_path": "b.go", "new_line": 5,
				"line_range": map[string]any{"start": map[string]any{"new_line": 5}, "end": map[string]any{"new_line": 8}}}, true),
		}},
		// A person pasted the marker.
		{"id": "d-impostor", "notes": []map[string]any{note(30, 9, marked("fake", "fp3"), pos("a.go", 1), false)}},
		// The bot's, without a marker.
		{"id": "d-nomarker", "notes": []map[string]any{note(40, reviewUserID, "just a note", pos("a.go", 2), false)}},
		// The bot's marker, but the first note is not the bot's.
		{"id": "d-reply", "notes": []map[string]any{
			note(50, 9, "question", pos("a.go", 3), false),
			note(51, reviewUserID, marked("answer", "fp4"), nil, false),
		}},
		// The bot's marker on a plain MR note (not on the diff).
		{"id": "d-plain", "notes": []map[string]any{note(60, reviewUserID, marked("summary", "fp5"), nil, false)}},
		{"id": "d-empty", "notes": []map[string]any{}},
	})
	got, err := f.forge().Threads(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Thread{
		{Ref: forge.ThreadRef{ID: "d-bot", CommentID: 10}, Body: marked("finding", "fp1"), Path: "a.go", Line: 12, EndLine: 12},
		{Ref: forge.ThreadRef{ID: "d-range", CommentID: 20}, Body: marked("range", "fp2"), Path: "b.go", Line: 5, EndLine: 8, Resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Threads = %+v\nwant %+v", got, want)
	}
}

// Without the bot's user ID every author would be unknown: an error, not an empty list.
func TestThreadsNeedsBotID(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, mr+"/discussions", http.StatusOK, []map[string]any{})
	fg := NewForge(gitlab.New(f.srv.URL, reviewToken, f.srv.Client()), gitlab.User{})
	got, err := fg.Threads(context.Background(), 7, 3)
	if err == nil || got != nil {
		t.Errorf("got=%+v err=%v, want an error", got, err)
	}
}

// diffRefs is a change whose diff GitLab has computed.
var diffRefs = forge.Change{Number: 3, HeadSHA: "head", BaseSHA: "base", StartSHA: "start"}

func TestPostThreadsPositions(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/discussions", http.StatusCreated, map[string]any{"id": "disc1", "notes": []map[string]any{{"id": 77}}})
	refs, errs, err := f.forge().PostThreads(context.Background(), 7, diffRefs, []forge.NewThread{
		{Path: "a.go", OldPath: "", Line: 12, EndLine: 14, Body: marked("added", "fp1")},
		{Path: "new.go", OldPath: "old.go", Line: 20, OldLine: 18, Body: marked("unchanged", "fp2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("errs[%d] = %v", i, e)
		}
	}
	for i, r := range refs {
		if r != (forge.ThreadRef{ID: "disc1", CommentID: 77}) {
			t.Errorf("refs[%d] = %+v", i, r)
		}
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v", reqs)
	}
	for i, want := range []map[string]any{
		// An added line has no old_line, and a multi-line finding is anchored
		// on its first line alone.
		{"position_type": "text", "base_sha": "base", "start_sha": "start", "head_sha": "head",
			"old_path": "a.go", "new_path": "a.go", "new_line": float64(12)},
		// An unchanged line has both.
		{"position_type": "text", "base_sha": "base", "start_sha": "start", "head_sha": "head",
			"old_path": "old.go", "new_path": "new.go", "new_line": float64(20), "old_line": float64(18)},
	} {
		r := reqs[i]
		if r.Method != http.MethodPost || r.Path != mr+"/discussions" || r.Token != reviewToken {
			t.Errorf("request %d = %+v", i, r)
		}
		if got := r.Body["position"]; !reflect.DeepEqual(got, want) {
			t.Errorf("request %d position = %v, want %v", i, got, want)
		}
	}
	if reqs[0].Body["body"] != marked("added", "fp1") || reqs[1].Body["body"] != marked("unchanged", "fp2") {
		t.Errorf("bodies = %v, %v", reqs[0].Body["body"], reqs[1].Body["body"])
	}
}

func TestPostThreadsNeedsDiffRefs(t *testing.T) {
	for name, ch := range map[string]forge.Change{
		"no base":  {HeadSHA: "h", StartSHA: "s"},
		"no start": {HeadSHA: "h", BaseSHA: "b"},
		"no head":  {BaseSHA: "b", StartSHA: "s"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			refs, errs, err := f.forge().PostThreads(context.Background(), 7, ch, []forge.NewThread{{Path: "a.go", Line: 1, Body: marked("x", "fp1")}})
			if err == nil {
				t.Fatal("want an error")
			}
			if len(refs) != 1 || len(errs) != 1 {
				t.Errorf("refs=%v errs=%v, want one slot each", refs, errs)
			}
			if reqs := f.requests(); len(reqs) != 0 {
				t.Errorf("requests = %+v, want none", reqs)
			}
		})
	}
}

// postServer answers each POST to the discussions endpoint with handle(n),
// n counting from 1.
func postServer(t *testing.T, handle func(n int, w http.ResponseWriter)) *Forge {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		handle(k, w)
	}))
	t.Cleanup(srv.Close)
	return NewForge(gitlab.New(srv.URL, reviewToken, srv.Client()), gitlab.User{ID: reviewUserID, Username: reviewName})
}

func TestPostThreadsOneFailsAlone(t *testing.T) {
	f := postServer(t, func(n int, w http.ResponseWriter) {
		if n == 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"line_code can't be blank"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "disc" + string(rune('0'+n)), "notes": []map[string]any{{"id": 70 + n}}})
	})
	refs, errs, err := f.PostThreads(context.Background(), 7, diffRefs, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: marked("one", "fp1")}, {Path: "a.go", Line: 999, Body: marked("two", "fp2")}, {Path: "a.go", Line: 3, Body: marked("three", "fp3")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if refs[0] != (forge.ThreadRef{ID: "disc1", CommentID: 71}) || refs[2] != (forge.ThreadRef{ID: "disc3", CommentID: 73}) {
		t.Errorf("refs = %+v", refs)
	}
	if refs[1] != (forge.ThreadRef{}) || errs[1] == nil || errs[0] != nil || errs[2] != nil {
		t.Errorf("refs[1]=%+v errs=%v", refs[1], errs)
	}
}

// cancelAfterFirst is a transport that cancels the context once the first
// reply is fully read: the caller gives up between two posts.
type cancelAfterFirst struct {
	next   http.RoundTripper
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterFirst) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	c.once.Do(c.cancel)
	return resp, nil
}

// A cancelled context stops the posting; threads never tried have an error.
func TestPostThreadsStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"disc1","notes":[{"id":71}]}`)
	}))
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &cancelAfterFirst{next: srv.Client().Transport, cancel: cancel}}
	f := NewForge(gitlab.New(srv.URL, reviewToken, hc), gitlab.User{ID: reviewUserID, Username: reviewName})
	refs, errs, err := f.PostThreads(ctx, 7, diffRefs, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: marked("one", "fp1")}, {Path: "a.go", Line: 2, Body: marked("two", "fp2")}, {Path: "a.go", Line: 3, Body: marked("three", "fp3")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] != nil || refs[0].ID != "disc1" {
		t.Errorf("first: ref=%+v err=%v", refs[0], errs[0])
	}
	for i := 1; i < 3; i++ {
		if !errors.Is(errs[i], context.Canceled) || refs[i] != (forge.ThreadRef{}) {
			t.Errorf("thread %d: ref=%+v err=%v, want context.Canceled", i, refs[i], errs[i])
		}
	}
}

// A thread with no path or line can't be anchored: its error, no request.
func TestPostThreadsUnanchored(t *testing.T) {
	f := newFake(t)
	_, errs, err := f.forge().PostThreads(context.Background(), 7, diffRefs, []forge.NewThread{
		{Path: "", Line: 1, Body: marked("x", "fp1")}, {Path: "a.go", Line: 0, Body: marked("y", "fp2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] == nil || errs[1] == nil {
		t.Errorf("errs = %v", errs)
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("requests = %+v", reqs)
	}
}

// A thread without a finding marker could not be found again: not posted.
func TestPostThreadsNeedsMarker(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/discussions", http.StatusCreated, map[string]any{"id": "disc1", "notes": []map[string]any{{"id": 77}}})
	refs, errs, err := f.forge().PostThreads(context.Background(), 7, diffRefs, []forge.NewThread{
		{Path: "a.go", Line: 1, Body: "no marker"},
		{Path: "a.go", Line: 2, Body: marked("marked", "fp1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] == nil || refs[0] != (forge.ThreadRef{}) {
		t.Errorf("unmarked: ref=%+v err=%v", refs[0], errs[0])
	}
	if errs[1] != nil || refs[1].ID != "disc1" {
		t.Errorf("marked: ref=%+v err=%v", refs[1], errs[1])
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// A discussion is created with one request: a retry could start a second thread.
func TestPostThreadsDoesNotRetry(t *testing.T) {
	var n atomic.Int32
	f := postServer(t, func(k int, w http.ResponseWriter) {
		n.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	refs, errs, err := f.PostThreads(context.Background(), 7, diffRefs, []forge.NewThread{{Path: "a.go", Line: 1, Body: marked("one", "fp1")}})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] == nil || refs[0] != (forge.ThreadRef{}) {
		t.Errorf("ref=%+v err=%v", refs[0], errs[0])
	}
	if n.Load() != 1 {
		t.Errorf("requests = %d, want 1", n.Load())
	}
}

func TestReplyThread(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/discussions/disc1/notes", http.StatusCreated, map[string]any{"id": 88})
	if err := f.forge().ReplyThread(context.Background(), 7, 3, forge.ThreadRef{ID: "disc1", CommentID: 77}, "fixed"); err != nil {
		t.Fatal(err)
	}
	r := f.only()
	if r.Method != http.MethodPost || r.Path != mr+"/discussions/disc1/notes" || r.Body["body"] != "fixed" || r.Token != reviewToken {
		t.Errorf("request = %+v", r)
	}
}

func TestReplyThreadGone(t *testing.T) {
	f := newFake(t)
	err := f.forge().ReplyThread(context.Background(), 7, 3, forge.ThreadRef{ID: "disc1"}, "x")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want forge.ErrNotFound", err)
	}
}

func TestResolveThread(t *testing.T) {
	for _, resolved := range []bool{true, false} {
		f := newFake(t)
		f.on(http.MethodPut, mr+"/discussions/disc1", http.StatusOK, map[string]any{"id": "disc1"})
		if err := f.forge().ResolveThread(context.Background(), 7, 3, forge.ThreadRef{ID: "disc1", CommentID: 77}, resolved); err != nil {
			t.Fatal(err)
		}
		want := "resolved=false"
		if resolved {
			want = "resolved=true"
		}
		if r := f.only(); r.Method != http.MethodPut || r.Path != mr+"/discussions/disc1" || r.Query != want {
			t.Errorf("request = %+v, want query %s", r, want)
		}
	}
}

func TestReactThread(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/notes/77/award_emoji", http.StatusCreated, map[string]any{"id": 1})
	if err := f.forge().ReactThread(context.Background(), 7, 3, 77, forge.ReactDone); err != nil {
		t.Fatal(err)
	}
	if r := f.only(); r.Path != mr+"/notes/77/award_emoji" || r.Body["name"] != "thumbsup" {
		t.Errorf("request = %+v", r)
	}
	if err := f.forge().ReactThread(context.Background(), 7, 3, 77, forge.Reaction("nope")); err == nil {
		t.Error("an unknown reaction must be an error")
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestThreadsBlockMerge(t *testing.T) {
	for name, tc := range map[string]struct {
		project map[string]any
		want    bool
	}{
		"true":    {map[string]any{"id": 7, "only_allow_merge_if_all_discussions_are_resolved": true}, true},
		"false":   {map[string]any{"id": 7, "only_allow_merge_if_all_discussions_are_resolved": false}, false},
		"missing": {map[string]any{"id": 7}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.on(http.MethodGet, "/projects/7", http.StatusOK, tc.project)
			got, err := f.forge().ThreadsBlockMerge(context.Background(), 7, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ThreadsBlockMerge = %v, want %v", got, tc.want)
			}
			if r := f.only(); r.Method != http.MethodGet || r.Path != "/projects/7" {
				t.Errorf("request = %+v", r)
			}
		})
	}
}

func TestThreadsBlockMergeProjectGone(t *testing.T) {
	f := newFake(t)
	if _, err := f.forge().ThreadsBlockMerge(context.Background(), 7, "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want forge.ErrNotFound", err)
	}
}
