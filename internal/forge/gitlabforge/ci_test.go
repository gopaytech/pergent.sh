package gitlabforge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ciFake serves routes by path (relative to /api/v4); a route is a status and
// a body. An unknown path is 404. It records every path asked.
type ciFake struct {
	mu     sync.Mutex
	routes map[string][2]any
	paths  []string
}

func newCI(t *testing.T, routes map[string][2]any) (*Forge, *ciFake) {
	t.Helper()
	f := &ciFake{routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, strings.TrimPrefix(r.URL.Path, "/api/v4"))
		f.mu.Unlock()
		rt, ok := f.routes[strings.TrimPrefix(r.URL.Path, "/api/v4")]
		if !ok {
			http.Error(w, "nope", 404)
			return
		}
		w.WriteHeader(rt[0].(int))
		_, _ = w.Write([]byte(rt[1].(string)))
	}))
	t.Cleanup(srv.Close)
	return NewForge(gitlab.New(srv.URL, reviewToken, srv.Client()), gitlab.User{ID: reviewUserID}), f
}

func (f *ciFake) asked(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func ok(body string) [2]any { return [2]any{200, body} }

var target = forge.CITarget{ProjectID: 7, Change: 3, Head: "head1"}

func statusJob(name string) store.CIJob {
	return store.CIJob{Name: name, Read: "status", BlocksApproval: true}
}

func TestReadCIMergedResultsNeedsHeadParent(t *testing.T) {
	routes := map[string][2]any{
		"/projects/7/pipelines":                  ok(`[]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[{"id":10,"sha":"m1","ref":"refs/merge-requests/3/merge","status":"success"},{"id":11,"sha":"m2","ref":"refs/merge-requests/3/merge","status":"failed"},{"id":12,"sha":"head1","ref":"refs/merge-requests/3/head","status":"success"}]`),
		"/projects/7/repository/commits/m1":      ok(`{"parent_ids":["base","head1"]}`),
		"/projects/7/repository/commits/m2":      ok(`{"parent_ids":["base","old"]}`),
		"/projects/7/pipelines/10/jobs":          ok(`[{"id":1,"name":"test","status":"success"}]`),
		"/projects/7/pipelines/10/bridges":       ok(`[]`),
		"/projects/7/pipelines/11/jobs":          ok(`[{"id":2,"name":"test","status":"failed"}]`),
		"/projects/7/pipelines/11/bridges":       ok(`[]`),
		"/projects/7/pipelines/12/jobs":          ok(`[{"id":0,"name":"other","status":"success"}]`),
		"/projects/7/pipelines/12/bridges":       ok(`[]`),
		"/projects/7/merge_requests/3":           ok(`{"head_pipeline":{"id":11}}`),
	}
	f, _ := newCI(t, routes)
	snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("test")}, false)
	if err != nil || snap.Jobs[0].Result != ci.Passed {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}

func TestReadCINewestAttemptWins(t *testing.T) {
	f, _ := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"success"}]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[]`),
		"/projects/7/pipelines/1/jobs":           ok(`[{"id":9,"name":"test","status":"success"},{"id":5,"name":"test","status":"failed"}]`),
		"/projects/7/pipelines/1/bridges":        ok(`[]`),
	})
	snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("test")}, false)
	if err != nil || snap.Jobs[0].Result != ci.Passed || !snap.Jobs[0].Ended || !snap.Jobs[0].Blocks {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}

func TestReadCIMissingAndFinished(t *testing.T) {
	// Finished is read from the jobs and bridges, not the pipeline's status:
	// pergent's own pending commit status can keep a pipeline running.
	for _, tc := range []struct {
		name, pipeline, job, bridge string
		finished                    bool
	}{
		{"all ended", "success", "success", "success", true},
		{"pipeline running, every job ended", "running", "success", "success", true},
		{"a job still running", "running", "running", "success", false},
		{"a bridge's child still running", "success", "success", "running", false},
	} {
		status, finished := tc.name, tc.finished
		f, _ := newCI(t, map[string][2]any{
			"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"` + tc.pipeline + `"}]`),
			"/projects/7/merge_requests/3/pipelines": ok(`[]`),
			"/projects/7/pipelines/1/jobs":           ok(`[{"id":5,"name":"test","status":"` + tc.job + `"}]`),
			"/projects/7/pipelines/1/bridges":        ok(`[{"id":6,"name":"child","status":"success","downstream_pipeline":{"id":40,"status":"` + tc.bridge + `"}}]`),
		})
		snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("lint")}, false)
		if err != nil || snap.Jobs[0].Result != ci.Missing || snap.Finished != finished || snap.Done() != finished {
			t.Fatalf("%s: snap=%+v err=%v", status, snap, err)
		}
	}
}

