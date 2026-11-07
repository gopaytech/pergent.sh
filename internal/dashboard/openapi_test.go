package dashboard

// This file keeps openapi.yaml (repo root) in step with the code. It needs no
// database and starts no server:
//
//   - the routes come from the Go source (go/parser), so a route added in a
//     new file, or through a helper like admin("GET /api/x", h), is seen;
//   - the schemas named after Go types are compared with the types' json tags
//     by reflection;
//   - the document itself is checked for being valid enough to trust (3.1,
//     every $ref resolves, every operation has responses, path parameters
//     match their template).

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// --- loading the document ---

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func loadSpec(t *testing.T) specDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if root == nil {
		t.Fatal("openapi.yaml is empty")
	}
	return specDoc{root: root}
}

type specDoc struct{ root map[string]any }

func specMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func specList(v any) []any {
	l, _ := v.([]any)
	return l
}

func specString(v any) string {
	s, _ := v.(string)
	return s
}

// resolve follows a local $ref like "#/components/schemas/Name".
func (d specDoc) resolve(ref string) (map[string]any, bool) {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	var cur any = d.root
	for _, part := range strings.Split(rest, "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m := specMap(cur)
		if m == nil {
			return nil, false
		}
		next, ok := m[part]
		if !ok {
			return nil, false
		}
		cur = next
	}
	m := specMap(cur)
	return m, m != nil
}

func (d specDoc) schema(name string) map[string]any {
	return specMap(specMap(specMap(d.root["components"])["schemas"])[name])
}

// operations lists every operation of the paths object, in a stable order.
type specOp struct {
	Path, Method string
	Op           map[string]any
	PathItem     map[string]any
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func (d specDoc) operations() []specOp {
	var out []specOp
	paths := specMap(d.root["paths"])
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		item := specMap(paths[p])
		for _, m := range httpMethods {
			if op := specMap(item[m]); op != nil {
				out = append(out, specOp{Path: p, Method: strings.ToUpper(m), Op: op, PathItem: item})
			}
		}
	}
	return out
}

// --- the routes in the code ---

// Access levels a route can be behind in the router: nothing, a session
// (Auth.Require) or an admin (requireAdmin). The handler may check more (a
// repository role); the spec says so in prose.
var accessRank = map[string]int{"none": 0, "session": 1, "admin": 2}

type codeRoute struct {
	Pattern string // "GET /api/me", or "/api/" for a method-less one
	Access  string
	Where   string
}

func (r codeRoute) method() string {
	m, _, ok := strings.Cut(r.Pattern, " ")
	if !ok {
		return ""
	}
	return m
}

func (r codeRoute) path() string {
	if _, p, ok := strings.Cut(r.Pattern, " "); ok {
		return p
	}
	return r.Pattern
}

func maxAccess(a, b string) string {
	if accessRank[b] > accessRank[a] {
		return b
	}
	return a
}

// gateOf reads how a handler expression is gated: it finds Require and
// requireAdmin anywhere inside it.
func gateOf(nodes ...ast.Node) string {
	access := "none"
	for _, n := range nodes {
		if n == nil {
			continue
		}
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "Require" {
					access = maxAccess(access, "session")
				}
			case *ast.Ident:
				switch x.Name {
				case "requireAdmin":
					access = maxAccess(access, "admin")
				}
			}
			return true
		})
	}
	return access
}

type routeHelper struct {
	lit    *ast.FuncLit
	prefix string // "GET " for get := func(pattern ...) { mux.Handle("GET "+pattern, ...) }
	access string
}

type rangeVar struct {
	name       string
	values     []string
	start, end token.Pos
}

// muxVars returns the names one declaration binds to a mux: a *http.ServeMux
// parameter, or a variable set from http.NewServeMux.
func muxVars(decl ast.Decl) map[string]bool {
	isMuxType := func(e ast.Expr) bool {
		if star, ok := e.(*ast.StarExpr); ok {
			e = star.X
		}
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ServeMux" {
			return false
		}
		pkg, _ := sel.X.(*ast.Ident)
		return pkg != nil && pkg.Name == "http"
	}
	vars := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			if isMuxType(x.Type) {
				for _, name := range x.Names {
					vars[name.Name] = true
				}
			}
		case *ast.ValueSpec:
			if x.Type != nil && isMuxType(x.Type) {
				for _, name := range x.Names {
					vars[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
				return true
			}
			id, _ := x.Lhs[0].(*ast.Ident)
			call, _ := x.Rhs[0].(*ast.CallExpr)
			if id == nil || call == nil {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewServeMux" {
				if pkg, _ := sel.X.(*ast.Ident); pkg != nil && pkg.Name == "http" {
					vars[id.Name] = true
				}
			}
		}
		return true
	})
	return vars
}

