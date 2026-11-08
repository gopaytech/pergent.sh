package gitlabforge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

const (
	reviewToken   = "review-token-example"
	approveToken  = "approve-token-example"
	reviewUserID  = int64(100)
	reviewName    = "pergent-review"
	approveName   = "pergent-approve"
	approveUserID = int64(200)
)

var (
	_ forge.Forge        = (*Forge)(nil)
	_ forge.Approver     = (*Approver)(nil)
	_ forge.SourceReader = (*Forge)(nil)
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
	pages  map[string]func(query url.Values) (status int, body any, next string)
	reqs   []request
}

type reply struct {
	status int
	body   any
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, routes: map[string]reply{}, pages: map[string]func(url.Values) (int, any, string){}}
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
		page := f.pages[r.Method+" "+p]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if page != nil {
			status, body, next := page(r.URL.Query())
			if next != "" {
				w.Header().Set("X-Next-Page", next)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
			return
		}
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

// onPages answers a route by the request's query, and may name the next page.
func (f *fake) onPages(method, path string, h func(query url.Values) (status int, body any, next string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[method+" "+path] = h
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
	return NewForge(gitlab.New(f.srv.URL, reviewToken, f.srv.Client()), gitlab.User{ID: reviewUserID, Username: reviewName})
}

func (f *fake) approver(userID int64) *Approver {
	return NewApprover(gitlab.New(f.srv.URL, approveToken, f.srv.Client()), gitlab.User{ID: userID, Username: approveName})
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

// The title, web address, source branch and author (name and ID) are what the
// dashboard shows of a merge request.
func TestGetChangeDetails(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, mr, http.StatusOK, map[string]any{"iid": 3, "state": "opened", "sha": "h",
		"target_branch": "main", "source_branch": "feature/login", "title": "Add login",
		"description": "Why it changes.",
		"web_url":     "https://gitlab.example.com/acme/widgets/-/merge_requests/3",
		"author":      map[string]any{"id": 5, "username": "alice"}})
	got, err := f.forge().GetChange(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := forge.Change{Number: 3, State: forge.StateOpen, HeadSHA: "h", TargetBranch: "main",
		SourceBranch: "feature/login", Title: "Add login", Description: "Why it changes.", Author: "alice", AuthorID: 5,
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
		forge.StatusPending:  "pending",
		"":                   "failed",
		"weird":              "failed",
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
		forge.StatusPending:  gitlab.StatusPending,
		"":                   gitlab.StatusFailed,
		"weird":              gitlab.StatusFailed,
	} {
		if got := State(in); got != want {
			t.Errorf("State(%q) = %q, want %q", in, got, want)
		}
	}
}

// The review bot reaches the projects where it is Maintainer or above: it
// creates their webhooks and deploy tokens.
func TestReachableRepos(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/projects", 200, []map[string]any{{"path_with_namespace": "acme/widgets"}})
	got, truncated, err := f.forge().ReachableRepos(context.Background(), "wid", 10)
	if err != nil || truncated || len(got) != 1 || got[0] != "acme/widgets" {
		t.Fatalf("got=%v truncated=%v err=%v", got, truncated, err)
	}
	if q := f.only().Query; q != "min_access_level=40&per_page=100&search=wid&search_namespaces=true&simple=true" {
		t.Errorf("query = %q", q)
	}
}

// The review bot can set a project up exactly when the picker lists it. The
// project is looked up by its path (for its ID and GitLab's spelling), and the
// listing the picker reads is asked about that one ID: access gained through an
// ancestor group or a share shows there, while the project's own permissions
// are null. No search, no paging.

// reachFake answers GET /projects/<path> with one project whose permissions
// are null, and GET /projects like GitLab does for id_after, id_before and
// min_access_level: the listed IDs between the two bounds, only at Maintainer.
func reachFake(t *testing.T, path string, id int, listed ...int) *fake {
	t.Helper()
	f := newFake(t)
	f.on("GET", "/projects/"+url.PathEscape(path), 200, map[string]any{"id": id, "path_with_namespace": path,
		"permissions": map[string]any{"project_access": nil, "group_access": nil}})
	f.onPages("GET", "/projects", func(q url.Values) (int, any, string) {
		after, _ := strconv.Atoi(q.Get("id_after"))
		before, _ := strconv.Atoi(q.Get("id_before"))
		out := []map[string]any{}
		for _, l := range listed {
			if q.Get("min_access_level") == "40" && l > after && l < before {
				out = append(out, map[string]any{"id": l, "path_with_namespace": "acme/listed"})
			}
		}
		return 200, out, ""
	})
	return f
}

func TestCanReach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed []int
		want   bool
	}{
		{"listed", []int{7}, true},
		{"listed among others", []int{3, 7, 9}, true},
		{"not listed", nil, false},
		{"only other projects are listed", []int{3, 8}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := reachFake(t, "acme/widgets", 7, tc.listed...)
			ok, err := f.forge().CanReach(context.Background(), "acme/widgets")
			if err != nil || ok != tc.want {
				t.Fatalf("CanReach = %v, %v; want %v", ok, err, tc.want)
			}
		})
	}
}

func TestCanReachRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   bool
		err    bool
	}{
		{"not found", 404, false, false},
		{"forbidden", 403, false, false},
		{"broken", 400, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.on("GET", "/projects/acme%2Fwidgets", tc.status, map[string]any{"message": "no"})
			ok, err := f.forge().CanReach(context.Background(), "acme/widgets")
			if ok != tc.want || (err != nil) != tc.err {
				t.Fatalf("CanReach = %v, %v", ok, err)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("made %d requests, want 1 (no listing for a project it can't read)", n)
			}
		})
	}
	// The listing failing is an error, not a no.
	f := newFake(t)
	f.on("GET", "/projects/acme%2Fwidgets", 200, map[string]any{"id": 7, "path_with_namespace": "acme/widgets"})
	f.on("GET", "/projects", 400, map[string]any{"message": "400 Bad request"})
	if ok, err := f.forge().CanReach(context.Background(), "acme/widgets"); ok || err == nil {
		t.Fatalf("CanReach with the listing broken = %v, %v; want an error", ok, err)
	}
}

// A project reached through a group above its own, or a share, has null
// permissions on GET /projects/:path; the listing narrowed to its ID is what
// says it is reached, and it is asked at Maintainer level.
func TestCanReachInheritedAccess(t *testing.T) {
	f := reachFake(t, "acme/team/widgets", 7, 7)
	ok, err := f.forge().CanReach(context.Background(), "acme/team/widgets")
	if err != nil || !ok {
		t.Fatalf("CanReach = %v, %v; want true", ok, err)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].Path != "/projects/acme%2Fteam%2Fwidgets" || reqs[0].Query != "" ||
		reqs[1].Path != "/projects" || reqs[1].Query != "id_after=6&id_before=8&min_access_level=40&simple=true" {
		t.Errorf("requests = %+v; want the project, then the listing narrowed to ID 7 at level 40", reqs)
	}
}

// Nothing depends on the name: a short one, and one that is a substring of
// many projects, take the same two requests, with no search and no page.
func TestCanReachNeedsNoSearch(t *testing.T) {
	for _, path := range []string{"acme/x", "acme/ab", "acme/api"} {
		t.Run(path, func(t *testing.T) {
			f := reachFake(t, path, 12, 12)
			ok, err := f.forge().CanReach(context.Background(), path)
			if err != nil || !ok {
				t.Fatalf("CanReach = %v, %v; want true", ok, err)
			}
			reqs := f.requests()
			if len(reqs) != 2 {
				t.Fatalf("requests = %+v, want 2", reqs)
			}
			for _, r := range reqs {
				if strings.Contains(r.Query, "search") || strings.Contains(r.Query, "page") {
					t.Errorf("request %s ?%s searches or pages", r.Path, r.Query)
				}
			}
		})
	}
}

// ReachableName answers with GitLab's spelling of the path, whatever case was
// asked for (GitLab finds a project by its path without regard to case).
func TestReachableNameIsGitLabsSpelling(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/projects/Acme%2FWidgets", 200, map[string]any{"id": 7, "path_with_namespace": "acme/widgets"})
	f.on("GET", "/projects", 200, []map[string]any{{"id": 7, "path_with_namespace": "acme/widgets"}})
	name, ok, err := f.forge().ReachableName(context.Background(), "Acme/Widgets")
	if err != nil || !ok || name != "acme/widgets" {
		t.Fatalf("ReachableName = %q, %v, %v; want acme/widgets, true", name, ok, err)
	}

	f = reachFake(t, "acme/widgets", 7)
	if name, ok, err := f.forge().ReachableName(context.Background(), "acme/widgets"); err != nil || ok || name != "" {
		t.Fatalf("ReachableName of a project not listed = %q, %v, %v; want none", name, ok, err)
	}
}