func TestReadCIBridgeStatus(t *testing.T) {
	f, fk := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"success"}]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[]`),
		"/projects/7/pipelines/1/jobs":           ok(`[]`),
		"/projects/7/pipelines/1/bridges":        ok(`[{"id":4,"name":"deploy-child","status":"failed"}]`),
	})
	for _, withText := range []bool{false, true} {
		snap, err := f.ReadCI(context.Background(), target, []store.CIJob{
			{Name: "deploy-child", Read: "log"},
			{Name: "deploy-child", Read: "report", Report: &store.CIReport{Path: "r.json"}},
		}, withText)
		if err != nil || snap.Jobs[0].Result != ci.Failed || snap.Jobs[0].Log != "" || fk.asked("/trace") || fk.asked("/artifacts") {
			t.Fatalf("withText=%v snap=%+v err=%v", withText, snap, err)
		}
		if j := snap.Jobs[1]; j.Result != ci.Unreadable || j.Reason != "error" || !j.Ended {
			t.Fatalf("withText=%v report job = %+v", withText, j)
		}
	}
}

func TestReadCIWithText(t *testing.T) {
	routes := map[string][2]any{
		"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"failed"}]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[]`),
		"/projects/7/pipelines/1/jobs": ok(`[{"id":1,"name":"build","status":"failed"},{"id":2,"name":"unit","status":"success"},
			{"id":3,"name":"scan","status":"success"},{"id":4,"name":"gone","status":"success"},{"id":5,"name":"locked","status":"success"}]`),
		"/projects/7/pipelines/1/bridges":                ok(`[]`),
		"/projects/7/jobs/1/trace":                       ok("section_start:1:x\r\nline1\r\nboom\r\nsection_end:1:x\r\n"),
		"/projects/7/jobs/2/trace":                       ok("passed log"),
		"/projects/7/jobs/3/artifacts/reports/scan.json": ok(`{"ok":true}`),
		"/projects/7/jobs/5/artifacts/reports/scan.json": {403, "denied"},
	}
	jobs := []store.CIJob{
		{Name: "build", Read: "log"},
		{Name: "unit", Read: "log"},
		{Name: "scan", Read: "report", Report: &store.CIReport{Path: "reports/scan.json"}},
		{Name: "gone", Read: "report", Report: &store.CIReport{Path: "reports/scan.json"}},
		{Name: "locked", Read: "report", Report: &store.CIReport{Path: "reports/scan.json"}},
	}
	f, fk := newCI(t, routes)
	snap, err := f.ReadCI(context.Background(), target, jobs, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Jobs[0].Log; got != "line1\nboom" {
		t.Fatalf("log = %q", got)
	}
	if snap.Jobs[1].Log != "" || fk.asked("/jobs/2/trace") {
		t.Fatalf("passed job log = %q", snap.Jobs[1].Log)
	}
	if snap.Jobs[2].Report != `{"ok":true}` {
		t.Fatalf("report = %q", snap.Jobs[2].Report)
	}
	if j := snap.Jobs[3]; j.Result != ci.Unreadable || j.Reason != "error" || !j.Ended {
		t.Fatalf("404 job = %+v", j)
	}
	if j := snap.Jobs[4]; j.Result != ci.Unreadable || j.Reason != "no permission" {
		t.Fatalf("403 job = %+v", j)
	}

	f2, fk2 := newCI(t, routes)
	if _, err := f2.ReadCI(context.Background(), target, jobs, false); err != nil {
		t.Fatal(err)
	}
	if fk2.asked("/trace") || fk2.asked("/artifacts") {
		t.Fatalf("status-only read asked for text: %v", fk2.paths)
	}
}