func isMuxHandleCall(call *ast.CallExpr, muxes map[string]bool) (isHandle, isMux bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") || len(call.Args) != 2 {
		return false, false
	}
	// A log handler's Handle(ctx, record) is the one other shape that is
	// none of ours. Any other receiver has to be proved a mux, or the test
	// fails: a route on one it can't tell would be a hole in the check.
	if ctx, ok := call.Args[0].(*ast.Ident); ok && ctx.Name == "ctx" {
		return false, false
	}
	recv, _ := sel.X.(*ast.Ident)
	return true, recv != nil && (recv.Name == "http" || muxes[recv.Name])
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// scanRoutesInFile returns the routes one Go file registers on a mux, and the
// registrations it could not read (the test fails on those: a route that is
// invisible to the check would be a hole in it).
func scanRoutesInFile(fset *token.FileSet, file *ast.File) (routes []codeRoute, unresolved []string) {
	where := func(p token.Pos) string {
		pos := fset.Position(p)
		return fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
	}
	for _, decl := range file.Decls {
		muxes := muxVars(decl)
		helpers := map[string]*routeHelper{}
		var ranges []rangeVar
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
					return true
				}
				id, _ := x.Lhs[0].(*ast.Ident)
				lit, _ := x.Rhs[0].(*ast.FuncLit)
				if id == nil || lit == nil || len(lit.Type.Params.List) == 0 ||
					len(lit.Type.Params.List[0].Names) == 0 || lit.Type.Params.List[0].Names[0].Name != "pattern" {
					return true
				}
				h := &routeHelper{lit: lit, access: gateOf(lit.Body)}
				ast.Inspect(lit.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if isHandle, _ := isMuxHandleCall(call, muxes); !isHandle {
						return true
					}
					switch a := call.Args[0].(type) {
					case *ast.Ident:
						if a.Name == "pattern" {
							h.prefix = ""
							return true
						}
					case *ast.BinaryExpr:
						if pre, ok := stringLit(a.X); ok && a.Op == token.ADD {
							if id, _ := a.Y.(*ast.Ident); id != nil && id.Name == "pattern" {
								h.prefix = pre
								return true
							}
						}
					}
					unresolved = append(unresolved, where(call.Pos())+": a route helper builds its pattern in a way this test can't read")
					return true
				})
				helpers[id.Name] = h
			case *ast.RangeStmt:
				id, _ := x.Value.(*ast.Ident)
				comp, _ := x.X.(*ast.CompositeLit)
				if id == nil || comp == nil {
					return true
				}
				var values []string
				for _, e := range comp.Elts {
					s, ok := stringLit(e)
					if !ok {
						return true
					}
					values = append(values, s)
				}
				ranges = append(ranges, rangeVar{name: id.Name, values: values, start: x.Body.Pos(), end: x.Body.End()})
			}
			return true
		})

		insideHelper := func(p token.Pos) bool {
			for _, h := range helpers {
				if p >= h.lit.Pos() && p < h.lit.End() {
					return true
				}
			}
			return false
		}
		add := func(pattern, access string, p token.Pos) {
			routes = append(routes, codeRoute{Pattern: pattern, Access: access, Where: where(p)})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isHandle, isMux := isMuxHandleCall(call, muxes); isHandle {
				if !isMux {
					unresolved = append(unresolved, where(call.Pos())+": a Handle call on something this test can't prove is a *http.ServeMux")
					return true
				}
				if insideHelper(call.Pos()) {
					return true // read through the helper's callers
				}
				access := gateOf(call.Args[1])
				switch a := call.Args[0].(type) {
				case *ast.BasicLit:
					if s, ok := stringLit(a); ok {
						add(s, access, call.Pos())
						return true
					}
				case *ast.BinaryExpr:
					id, _ := a.X.(*ast.Ident)
					suffix, okLit := stringLit(a.Y)
					if id != nil && okLit && a.Op == token.ADD {
						for _, rv := range ranges {
							if rv.name == id.Name && call.Pos() >= rv.start && call.Pos() < rv.end {
								for _, v := range rv.values {
									add(v+suffix, access, call.Pos())
								}
								return true
							}
						}
					}
				}
				unresolved = append(unresolved, where(call.Pos())+": a route pattern this test can't read")
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				if h := helpers[id.Name]; h != nil && len(call.Args) > 0 {
					lit, ok := stringLit(call.Args[0])
					if !ok {
						unresolved = append(unresolved, where(call.Pos())+": "+id.Name+" is called with a pattern that is not a string literal")
						return true
					}
					var rest []ast.Node
					for _, a := range call.Args[1:] {
						rest = append(rest, a)
					}
					access := maxAccess(h.access, gateOf(rest...))
					add(h.prefix+lit, access, call.Pos())
				}
			}
			return true
		})
	}
	return routes, unresolved
}

// scanRoutes reads every non-test Go file under the repo's internal/ and cmd/
// folders, so a route in a new file or package is found without being listed.
func scanRoutes(t *testing.T) []codeRoute {
	t.Helper()
	root := repoRoot(t)
	var routes []codeRoute
	var unresolved []string
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			r, u := scanRoutesInFile(fset, f)
			routes = append(routes, r...)
			unresolved = append(unresolved, u...)
			return nil
		})
		if err != nil {
			t.Fatalf("reading the Go source: %v", err)
		}
	}
	for _, u := range unresolved {
		t.Errorf("unresolved route registration: %s", u)
	}
	return routes
}

// Routes that exist in the code and are deliberately not in the spec.
var (
	// Method-less patterns: the SPA and the catch-alls. "/" is the embedded
	// dashboard (not an API); "/api/" and "/auth/" answer what no route
	// does (401 without a session, else 404 JSON; the sign-in routes that
	// match first are in the spec).
	undocumentedCatchAlls = map[string]string{
		"/":      "the embedded SPA",
		"/api/":  "catch-all: 404 for an unknown API path",
		"/auth/": "catch-all in the server mux that hands /auth/ to the dashboard",
	}
	// Registered only so the mux answers 404 (JSON) instead of redirecting
	// the instance alone to the {rest...} routes. Same answer as any
	// unknown path under /api/.
	undocumentedPlaceholders = map[string]string{
		"GET /api/reviews/{instance}":         "404 placeholder",
		"POST /api/reviews/{instance}":        "404 placeholder",
		"GET /api/repositories/{instance}":    "404 placeholder",
		"POST /api/repositories/{instance}":   "404 placeholder",
		"PUT /api/repositories/{instance}":    "404 placeholder",
		"DELETE /api/repositories/{instance}": "404 placeholder",
	}
)

type specRoute struct {
	Key, Access, Where string
	Op                 specOp
}