// GitLab answers a path that is another project's (a renamed project's old
// path, or a number, which is an ID) with that project; it is not the one
// asked for, so it is not reached, and the listing isn't asked.
func TestReachableNameIsForTheProjectAskedFor(t *testing.T) {
	for _, asked := range []string{"7", "acme/old-name"} {
		t.Run(asked, func(t *testing.T) {
			f := newFake(t)
			f.on("GET", "/projects/"+url.PathEscape(asked), 200, map[string]any{"id": 7, "path_with_namespace": "acme/widgets"})
			name, ok, err := f.forge().ReachableName(context.Background(), asked)
			if err != nil || ok || name != "" {
				t.Fatalf("ReachableName(%q) = %q, %v, %v; want none", asked, name, ok, err)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("made %d requests, want 1", n)
			}
		})
	}
	f := newFake(t)
	for _, asked := range []string{"", "acme/"} {
		if _, ok, err := f.forge().ReachableName(context.Background(), asked); err != nil || ok {
			t.Errorf("ReachableName(%q) = %v, %v; want false", asked, ok, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("made %d requests for an empty or open path, want 0", n)
	}
}

func TestBotAndMention(t *testing.T) {
	f := newFake(t)
	fg := f.forge()
	if got := fg.Bot(); got != (forge.Identity{ID: reviewUserID, Username: reviewName}) {
		t.Errorf("Bot = %+v", got)
	}
	if got := fg.Mention(); got != reviewName {
		t.Errorf("Mention = %q, want the bot's username", got)
	}
	if got := f.approver(approveUserID).Bot(); got != (forge.Identity{ID: approveUserID, Username: approveName}) {
		t.Errorf("Approver.Bot = %+v", got)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("made %d requests, want 0", n)
	}
}

func TestReact(t *testing.T) {
	for r, want := range map[forge.Reaction]string{
		forge.ReactSeen: "eyes", forge.ReactDone: "thumbsup", forge.ReactRefused: "thumbsdown",
	} {
		t.Run(want, func(t *testing.T) {
			f := newFake(t)
			f.on(http.MethodPost, mr+"/notes/42/award_emoji", http.StatusCreated, map[string]any{"id": 1})
			if err := f.forge().React(context.Background(), 7, 3, 42, r); err != nil {
				t.Fatal(err)
			}
			if got := f.only(); got.Body["name"] != want || got.Token != reviewToken {
				t.Errorf("request = %+v, want name %q by the review bot", got, want)
			}
		})
	}

	f := newFake(t)
	if err := f.forge().React(context.Background(), 7, 3, 42, forge.Reaction("party")); err == nil {
		t.Error("React with an unknown reaction = nil")
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("an unknown reaction sent %d requests", n)
	}

	gone := newFake(t)
	if err := gone.forge().React(context.Background(), 7, 3, 42, forge.ReactSeen); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("React on a deleted note = %v, want forge.ErrNotFound", err)
	}
}

// A reply is a new note with the text as given: no marker is added.
func TestReply(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodPost, mr+"/notes", http.StatusCreated, map[string]any{"id": 43})
	if err := f.forge().Reply(context.Background(), 7, 3, "nothing failed"); err != nil {
		t.Fatal(err)
	}
	if r := f.only(); r.Body["body"] != "nothing failed" || r.Token != reviewToken {
		t.Errorf("request = %+v", r)
	}
}

func TestIsBot(t *testing.T) {
	for _, bot := range []bool{true, false} {
		f := newFake(t)
		f.on(http.MethodGet, "/users/12", http.StatusOK, map[string]any{"id": 12, "username": "project_7_bot", "bot": bot})
		got, err := f.forge().IsBot(context.Background(), 12)
		if err != nil || got != bot {
			t.Errorf("bot %v = %v, %v", bot, got, err)
		}
		if r := f.only(); r.Token != reviewToken || r.Path != "/users/12" {
			t.Errorf("request = %+v", r)
		}
	}
	// No ID: not looked up.
	f := newFake(t)
	if got, err := f.forge().IsBot(context.Background(), 0); err != nil || got {
		t.Errorf("no ID = %v, %v", got, err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests for no ID", n)
	}
	// A user GitLab can't find or read is an error, never a person.
	f = newFake(t)
	if _, err := f.forge().IsBot(context.Background(), 12); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("unknown user: err = %v, want ErrNotFound", err)
	}
}

