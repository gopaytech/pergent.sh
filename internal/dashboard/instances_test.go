package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
)

// instancesDashboard builds a dashboard without a store: GET /api/instances
// answers without a session or a database.
func instancesDashboard(t *testing.T) http.Handler {
	t.Helper()
	// An old config still names OAuth clients; nothing offers a sign-in
	// through them any more.
	t.Setenv("PERGENT_TEST_GL_ID", "gl-client-id")
	t.Setenv("PERGENT_TEST_GL_SECRET", "gl-client-secret-SECRET")
	cfg := &config.Config{
		GitLab: []config.Instance{
			{Name: "main", URL: "https://gitlab.example.com",
				OAuthClientIDEnv: "PERGENT_TEST_GL_ID", OAuthClientSecretEnv: "PERGENT_TEST_GL_SECRET"},
			{Name: "nooauth", URL: "https://gitlab2.example.com"},
		},
		GitHub:    []config.GitHubInstance{{Name: "gh", APIURL: "https://api.github.com"}},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	reg := instances.New(cfg, nil, nil)
	reg.Put(instances.Instance{Name: "lab", Platform: "gitlab", Source: instances.SourceDashboard,
		GitLab: &config.Instance{Name: "lab"}, OAuthClientID: "lab-client"})
	d, err := New(Options{Config: cfg, Instances: reg, SecretKey: testKey(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d.Handler()
}

// The sign-in page asks what to offer: the password form of the local
// admins, and no instance, whatever the config or the dashboard holds.
func TestInstancesOffersOnlyThePasswordForm(t *testing.T) {
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
	if !got.Password || got.Instances == nil || len(got.Instances) != 0 {
		t.Errorf("answer = %+v, want the password form and an empty list", got)
	}
	body := rec.Body.String()
	for _, secret := range []string{"gl-client-id", "gl-client-secret", "lab-client", "gitlab.example.com", "api.github.com", "PERGENT_TEST"} {
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