// specRoutes maps each operation to the registered pattern it documents: the
// operation's own "METHOD /path", or its x-pergent-pattern when the pattern
// has a {rest...} wildcard that the spec splits into several paths.
func specRoutes(t *testing.T, d specDoc) []specRoute {
	t.Helper()
	var out []specRoute
	for _, op := range d.operations() {
		key := op.Method + " " + op.Path
		where := key
		if pat := specString(op.Op["x-pergent-pattern"]); pat != "" {
			key = pat
			m, p, _ := strings.Cut(pat, " ")
			prefix, _, wild := strings.Cut(p, "{rest...}")
			switch {
			case m != op.Method:
				t.Errorf("%s: x-pergent-pattern %q has another method", where, pat)
			case !wild:
				t.Errorf("%s: x-pergent-pattern is for patterns with a {rest...} wildcard; write this path literally", where)
			case !strings.HasPrefix(op.Path, prefix):
				t.Errorf("%s: path must start with %q (the pattern's literal part)", where, prefix)
			}
		}
		out = append(out, specRoute{Key: key, Access: specString(op.Op["x-pergent-access"]), Where: where, Op: op})
	}
	return out
}

func TestOpenAPIRoutesMatchCode(t *testing.T) {
	d := loadSpec(t)
	code := scanRoutes(t)

	// One entry per pattern. A pattern registered twice (the /api/ catch-all
	// is, once per mux) must agree with itself unless it's undocumented.
	codeByPattern := map[string]codeRoute{}
	for _, r := range code {
		if prev, ok := codeByPattern[r.Pattern]; ok {
			if prev.Access != r.Access {
				if _, skip := undocumentedCatchAlls[r.Pattern]; !skip {
					t.Errorf("%s is registered twice with different access (%s at %s, %s at %s)",
						r.Pattern, prev.Access, prev.Where, r.Access, r.Where)
				}
			}
			continue
		}
		codeByPattern[r.Pattern] = r
	}

	// The allow lists must not go stale.
	for p := range undocumentedCatchAlls {
		if _, ok := codeByPattern[p]; !ok {
			t.Errorf("undocumentedCatchAlls lists %q, which the code no longer registers", p)
		}
	}
	for p := range undocumentedPlaceholders {
		if _, ok := codeByPattern[p]; !ok {
			t.Errorf("undocumentedPlaceholders lists %q, which the code no longer registers", p)
		}
	}

	spec := specRoutes(t, d)
	inSpec := map[string]bool{}
	for _, s := range spec {
		inSpec[s.Key] = true
		r, ok := codeByPattern[s.Key]
		if !ok {
			t.Errorf("openapi.yaml documents %s, which the code does not register", s.Key)
			continue
		}
		if s.Access != r.Access {
			t.Errorf("%s: x-pergent-access is %q in openapi.yaml but the router gates it as %q (%s)", s.Where, s.Access, r.Access, r.Where)
		}
	}
	patterns := make([]string, 0, len(codeByPattern))
	for p := range codeByPattern {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		r := codeByPattern[p]
		if _, skip := undocumentedCatchAlls[p]; skip {
			continue
		}
		if _, skip := undocumentedPlaceholders[p]; skip {
			continue
		}
		if r.method() == "" {
			t.Errorf("%s (%s) is a method-less route: document it or add it to undocumentedCatchAlls", p, r.Where)
			continue
		}
		if !inSpec[p] {
			t.Errorf("%s is registered in the code (%s) but missing from openapi.yaml", p, r.Where)
		}
	}
}

// A {rest...} route is one registered pattern that its handler splits by the
// end of the path (".../inputs", ".../settings/reset"). The route check can't
// see inside a handler, so this reads the dashboard's source for the
// suffixes the handlers look for and wants each documented as a path ending.
func TestOpenAPIWildcardSubroutesAreDocumented(t *testing.T) {
	d := loadSpec(t)
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	isRest := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name == "rest"
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "PathValue" && len(x.Args) == 1 {
				v, _ := stringLit(x.Args[0])
				return v == "rest"
			}
		}
		return false
	}
	suffixes := map[string]string{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			// reviewAction takes the last path segment as action and
			// compares it: action != "rerun".
			if cmp, ok := n.(*ast.BinaryExpr); ok && (cmp.Op == token.EQL || cmp.Op == token.NEQ) {
				if id, _ := cmp.X.(*ast.Ident); id != nil && id.Name == "action" {
					if v, ok := stringLit(cmp.Y); ok {
						pos := fset.Position(cmp.Pos())
						suffixes["/"+v] = fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
					}
				}
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			where := fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
			switch {
			case (sel.Sel.Name == "CutSuffix" || sel.Sel.Name == "HasSuffix") && len(call.Args) == 2 && isRest(call.Args[0]):
				if v, ok := stringLit(call.Args[1]); ok && v != "" {
					suffixes[v] = where
				}
			case (sel.Sel.Name == "target" || sel.Sel.Name == "targetAny" || sel.Sel.Name == "targetRepo") && len(call.Args) >= 3:
				if v, ok := stringLit(call.Args[2]); ok && strings.HasPrefix(v, "/") {
					suffixes[v] = where
				}
			}
			return true
		})
	}
	if len(suffixes) < 6 {
		t.Fatalf("found only %d wildcard suffixes in the source (%v): the test no longer reads the handlers", len(suffixes), suffixes)
	}
	for _, s := range []string{"/rerun", "/dismiss", "/restore"} {
		if suffixes[s] == "" {
			t.Errorf("the review action %q was not found in the source: the test no longer reads reviewAction", s)
		}
	}
	var wild []specOp
	for _, op := range d.operations() {
		if specString(op.Op["x-pergent-pattern"]) != "" {
			wild = append(wild, op)
		}
	}
	ends := make([]string, 0, len(suffixes))
	for s := range suffixes {
		ends = append(ends, s)
	}
	sort.Strings(ends)
	for _, s := range ends {
		found := false
		for _, op := range wild {
			found = found || strings.HasSuffix(op.Path, s)
		}
		if !found {
			t.Errorf("a handler looks for the path ending %q (%s), but no operation of openapi.yaml with an x-pergent-pattern ends so", s, suffixes[s])
		}
	}
}