func TestAccessLevel(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  forge.Access
	}{
		{10, forge.AccessRead}, {20, forge.AccessRead},
		{30, forge.AccessWrite},
		{40, forge.AccessMaintain}, {50, forge.AccessMaintain},
	} {
		f := newFake(t)
		f.on(http.MethodGet, "/projects/7/members/all/12", http.StatusOK, map[string]any{"id": 12, "access_level": tc.level, "state": "active"})
		got, err := f.forge().AccessLevel(context.Background(), 7, "dev", 12)
		if err != nil || got != tc.want {
			t.Errorf("level %d = %v, %v; want %v", tc.level, got, err, tc.want)
		}
		if r := f.only(); r.Token != reviewToken || r.Path != "/projects/7/members/all/12" {
			t.Errorf("request = %+v", r)
		}
	}

	// A blocked maintainer is nobody.
	f := newFake(t)
	f.on(http.MethodGet, "/projects/7/members/all/12", http.StatusOK, map[string]any{"id": 12, "access_level": 40, "state": "blocked"})
	if got, err := f.forge().AccessLevel(context.Background(), 7, "dev", 12); err != nil || got != forge.AccessNone {
		t.Errorf("blocked = %v, %v; want AccessNone, nil", got, err)
	}

	// Not a member: 404.
	f = newFake(t)
	if got, err := f.forge().AccessLevel(context.Background(), 7, "dev", 12); err != nil || got != forge.AccessNone {
		t.Errorf("non-member = %v, %v; want AccessNone, nil", got, err)
	}

	// No user id: nothing is asked.
	f = newFake(t)
	if got, err := f.forge().AccessLevel(context.Background(), 7, "dev", 0); err != nil || got != forge.AccessNone {
		t.Errorf("no id = %v, %v; want AccessNone, nil", got, err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("no user id sent %d requests", n)
	}

	// A failure is an error, never a level.
	f = newFake(t)
	f.on(http.MethodGet, "/projects/7/members/all/12", http.StatusForbidden, map[string]any{"message": "403"})
	if got, err := f.forge().AccessLevel(context.Background(), 7, "dev", 12); err == nil || got != forge.AccessNone {
		t.Errorf("403 = %v, %v; want AccessNone and an error", got, err)
	}
}

const commitSHA = "0123456789abcdef0123456789abcdef01234567"

// A repository by its path, which pergent.sh has not switched on: the review
// bot (Reporter access) reads it at the commit the ref resolves to.
func TestGitLabSourceReader(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/acme%2Fskills", http.StatusOK, map[string]any{"id": 42})
	f.on(http.MethodGet, "/projects/42/repository/commits/release%2Fv1", http.StatusOK, map[string]any{"id": commitSHA})
	f.on(http.MethodGet, "/projects/42/repository/tree", http.StatusOK, []map[string]any{
		{"name": "a.md", "path": "skills/a.md", "type": "blob"},
	})
	f.on(http.MethodGet, "/projects/42/repository/files/skills%2Fa.md/raw", http.StatusOK, "# a")
	g := f.forge()
	ctx := context.Background()

	got, err := g.ResolveRef(ctx, "acme/skills", "release/v1")
	if err != nil || got != commitSHA {
		t.Fatalf("ResolveRef = %q, %v", got, err)
	}
	entries, err := g.ListTreeAt(ctx, "acme/skills", "skills", commitSHA)
	if err != nil || len(entries) != 1 || entries[0] != (forge.TreeEntry{Name: "a.md", Path: "skills/a.md", Type: "blob"}) {
		t.Fatalf("ListTreeAt = %+v, %v", entries, err)
	}
	b, ok, err := g.GetFileAt(ctx, "acme/skills", "skills/a.md", commitSHA)
	if err != nil || !ok || string(b) != "# a" {
		t.Fatalf("GetFileAt = %q, %v, %v", b, ok, err)
	}
	for _, r := range f.requests() {
		if r.Token != reviewToken {
			t.Errorf("request %s %s used token %q; want the review bot's", r.Method, r.Path, r.Token)
		}
		if strings.Contains(r.Path, "/files/") && r.Query != "ref="+commitSHA {
			t.Errorf("file read at %q; want the commit", r.Query)
		}
	}

	// Same not-found semantics as the Source methods: a missing folder is an
	// empty list and a missing file is ok == false.
	f.on(http.MethodGet, "/projects/42/repository/tree", http.StatusNotFound, nil)
	if e, err := g.ListTreeAt(ctx, "acme/skills", "nope", commitSHA); err != nil || len(e) != 0 {
		t.Errorf("ListTreeAt of a missing folder = %+v, %v; want empty, nil", e, err)
	}
	if b, ok, err := g.GetFileAt(ctx, "acme/skills", "nope.md", commitSHA); err != nil || ok || b != nil {
		t.Errorf("GetFileAt of a missing file = %q, %v, %v; want nil, false, nil", b, ok, err)
	}
}

