package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ciSource is a repository reader that is also a forge.CIReader: it answers
// RecentCINames with names (or err) and records the target it was asked for.
type ciSource struct {
	*fakeSource
	mu     sync.Mutex
	names  []string
	err    error
	target forge.CITarget
}

func (c *ciSource) ReadCI(context.Context, forge.CITarget, []store.CIJob, bool) (ci.Snapshot, error) {
	return ci.Snapshot{}, errors.New("not used")
}

func (c *ciSource) RecentCINames(_ context.Context, t forge.CITarget) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.target = t
	return c.names, c.err
}

const ciJobsBody = `{"ciJobs":{"wait":"45m","jobs":[` +
	`{"name":"test","read":"log","blocksApproval":true},` +
	`{"name":"scan","read":"report","report":{"path":"out/scan.json"},"blocksApproval":false}]}}`

// The CI jobs setting is stored as given, shown as {wait, jobs}, cleared by
// null, refused when ci.Validate refuses it, and changed by a maintainer but
// not a viewer. A repository the config lists shows the config's value and
// refuses a change.
func TestCIJobsSettingReadWrite(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.aliceOn("main", store.RoleMaintainer)
	c := e.aliceSession()
	target := "/api/repositories/main/acme/gadgets/settings"

	if s := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets").Settings["ciJobs"]; s.Overridden {
		t.Errorf("ciJobs before any save = %+v; want not overridden", s)
	}

	rec := e.send(c, "PUT", target, ciJobsBody)
	wantCode(t, rec, "PUT ciJobs", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(detail.Settings["ciJobs"])
	want := `{"value":{"wait":"45m","jobs":[` +
		`{"name":"test","read":"log","blocksApproval":true},` +
		`{"name":"scan","read":"report","report":{"path":"out/scan.json"},"blocksApproval":false}]},"overridden":true}`
	if string(raw) != want {
		t.Errorf("ciJobs view = %s\nwant %s", raw, want)
	}
	row, _ := e.storedRepo("main", "acme/gadgets")
	if row.CIJobs == nil || row.CIJobs.WaitSeconds != 45*60 || len(row.CIJobs.Jobs) != 2 ||
		row.CIJobs.Jobs[1].Report == nil || row.CIJobs.Jobs[1].Report.Path != "out/scan.json" {
		t.Errorf("stored = %+v; want the 45 minute wait and both jobs", row.CIJobs)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || len(g.CIJobs.Jobs) != 2 {
		t.Errorf("effective = %+v, %v; want 2 jobs", g.CIJobs, err)
	}

	// Refused: nothing is saved, and the status is 400.
	for _, body := range []string{
		`{"ciJobs":{"wait":"1m","jobs":[{"name":"a","read":"status"}]}}`,
		`{"ciJobs":{"wait":"3h","jobs":[{"name":"a","read":"status"}]}}`,
		`{"ciJobs":{"wait":"soon","jobs":[{"name":"a","read":"status"}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"pergent","read":"status","blocksApproval":true}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"status"},{"name":"a","read":"log"}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"log","report":{"path":"x"}}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"report"}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"report","report":{"path":"../x"}}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"other"}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"log","extra":1}]}}`,
		`{"ciJobs":{"wait":"30m","jobs":[],"extra":1}}`,
		`{"ciJobs":[]}`,
		`{"ciJobs":"x"}`,
	} {
		rec := e.send(c, "PUT", target, body)
		wantCode(t, rec, "PUT "+body, http.StatusBadRequest)
		if errorOf(t, rec) == "" {
			t.Errorf("PUT %s: no error message", body)
		}
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CIJobs == nil || len(r.CIJobs.Jobs) != 2 {
		t.Errorf("a refused PUT changed the row: %+v", r.CIJobs)
	}
	if !strings.Contains(msgBadSettings, "ciJobs") {
		t.Errorf("msgBadSettings = %q; want ciJobs named", msgBadSettings)
	}

	// null, and an empty list, clear it.
	wantCode(t, e.send(c, "PUT", target, `{"ciJobs":null}`), "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CIJobs != nil {
		t.Errorf("stored after null = %+v; want NULL", r.CIJobs)
	}
	e.send(c, "PUT", target, ciJobsBody)
	wantCode(t, e.send(c, "PUT", target, `{"ciJobs":{"wait":"30m","jobs":[]}}`), "PUT no jobs", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CIJobs != nil {
		t.Errorf("stored after no jobs = %+v; want NULL", r.CIJobs)
	}

	// A viewer may not.
	v := newEnv(t, nil)
	v.dashboardRepo("main", "acme/gadgets", nil)
	v.aliceOn("main", store.RoleViewer)
	wantCode(t, v.send(v.aliceSession(), "PUT", target, ciJobsBody), "viewer PUT", http.StatusForbidden)
	if r, _ := v.storedRepo("main", "acme/gadgets"); r.CIJobs != nil {
		t.Errorf("a viewer's PUT changed the row: %+v", r.CIJobs)
	}

	// On GitHub a report needs the workflow artifact's name.
	g := newEnv(t, nil)
	g.dashboardRepo("gh", "acme/gadgets", nil)
	g.aliceOn("gh", store.RoleMaintainer)
	gt := "/api/repositories/gh/acme/gadgets/settings"
	noArtifact := `{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"report","report":{"path":"x.json"}}]}}`
	wantCode(t, g.send(g.aliceSession(), "PUT", gt, noArtifact), "GitHub report without an artifact", http.StatusBadRequest)
	withArtifact := `{"ciJobs":{"wait":"30m","jobs":[{"name":"a","read":"report","report":{"artifact":"rep","path":"x.json"}}]}}`
	wantCode(t, g.send(g.aliceSession(), "PUT", gt, withArtifact), "GitHub report with an artifact", http.StatusOK)

	// The config wins: its value is shown, overridden, and read-only.
	k := newEnv(t, func(cfg *config.Config) {
		cfg.Repositories[0].CIJobs = &store.CIJobs{WaitSeconds: 600, Jobs: []store.CIJob{{Name: "build", Read: "status", BlocksApproval: true}}}
	})
	k.cat["main"].set("acme/widgets")
	cfgSession := k.userOnMain(true, "acme/widgets")
	s := getJSON[RepositoryDetailView](k, cfgSession, "/api/repositories/main/acme/widgets").Settings["ciJobs"]
	raw, _ = json.Marshal(s)
	if want := `{"value":{"wait":"10m","jobs":[{"name":"build","read":"status","blocksApproval":true}]},"overridden":true}`; string(raw) != want {
		t.Errorf("config ciJobs = %s\nwant %s", raw, want)
	}
	wantCode(t, k.send(cfgSession, "PUT", "/api/repositories/main/acme/widgets/settings", ciJobsBody), "PUT on a config repository", http.StatusConflict)
}

// A maintainer's reset clears the CI jobs, which are theirs to change.
func TestResetClearsCIJobs(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) {
		r.CIJobs = &store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{{Name: "test", Read: "log"}}}
	})
	e.aliceOn("main", store.RoleMaintainer)
	wantCode(t, e.send(e.aliceSession(), "POST", "/api/repositories/main/acme/gadgets/settings/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CIJobs != nil {
		t.Errorf("stored after a maintainer's reset = %+v; want NULL", r.CIJobs)
	}
}

// The names the platform shows come back sorted as given; a platform with no
// CI reader, a repository not synced, and a viewer are each handled.
func TestCIJobNames(t *testing.T) {
	e := newEnv(t, nil)
	rd := &ciSource{fakeSource: newFakeSource(), names: []string{"build", "test (1.22)"}}
	e.sources.Set("gh", rd)
	e.addRun(runFix{instance: "gh", project: "acme/widgets", number: 1, head: head('a'), at: e.clock.now().Add(-2 * time.Hour)})
	e.addRun(runFix{instance: "gh", project: "acme/widgets", number: 2, head: head('b'), at: e.clock.now().Add(-time.Hour)})
	e.aliceOn("gh", store.RoleMaintainer)
	c := e.aliceSession()

	got := getJSON[CIJobNamesView](e, c, "/api/repositories/gh/acme/widgets/ci-job-names")
	if !slices.Equal(got.Names, []string{"build", "test (1.22)"}) {
		t.Errorf("names = %v; want what the reader shows", got.Names)
	}
	rd.mu.Lock()
	tg := rd.target
	rd.mu.Unlock()
	if tg.Project != "acme/widgets" || tg.ProjectID != testID("acme/widgets") ||
		!slices.Equal(tg.RecentHeads, []string{head('b'), head('a')}) {
		t.Errorf("target = %+v; want the project and the heads of its runs, newest first", tg)
	}

	// No CI reader on the instance: an empty list, never null.
	rec := e.get(c, "/api/repositories/main/acme/widgets/ci-job-names")
	wantCode(t, rec, "no reader", http.StatusOK)
	if body := strings.TrimSpace(rec.Body.String()); body != `{"names":[]}` {
		t.Errorf("no reader body = %s; want {\"names\":[]}", body)
	}

	// A reader that fails: 502, with nothing of its error.
	rd.mu.Lock()
	rd.err = errors.New("github: secret-detail")
	rd.mu.Unlock()
	rec = e.get(c, "/api/repositories/gh/acme/widgets/ci-job-names")
	wantCode(t, rec, "a failing reader", http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "secret-detail") {
		t.Errorf("body = %s; leaks the platform's error", rec.Body)
	}

	// A viewer may not ask.
	v := newEnv(t, nil)
	v.sources.Set("gh", &ciSource{fakeSource: newFakeSource(), names: []string{"build"}})
	v.addRun(runFix{instance: "gh", project: "acme/widgets", number: 1, head: head('a')})
	v.aliceOn("gh", store.RoleViewer)
	wantCode(t, v.get(v.aliceSession(), "/api/repositories/gh/acme/widgets/ci-job-names"), "viewer", http.StatusForbidden)
}

// A run shows the snapshot of its CI jobs and when it stops waiting, and no
// text of any job.
func TestRunViewCIJobs(t *testing.T) {
	e := newEnv(t, nil)
	id := e.addRun(runFix{project: "acme/widgets", number: 1, head: head('a'), result: cleanResult(head('a')), outcome: OutcomeCI,
		reason: "CI blocks approval: test failed", status: "failed"})
	plain := e.addRun(runFix{project: "acme/widgets", number: 2, head: head('b'), result: cleanResult(head('b')), outcome: OutcomeApproved, approved: true})
	wait := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('c'), state: store.Waiting, undispatched: true, outcome: OutcomeWaiting, skipReason: store.WaitCIPending})
	jobs := []store.CIJobResult{
		{Name: "test", Result: "failed", Read: "log", Size: 2048, Cut: true, Blocks: true},
		{Name: "scan", Result: "unreadable", Reason: "no permission", Read: "report", Blocks: false},
	}
	if err := e.st.SetRunCI(ctx, id, jobs); err != nil {
		t.Fatal(err)
	}
	deadline := e.clock.now().Add(20 * time.Minute)
	e.exec(`UPDATE runs SET ci_deadline = $2 WHERE id = $1::uuid`, wait, deadline)
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[RunDetailView](e, c, "/api/runs/"+id)
	if len(v.CIJobs) != 2 || v.CIJobs[0] != (CIJobResultView{Name: "test", Result: "failed", Read: "log", Size: 2048, Cut: true, Blocks: true}) ||
		v.CIJobs[1].Reason != "no permission" || v.CIJobs[1].Result != "unreadable" {
		t.Errorf("ciJobs = %+v; want the snapshot", v.CIJobs)
	}
	if v.CIDeadline != nil {
		t.Errorf("ciDeadline = %v; want null for a run that isn't waiting", v.CIDeadline)
	}
	var m map[string]any
	if err := json.Unmarshal(e.get(c, "/api/runs/"+id).Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, j := range m["ciJobs"].([]any) {
		for k := range j.(map[string]any) {
			if !slices.Contains([]string{"name", "result", "reason", "read", "size", "cut", "blocks"}, k) {
				t.Errorf("a CI job has the key %q; want the snapshot's keys only, no CI text", k)
			}
		}
	}

	raw := e.get(c, "/api/runs/"+plain).Body.String()
	if !strings.Contains(raw, `"ciJobs":null`) || !strings.Contains(raw, `"ciDeadline":null`) {
		t.Errorf("a run with no CI = %s; want ciJobs and ciDeadline null", raw)
	}

	w := getJSON[RunDetailView](e, c, "/api/runs/"+wait)
	if w.Run.Reason != "Waiting for CI." || w.CIDeadline == nil || !w.CIDeadline.Equal(deadline) {
		t.Errorf("waiting run = %q, deadline %v; want Waiting for CI. until %v", w.Run.Reason, w.CIDeadline, deadline)
	}
}