// The scanner is what the route check stands on: prove it reads the shapes
// pergent.sh registers routes with, and refuses what it can't read.
func TestOpenAPIRouteScanner(t *testing.T) {
	const src = `package x
import "net/http"
func (d *D) routes(mux *http.ServeMux) {
	mux.Handle("POST /a/{id}", h)
	mux.HandleFunc("GET /b", h)
	mux.Handle("/", h)
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, d.Auth.Require(h)) }
	get("/c", d.c)
	get("/d", requireAdmin(d.d))
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(requireAdmin(h))) }
	admin("PUT /e", d.e)
	admin2 := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(h)) }
	admin2("DELETE /f", d.f)
	for _, method := range []string{"PUT", "DELETE"} {
		mux.Handle(method+" /g", d.Auth.Require(none))
	}
	logger.Handle(ctx, rec)
}`
	routes, unresolved := scanSource(t, src)
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %v", unresolved)
	}
	got := map[string]string{}
	for _, r := range routes {
		got[r.Pattern] = r.Access
	}
	want := map[string]string{
		"POST /a/{id}": "none", "GET /b": "none", "/": "none",
		"GET /c": "session", "GET /d": "admin", "PUT /e": "admin", "DELETE /f": "session",
		"PUT /g": "session", "DELETE /g": "session",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}

	routes, unresolved = scanSource(t, `package x
func f(m *http.ServeMux) {
	m.Handle("GET /h", h)
	n := http.NewServeMux()
	n.Handle("GET /i", h)
	var o http.ServeMux
	o.HandleFunc("GET /j", h)
	http.Handle("GET /k", h)
}`)
	if len(unresolved) != 0 || len(routes) != 4 {
		t.Errorf("a mux is told by its type, not its name: routes %v, unresolved %v", routes, unresolved)
	}
	for name, src := range map[string]string{
		"a variable pattern on a mux of any name": `func f(m *http.ServeMux, p string) { m.Handle(p, h) }`,
		"a receiver not proved a mux":             `func f() { logger.Handle("GET /x", h) }`,
		"a mux held in a field":                   `func f(s *S) { s.mux.Handle("GET /x", h) }`,
		"a mux made by a function":                `func f() { m := newMux(); m.Handle("GET /x", h) }`,
	} {
		routes, unresolved = scanSource(t, "package x\n"+src)
		if len(unresolved) != 1 || len(routes) != 0 {
			t.Errorf("%s must be reported as unresolved: routes %v, unresolved %v", name, routes, unresolved)
		}
	}

	_, unresolved = scanSource(t, `package x
func f(mux *http.ServeMux, p string) {
	mux.Handle(p, h)
}`)
	if len(unresolved) != 1 {
		t.Errorf("a pattern held in a variable must be reported as unresolved, got %v", unresolved)
	}
	_, unresolved = scanSource(t, `package x
func f(mux *http.ServeMux, p string) {
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, h) }
	get(p, h)
}`)
	if len(unresolved) != 1 {
		t.Errorf("a helper called with a variable must be reported as unresolved, got %v", unresolved)
	}
}

func scanSource(t *testing.T, src string) ([]codeRoute, []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return scanRoutesInFile(fset, f)
}

// --- the schemas ---

// A schema named after a Go type. Response types are held to the exact
// properties, required set and nullability of their json tags; the types
// pergent and its Jobs send (Input) are held to the property names, as a
// reader of those is lenient about what it is sent.
type schemaType struct {
	Type  reflect.Type
	Input bool
}

func outputSchema(v any) schemaType { return schemaType{Type: reflect.TypeOf(v)} }
func inputSchema(v any) schemaType  { return schemaType{Type: reflect.TypeOf(v), Input: true} }

