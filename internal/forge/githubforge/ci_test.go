package githubforge

import (
	"archive/zip"
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var _ forge.CIReader = (*Forge)(nil)

const (
	runsPath   = "/repos/acme/widgets/commits/head1/check-runs"
	suitesPath = "/repos/acme/widgets/commits/head1/check-suites"
)

var ciTarget = forge.CITarget{ProjectID: repoID, Project: "acme/widgets", Change: 3, Head: "head1"}

func ciJob(name, read string) store.CIJob {
	return store.CIJob{Name: name, Read: read, BlocksApproval: true}
}

func run(id int64, name, status, conclusion, slug string, appID int64) map[string]any {
	return map[string]any{"id": id, "name": name, "status": status, "conclusion": conclusion,
		"app": map[string]any{"id": appID, "slug": slug}, "output": map[string]any{"title": "T", "summary": "S", "text": "X"}}
}

func runs(rs ...map[string]any) map[string]any {
	return map[string]any{"total_count": len(rs), "check_runs": rs}
}

func suites(ss ...map[string]any) map[string]any {
	return map[string]any{"total_count": len(ss), "check_suites": ss}
}

func suite(status string, n int) map[string]any {
	return map[string]any{"id": 1, "status": status, "latest_check_runs_count": n, "app": map[string]any{"id": 5}}
}

func readOne(t *testing.T, f *fakeGitHub, job store.CIJob, withText bool) ci.Snapshot {
	t.Helper()
	snap, err := f.forge(t).ReadCI(ctx, ciTarget, []store.CIJob{job}, withText)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Jobs) != 1 {
		t.Fatalf("jobs = %+v", snap.Jobs)
	}
	return snap
}

func TestReadCIAllRunsWithNameMustPass(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites(suite("completed", 2)))
	f.reply("GET", runsPath, 200, runs(
		run(1, "test", "completed", "success", "github-actions", 15368),
		run(2, "test", "completed", "failure", "github-actions", 15368)))
	snap := readOne(t, f, ciJob("test", "status"), false)
	if j := snap.Jobs[0]; j.Result != ci.Failed || !j.Ended {
		t.Errorf("job = %+v; want failed, ended", j)
	}
	if !snap.Finished || snap.Head != "head1" {
		t.Errorf("snap = %+v", snap)
	}
	// First non-passed in table order: running beats failed.
	f.reply("GET", runsPath, 200, runs(
		run(1, "test", "completed", "failure", "github-actions", 15368),
		run(2, "test", "in_progress", "", "github-actions", 15368)))
	if j := readOne(t, f, ciJob("test", "status"), false).Jobs[0]; j.Result != ci.Running || j.Ended {
		t.Errorf("job = %+v; want running, not ended", j)
	}
	f.reply("GET", runsPath, 200, runs(
		run(1, "test", "completed", "skipped", "github-actions", 15368),
		run(2, "test", "completed", "cancelled", "github-actions", 15368),
		run(3, "test", "completed", "failure", "github-actions", 15368)))
	if j := readOne(t, f, ciJob("test", "status"), false).Jobs[0]; j.Result != ci.Failed {
		t.Errorf("job = %+v; want failed (failed, cancelled, skipped order)", j)
	}
}

func TestReadCIMissing(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites(suite("completed", 1)))
	f.reply("GET", runsPath, 200, runs())
	if j := readOne(t, f, ciJob("test", "status"), false).Jobs[0]; j.Result != ci.Missing || j.Ended {
		t.Errorf("job = %+v; want missing", j)
	}
}

func TestReadCINeutralPasses(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites(suite("completed", 1)))
	f.reply("GET", runsPath, 200, runs(run(1, "scan", "completed", "neutral", "scanner", 9)))
	if j := readOne(t, f, ciJob("scan", "status"), false).Jobs[0]; j.Result != ci.Passed || !j.Ended {
		t.Errorf("job = %+v; want passed", j)
	}
}

func TestReadCIIgnoresOwnApp(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites())
	// 123 is the review App (f.app), 456 the approve App.
	f.reply("GET", runsPath, 200, runs(run(1, "pergent", "completed", "success", "acme-review", 123)))
	if j := readOne(t, f, ciJob("pergent", "status"), false).Jobs[0]; j.Result != ci.Missing {
		t.Errorf("review App run read: %+v; want missing", j)
	}
	f.reply("GET", runsPath, 200, runs(run(1, "pergent", "completed", "success", "acme-approve", 456)))
	snap, err := f.forge(t).IgnoreApps(456).ReadCI(ctx, ciTarget, []store.CIJob{ciJob("pergent", "status")}, false)
	if err != nil || snap.Jobs[0].Result != ci.Missing {
		t.Errorf("approve App run read: %+v, %v; want missing", snap.Jobs, err)
	}
}

func TestReadCIFinishedIgnoresEmptySuites(t *testing.T) {
	f := newFake(t)
	f.reply("GET", runsPath, 200, runs())
	f.reply("GET", suitesPath, 200, suites(suite("queued", 0), suite("completed", 2)))
	if snap := readOne(t, f, ciJob("test", "status"), false); !snap.Finished {
		t.Error("an empty queued suite stopped Finished")
	}
	f.reply("GET", suitesPath, 200, suites(suite("queued", 1), suite("completed", 2)))
	if snap := readOne(t, f, ciJob("test", "status"), false); snap.Finished {
		t.Error("a queued suite with runs didn't stop Finished")
	}
	f.reply("GET", suitesPath, 200, suites())
	if snap := readOne(t, f, ciJob("test", "status"), false); !snap.Finished {
		t.Error("no suites at all: want Finished")
	}
}

