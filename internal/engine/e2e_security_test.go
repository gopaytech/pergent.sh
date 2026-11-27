package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A security scan from the API to the store, through the routes main serves:
// a maintainer starts it on the head the platform gives; its one Job has the
// security label and no git token; the Job downloads the platform's archive of
// that commit with its token and posts its result; the tab counts the
// finding, the repository's quota counts the tokens, and a dismiss is audited.
// It is the one test of that wiring, so it is slower than the unit tests.
func TestSecurityScanEndToEnd(t *testing.T) {
	t.Parallel()
	d := newDashboardE2E(t)
	env := d.env
	secOn(env)
	env.Engine.Archives = func(instance string) (forge.ArchiveReader, bool) {
		p, ok := env.Engine.Platforms.Get(instance)
		if !ok {
			return nil, false
		}
		r, ok := p.Forge.(forge.ArchiveReader)
		return r, ok
	}
	env.GitLab.Handle(http.MethodGet, projectPath()+"/repository/archive.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sha") != sha('c') {
			http.Error(w, "wrong commit", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "gitlab-archive")
	})
	acc, err := env.Store.SignInSSOAccount(env.ctx, bobEmail, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	team, err := env.Store.CreateTeam(env.ctx, "Widgets", env.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := env.Store.UpdateTeam(env.ctx, team.ID, func(tm *store.Team) error {
		tm.Members = []store.TeamMember{{AccountID: acc.ID, Role: store.RoleMaintainer}}
		tm.Repos = []store.TeamRepo{{Instance: testInstance}}
		return nil
	}); err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	bob := d.signIn(t, bobEmail)
	api := "/api/repositories/" + testInstance + "/" + testProjectPath + "/security"

	status, body := d.write(t, bob, http.MethodPost, api+"/scans", map[string]any{})
	var started struct {
		Scan dashboard.SecurityScanView `json:"scan"`
	}
	if status != http.StatusAccepted || json.Unmarshal(body, &started) != nil {
		t.Fatalf("Start scan = %d %s\n%s", status, body, env.Logs())
	}
	if started.Scan.Commit != sha('c') || started.Scan.Branch != "main" || started.Scan.StartedBy != bobEmail {
		t.Errorf("started scan = %+v; want main at the platform's head", started.Scan)
	}
	id := started.Scan.ID

	secLaunch(env)
	jobs := secJobs(t, env)
	if len(jobs) != 1 || jobs[0].Labels[kube.LabelSecurityScan] != id {
		t.Fatalf("security Jobs = %d; want 1 labelled %s\n%s", len(jobs), id, env.Logs())
	}
	spec := jobs[0].Spec.Template.Spec
	secret := secSecret(t, env, id)
	if len(spec.InitContainers) != 0 || secret.Data["git-token"] != nil {
		t.Errorf("the scan Job has %d init containers and git-token %q; want no clone and no git token",
			len(spec.InitContainers), secret.Data["git-token"])
	}
	for k, v := range secret.Data {
		if strings.Contains(string(v), testDeployToken) {
			t.Errorf("the scan Job's secret %s holds the deploy token", k)
		}
	}
	for _, v := range spec.Containers[0].Env {
		if strings.Contains(v.Value, testDeployToken) {
			t.Errorf("the scan Job's env %s holds the deploy token", v.Name)
		}
	}
	token := string(secret.Data["result-token"])
	bearer := map[string]string{"Authorization": "Bearer " + token}

	status, body, header, _ := d.do(t, http.MethodGet, "/security-scans/"+id+"/source", nil, nil, bearer)
	if status != http.StatusOK || string(body) != "gitlab-archive" || header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("GET source = %d %q %q\n%s", status, body, header.Get("Content-Type"), env.Logs())
	}
	status, body, _, _ = d.do(t, http.MethodPost, "/security-scans/"+id+"/result", json.RawMessage(secBody(t, sha('c'), nil)), nil, bearer)
	if status != http.StatusNoContent {
		t.Fatalf("POST result = %d %s\n%s", status, body, env.Logs())
	}

	var tab dashboard.SecurityView
	d.getJSON(t, bob, api, &tab)
	if tab.Counts.High != 1 || tab.Last == nil || tab.Last.ID != id || tab.Last.State != store.SecurityComplete ||
		tab.Last.Tokens == nil || tab.Last.Tokens.Total != 1760 || tab.Running != nil || !tab.CanStart {
		t.Errorf("tab = %+v", tab)
	}
	if n, err := env.Store.TokensSince(env.ctx, testInstance, testProjectID, quota.MonthStart(env.Now())); err != nil || n != 1760 {
		t.Errorf("the repository's tokens this month = %d, %v; want the scan's 1760", n, err)
	}

	var findings dashboard.SecurityFindingsView
	d.getJSON(t, bob, api+"/findings", &findings)
	if len(findings.Findings) != 1 {
		t.Fatalf("findings = %+v", findings)
	}
	fid := strconv.FormatInt(findings.Findings[0].ID, 10)
	status, body = d.write(t, bob, http.MethodPost, api+"/findings/"+fid+"/dismiss",
		map[string]string{"reason": store.DismissFalsePositive, "comment": "test data"})
	if status != http.StatusOK {
		t.Fatalf("dismiss = %d %s", status, body)
	}
	entries, err := env.Store.AuditLog(env.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := len(entries) - 1; i >= 0; i-- {
		if a := entries[i]; strings.HasPrefix(a.Action, "security_") {
			got = append(got, a.Actor+" "+a.Action+" "+a.Project)
		}
	}
	want := []string{bobEmail + " security_scan_started " + testProjectPath, bobEmail + " security_finding_dismissed " + testProjectPath}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit = %q; want %q", got, want)
	}
}