// openAPISchemas is every schema that has a Go type. Names are the type's name;
// a type outside this package is spelled with its package, as in
// "readiness.Checklist".
var openAPISchemas = map[string]schemaType{
	"meView":                 outputSchema(meView{}),
	"instancesView":          outputSchema(instancesView{}),
	"InstanceView":           outputSchema(InstanceView{}),
	"OverviewView":           outputSchema(OverviewView{}),
	"SetupView":              outputSchema(SetupView{}),
	"StatsView":              outputSchema(StatsView{}),
	"ReviewSummary":          outputSchema(ReviewSummary{}),
	"ReviewListView":         outputSchema(ReviewListView{}),
	"ReviewDetailView":       outputSchema(ReviewDetailView{}),
	"CanView":                outputSchema(CanView{}),
	"ReviewHeader":           outputSchema(ReviewHeader{}),
	"RunSummary":             outputSchema(RunSummary{}),
	"RunDetailView":          outputSchema(RunDetailView{}),
	"TokensView":             outputSchema(TokensView{}),
	"FilesView":              outputSchema(FilesView{}),
	"SkippedFile":            outputSchema(SkippedFile{}),
	"NotReviewedFile":        outputSchema(NotReviewedFile{}),
	"SkillView":              outputSchema(SkillView{}),
	"FindingView":            outputSchema(FindingView{}),
	"DismissedView":          outputSchema(DismissedView{}),
	"CheckView":              outputSchema(CheckView{}),
	"LogView":                outputSchema(LogView{}),
	"RepositoryView":         outputSchema(RepositoryView{}),
	"Last7View":              outputSchema(Last7View{}),
	"RepositoryDetailView":   outputSchema(RepositoryDetailView{}),
	"SettingView":            outputSchema(SettingView{}),
	"InputsView":             outputSchema(InputsView{}),
	"InputSkill":             outputSchema(InputSkill{}),
	"DetectedSkillView":      outputSchema(DetectedSkillView{}),
	"ReadinessView":          outputSchema(ReadinessView{}),
	"ScanView":               outputSchema(ScanView{}),
	"PastDismissalView":      outputSchema(PastDismissalView{}),
	"SkillResultView":        outputSchema(SkillResultView{}),
	"SkillSourceView":        outputSchema(SkillSourceView{}),
	"LibrarySkillView":       outputSchema(LibrarySkillView{}),
	"SkillVersionView":       outputSchema(SkillVersionView{}),
	"SkillDetailView":        outputSchema(SkillDetailView{}),
	"SkillVersionDetailView": outputSchema(SkillVersionDetailView{}),
	"SkillWrittenView":       outputSchema(SkillWrittenView{}),
	"SkillInUseView":         outputSchema(SkillInUseView{}),
	"SkillUserRepoView":      outputSchema(SkillUserRepoView{}),
	"SkillFetchStartedView":  outputSchema(SkillFetchStartedView{}),
	"SkillFetchView":         outputSchema(SkillFetchView{}),
	"FetchedSkillView":       outputSchema(FetchedSkillView{}),
	"SkillUpdateView":        outputSchema(SkillUpdateView{}),
	"SkillFetchResultView":   outputSchema(SkillFetchResultView{}),
	"AttentionView":          outputSchema(AttentionView{}),
	"IntegrationView":        outputSchema(IntegrationView{}),
	"IntegrationRepo":        outputSchema(IntegrationRepo{}),
	"AdminSettingsView":      outputSchema(AdminSettingsView{}),
	"DefaultsView":           outputSchema(DefaultsView{}),
	"DefaultValues":          outputSchema(DefaultValues{}),
	"LLMView":                outputSchema(LLMView{}),
	"AvailableView":          outputSchema(AvailableView{}),
	"AvailableRepo":          outputSchema(AvailableRepo{}),
	"UserView":               outputSchema(UserView{}),
	"TeamRef":                outputSchema(TeamRef{}),
	"NewAccountView":         outputSchema(NewAccountView{}),
	"TeamView":               outputSchema(TeamView{}),
	"TeamMemberView":         outputSchema(TeamMemberView{}),
	"TeamRepoView":           outputSchema(TeamRepoView{}),
	"AuditPageView":          outputSchema(AuditPageView{}),
	"AuditView":              outputSchema(AuditView{}),
	"SecretView":             outputSchema(SecretView{}),
	"InstanceAdminView":      outputSchema(InstanceAdminView{}),
	"LLMAdminView":           outputSchema(LLMAdminView{}),
	"SignInAdminView":        outputSchema(SignInAdminView{}),
	"SignInTestView":         outputSchema(SignInTestView{}),
	"ssoView":                outputSchema(ssoView{}),
	"readiness.Checklist":    outputSchema(readiness.Checklist{}),
	"readiness.Section":      outputSchema(readiness.Section{}),
	"readiness.Item":         outputSchema(readiness.Item{}),
	"readiness.NoiseFile":    outputSchema(readiness.NoiseFile{}),
	"readiness.JobResult":    inputSchema(readiness.JobResult{}),
	"readiness.Context":      inputSchema(readiness.Context{}),
	"readiness.SkillFit":     inputSchema(readiness.SkillFit{}),
	"readiness.Language":     inputSchema(readiness.Language{}),
	"skillfetch.JobResult":   inputSchema(skillfetch.JobResult{}),
	"store.FetchedFile":      inputSchema(store.FetchedFile{}),
	"result.Result":          inputSchema(result.Result{}),
	"result.Incremental":     inputSchema(result.Incremental{}),
	"result.Skill":           inputSchema(result.Skill{}),
	"result.Finding":         inputSchema(result.Finding{}),
	"result.Failure":         inputSchema(result.Failure{}),
	"result.Summary":         inputSchema(result.Summary{}),
	"result.BatchChecks":     inputSchema(result.BatchChecks{}),
	"result.Check":           inputSchema(result.Check{}),
	"result.Files":           inputSchema(result.Files{}),
	"result.Skipped":         inputSchema(result.Skipped{}),
	"result.Error":           inputSchema(result.Error{}),
	"result.Comment":         inputSchema(result.Comment{}),
}

// Schemas with no Go type to compare with: the error body, and the request
// bodies the handlers decode into a struct declared inside the function.
var openAPIUntypedSchemas = map[string]string{
	"ErrorBody":                 "map[string]string{error, code}",
	"StatusBody":                "map[string]string{status}",
	"Outcome":                   "the Outcome* constants, spelled in the enum",
	"CSRFTokenBody":             "map[string]string{csrfToken}",
	"PasswordSignInRequest":     "struct in Auth.passwordSignIn",
	"PasswordChangeRequest":     "struct in Auth.changePassword",
	"SwitchOnRequest":           "struct in Dashboard.switchOn",
	"RerunRequest":              "struct in Dashboard.reviewAction",
	"DismissRequest":            "struct in Dashboard.reviewAction",
	"RestoreRequest":            "struct in Dashboard.reviewAction",
	"StopUsingRequest":          "struct in Dashboard.stopUsing",
	"ScanStartedBody":           "struct in Dashboard.scanNow",
	"ScanBusyBody":              "struct in Dashboard.scanNow",
	"ScanTooSoonBody":           "struct in Dashboard.scanNow",
	"CreateAccountRequest":      "struct in Dashboard.createPasswordAccount",
	"SetAccountRoleRequest":     "struct in Dashboard.setUserRole",
	"CreateTeamRequest":         "struct in Dashboard.createTeam",
	"UpdateTeamRequest":         "struct in Dashboard.updateTeam",
	"CreateInstanceRequest":     "struct in Dashboard.addInstance",
	"UpdateInstanceRequest":     "struct in Dashboard.updateInstance",
	"PutLLMRequest":             "struct in Dashboard.putLLM",
	"PutSignInRequest":          "putSignInBody, decoded in Dashboard.putSignIn",
	"TestSignInRequest":         "struct in Dashboard.testSignIn",
	"CreateSkillRequest":        "struct in Dashboard.createSkill",
	"UpdateSkillRequest":        "struct in Dashboard.updateSkill",
	"CopySkillRequest":          "struct in Dashboard.copySkill",
	"StartSkillFetchRequest":    "struct in Dashboard.startSkillFetch",
	"InstallSkillsRequest":      "struct in Dashboard.installSkills",
	"UpdateSkillsRequest":       "struct in Dashboard.updateSkills",
	"RepositorySettingsRequest": "settings object parsed by parseSettings (map of raw values)",
	"DefaultsRequest":           "settings object parsed by parseDefaults (map of raw values)",
}

