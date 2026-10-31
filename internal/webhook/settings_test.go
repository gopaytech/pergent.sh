package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("PERGENT_SH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PERGENT_SH_TEST_DATABASE_URL is not set")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.OpenInSchema(ctx, url, "test_webhook_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		if err := st.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// dashboardConfig has the instances of both rigs and no config repositories.
func dashboardConfig() *config.Config {
	return &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
		GitHub: []config.GitHubInstance{{Name: "gh"}},
	}
}

// dashboardRig serves both webhooks with repositories looked up in repos.
func dashboardRig(t *testing.T, repos Repositories) *rig {
	t.Helper()
	r := &rig{q: &fakeQueue{created: true, republished: true}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	cfg := dashboardConfig()
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/gitlab/{instance}", &Handler{
		Config: cfg, Instances: instances.New(cfg, nil, nil), Secrets: syncmap.New(map[string]string{"main": testSecret}), Queue: r.q, Repos: repos,
		Wake: func() { r.wakes++ }, Log: log,
	})
	r.mux.Handle("/webhooks/github/{instance}", &GitHubHandler{
		Config: cfg, Instances: instances.New(cfg, nil, nil), Secrets: syncmap.New(map[string]string{"gh": ghSecret}), Queue: r.q, Repos: repos,
		Wake: func() { r.wakes++ }, Log: log,
	})
	return r
}

func TestWebhookAcceptsDashboardRepo(t *testing.T) {
	st := newStore(t)
	for _, row := range []store.DashboardRepo{
		{Instance: "main", Project: "Acme/Widgets", CreatedBy: "alice"},
		{Instance: "gh", Project: "Acme/Widgets", CreatedBy: "alice"},
	} {
		if err := st.UpsertDashboardRepo(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	cfg := dashboardConfig()
	repos := &settings.Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}

	t.Run("gitlab", func(t *testing.T) {
		r := dashboardRig(t, repos)
		wantStatus(t, r.hook(fixture(t, "mr_open.json")), 200)
		want := enqueueCall{store.MRKey{Instance: "main", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
		if len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
			t.Fatalf("enqueues = %+v, want [%+v]", r.q.enqueues, want)
		}
	})
	t.Run("github", func(t *testing.T) {
		r := dashboardRig(t, repos)
		wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
		want := enqueueCall{store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
		if len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
			t.Fatalf("enqueues = %+v, want [%+v]", r.q.enqueues, want)
		}
	})
	t.Run("not switched on", func(t *testing.T) {
		if err := st.DeleteDashboardRepo(context.Background(), "main", "acme/widgets"); err != nil {
			t.Fatal(err)
		}
		r := dashboardRig(t, repos)
		wantStatus(t, r.hook(fixture(t, "mr_open.json")), 200)
		noQueueCalls(t, r)
	})
}

type failingRepos struct{}

func (failingRepos) Get(context.Context, string, string) (settings.Effective, bool, error) {
	return settings.Effective{}, false, errors.New("database is down")
}

// A repository that can't be looked up is a storage error, so the platform
// delivers the event again; it is never taken for a repository switched off.
func TestWebhookRepositoryLookupFails(t *testing.T) {
	r := dashboardRig(t, failingRepos{})
	wantStatus(t, r.hook(fixture(t, "mr_open.json")), http.StatusInternalServerError)
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), http.StatusInternalServerError)
	noQueueCalls(t, r)
}
