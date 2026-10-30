package gitlabforge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

const (
	reviewToken   = "review-token-example"
	approveToken  = "approve-token-example"
	reviewUserID  = int64(100)
	approveUserID = int64(200)
)

var (
	_ forge.Forge    = (*Forge)(nil)
	_ forge.Approver = (*Approver)(nil)
)

type request struct {
	Method, Path, Query, Token string
	Body                       map[string]any
}

// fake is an httptest GitLab: routes map "METHOD path" (path relative to
// /api/v4, escaped) to a status and body; anything else is 404.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]reply
	reqs   []request
}

type reply struct {
	status int
	body   any
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, routes: map[string]reply{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		p := r.URL.EscapedPath()[len("/api/v4"):]
		f.mu.Lock()
		f.reqs = append(f.reqs, request{r.Method, p, r.URL.RawQuery, r.Header.Get("PRIVATE-TOKEN"), body})
		rep, ok := f.routes[r.Method+" "+p]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"404 Not Found"}`)
			return
		}
		w.WriteHeader(rep.status)
		switch b := rep.body.(type) {
		case nil:
		case string:
			_, _ = io.WriteString(w, b)
		default:
			_ = json.NewEncoder(w).Encode(b)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) on(method, path string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = reply{status, body}
}

func (f *fake) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

// only returns the one request made, failing unless there was exactly one.
func (f *fake) only() request {
	f.t.Helper()
	reqs := f.requests()
	if len(reqs) != 1 {
		f.t.Fatalf("got %d requests, want 1: %+v", len(reqs), reqs)
	}
	return reqs[0]
}

func (f *fake) forge() *Forge {
	return NewForge(gitlab.New(f.srv.URL, reviewToken, f.srv.Client()), gitlab.User{ID: reviewUserID})
}

func (f *fake) approver(userID int64) *Approver {
	return NewApprover(gitlab.New(f.srv.URL, approveToken, f.srv.Client()), gitlab.User{ID: userID})
}

const mr = "/projects/7/merge_requests/3"

func TestGetChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		mr   map[string]any
		want forge.Change
	}{
		{
			name: "opened is open",
			mr: map[string]any{"iid": 3, "state": "opened", "draft": false, "sha": "h", "target_branch": "main",
				"diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}},
			want: forge.Change{Number: 3, State: forge.StateOpen, HeadSHA: "h", TargetBranch: "main", BaseSHA: "b", StartSHA: "s"},
		},
		{
			name: "draft",
			mr: map[string]any{"iid": 3, "state": "opened", "draft": true, "sha": "h", "target_branch": "main",
				"diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}},
			want: forge.Change{Number: 3, State: forge.StateOpen, Draft: true, HeadSHA: "h", TargetBranch: "main", BaseSHA: "b", StartSHA: "s"},
		},
		{
			name: "merged",
			mr:   map[string]any{"iid": 3, "state": "merged", "sha": "h", "target_branch": "main"},
			want: forge.Change{Number: 3, State: forge.StateMerged, HeadSHA: "h", TargetBranch: "main"},
		},
		{
			name: "closed",
			mr:   map[string]any{"iid": 3, "state": "closed", "sha": "h", "target_branch": "main"},
			want: forge.Change{Number: 3, State: forge.StateClosed, HeadSHA: "h", TargetBranch: "main"},
		},
		{
			name: "locked",
			mr:   map[string]any{"iid": 3, "state": "locked", "sha": "h", "target_branch": "main"},
			want: forge.Change{Number: 3, State: forge.StateLocked, HeadSHA: "h", TargetBranch: "main"},
		},
		{
			// Diff refs computed for an older head: not ready for this one.
			name: "diff refs of another head",
			mr: map[string]any{"iid": 3, "state": "opened", "sha": "h2", "target_branch": "main",
				"diff_refs": map[string]any{"base_sha": "b", "head_sha": "h1", "start_sha": "s"}},
			want: forge.Change{Number: 3, State: forge.StateOpen, HeadSHA: "h2", TargetBranch: "main"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.on(http.MethodGet, mr, http.StatusOK, tc.mr)
			got, err := f.forge().GetChange(context.Background(), 7, 3)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("GetChange = %+v, want %+v", got, tc.want)
			}
			if r := f.only(); r.Method != http.MethodGet || r.Path != mr || r.Token != reviewToken {
				t.Errorf("request = %+v", r)
			}
		})
	}
}

// The title, web address, source branch and author are what the
// dashboard shows of a merge request.
func TestGetChangeDetails(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, mr, http.StatusOK, map[string]any{"iid": 3, "state": "opened", "sha": "h",
		"target_branch": "main", "source_branch": "feature/login", "title": "Add login",
		"web_url": "https://gitlab.example.com/acme/widgets/-/merge_requests/3",
		"author":  map[string]any{"id": 5, "username": "alice"}})
	got, err := f.forge().GetChange(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := forge.Change{Number: 3, State: forge.StateOpen, HeadSHA: "h", TargetBranch: "main",
		SourceBranch: "feature/login", Title: "Add login", Author: "alice",
		WebURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/3"}
	if got != want {
		t.Errorf("GetChange = %+v\nwant %+v", got, want)
	}
}

func TestDefaultBranch(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/7", http.StatusOK, map[string]any{"id": 7, "default_branch": "trunk"})
	got, err := f.forge().DefaultBranch(context.Background(), 7)
	if err != nil || got != "trunk" {
		t.Fatalf("DefaultBranch = %q, %v; want trunk", got, err)
	}
	if r := f.only(); r.Token != reviewToken {
		t.Errorf("request = %+v; want the review token", r)
	}
	if _, err := newFake(t).forge().DefaultBranch(context.Background(), 7); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("DefaultBranch of a missing project = %v; want forge.ErrNotFound", err)
	}
}

func TestGetChangeNotFound(t *testing.T) {
	f := newFake(t)
	_, err := f.forge().GetChange(context.Background(), 7, 3)
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want forge.ErrNotFound", err)
	}
	// The message stays the client's: it can reach a failure reason.
	if want := `gitlab: status 404: {"message":"404 Not Found"}`; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestSetStatus(t *testing.T) {
	for in, want := range map[forge.StatusState]string{
		forge.StatusRunning:  "running",
		forge.StatusSuccess:  "success",
		forge.StatusFailed:   "failed",
		forge.StatusNeutral:  "skipped",
		forge.StatusCanceled: "canceled",
		"":                   "failed",
		"pending":            "failed",
	} {
		t.Run(string(in), func(t *testing.T) {
			f := newFake(t)
			f.on(http.MethodPost, "/projects/7/statuses/abc", http.StatusCreated, map[string]any{"id": 1})
			if err := f.forge().SetStatus(context.Background(), 7, "abc", in, "No blocking issues"); err != nil {
				t.Fatal(err)
			}
			r := f.only()
			if r.Token != reviewToken {
				t.Errorf("token = %q", r.Token)
			}
			if r.Body["state"] != want || r.Body["name"] != "pergent" || r.Body["description"] != "No blocking issues" {
				t.Errorf("body = %v, want state %q", r.Body, want)
			}
		})
	}
}

func TestFindCommentOwnOnly(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, mr+"/notes", http.StatusOK, []map[string]any{
		{"id": 1, "body": "<!-- pergent --> forged", "author": map[string]any{"id": 999}},
		{"id": 2, "body": "unrelated", "author": map[string]any{"id": reviewUserID}},
		{"id": 3, "body": "x <!-- pergent --> y", "author": map[string]any{"id": reviewUserID}},
	})
	id, found, err := f.forge().FindComment(context.Background(), 7, 3, "<!-- pergent -->")
	if err != nil || !found || id != 3 {
		t.Fatalf("FindComment = %d, %v, %v; want 3, true, nil", id, found, err)
	}

	f2 := newFake(t)
	f2.on(http.MethodGet, mr+"/notes", http.StatusOK, []map[string]any{
		{"id": 1, "body": "<!-- pergent -->", "author": map[string]any{"id": 999}},
	})
	if id, found, err := f2.forge().FindComment(context.Background(), 7, 3, "<!-- pergent -->"); err != nil || found || id != 0 {
		t.Fatalf("FindComment = %d, %v, %v; want 0, false, nil", id, found, err)
	}
}

func TestCreateComment(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/notes", http.StatusCreated, map[string]any{"id": 42})
	id, err := f.forge().CreateComment(context.Background(), 7, 3, "hello")
	if err != nil || id != 42 {
		t.Fatalf("CreateComment = %d, %v; want 42, nil", id, err)
	}
	if r := f.only(); r.Body["body"] != "hello" || r.Token != reviewToken {
		t.Errorf("request = %+v", r)
	}
}

func TestUpdateComment(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPut, mr+"/notes/42", http.StatusOK, map[string]any{"id": 42})
	if err := f.forge().UpdateComment(context.Background(), 7, 3, 42, "new"); err != nil {
		t.Fatal(err)
	}
	if r := f.only(); r.Body["body"] != "new" || r.Token != reviewToken {
		t.Errorf("request = %+v", r)
	}

	gone := newFake(t)
	if err := gone.forge().UpdateComment(context.Background(), 7, 3, 42, "new"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("UpdateComment of a deleted note = %v, want forge.ErrNotFound", err)
	}
}

func TestGetFileAndListTree(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/7/repository/files/.pergent%2Finstructions.md/raw", http.StatusOK, "be kind")
	f.on(http.MethodGet, "/projects/7/repository/tree", http.StatusOK, []map[string]any{
		{"name": "a.md", "path": ".pergent/skills/a.md", "type": "blob"},
	})
	g := f.forge()
	b, ok, err := g.GetFile(context.Background(), 7, ".pergent/instructions.md", "s")
	if err != nil || !ok || string(b) != "be kind" {
		t.Fatalf("GetFile = %q, %v, %v", b, ok, err)
	}
	if _, ok, err := g.GetFile(context.Background(), 7, "missing.md", "s"); err != nil || ok {
		t.Fatalf("GetFile of a missing file = %v, %v; want false, nil", ok, err)
	}
	entries, err := g.ListTree(context.Background(), 7, ".pergent/skills", "s")
	want := []forge.TreeEntry{{Name: "a.md", Path: ".pergent/skills/a.md", Type: "blob"}}
	if err != nil || len(entries) != 1 || entries[0] != want[0] {
		t.Fatalf("ListTree = %+v, %v; want %+v", entries, err, want)
	}
}

func TestChangeRef(t *testing.T) {
	if got := (&Forge{}).ChangeRef(12); got != "refs/merge-requests/12/head" {
		t.Errorf("ChangeRef(12) = %q", got)
	}
}

func TestApprove(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/approve", http.StatusCreated, map[string]any{})
	if err := f.approver(approveUserID).Approve(context.Background(), 7, 3, "abc"); err != nil {
		t.Fatal(err)
	}
	if r := f.only(); r.Body["sha"] != "abc" || r.Token != approveToken {
		t.Errorf("request = %+v", r)
	}

	moved := newFake(t)
	moved.on(http.MethodPost, mr+"/approve", http.StatusConflict, map[string]any{"message": "SHA does not match"})
	if err := moved.approver(approveUserID).Approve(context.Background(), 7, 3, "abc"); !errors.Is(err, forge.ErrHeadMoved) {
		t.Fatalf("Approve of a moved head = %v, want forge.ErrHeadMoved", err)
	}

	refused := newFake(t)
	refused.on(http.MethodPost, mr+"/approve", http.StatusUnauthorized, map[string]any{"message": "401"})
	err := refused.approver(approveUserID).Approve(context.Background(), 7, 3, "abc")
	if err == nil || errors.Is(err, forge.ErrHeadMoved) || errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("Approve refused = %v, want a plain error", err)
	}
}

func TestUnapprove(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/unapprove", http.StatusCreated, map[string]any{})
	if err := f.approver(approveUserID).Unapprove(context.Background(), 7, 3); err != nil {
		t.Fatal(err)
	}
	if r := f.only(); r.Token != approveToken {
		t.Errorf("request = %+v", r)
	}

	// Not approved: GitLab answers 404, which is nothing to undo.
	none := newFake(t)
	if err := none.approver(approveUserID).Unapprove(context.Background(), 7, 3); err != nil {
		t.Fatalf("Unapprove with nothing to undo = %v, want nil", err)
	}
}

func TestHasApproved(t *testing.T) {
	approvals := func(ids ...int64) map[string]any {
		var by []map[string]any
		for _, id := range ids {
			by = append(by, map[string]any{"user": map[string]any{"id": id}})
		}
		return map[string]any{"approved_by": by}
	}
	for _, tc := range []struct {
		name string
		ids  []int64
		want bool
	}{
		{"among the approvers", []int64{31, approveUserID}, true},
		{"not among them", []int64{31}, false},
		{"no approvers", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.on(http.MethodGet, mr+"/approvals", http.StatusOK, approvals(tc.ids...))
			got, err := f.approver(approveUserID).HasApproved(context.Background(), 7, 3, "abc")
			if err != nil || got != tc.want {
				t.Fatalf("HasApproved = %v, %v; want %v, nil", got, err, tc.want)
			}
			if r := f.only(); r.Token != approveToken {
				t.Errorf("request = %+v", r)
			}
		})
	}

	// An approve bot whose user id is unknown never asks.
	f := newFake(t)
	if got, err := f.approver(0).HasApproved(context.Background(), 7, 3, "abc"); err != nil || got {
		t.Fatalf("HasApproved without a user id = %v, %v; want false, nil", got, err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("made %d requests without a user id, want 0", n)
	}

	broken := newFake(t)
	broken.on(http.MethodGet, mr+"/approvals", http.StatusForbidden, map[string]any{"message": "403"})
	if _, err := broken.approver(approveUserID).HasApproved(context.Background(), 7, 3, "abc"); err == nil {
		t.Fatal("HasApproved on a refused request = nil error")
	}
}

// GitLab has no neutral commit status, so neutral is "skipped"; anything
// pergent.sh doesn't know is failed, never success. (Moved here from the
// engine's GitLabState test.)
func TestState(t *testing.T) {
	for in, want := range map[forge.StatusState]gitlab.StatusState{
		forge.StatusRunning:  gitlab.StatusRunning,
		forge.StatusSuccess:  gitlab.StatusSuccess,
		forge.StatusFailed:   gitlab.StatusFailed,
		forge.StatusNeutral:  gitlab.StatusSkipped,
		forge.StatusCanceled: gitlab.StatusCanceled,
		"":                   gitlab.StatusFailed,
		"pending":            gitlab.StatusFailed,
	} {
		if got := State(in); got != want {
			t.Errorf("State(%q) = %q, want %q", in, got, want)
		}
	}
}