// A project the bot can't see (GitLab says 404), an empty path, or a ref that
// doesn't exist is forge.ErrSourceNotFound, from all three methods.
func TestGitLabSourceReaderNotFound(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/acme%2Fskills", http.StatusOK, map[string]any{"id": 42})
	g := f.forge()
	ctx := context.Background()

	if _, err := g.ResolveRef(ctx, "acme/skills", "no-such-ref"); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("ResolveRef of an unknown ref = %v; want ErrSourceNotFound", err)
	}
	for _, project := range []string{"acme/private", ""} {
		if _, err := g.ResolveRef(ctx, project, "main"); !errors.Is(err, forge.ErrSourceNotFound) {
			t.Errorf("ResolveRef(%q) = %v; want ErrSourceNotFound", project, err)
		}
		if _, err := g.ListTreeAt(ctx, project, "skills", commitSHA); !errors.Is(err, forge.ErrSourceNotFound) {
			t.Errorf("ListTreeAt(%q) = %v; want ErrSourceNotFound", project, err)
		}
		if _, _, err := g.GetFileAt(ctx, project, "a.md", commitSHA); !errors.Is(err, forge.ErrSourceNotFound) {
			t.Errorf("GetFileAt(%q) = %v; want ErrSourceNotFound", project, err)
		}
	}
}

// A file bigger than a review can carry is never read whole into memory.
func TestGitLabSourceReaderFileTooLarge(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/projects/acme%2Fskills", http.StatusOK, map[string]any{"id": 42})
	f.on(http.MethodGet, "/projects/42/repository/files/big.md/raw", http.StatusOK, strings.Repeat("x", forge.MaxSourceFile+1))
	f.on(http.MethodGet, "/projects/42/repository/files/ok.md/raw", http.StatusOK, strings.Repeat("x", forge.MaxSourceFile))
	g := f.forge()
	if _, ok, err := g.GetFileAt(context.Background(), "acme/skills", "big.md", commitSHA); ok || !errors.Is(err, forge.ErrFileTooLarge) {
		t.Errorf("GetFileAt of a big file = %v, %v; want ErrFileTooLarge", ok, err)
	}
	if b, ok, err := g.GetFileAt(context.Background(), "acme/skills", "ok.md", commitSHA); err != nil || !ok || len(b) != forge.MaxSourceFile {
		t.Errorf("GetFileAt at the limit = %d, %v, %v", len(b), ok, err)
	}
}

// A project the bot sees but may not read (a bot below Reporter, a restricted
// repository feature: GitLab answers 403) is forge.ErrSourceNotFound too, so
// the admin is never shown a raw platform error.
func TestGitLabSourceReaderForbidden(t *testing.T) {
	ctx := context.Background()
	forbidden := `{"message":"403 Forbidden"}`

	// The project itself is forbidden.
	f := newFake(t)
	f.on(http.MethodGet, "/projects/acme%2Fsecret", http.StatusForbidden, forbidden)
	g := f.forge()
	if _, err := g.ResolveRef(ctx, "acme/secret", "main"); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("ResolveRef = %v; want ErrSourceNotFound", err)
	}
	if _, err := g.ListTreeAt(ctx, "acme/secret", "skills", commitSHA); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("ListTreeAt = %v; want ErrSourceNotFound", err)
	}
	if _, _, err := g.GetFileAt(ctx, "acme/secret", "a.md", commitSHA); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("GetFileAt = %v; want ErrSourceNotFound", err)
	}

	// The project shows, its repository does not.
	f = newFake(t)
	f.on(http.MethodGet, "/projects/acme%2Fskills", http.StatusOK, map[string]any{"id": 42})
	f.on(http.MethodGet, "/projects/42/repository/commits/main", http.StatusForbidden, forbidden)
	f.on(http.MethodGet, "/projects/42/repository/tree", http.StatusForbidden, forbidden)
	f.on(http.MethodGet, "/projects/42/repository/files/a.md/raw", http.StatusForbidden, forbidden)
	g = f.forge()
	if _, err := g.ResolveRef(ctx, "acme/skills", "main"); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("ResolveRef with a forbidden repository = %v; want ErrSourceNotFound", err)
	}
	if _, err := g.ListTreeAt(ctx, "acme/skills", "skills", commitSHA); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("ListTreeAt with a forbidden repository = %v; want ErrSourceNotFound", err)
	}
	if _, _, err := g.GetFileAt(ctx, "acme/skills", "a.md", commitSHA); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("GetFileAt with a forbidden repository = %v; want ErrSourceNotFound", err)
	}
}

