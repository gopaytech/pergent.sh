package dashboard

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// fakeGraphs records what the switch asked the engine for.
type fakeGraphs struct {
	on bool

	mu      sync.Mutex
	wanted  []string
	dropped []string
}

func (f *fakeGraphs) GraphsOn() bool { return f.on }
func (f *fakeGraphs) WantGraph(instance, project string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wanted = append(f.wanted, instance+"/"+project)
}
func (f *fakeGraphs) DropGraph(_ context.Context, instance, project string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, instance+"/"+project)
}

// A maintainer turns the switch on and off: on asks for a head check, off
// deletes the graph, each change has its audit entry, the same value again
// none. The detail view carries the switch and the stored graph's status.
func TestGraphSwitch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	g := &fakeGraphs{on: true}
	e.d.graphs = g
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.aliceOn("main", store.RoleMaintainer)
	c := e.aliceSession()

	detail := decode[RepositoryDetailView](t, e.get(c, "/api/repositories/main/acme/gadgets"), "detail", http.StatusOK)
	if detail.Graph.Enabled || !detail.Graph.Available || detail.Graph.Commit != "" {
		t.Errorf("before = %+v", detail.Graph)
	}

	detail = decode[RepositoryDetailView](t, e.send(c, "PUT", "/api/repositories/main/acme/gadgets/graph", enabledBody(true)), "on", http.StatusOK)
	if !detail.Graph.Enabled {
		t.Errorf("after on = %+v", detail.Graph)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); !r.GraphContext {
		t.Errorf("row = %+v; want the switch on", r)
	}
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/ACME/gadgets/graph", enabledBody(true)), "on again", http.StatusOK)

	// A stored graph shows in the view.
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if _, err := e.st.TakeGraphCheck(ctx, "main", "acme/gadgets", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.QueueGraphBuild(ctx, "main", "acme/gadgets", "main", "abc", now); err != nil {
		t.Fatal(err)
	}
	detail = decode[RepositoryDetailView](t, e.get(c, "/api/repositories/main/acme/gadgets"), "detail", http.StatusOK)
	if detail.Graph.Building != "queued" || detail.Graph.Commit != "" {
		t.Errorf("queued = %+v", detail.Graph)
	}

	detail = decode[RepositoryDetailView](t, e.send(c, "PUT", "/api/repositories/main/acme/gadgets/graph", enabledBody(false)), "off", http.StatusOK)
	if detail.Graph.Enabled || detail.Graph.Building != "" {
		t.Errorf("after off = %+v", detail.Graph)
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"graph_on", "graph_off"}) {
		t.Errorf("audit = %v", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Equal(g.wanted, []string{"main/acme/gadgets", "main/acme/gadgets"}) || !slices.Equal(g.dropped, []string{"main/acme/gadgets"}) {
		t.Errorf("wanted %v, dropped %v", g.wanted, g.dropped)
	}
}

// Turning it on is 409 when the install keeps no graphs; off still works.
// A viewer is 403, a config repository 409, a bad body 400.
func TestGraphSwitchRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	admin := e.userOnMain(true)
	for _, g := range []Graphs{nil, &fakeGraphs{on: false}} {
		e.d.graphs = g
		rec := e.send(admin, "PUT", "/api/repositories/main/acme/gadgets/graph", enabledBody(true))
		wantCode(t, rec, "on without graphs", http.StatusConflict)
		wantCode(t, e.send(admin, "PUT", "/api/repositories/main/acme/gadgets/graph", enabledBody(false)), "off", http.StatusOK)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.GraphContext {
		t.Errorf("row = %+v; want off", r)
	}
	e.d.graphs = &fakeGraphs{on: true}
	wantCode(t, e.send(admin, "PUT", "/api/repositories/main/acme/widgets/graph", enabledBody(true)), "config repository", http.StatusConflict)
	for _, body := range []string{`{}`, `{"enabled": "yes"}`, `{"enabled": true, "x": 1}`} {
		wantCode(t, e.send(admin, "PUT", "/api/repositories/main/acme/gadgets/graph", body), body, http.StatusBadRequest)
	}

	v := newEnv(t, nil)
	v.d.graphs = &fakeGraphs{on: true}
	v.dashboardRepo("main", "acme/gadgets", nil)
	v.aliceOn("main", store.RoleViewer)
	wantCode(t, v.send(v.aliceSession(), "PUT", "/api/repositories/main/acme/gadgets/graph", enabledBody(true)), "viewer", http.StatusForbidden)
	if r, _ := v.storedRepo("main", "acme/gadgets"); r.GraphContext {
		t.Errorf("a viewer turned it on")
	}
}
