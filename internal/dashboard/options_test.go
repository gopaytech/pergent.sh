package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
)

// optionsDashboard builds a dashboard without a store: GET /api/sign-in-options
// answers without a session or a database.
func optionsDashboard(t *testing.T) http.Handler {
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

// The sign-in pages ask what to offer: the password form of the local
// admins, and no instance, whatever the config or the dashboard holds.
func TestSignInOptionsOfferOnlyThePasswordForm(t *testing.T) {
	h := optionsDashboard(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sign-in-options", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/sign-in-options without a session = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"password": true, "jwt": false}); !reflect.DeepEqual(got, want) {
		t.Errorf("answer = %v, want %v: the password form, and nothing else", got, want)
	}
	body := rec.Body.String()
	for _, secret := range []string{"gl-client-id", "gl-client-secret", "lab-client", "gitlab.example.com", "api.github.com", "PERGENT_TEST"} {
		if strings.Contains(body, secret) {
			t.Errorf("GET /api/sign-in-options shows %q: %s", secret, body)
		}
	}
}

func TestSignInOptionsIsGetOnly(t *testing.T) {
	h := optionsDashboard(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sign-in-options", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("POST /api/sign-in-options = 200, want an error")
	}
}

// In JWT mode a proxy signs people in: there is no button, and the page says
// so.
func TestSignInOptionsInJWTMode(t *testing.T) {
	j := newJWTHarness(t, nil)
	rec := j.serve(j.h, httptest.NewRequest("GET", "/api/sign-in-options", nil))
	var got map[string]any
	decodeInto(t, rec, &got)
	if want := (map[string]any{"password": true, "jwt": true}); rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Errorf("GET /api/sign-in-options in jwt mode = %d %v, want %v", rec.Code, got, want)
	}
	// No address, issuer or header name leaks.
	for _, s := range []string{j.f.url(), jwtHeader, jwtAudience} {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("the options show %q: %s", s, rec.Body)
		}
	}
}

// /api/me names the account and how it signed in, and nothing of a platform
// account, an instance or an "operator".
func TestMeViewHasNoPlatformFields(t *testing.T) {
	data, err := json.Marshal(meView{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"admin", "csrfToken", "email", "kind", "mustChangePassword", "role", "signIn", "username", "waiting", "waitingUsers"}
	if !slices.Equal(keys, want) {
		t.Errorf("/api/me keys = %v, want %v", keys, want)
	}
}
