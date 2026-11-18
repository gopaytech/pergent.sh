package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

const ciHead = "0123456789abcdef0123456789abcdef01234567"

type ciCall struct {
	instance, project, head string
	iid                     int64
}

type fakeCI struct {
	mu    sync.Mutex
	calls []ciCall
}

func (f *fakeCI) CIChanged(instance, project, head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ciCall{instance: instance, project: project, head: head})
}

func (f *fakeCI) CIChangedMR(instance, project string, iid int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ciCall{instance: instance, project: project, iid: iid})
}

// ciRepos answers for one repository, with or without ci_jobs.
type ciRepos struct {
	platform, instance, project string
	jobs                        []store.CIJob
}

func (c ciRepos) Get(_ context.Context, instance, project string) (settings.Effective, bool, error) {
	if instance != c.instance || !strings.EqualFold(project, c.project) {
		return settings.Effective{}, false, nil
	}
	return settings.Effective{Platform: c.platform, Instance: instance, Project: c.project,
		CIJobs: store.CIJobs{WaitSeconds: 1800, Jobs: c.jobs}}, true, nil
}

var someJobs = []store.CIJob{{Name: "test", Read: "status", BlocksApproval: true}}

func gitlabCIRig(jobs []store.CIJob, ci CINotifier) http.Handler {
	cfg := &config.Config{GitLab: []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}}}
	return &Handler{
		Config: cfg, Instances: instances.New(cfg, nil, nil),
		Secrets: syncmap.New(map[string]string{"main": testSecret}), Queue: &fakeQueue{},
		Repos: ciRepos{config.PlatformGitLab, "main", "Acme/Widgets", jobs}, CI: ci,
		Log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
	}
}