// goJSONField is one JSON property of a Go struct.
type goJSONField struct {
	Name string
	Omit bool // omitempty or omitzero
	Type reflect.Type
}

// goJSONFields lists the properties encoding/json gives t: embedded structs
// are flattened, "-" and unexported fields are left out.
func goJSONFields(t reflect.Type) []goJSONField {
	var out []goJSONField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, hasTag := f.Tag.Lookup("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, goJSONFields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if !hasTag || name == "" {
			name = f.Name
		}
		out = append(out, goJSONField{
			Name: name, Type: f.Type,
			Omit: strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,"),
		})
	}
	return out
}

// props collects a schema's properties and required names, following $ref
// and allOf (how an embedded struct is written).
func (d specDoc) props(s map[string]any, seen map[string]bool) (props map[string]map[string]any, required map[string]bool) {
	props, required = map[string]map[string]any{}, map[string]bool{}
	if ref := specString(s["$ref"]); ref != "" {
		if seen[ref] {
			return
		}
		seen[ref] = true
		if target, ok := d.resolve(ref); ok {
			return d.props(target, seen)
		}
		return
	}
	for _, member := range specList(s["allOf"]) {
		p, r := d.props(specMap(member), seen)
		for k, v := range p {
			props[k] = v
		}
		for k := range r {
			required[k] = true
		}
	}
	for k, v := range specMap(s["properties"]) {
		props[k] = specMap(v)
	}
	for _, r := range specList(s["required"]) {
		required[specString(r)] = true
	}
	return
}

// jsonTypes is the set of JSON types a schema allows; any is true for a
// schema that says nothing about type.
func (d specDoc) jsonTypes(s map[string]any, depth int) (types map[string]bool, untyped bool) {
	types = map[string]bool{}
	if depth > 8 || s == nil {
		return types, true
	}
	if ref := specString(s["$ref"]); ref != "" {
		if target, ok := d.resolve(ref); ok {
			return d.jsonTypes(target, depth+1)
		}
		return types, true
	}
	switch tv := s["type"].(type) {
	case string:
		types[tv] = true
		return types, false
	case []any:
		for _, x := range tv {
			types[specString(x)] = true
		}
		return types, false
	}
	found := false
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		for _, member := range specList(s[key]) {
			found = true
			t, a := d.jsonTypes(specMap(member), depth+1)
			if a {
				return types, true
			}
			for k := range t {
				types[k] = true
			}
		}
	}
	return types, !found
}

func jsonTypeOf(t reflect.Type) (name string, skip bool) {
	if t == reflect.TypeOf(time.Time{}) {
		return "string", false
	}
	switch t.Kind() {
	case reflect.Bool:
		return "boolean", false
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer", false
	case reflect.Float32, reflect.Float64:
		return "number", false
	case reflect.String:
		return "string", false
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 { // []byte, json.RawMessage
			return "", true
		}
		return "array", false
	case reflect.Array:
		return "array", false
	case reflect.Map, reflect.Struct:
		return "object", false
	}
	return "", true // interface{}: anything
}

func TestOpenAPISchemasMatchGoTypes(t *testing.T) {
	d := loadSpec(t)
	schemas := specMap(specMap(d.root["components"])["schemas"])

	names := make([]string, 0, len(openAPISchemas))
	for n := range openAPISchemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		st := openAPISchemas[name]
		schema := d.schema(name)
		if schema == nil {
			t.Errorf("components.schemas.%s is missing (it is the schema of Go type %s)", name, st.Type)
			continue
		}
		props, required := d.props(schema, map[string]bool{})
		fields := goJSONFields(st.Type)
		goNames := map[string]goJSONField{}
		for _, f := range fields {
			goNames[f.Name] = f
		}
		for _, f := range fields {
			prop, ok := props[f.Name]
			if !ok {
				t.Errorf("%s: JSON property %q of Go type %s is missing from the schema", name, f.Name, st.Type)
				continue
			}
			if st.Input {
				continue
			}
			if required[f.Name] == f.Omit {
				if f.Omit {
					t.Errorf("%s.%s: Go tag has omitempty/omitzero, so the property must not be in required", name, f.Name)
				} else {
					t.Errorf("%s.%s: Go always writes this property, so it must be in required", name, f.Name)
				}
			}
			checkPropType(t, d, name, f, prop)
		}
		for p := range props {
			if _, ok := goNames[p]; !ok {
				t.Errorf("%s: schema property %q is not a JSON property of Go type %s", name, p, st.Type)
			}
		}
		for r := range required {
			if _, ok := props[r]; !ok {
				t.Errorf("%s: required lists %q, which is not a property", name, r)
			}
		}
	}
	for name := range schemas {
		if _, ok := openAPISchemas[name]; ok {
			continue
		}
		if _, ok := openAPIUntypedSchemas[name]; ok {
			continue
		}
		t.Errorf("components.schemas.%s has no Go type in openAPISchemas and is not in openAPIUntypedSchemas", name)
	}
	for name := range openAPIUntypedSchemas {
		if d.schema(name) == nil {
			t.Errorf("openAPIUntypedSchemas lists %s, which openapi.yaml does not define", name)
		}
		if _, dup := openAPISchemas[name]; dup {
			t.Errorf("%s is in both schema lists", name)
		}
	}
}