func zipOf(t *testing.T, name, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	fw, _ := w.Create(name)
	_, _ = fw.Write([]byte(body))
	_ = w.Close()
	return b.Bytes()
}

func TestReadCIActionsLogAndReport(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites(suite("completed", 1)))
	f.reply("GET", runsPath, 200, runs(run(55, "test", "completed", "failure", "github-actions", 15368)))
	f.handle("GET", "/repos/acme/widgets/actions/jobs/55/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a\nb\nboom\n"))
	})
	snap := readOne(t, f, ciJob("test", "log"), true)
	if j := snap.Jobs[0]; j.Result != ci.Failed || !strings.Contains(j.Log, "boom") || j.Report != "" {
		t.Errorf("job = %+v; want the log tail", j)
	}

	// Report: job -> run id -> artifact -> zip file.
	f.reply("GET", runsPath, 200, runs(run(55, "scan", "completed", "success", "github-actions", 15368)))
	f.reply("GET", "/repos/acme/widgets/actions/jobs/55", 200, map[string]any{"run_id": 77})
	f.reply("GET", "/repos/acme/widgets/actions/runs/77/artifacts", 200,
		map[string]any{"artifacts": []map[string]any{{"id": 4, "name": "rep"}}})
	z := zipOf(t, "cycode.json", `{"issues":0}`)
	f.handle("GET", "/repos/acme/widgets/actions/artifacts/4/zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(z) })
	job := store.CIJob{Name: "scan", Read: "report", BlocksApproval: true, Report: &store.CIReport{Artifact: "rep", Path: "cycode.json"}}
	if j := readOne(t, f, job, true).Jobs[0]; j.Result != ci.Passed || j.Report != `{"issues":0}` || j.Log != "" {
		t.Errorf("job = %+v; want the report", j)
	}
	// A status read, or a skipped job, asks for no text.
	before := len(f.requests("GET", "/repos/acme/widgets/actions/jobs/55"))
	readOne(t, f, job, false)
	if n := len(f.requests("GET", "/repos/acme/widgets/actions/jobs/55")); n != before {
		t.Error("withText=false asked for the report")
	}
	f.reply("GET", runsPath, 200, runs(run(55, "scan", "completed", "skipped", "github-actions", 15368)))
	if j := readOne(t, f, job, true).Jobs[0]; j.Result != ci.Skipped || j.Report != "" {
		t.Errorf("skipped job = %+v; want no text", j)
	}
}

func TestReadCIOtherAppOutputAsLog(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites(suite("completed", 1)))
	f.reply("GET", runsPath, 200, runs(run(8, "scan", "completed", "failure", "scanner", 9)))
	f.reply("GET", "/repos/acme/widgets/check-runs/8/annotations", 200, []map[string]any{
		{"path": "a.go", "start_line": 2, "annotation_level": "failure", "message": "leak"}})
	j := readOne(t, f, ciJob("scan", "log"), true).Jobs[0]
	for _, want := range []string{"T", "S", "X", "a.go:2", "leak"} {
		if !strings.Contains(j.Log, want) {
			t.Errorf("log %q misses %q", j.Log, want)
		}
	}
	// No annotation request without text.
	n := len(f.requests("GET", "/repos/acme/widgets/check-runs/8/annotations"))
	readOne(t, f, ciJob("scan", "log"), false)
	if len(f.requests("GET", "/repos/acme/widgets/check-runs/8/annotations")) != n {
		t.Error("withText=false asked for annotations")
	}
	// report on another app's run: unreadable in every read, no I/O.
	job := store.CIJob{Name: "scan", Read: "report", Report: &store.CIReport{Path: "x.json"}}
	for _, withText := range []bool{false, true} {
		if j := readOne(t, f, job, withText).Jobs[0]; j.Result != ci.Unreadable || j.Reason != "error" || !j.Ended || j.TextFailed {
			t.Errorf("withText=%v job = %+v; want unreadable/error/ended", withText, j)
		}
	}
	if len(f.requests("GET", "/repos/acme/widgets/actions/jobs/8")) != 0 {
		t.Error("a non-Actions report made an Actions request")
	}
}

func TestReadCIActionsForbidden(t *testing.T) {
	f := newFake(t)
	f.reply("GET", suitesPath, 200, suites(suite("completed", 1)))
	f.reply("GET", runsPath, 200, runs(run(55, "test", "completed", "failure", "github-actions", 15368)))
	f.reply("GET", "/repos/acme/widgets/actions/jobs/55/logs", 403, map[string]string{"message": "Resource not accessible by integration"})
	j := readOne(t, f, ciJob("test", "log"), true).Jobs[0]
	if j.Result != ci.Unreadable || j.Reason != "no permission" || !j.Ended || !j.TextFailed {
		t.Errorf("job = %+v; want unreadable/no permission", j)
	}
	f.reply("GET", "/repos/acme/widgets/actions/jobs/55/logs", 500, map[string]string{"message": "x"})
	j = readOne(t, f, ciJob("test", "log"), true).Jobs[0]
	if j.Result != ci.Unreadable || j.Reason != "error" || !j.TextFailed {
		t.Errorf("job = %+v; want unreadable/error", j)
	}
}

func TestRecentCINames(t *testing.T) {
	f := newFake(t)
	f.reply("GET", runsPath, 200, runs(run(1, "test", "completed", "success", "a", 1), run(2, "pergent", "completed", "success", "a", 123)))
	tgt := ciTarget
	tgt.RecentHeads = []string{"head1"}
	got, err := f.forge(t).RecentCINames(ctx, tgt)
	if err != nil || strings.Join(got, ",") != "test" {
		t.Fatalf("names = %v, %v; want test (pergent is refused as a name)", got, err)
	}
}
