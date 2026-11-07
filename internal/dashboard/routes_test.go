package dashboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// adminRoutesOf builds a dashboard with no store and returns the patterns
// Handler registered through adminRoute.
func adminRoutesOf(t *testing.T) []string {
	t.Helper()
	cfg := &config.Config{Dashboard: config.Dashboard{URL: dashboardURL}}
	d, err := New(Options{Config: cfg, SecretKey: testKey(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Handler()
	return slices.Clone(d.adminRoutes)
}

// The admin API is registered one way, so a route cannot skip the check: the
// patterns adminRoute recorded are the ones the router scan reads as admin
// (the scan reads the source, so a route registered another way with an admin
// handler shows as a session route and the two lists differ), and
// Auth.RequireAdmin is named nowhere but in adminRoute.
func TestAdminRoutesAreRegisteredOnlyThroughTheHelper(t *testing.T) {
	registered := adminRoutesOf(t)
	if len(registered) < 30 || !slices.Contains(registered, "GET /api/users") || !slices.Contains(registered, "GET /api/repositories/available") {
		t.Fatalf("adminRoute recorded %d routes: %v", len(registered), registered)
	}
	sort.Strings(registered)
	for i := 1; i < len(registered); i++ {
		if registered[i] == registered[i-1] {
			t.Errorf("%s is registered twice", registered[i])
		}
	}
	var scanned []string
	for _, r := range scanRoutes(t) {
		if r.Access == "admin" {
			scanned = append(scanned, r.Pattern)
		}
	}
	sort.Strings(scanned)
	if !slices.Equal(registered, scanned) {
		t.Errorf("adminRoute registered\n%v\nbut the source scan reads these as admin routes\n%v", registered, scanned)
	}

	// The spec names the admin routes too, by hand. One it calls admin that the
	// helper did not register is an admin handler behind a plain session.
	var specAdmin []string
	for _, r := range specRoutes(t, loadSpec(t)) {
		if r.Access == "admin" {
			specAdmin = append(specAdmin, r.Key)
		}
	}
	sort.Strings(specAdmin)
	if !slices.Equal(registered, specAdmin) {
		t.Errorf("adminRoute registered\n%v\nbut openapi.yaml calls these admin routes\n%v", registered, specAdmin)
	}

	// Nothing but adminRoute may use RequireAdmin, and nothing but Require and
	// RequireAdmin may call require.
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case sel.Sel.Name == "RequireAdmin" && fd.Name.Name != "adminRoute":
					t.Errorf("%s: %s uses RequireAdmin; register an admin route with adminRoute", fset.Position(sel.Pos()), fd.Name.Name)
				case sel.Sel.Name == "require" && fd.Name.Name != "Require" && fd.Name.Name != "RequireAdmin":
					t.Errorf("%s: %s calls require; use Require or adminRoute", fset.Position(sel.Pos()), fd.Name.Name)
				}
				return true
			})
		}
	}
}

var (
	wildcardRest = regexp.MustCompile(`\{[a-zA-Z]+\.\.\.\}`)
	wildcardOne  = regexp.MustCompile(`\{[a-zA-Z]+\}`)
)

// requestFor turns a registered pattern into a request that matches it.
func requestFor(pattern string) (method, target string) {
	method, target, _ = strings.Cut(pattern, " ")
	target = wildcardRest.ReplaceAllString(target, "x/1")
	target = wildcardOne.ReplaceAllString(target, "1")
	return method, target
}

func TestRequestFor(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /api/users":                                "GET /api/users",
		"DELETE /api/users/{id}":                        "DELETE /api/users/1",
		"PUT /api/admin/instances/{name}":               "PUT /api/admin/instances/1",
		"DELETE /api/repositories/{instance}/{rest...}": "DELETE /api/repositories/1/x/1",
	} {
		if m, p := requestFor(pattern); m+" "+p != want {
			t.Errorf("requestFor(%q) = %s %s, want %s", pattern, m, p, want)
		}
	}
}

// [CI] Review Focus 2: every admin API route is "not found" for a viewer, a
// maintainer and a user on no team, with the body an unknown path gets, and
// nothing runs. (A user on no team gets 403 "waiting" from an unknown path,
// since the waiting gate comes first there; for an admin route the answer is
// 404 all the same.) The walk is over the routes adminRoute recorded.
func TestEveryAdminRouteIsNotFoundForAUser(t *testing.T) {
	e := newEnv(t, nil)
	routes := slices.Clone(e.d.adminRoutes)
	if len(routes) < 30 {
		t.Fatalf("adminRoute recorded only %d routes: %v", len(routes), routes)
	}
	viewer := e.sessionFor(e.sso("viewer@example.com"))
	maintainer := e.sessionFor(e.sso("maintainer@example.com"))
	waiting := e.sessionFor(e.sso("waiting@example.com"))
	e.team("viewers", []store.TeamMember{{AccountID: e.accountOf(viewer), Role: store.RoleViewer}}, store.TeamRepo{Instance: "main"})
	e.team("maintainers", []store.TeamMember{{AccountID: e.accountOf(maintainer), Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})

	unknown := e.get(viewer, "/api/no-such-route")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("an unknown path = %d %s, want 404", unknown.Code, unknown.Body)
	}
	for name, c := range map[string]*http.Cookie{"viewer": viewer, "maintainer": maintainer, "user on no team": waiting} {
		for _, pattern := range routes {
			method, target := requestFor(pattern)
			var rec *httptest.ResponseRecorder
			if method == "GET" {
				rec = e.get(c, target)
			} else {
				rec = e.send(c, method, target, `{}`)
			}
			if rec.Code != http.StatusNotFound || rec.Body.String() != unknown.Body.String() {
				t.Errorf("%s as a %s = %d %s; want 404 %s", pattern, name, rec.Code, rec.Body, unknown.Body)
			}
		}
	}
	// Nothing was changed by them.
	if teams, _ := e.st.Teams(ctx); len(teams) != 2 {
		t.Errorf("%d teams after the walk, want the 2 the test made", len(teams))
	}
}