func TestReadCIForkPipelineMissing(t *testing.T) {
	f, _ := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[]`),
	})
	snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("test")}, false)
	if err != nil || snap.Jobs[0].Result != ci.Missing || !snap.Finished || !snap.Done() {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}

func TestRecentCINames(t *testing.T) {
	f, _ := newCI(t, map[string][2]any{
		"/projects/7/pipelines":           ok(`[{"id":1},{"id":2}]`),
		"/projects/7/pipelines/1/jobs":    ok(`[{"id":1,"name":"b"},{"id":2,"name":"a"}]`),
		"/projects/7/pipelines/1/bridges": ok(`[]`),
		"/projects/7/pipelines/2/jobs":    ok(`[{"id":3,"name":"a"}]`),
		"/projects/7/pipelines/2/bridges": ok(`[{"id":4,"name":"c"}]`),
	})
	names, err := f.RecentCINames(context.Background(), target)
	if err != nil || strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names=%v err=%v", names, err)
	}
}

func TestReadCIPrunedMergeCommitSkipped(t *testing.T) {
	f, _ := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"success"}]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[{"id":10,"sha":"gone","ref":"refs/merge-requests/3/merge","status":"failed"}]`),
		"/projects/7/pipelines/1/jobs":           ok(`[{"id":5,"name":"test","status":"success"}]`),
		"/projects/7/pipelines/1/bridges":        ok(`[]`),
	})
	snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("test")}, false)
	if err != nil || snap.Jobs[0].Result != ci.Passed {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}

func TestReadCILogForNonPassed(t *testing.T) {
	f, fk := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"running"}]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[]`),
		"/projects/7/pipelines/1/jobs": ok(`[{"id":1,"name":"c","status":"canceled"},{"id":2,"name":"r","status":"running"},
			{"id":3,"name":"s","status":"skipped"},{"id":4,"name":"p","status":"success"}]`),
		"/projects/7/pipelines/1/bridges":     ok(`[]`),
		"/projects/7/jobs/1/trace":            ok("cancelled log"),
		"/projects/7/jobs/2/trace":            ok("running log"),
		"/projects/7/jobs/1/artifacts/r.json": ok("x"),
		"/projects/7/jobs/2/artifacts/r.json": ok("x"),
	})
	logs := []store.CIJob{{Name: "c", Read: "log"}, {Name: "r", Read: "log"}, {Name: "s", Read: "log"}, {Name: "p", Read: "log"}}
	snap, err := f.ReadCI(context.Background(), target, logs, true)
	if err != nil || snap.Jobs[0].Log != "cancelled log" || snap.Jobs[1].Log != "running log" || snap.Jobs[2].Log != "" || snap.Jobs[3].Log != "" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if fk.asked("/jobs/3/trace") || fk.asked("/jobs/4/trace") {
		t.Fatalf("read a log it should not: %v", fk.paths)
	}
	rep := &store.CIReport{Path: "r.json"}
	snap, err = f.ReadCI(context.Background(), target, []store.CIJob{{Name: "c", Read: "report", Report: rep}, {Name: "r", Read: "report", Report: rep}}, true)
	if err != nil || snap.Jobs[0].Report != "" || snap.Jobs[1].Report != "" || fk.asked("/jobs/1/artifacts") || fk.asked("/jobs/2/artifacts") {
		t.Fatalf("snap=%+v err=%v paths=%v", snap, err, fk.paths)
	}
}

func TestReadCITextFailed(t *testing.T) {
	f, _ := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"success"}]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[]`),
		"/projects/7/pipelines/1/jobs":           ok(`[{"id":1,"name":"scan","status":"success"}]`),
		"/projects/7/pipelines/1/bridges":        ok(`[]`),
	})
	jobs := []store.CIJob{{Name: "scan", Read: "report", Report: &store.CIReport{Path: "r.json"}}}
	snap, err := f.ReadCI(context.Background(), target, jobs, true)
	if err != nil || snap.Jobs[0].Result != ci.Unreadable || !snap.Jobs[0].TextFailed {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	snap, err = f.ReadCI(context.Background(), target, jobs, false)
	if err != nil || snap.Jobs[0].Result != ci.Passed || snap.Jobs[0].TextFailed {
		t.Fatalf("status-only snap=%+v err=%v", snap, err)
	}
}

