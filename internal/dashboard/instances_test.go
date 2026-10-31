package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
)

// instancesDashboard builds a dashboard without a store: GET /api/instances
// reads only the registry's instances.
func instancesDashboard(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("PERGENT_TEST_GL_ID", glClientID)
	t.Setenv("PERGENT_TEST_GL_SECRET", glClientSecret)
	t.Setenv("PERGENT_TEST_GH_ID", ghClientID)
	t.Setenv("PERGENT_TEST_GH_SECRET", ghClientSecret)
	cfg := &config.Config{
		GitLab: []config.Instance{
			{Name: "main", URL: "https://gitlab.example.com",
				OAuthClientIDEnv: "PERGENT_TEST_GL_ID", OAuthClientSecretEnv: "PERGENT_TEST_GL_SECRET"},
			// No sign-in: not listed.
			{Name: "nooauth", URL: "https://gitlab2.example.com"},
		},
		GitHub: []config.GitHubInstance{{Name: "gh", APIURL: "https://api.github.com",
			OAuthClientIDEnv: "PERGENT_TEST_GH_ID", OAuthClientSecretEnv: "PERGENT_TEST_GH_SECRET"}},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	d, err := New(Options{Config: cfg, SecretKey: testKey(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d.Handler()
}

func TestInstancesListsSignInsWithoutSession(t *testing.T) {
	h := instancesDashboard(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/instances", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/instances without a session = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	var got struct {
		Password  bool           `json:"password"`
		Instances []InstanceView `json:"instances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []InstanceView{
		{Platform: "gitlab", Instance: "main", Label: "GitLab (main)"},
		{Platform: "github", Instance: "gh", Label: "GitHub (gh)"},
	}
	if !slices.Equal(got.Instances, want) {
		t.Errorf("instances = %+v, want %+v", got.Instances, want)
	}
	if !got.Password {
		t.Error("password = false, want true: the dashboard is on")
	}
	body := rec.Body.String()
	for _, secret := range []string{glClientID, glClientSecret, ghClientID, ghClientSecret,
		"gitlab.example.com", "api.github.com", "PERGENT_TEST"} {
		if strings.Contains(body, secret) {
			t.Errorf("GET /api/instances shows %q: %s", secret, body)
		}
	}
}

func TestInstancesIsGetOnly(t *testing.T) {
	h := instancesDashboard(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/instances", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("POST /api/instances = 200, want an error")
	}
}

// An instance added in the dashboard that has a client ID is listed after the
// config's of its platform; one being removed, or without a client ID, is not.
func TestInstancesListsDashboardInstances(t *testing.T) {
	cfg := &config.Config{
		GitLab:    []config.Instance{{Name: "main", URL: "https://gitlab.example.com", OAuthClientIDEnv: "A", OAuthClientSecretEnv: "B"}},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	reg := instances.New(cfg, nil, nil)
	lab := func(name string, oauth string, removing bool) instances.Instance {
		return instances.Instance{Name: name, Platform: "gitlab", Source: instances.SourceDashboard, Removing: removing,
			GitLab: &config.Instance{Name: name}, OAuthClientID: oauth}
	}
	reg.Put(lab("lab", "id", false))
	reg.Put(lab("quiet", "", false))
	reg.Put(lab("going", "id", true))
	d, err := New(Options{Config: cfg, Instances: reg, SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/instances", nil))
	var got struct {
		Instances []InstanceView `json:"instances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []InstanceView{
		{Platform: "gitlab", Instance: "main", Label: "GitLab (main)"},
		{Platform: "gitlab", Instance: "lab", Label: "GitLab (lab)"},
	}
	if !slices.Equal(got.Instances, want) {
		t.Errorf("instances = %+v, want %+v", got.Instances, want)
	}
}
