package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
)

// Both webhook routes are registered; an unsigned request reaches the handler
// (401) before it touches the store, which is nil here.
func TestWebhookRoutesRegistered(t *testing.T) {
	mux := Routes(Deps{
		Config: &config.Config{
			GitLab: []config.Instance{{Name: "main"}},
			GitHub: []config.GitHubInstance{{Name: "gh"}},
		},
		WebhookSecrets:       map[string]string{"main": "gitlab-secret"},
		GitHubWebhookSecrets: map[string]string{"gh": "github-secret"},
	})
	for path, want := range map[string]int{
		"/webhooks/gitlab/main":  http.StatusUnauthorized,
		"/webhooks/github/gh":    http.StatusUnauthorized,
		"/webhooks/github/main":  http.StatusNotFound,
		"/webhooks/gitlab/gh":    http.StatusNotFound,
		"/webhooks/github/other": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != want {
			t.Errorf("POST %s = %d, want %d", path, rec.Code, want)
		}
	}
}
