package ci

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func jobs(js ...store.CIJob) store.CIJobs {
	return store.CIJobs{WaitSeconds: int64(DefaultWait / time.Second), Jobs: js}
}

func logJob(name string) store.CIJob { return store.CIJob{Name: name, Read: "log"} }

func TestValidate(t *testing.T) {
	eleven := make([]store.CIJob, 11)
	for i := range eleven {
		eleven[i] = logJob(fmt.Sprintf("job%d", i))
	}
	report := func(read string, r *store.CIReport) store.CIJob {
		return store.CIJob{Name: "scan", Read: read, Report: r}
	}
	withWait := func(d time.Duration) store.CIJobs {
		c := jobs(logJob("test"))
		c.WaitSeconds = int64(d / time.Second)
		return c
	}
	tests := []struct {
		name    string
		in      store.CIJobs
		github  bool
		wantErr string // empty: valid
	}{
		{"a log job", jobs(logJob("test")), false, ""},
		{"a status job", jobs(store.CIJob{Name: "test", Read: "status"}), true, ""},
		{"a report job on GitLab", jobs(report("report", &store.CIReport{Path: "out/r.json"})), false, ""},
		{"a report job on GitHub", jobs(report("report", &store.CIReport{Artifact: "a", Path: "r.json"})), true, ""},
		{"no jobs", jobs(), false, ""},
		{"the shortest wait", withWait(MinWait), false, ""},
		{"the longest wait", withWait(MaxWait), false, ""},
		{"ten jobs", jobs(eleven[:10]...), false, ""},
		{"eleven jobs", jobs(eleven...), false, "ci_jobs.jobs"},
		{"a duplicate name", jobs(logJob("a"), logJob("a")), false, "ci_jobs.jobs[1].name"},
		{"pergent", jobs(logJob("Pergent")), false, "ci_jobs.jobs[0].name"},
		{"an empty name", jobs(logJob("")), false, "ci_jobs.jobs[0].name"},
		{"a long name", jobs(logJob(strings.Repeat("a", 256))), false, "ci_jobs.jobs[0].name"},
		{"a name with a space inside", jobs(logJob("unit tests")), false, ""},
		{"a leading space", jobs(logJob(" test")), false, "ci_jobs.jobs[0].name must not start or end with a space"},
		{"a trailing tab", jobs(logJob("test\t")), false, "ci_jobs.jobs[0].name must not start or end with a space"},
		{"a trailing newline", jobs(logJob("test\n")), false, "ci_jobs.jobs[0].name"},
		{"a control character", jobs(logJob("te\x1bst")), false, "ci_jobs.jobs[0].name must not hold control characters"},
		{"a DEL", jobs(logJob("te\x7fst")), false, "ci_jobs.jobs[0].name must not hold control characters"},
		{"a C1 control", jobs(logJob("te\u0085st")), false, "ci_jobs.jobs[0].name must not hold control characters"},
		{"read: logs", jobs(store.CIJob{Name: "a", Read: "logs"}), false, "ci_jobs.jobs[0].read"},
		{"no read", jobs(store.CIJob{Name: "a"}), false, "ci_jobs.jobs[0].read"},
		{"report with no report", jobs(report("report", nil)), false, "ci_jobs.jobs[0].report"},
		{"report with read: log", jobs(report("log", &store.CIReport{Path: "r"})), false, "ci_jobs.jobs[0].report"},
		{"an empty path", jobs(report("report", &store.CIReport{Path: ""})), false, "ci_jobs.jobs[0].report.path"},
		{"an absolute path", jobs(report("report", &store.CIReport{Path: "/etc/x"})), false, "ci_jobs.jobs[0].report.path must be relative"},
		{"a path with ..", jobs(report("report", &store.CIReport{Path: "a/../b"})), false, "ci_jobs.jobs[0].report.path"},
		{"a path of ..", jobs(report("report", &store.CIReport{Path: ".."})), false, "ci_jobs.jobs[0].report.path"},
		{"a second job's bad path", jobs(logJob("t"), report("report", &store.CIReport{Path: "/x"})), false, "ci_jobs.jobs[1].report.path must be relative"},
		{"a GitHub report with no artifact", jobs(report("report", &store.CIReport{Path: "r"})), true, "ci_jobs.jobs[0].report.artifact"},
		{"a wait of 4m", withWait(4 * time.Minute), false, "ci_jobs.wait"},
		{"a wait of 3h", withWait(3 * time.Hour), false, "ci_jobs.wait"},
		{"no wait", withWait(0), false, "ci_jobs.wait"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.in, tt.github)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCIJobsWait(t *testing.T) {
	if got := (store.CIJobs{WaitSeconds: 1800}).Wait(); got != 30*time.Minute {
		t.Errorf("Wait() = %v, want 30m", got)
	}
}