func checkPropType(t *testing.T, d specDoc, schemaName string, f goJSONField, prop map[string]any) {
	t.Helper()
	ft := f.Type
	ptr := ft.Kind() == reflect.Pointer
	if ptr {
		ft = ft.Elem()
	}
	types, anyType := d.jsonTypes(prop, 0)
	if anyType {
		return
	}
	nullable := types["null"]
	switch {
	case ptr && !nullable:
		t.Errorf("%s.%s: Go type %s can be null, so the schema must allow null", schemaName, f.Name, f.Type)
	case !ptr && nullable && ft.Kind() != reflect.Slice && ft.Kind() != reflect.Map && ft.Kind() != reflect.Interface:
		t.Errorf("%s.%s: Go type %s is never null, but the schema allows null", schemaName, f.Name, f.Type)
	}
	if want, skip := jsonTypeOf(ft); !skip && !types[want] {
		t.Errorf("%s.%s: Go type %s is a JSON %s, but the schema says %v", schemaName, f.Name, f.Type, want, sortedKeys(types))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every exported struct of view.go is a body some endpoint answers with: each
// must have a schema, so a new view type can't be left out of the spec.
func TestOpenAPIEveryViewTypeHasASchema(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "view.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			if _, isStruct := ts.Type.(*ast.StructType); !isStruct || !ts.Name.IsExported() {
				continue
			}
			n++
			if _, ok := openAPISchemas[ts.Name.Name]; !ok {
				t.Errorf("view.go declares %s, which has no entry in openAPISchemas (add the type and its schema)", ts.Name.Name)
			}
		}
	}
	if n == 0 {
		t.Fatal("found no exported struct in view.go: the test reads the wrong file")
	}
}

// stringConsts reads the string constants of the Go files matching glob (from
// the repo root) whose name starts with prefix; with block set, only those of
// the const declaration that holds the constant of that name. A constant that
// is not a literal, such as an alias of another package's, is left out.
func stringConsts(t *testing.T, glob, prefix, block string) map[string]string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(repoRoot(t), glob))
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			if block != "" && !slices.ContainsFunc(gd.Specs, func(spec ast.Spec) bool {
				return slices.ContainsFunc(spec.(*ast.ValueSpec).Names, func(id *ast.Ident) bool { return id.Name == block })
			}) {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) || !strings.HasPrefix(id.Name, prefix) {
						continue
					}
					if v, ok := stringLit(vs.Values[i]); ok {
						consts[id.Name] = v
					}
				}
			}
		}
	}
	return consts
}

// The enums of the spec are the code's constants. The lists below name the
// constants known today; the source is read as well, so one added later fails
// here until it is in a list and in openapi.yaml.
func TestOpenAPIEnumsMatchCode(t *testing.T) {
	d := loadSpec(t)
	outcomes := []string{OutcomeRunning, OutcomeApproved, OutcomeBlocking, OutcomeDraft, OutcomeNoBlocking,
		OutcomeFailed, OutcomeSuperseded, OutcomeSkipped, OutcomeUnknown}
	roles := []string{store.RoleAdmin, store.RoleMaintainer, store.RoleViewer}
	teamRoles := []string{store.RoleMaintainer, store.RoleViewer}
	accountRoles := []string{store.RoleAdmin, store.RoleUser}
	kinds := []string{store.AccountPassword, store.AccountSSO}
	signIns := []string{store.SignInPassword, store.SignInOIDC, store.SignInJWT}
	scanStates := []string{string(store.ScanQueued), string(store.ScanRunning), string(store.ScanDone), string(store.ScanFailed)}
	fetchStatuses := []string{store.FetchRunning, store.FetchDone, store.FetchFailed}
	fetchPurposes := []string{store.FetchInstall, store.FetchUpdate}
	healths := []string{healthOK, healthNeedsAttention}
	approvers := []string{approverKindApp, approverKindReviewApp, approverKindToken}
	platforms := []string{config.PlatformGitLab, config.PlatformGitHub}
	itemStates := []string{readiness.Passed, readiness.Warning, readiness.Missing, readiness.Unknown}
	cases := []struct {
		schema, prop string
		want         []string
	}{
		{"ReviewSummary", "outcome", outcomes},
		{"RunSummary", "outcome", outcomes},
		{"TeamMemberView", "role", teamRoles},
		{"ReviewDetailView", "role", append(append([]string{}, roles...), "")},
		{"RepositoryDetailView", "role", roles},
		{"UserView", "kind", kinds},
		{"UserView", "role", accountRoles},
		{"meView", "kind", kinds},
		{"meView", "role", accountRoles},
		{"meView", "signIn", signIns},
		{"SignInAdminView", "mode", []string{"", store.SignInModeOIDC, store.SignInModeJWT}},
		{"PutSignInRequest", "mode", []string{"", store.SignInModeOIDC, store.SignInModeJWT}},
		{"ScanView", "state", scanStates},
		{"SkillFetchView", "status", fetchStatuses},
		{"SkillFetchView", "purpose", fetchPurposes},
		{"RepositoryView", "health", healths},
		{"IntegrationView", "approver", approvers},
		{"InstanceAdminView", "approver", approvers},
		{"InstanceView", "platform", platforms},
		{"readiness.Item", "state", itemStates},
	}
	families := []struct {
		glob, prefix, block string
		known               [][]string
	}{
		{"internal/store/*.go", "Outcome", "", [][]string{outcomes}},
		// An account's role is admin or user (a team member's, viewer or
		// maintainer, is in the roles above). The platform kind is hidden: no
		// view of the API shows it.
		{"internal/store/*.go", "Role", "", [][]string{roles, accountRoles}},
		{"internal/store/*.go", "Account", "", [][]string{kinds, {store.AccountPlatform}}},
		{"internal/store/*.go", "SignIn", "", [][]string{signIns}},
		{"internal/store/*.go", "Scan", "", [][]string{scanStates}},
		{"internal/store/*.go", "Fetch", "", [][]string{fetchStatuses, fetchPurposes}},
		{"internal/dashboard/*.go", "health", "", [][]string{healths}},
		{"internal/dashboard/*.go", "approverKind", "", [][]string{approvers}},
		{"internal/config/*.go", "Platform", "", [][]string{platforms}},
		{"internal/readiness/*.go", "", "Passed", [][]string{itemStates}},
	}
	for _, f := range families {
		consts := stringConsts(t, f.glob, f.prefix, f.block)
		if len(consts) == 0 {
			t.Errorf("no constant %s* in %s: the test reads the wrong place", f.prefix, f.glob)
		}
		for name, v := range consts {
			if !slices.Contains(slices.Concat(f.known...), v) {
				t.Errorf("%s (%s) is a constant no enum here lists: add it to this test and to openapi.yaml", name, f.glob)
			}
		}
	}
	for _, c := range cases {
		prop := specMap(specMap(d.schema(c.schema)["properties"])[c.prop])
		if prop == nil {
			t.Errorf("%s.%s: not in the schema", c.schema, c.prop)
			continue
		}
		if ref := specString(prop["$ref"]); ref != "" {
			prop, _ = d.resolve(ref)
		}
		var got []string
		for _, e := range specList(prop["enum"]) {
			got = append(got, fmt.Sprint(e))
		}
		want := append([]string{}, c.want...)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s: enum is %q in openapi.yaml, but the code has %q", c.schema, c.prop, got, want)
		}
	}
}