// A trigger job without strategy: depend is success as soon as its child
// pipeline exists, so the child's status is the job's result.
func TestReadCIBridgeUsesDownstream(t *testing.T) {
	for _, tc := range []struct {
		bridge, downstream string
		want               ci.Result
		ended              bool
	}{
		{"success", "failed", ci.Failed, true},
		{"success", "running", ci.Running, false},
		{"success", "success", ci.Passed, true},
		{"running", "canceled", ci.Cancelled, true},
		{"success", "", ci.Running, false},
		{"pending", "", ci.Running, false},
		{"failed", "", ci.Failed, true},
		{"canceled", "", ci.Cancelled, true},
		{"skipped", "", ci.Skipped, true},
	} {
		bridge := `{"id":4,"name":"child","status":"` + tc.bridge + `"}`
		if tc.downstream != "" {
			bridge = `{"id":4,"name":"child","status":"` + tc.bridge + `","downstream_pipeline":{"id":40,"status":"` + tc.downstream + `"}}`
		}
		f, _ := newCI(t, map[string][2]any{
			"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"success"}]`),
			"/projects/7/merge_requests/3/pipelines": ok(`[]`),
			"/projects/7/pipelines/1/jobs":           ok(`[]`),
			"/projects/7/pipelines/1/bridges":        ok(`[` + bridge + `]`),
		})
		snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("child")}, false)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.bridge, tc.downstream, err)
		}
		if j := snap.Jobs[0]; j.Result != tc.want || j.Ended != tc.ended || snap.Finished != tc.ended {
			t.Fatalf("%s/%s: snap=%+v", tc.bridge, tc.downstream, snap)
		}
	}
}

func (f *ciFake) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

// A commit's parents never change: each merged-results commit is asked for
// once, however often the head is read.
func TestReadCICommitParentsMemoized(t *testing.T) {
	f, fk := newCI(t, map[string][2]any{
		"/projects/7/pipelines":                  ok(`[]`),
		"/projects/7/merge_requests/3/pipelines": ok(`[{"id":10,"sha":"m1","ref":"refs/merge-requests/3/merge","status":"success"}]`),
		"/projects/7/repository/commits/m1":      ok(`{"parent_ids":["base","head1"]}`),
		"/projects/7/pipelines/10/jobs":          ok(`[{"id":1,"name":"test","status":"success"}]`),
		"/projects/7/pipelines/10/bridges":       ok(`[]`),
	})
	for i := 0; i < 3; i++ {
		snap, err := f.ReadCI(context.Background(), target, []store.CIJob{statusJob("test")}, false)
		if err != nil || snap.Jobs[0].Result != ci.Passed {
			t.Fatalf("read %d: snap=%+v err=%v", i, snap, err)
		}
	}
	if n := fk.count("/projects/7/repository/commits/m1"); n != 1 {
		t.Fatalf("parents asked %d times, want 1", n)
	}
	// Another project's commit of the same sha is its own entry.
	other := target
	other.ProjectID = 8
	if _, err := f.commitParents(context.Background(), other.ProjectID, "m1"); err == nil {
		t.Fatal("project 8's commit came from project 7's memo")
	}
}

// The memo is bounded.
func TestCommitParentsMemoBounded(t *testing.T) {
	f, _ := newCI(t, nil)
	for i := 0; i < parentsMemoSize+50; i++ {
		f.rememberParents(7, fmt.Sprintf("sha%d", i), []string{"p"})
	}
	if n := len(f.parents); n > parentsMemoSize {
		t.Fatalf("memo holds %d entries, want at most %d", n, parentsMemoSize)
	}
}

// A report past the 1 MiB read limit is cut, even when what is kept cleans
// down to less than the report cap.
func TestReadCIReportPastReadLimitIsCut(t *testing.T) {
	for _, tc := range []struct {
		size int
		cut  bool
	}{{1 << 20, false}, {1<<20 + 1, true}} {
		body := `{"ok":true}` + strings.Repeat("\x00", tc.size-len(`{"ok":true}`))
		f, _ := newCI(t, map[string][2]any{
			"/projects/7/pipelines":                  ok(`[{"id":1,"sha":"head1","status":"success"}]`),
			"/projects/7/merge_requests/3/pipelines": ok(`[]`),
			"/projects/7/pipelines/1/jobs":           ok(`[{"id":3,"name":"scan","status":"success"}]`),
			"/projects/7/pipelines/1/bridges":        ok(`[]`),
			"/projects/7/jobs/3/artifacts/r.json":    ok(body),
		})
		jobs := []store.CIJob{{Name: "scan", Read: "report", Report: &store.CIReport{Path: "r.json"}}}
		snap, err := f.ReadCI(context.Background(), target, jobs, true)
		if err != nil {
			t.Fatal(err)
		}
		if j := snap.Jobs[0]; j.Report != `{"ok":true}` || j.Cut != tc.cut {
			t.Errorf("size %d: report %q cut %v, want cut %v", tc.size, j.Report, j.Cut, tc.cut)
		}
	}
}