func postGitLab(h http.Handler, event, token, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", strings.NewReader(body))
	req.SetPathValue("instance", "main")
	req.Header.Set("X-Gitlab-Event", event)
	req.Header.Set("X-Gitlab-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestPipelineHookNotifies(t *testing.T) {
	body := fmt.Sprintf(`{"object_kind":"pipeline","project":{"id":42,"path_with_namespace":"Acme/Widgets"},"object_attributes":{"sha":%q}}`, ciHead)
	ci := &fakeCI{}
	if code := postGitLab(gitlabCIRig(someJobs, ci), "Pipeline Hook", testSecret, body); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(ci.calls) != 1 || ci.calls[0] != (ciCall{instance: "main", project: "Acme/Widgets", head: ciHead}) {
		t.Fatalf("calls = %+v", ci.calls)
	}
	// No ci_jobs, no call.
	ci = &fakeCI{}
	postGitLab(gitlabCIRig(nil, ci), "Pipeline Hook", testSecret, body)
	// An unknown project is ignored.
	other := strings.Replace(body, "Acme/Widgets", "Other/Thing", 1)
	postGitLab(gitlabCIRig(someJobs, ci), "Pipeline Hook", testSecret, other)
	if len(ci.calls) != 0 {
		t.Fatalf("calls = %+v, want none", ci.calls)
	}
	// A bad token is 401 as today.
	if code := postGitLab(gitlabCIRig(someJobs, ci), "Pipeline Hook", "wrong", body); code != 401 {
		t.Fatalf("bad token: status %d, want 401", code)
	}
	if len(ci.calls) != 0 {
		t.Fatalf("bad token notified: %+v", ci.calls)
	}
}

func TestJobHookNotifies(t *testing.T) {
	body := fmt.Sprintf(`{"object_kind":"build","sha":%q,"project_id":42,"project":{"id":42,"path_with_namespace":"Acme/Widgets"}}`, ciHead)
	ci := &fakeCI{}
	if code := postGitLab(gitlabCIRig(someJobs, ci), "Job Hook", testSecret, body); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(ci.calls) != 1 || ci.calls[0] != (ciCall{instance: "main", project: "Acme/Widgets", head: ciHead}) {
		t.Fatalf("calls = %+v", ci.calls)
	}
	ci = &fakeCI{}
	postGitLab(gitlabCIRig(nil, ci), "Job Hook", testSecret, body)
	if len(ci.calls) != 0 {
		t.Fatalf("no ci_jobs notified: %+v", ci.calls)
	}
	// Without a path the project id finds it through Projects.
	noPath := fmt.Sprintf(`{"object_kind":"build","sha":%q,"project_id":42}`, ciHead)
	h := gitlabCIRig(someJobs, ci).(*Handler)
	h.Projects = fakeProjects{}
	if code := postGitLab(h, "Job Hook", testSecret, noPath); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(ci.calls) != 1 || ci.calls[0].project != "Acme/Widgets" {
		t.Fatalf("calls = %+v", ci.calls)
	}
	// No notifier: ignored.
	if code := postGitLab(gitlabCIRig(someJobs, nil), "Job Hook", testSecret, body); code != 200 {
		t.Fatalf("nil CI: status %d", code)
	}
}

type fakeProjects struct{}

func (fakeProjects) Project(_ context.Context, _ string, id int64) (store.Project, bool, error) {
	return store.Project{Path: "Acme/Widgets"}, id == 42, nil
}

func githubCIRig(jobs []store.CIJob, ci CINotifier) http.Handler {
	cfg := &config.Config{GitHub: []config.GitHubInstance{{Name: "gh",
		ReviewApp: config.App{AppID: 111}, ApproveApp: &config.App{AppID: 222}}}}
	return &GitHubHandler{
		Config: cfg, Instances: instances.New(cfg, nil, nil),
		Secrets: syncmap.New(map[string]string{"gh": ghSecret}), Queue: &fakeQueue{},
		Repos: ciRepos{config.PlatformGitHub, "gh", "Acme/Widgets", jobs}, CI: ci,
		Log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
	}
}

func postCheckRun(h http.Handler, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", strings.NewReader(body))
	req.SetPathValue("instance", "gh")
	req.Header.Set("X-GitHub-Event", "check_run")
	req.Header.Set("X-Hub-Signature-256", sign(ghSecret, []byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestGitHubCheckRunNotifies(t *testing.T) {
	// A fork: pull_requests is empty, and the event still counts.
	body := func(app int64) string {
		return fmt.Sprintf(`{"action":"completed","repository":{"full_name":"Acme/Widgets"},"check_run":{"head_sha":%q,"pull_requests":[],"app":{"id":%d}}}`, ciHead, app)
	}
	ci := &fakeCI{}
	if code := postCheckRun(githubCIRig(someJobs, ci), body(999)); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(ci.calls) != 1 || ci.calls[0] != (ciCall{instance: "gh", project: "Acme/Widgets", head: ciHead}) {
		t.Fatalf("calls = %+v", ci.calls)
	}
	for _, app := range []int64{111, 222} {
		ci = &fakeCI{}
		postCheckRun(githubCIRig(someJobs, ci), body(app))
		if len(ci.calls) != 0 {
			t.Fatalf("own app %d notified: %+v", app, ci.calls)
		}
	}
	ci = &fakeCI{}
	postCheckRun(githubCIRig(nil, ci), body(999))
	if len(ci.calls) != 0 {
		t.Fatalf("no ci_jobs notified: %+v", ci.calls)
	}
}

// A merged-results pipeline carries the merge commit's sha: the event names
// the merge request instead.
func TestMergedResultsEventsNotifyByMR(t *testing.T) {
	pipe := fmt.Sprintf(`{"object_kind":"pipeline","project":{"id":42,"path_with_namespace":"Acme/Widgets"},"object_attributes":{"sha":%q,"ref":"refs/merge-requests/12/merge"}}`, ciHead)
	job := fmt.Sprintf(`{"object_kind":"build","sha":%q,"ref":"refs/merge-requests/9/head","project":{"id":42,"path_with_namespace":"Acme/Widgets"}}`, ciHead)
	for event, tc := range map[string]struct {
		body string
		iid  int64
	}{"Pipeline Hook": {pipe, 12}, "Job Hook": {job, 9}} {
		ci := &fakeCI{}
		if code := postGitLab(gitlabCIRig(someJobs, ci), event, testSecret, tc.body); code != 200 {
			t.Fatalf("%s: status %d", event, code)
		}
		if len(ci.calls) != 1 || ci.calls[0] != (ciCall{instance: "main", project: "Acme/Widgets", iid: tc.iid}) {
			t.Fatalf("%s: calls = %+v", event, ci.calls)
		}
	}
	// A branch ref keeps the sha path.
	ci := &fakeCI{}
	postGitLab(gitlabCIRig(someJobs, ci), "Pipeline Hook", testSecret, strings.Replace(pipe, "refs/merge-requests/12/merge", "feature", 1))
	if len(ci.calls) != 1 || ci.calls[0].head != ciHead || ci.calls[0].iid != 0 {
		t.Fatalf("calls = %+v", ci.calls)
	}
}

// A dashboard GitHub instance's own Apps are ignored too.
func TestGitHubCheckRunIgnoresDashboardOwnApps(t *testing.T) {
	cfg := &config.Config{}
	reg := instances.New(cfg, nil, nil)
	reg.Put(instances.FromRow(store.InstanceRow{Name: "gh", Platform: config.PlatformGitHub, ReviewAppID: 111, ApproveAppID: 222}))
	ci := &fakeCI{}
	h := &GitHubHandler{Config: cfg, Instances: reg, Secrets: syncmap.New(map[string]string{"gh": ghSecret}),
		Queue: &fakeQueue{}, Repos: ciRepos{config.PlatformGitHub, "gh", "Acme/Widgets", someJobs}, CI: ci,
		Log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil))}
	for _, app := range []int64{111, 222} {
		postCheckRun(h, fmt.Sprintf(`{"repository":{"full_name":"Acme/Widgets"},"check_run":{"head_sha":%q,"app":{"id":%d}}}`, ciHead, app))
	}
	if len(ci.calls) != 0 {
		t.Fatalf("own apps notified: %+v", ci.calls)
	}
	postCheckRun(h, fmt.Sprintf(`{"repository":{"full_name":"Acme/Widgets"},"check_run":{"head_sha":%q,"app":{"id":5}}}`, ciHead))
	if len(ci.calls) != 1 {
		t.Fatalf("calls = %+v", ci.calls)
	}
}

// A merge request pipeline's own merge_request.iid decides, whatever the ref.
func TestPipelineHookMergeRequestIID(t *testing.T) {
	withMR := fmt.Sprintf(`{"object_kind":"pipeline","project":{"id":42,"path_with_namespace":"Acme/Widgets"},"merge_request":{"iid":7},"object_attributes":{"sha":%q,"ref":"feature"}}`, ciHead)
	ci := &fakeCI{}
	postGitLab(gitlabCIRig(someJobs, ci), "Pipeline Hook", testSecret, withMR)
	if len(ci.calls) != 1 || ci.calls[0] != (ciCall{instance: "main", project: "Acme/Widgets", iid: 7}) {
		t.Fatalf("calls = %+v, want one by MR 7", ci.calls)
	}
	ci = &fakeCI{}
	without := strings.Replace(withMR, `"merge_request":{"iid":7},`, "", 1)
	postGitLab(gitlabCIRig(someJobs, ci), "Pipeline Hook", testSecret, without)
	if len(ci.calls) != 1 || ci.calls[0] != (ciCall{instance: "main", project: "Acme/Widgets", head: ciHead}) {
		t.Fatalf("calls = %+v, want one by sha", ci.calls)
	}
}