// A note is read from the merge request it is said to be on.
func TestComment(t *testing.T) {
	f := newFake(t)
	f.routes["GET /projects/7/merge_requests/3/notes/9"] = reply{http.StatusOK, map[string]any{
		"id": 9, "body": "@bot review", "system": true, "author": map[string]any{"id": 5, "username": "mo"},
	}}
	got, err := f.forge().Comment(context.Background(), 7, 3, 9)
	if err != nil {
		t.Fatal(err)
	}
	if want := (forge.Comment{AuthorID: 5, Author: "mo", Body: "@bot review", System: true}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := f.forge().Comment(context.Background(), 7, 4, 9); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("a note on another merge request: err = %v, want forge.ErrNotFound", err)
	}
}

// GitLab's project visibility is read from the project.
func TestVisibility(t *testing.T) {
	for vis, want := range map[string]forge.Visibility{"public": forge.VisPublic, "internal": forge.VisInternal, "private": forge.VisPrivate} {
		f := newFake(t)
		f.on("GET", "/projects/acme%2Fwidgets", 200, map[string]any{"id": 7, "path_with_namespace": "acme/widgets", "visibility": vis})
		if got, err := f.forge().Visibility(context.Background(), "acme/widgets"); err != nil || got != want {
			t.Errorf("%s: Visibility = %q, %v; want %q", vis, got, err, want)
		}
	}
	for name, body := range map[string]map[string]any{
		"missing": {"id": 7, "path_with_namespace": "acme/widgets"},
		"unknown": {"id": 7, "path_with_namespace": "acme/widgets", "visibility": "everyone"},
	} {
		f := newFake(t)
		f.on("GET", "/projects/acme%2Fwidgets", 200, body)
		if got, err := f.forge().Visibility(context.Background(), "acme/widgets"); err == nil {
			t.Errorf("%s: Visibility = %q, want an error", name, got)
		}
	}
	f := newFake(t)
	f.on("GET", "/projects/acme%2Fwidgets", 404, map[string]any{"message": "404 Project Not Found"})
	if _, err := f.forge().Visibility(context.Background(), "acme/widgets"); !errors.Is(err, forge.ErrSourceNotFound) {
		t.Errorf("404: err = %v, want ErrSourceNotFound", err)
	}
}

// A username is looked up exactly, ignoring case.
func TestUserByName(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/users", 200, []map[string]any{{"id": 5, "username": "Alice"}})
	u, err := f.forge().UserByName(context.Background(), "acme/widgets", "alice")
	if err != nil || u != (forge.User{ID: 5, Username: "Alice"}) {
		t.Fatalf("UserByName = %+v, %v", u, err)
	}
	if q := f.only().Query; q != "username=alice" {
		t.Errorf("asked with query %q, want username=alice", q)
	}

	f = newFake(t)
	f.on("GET", "/users", 200, []map[string]any{{"id": 6, "username": "alice2"}})
	if _, err := f.forge().UserByName(context.Background(), "acme/widgets", "alice"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("no exact match: err = %v, want ErrNotFound", err)
	}
	f = newFake(t)
	f.on("GET", "/users", 500, map[string]any{"message": "boom"})
	if _, err := f.forge().UserByName(context.Background(), "acme/widgets", "alice"); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("500: err = %v, want an error that isn't ErrNotFound", err)
	}
}