// --- the document itself ---

var (
	schemaNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	pathParamPattern  = regexp.MustCompile(`\{([^}]+)\}`)
)

func TestOpenAPIDocumentIsValid(t *testing.T) {
	d := loadSpec(t)

	if v := specString(d.root["openapi"]); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi is %q, want 3.1.x", v)
	}
	info := specMap(d.root["info"])
	if specString(info["title"]) == "" || specString(info["version"]) == "" {
		t.Error("info.title and info.version are required")
	}
	chart, err := os.ReadFile(filepath.Join(repoRoot(t), "charts/pergent-sh/Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(chart, &meta); err != nil {
		t.Fatal(err)
	}
	if v := specString(info["version"]); v != meta.Version {
		t.Errorf("info.version is %q, but the chart is %q: a release bumps both", v, meta.Version)
	}
	if len(specMap(d.root["paths"])) == 0 {
		t.Fatal("paths is empty")
	}

	// Every $ref resolves, anywhere in the document.
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if _, found := d.resolve(ref); !found {
					t.Errorf("%s: $ref %q points at nothing", path, ref)
				}
			}
			for k, c := range x {
				walk(path+"/"+k, c)
			}
		case []any:
			for i, c := range x {
				walk(fmt.Sprintf("%s/%d", path, i), c)
			}
		}
	}
	walk("#", d.root)

	for name := range specMap(specMap(d.root["components"])["schemas"]) {
		if !schemaNamePattern.MatchString(name) {
			t.Errorf("components.schemas.%s: the name may hold letters, digits, '.', '_' and '-' only", name)
		}
	}
	schemes := specMap(specMap(d.root["components"])["securitySchemes"])
	usedOperationIDs := map[string]string{}
	paramIn := func(raw any) (name, in string, required bool) {
		p := specMap(raw)
		if ref := specString(p["$ref"]); ref != "" {
			p, _ = d.resolve(ref)
		}
		return specString(p["name"]), specString(p["in"]), p["required"] == true
	}

	for path, rawItem := range specMap(d.root["paths"]) {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("path %q must start with /", path)
		}
		item := specMap(rawItem)
		for k := range item {
			known := k == "parameters" || k == "summary" || k == "description" || k == "servers" || strings.HasPrefix(k, "x-")
			for _, m := range httpMethods {
				known = known || k == m
			}
			if !known {
				t.Errorf("%s: unknown key %q (an HTTP method must be lower case)", path, k)
			}
		}
	}

	for _, op := range d.operations() {
		id := op.Method + " " + op.Path
		if specString(op.Op["summary"]) == "" {
			t.Errorf("%s: summary is required", id)
		}
		oid := specString(op.Op["operationId"])
		if oid == "" {
			t.Errorf("%s: operationId is required", id)
		} else if prev, dup := usedOperationIDs[oid]; dup {
			t.Errorf("%s: operationId %q is also used by %s", id, oid, prev)
		} else {
			usedOperationIDs[oid] = id
		}
		responses := specMap(op.Op["responses"])
		if len(responses) == 0 {
			t.Errorf("%s: an operation needs at least one response", id)
		}
		for code, raw := range responses {
			resp := specMap(raw)
			if ref := specString(resp["$ref"]); ref != "" {
				resp, _ = d.resolve(ref)
			}
			if specString(resp["description"]) == "" {
				t.Errorf("%s: response %s needs a description", id, code)
			}
			if code != "default" && !regexp.MustCompile(`^[1-5]([0-9]{2}|XX)$`).MatchString(code) {
				t.Errorf("%s: %q is not a response status (quote the code in YAML)", id, code)
			}
		}
		if _, ok := op.Op["x-pergent-access"].(string); !ok {
			t.Errorf("%s: x-pergent-access is required (none, session, admin or operator)", id)
		} else if _, known := accessRank[specString(op.Op["x-pergent-access"])]; !known {
			t.Errorf("%s: x-pergent-access %q is not none, session or admin", id, op.Op["x-pergent-access"])
		}
		if sec, ok := op.Op["security"]; !ok {
			t.Errorf("%s: security is required (write [] for none)", id)
		} else {
			for _, req := range specList(sec) {
				for scheme := range specMap(req) {
					if _, ok := schemes[scheme]; !ok {
						t.Errorf("%s: security names %q, which is not in components.securitySchemes", id, scheme)
					}
				}
			}
		}
		// Path parameters: the template and the declarations agree.
		declared := map[string]bool{}
		for _, list := range [][]any{specList(op.PathItem["parameters"]), specList(op.Op["parameters"])} {
			for _, raw := range list {
				name, in, required := paramIn(raw)
				if in == "path" {
					declared[name] = true
					if !required {
						t.Errorf("%s: path parameter %q must be required: true", id, name)
					}
				}
			}
		}
		inTemplate := map[string]bool{}
		for _, m := range pathParamPattern.FindAllStringSubmatch(op.Path, -1) {
			inTemplate[m[1]] = true
			if !declared[m[1]] {
				t.Errorf("%s: path parameter {%s} is not declared", id, m[1])
			}
		}
		for name := range declared {
			if !inTemplate[name] {
				t.Errorf("%s: path parameter %q is declared but not in the path", id, name)
			}
		}
	}
}
