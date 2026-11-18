package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPipelinesForSHAQuery(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[{"id":1,"sha":"abc","ref":"main","status":"success"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":2,"sha":"abc","ref":"x","status":"failed"}]`))
	})
	ps, err := c.PipelinesForSHA(context.Background(), 7, "abc")
	if err != nil || len(ps) != 2 || ps[1].ID != 2 || ps[1].Status != "failed" {
		t.Fatalf("ps=%v err=%v", ps, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/pipelines", "per_page=100&sha=abc")
	expect(t, (*reqs)[1], "GET", "/api/v4/projects/7/pipelines", "page=2&per_page=100&sha=abc")
}

func TestPipelineJobsIncludesBridges(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bridges") {
			_, _ = w.Write([]byte(`[{"id":9,"name":"deploy-child","status":"running"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":3,"name":"test","status":"success"}]`))
	})
	jobs, err := c.PipelineJobs(context.Background(), 7, 5)
	if err != nil || len(jobs) != 2 || jobs[0].Bridge || !jobs[1].Bridge || jobs[1].Name != "deploy-child" {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/pipelines/5/jobs", "per_page=100")
	expect(t, (*reqs)[1], "GET", "/api/v4/projects/7/pipelines/5/bridges", "per_page=100")
}

func TestJobTraceStreams(t *testing.T) {
	size := maxResponse + 1<<20 // more than do() buffers
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", size)))
	})
	c.hc.Timeout = 0
	rc, err := c.JobTrace(context.Background(), 7, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil || int(n) != size {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestJobTraceForbiddenAndNotFound(t *testing.T) {
	for status, want := range map[int]error{401: ErrForbidden, 403: ErrForbidden, 404: ErrNotFound} {
		c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", status) })
		if _, err := c.JobTrace(context.Background(), 7, 4); !errors.Is(err, want) {
			t.Fatalf("%d: err=%v", status, err)
		}
	}
}

func TestJobArtifactPathEscaped(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	rc, err := c.JobArtifact(context.Background(), 7, 7, "reports/cycode.json")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	rc, err = c.JobArtifact(context.Background(), 7, 7, "my dir/a b.json")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if (*reqs)[0].Path != "/api/v4/projects/7/jobs/7/artifacts/reports/cycode.json" {
		t.Fatalf("path = %s", (*reqs)[0].Path)
	}
	if (*reqs)[1].Path != "/api/v4/projects/7/jobs/7/artifacts/my%20dir/a%20b.json" {
		t.Fatalf("path = %s", (*reqs)[1].Path)
	}
}

func TestCommitParents(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"m","parent_ids":["a","b"]}`))
	})
	ps, err := c.CommitParents(context.Background(), 7, "m")
	if err != nil || len(ps) != 2 || ps[1] != "b" {
		t.Fatalf("ps=%v err=%v", ps, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/repository/commits/m", "")
}

func TestEnsureHookCIEvents(t *testing.T) {
	for _, existing := range []bool{false, true} {
		c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				if existing {
					_, _ = w.Write([]byte(`[{"id":9,"url":"https://p.example.com/w"}]`))
				} else {
					_, _ = w.Write([]byte(`[]`))
				}
				return
			}
			_, _ = w.Write([]byte(`{"id":9}`))
		})
		if _, err := c.EnsureHook(context.Background(), 7, "https://p.example.com/w", "s"); err != nil {
			t.Fatal(err)
		}
		b := hookBody(t, (*reqs)[1].Body)
		if b["pipeline_events"] != true || b["job_events"] != true {
			t.Fatalf("existing=%v body = %v", existing, b)
		}
	}
}
