package links

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A repository links one as open or more open: public links public; internal
// links internal or public; private links anything.
func TestCheckVisibility(t *testing.T) {
	const (
		pub  = forge.VisPublic
		int_ = forge.VisInternal
		priv = forge.VisPrivate
	)
	for _, tc := range []struct {
		source, target forge.Visibility
		ok             bool
	}{
		{pub, pub, true},
		{pub, int_, false},
		{pub, priv, false},
		{int_, pub, true},
		{int_, int_, true},
		{int_, priv, false},
		{priv, pub, true},
		{priv, int_, true},
		{priv, priv, true},
		// A level nobody told us is never allowed, on either side.
		{"", pub, false},
		{pub, "", false},
		{"", "", false},
		{"secret", pub, false},
		{priv, "secret", false},
	} {
		err := Check(tc.source, tc.target)
		if (err == nil) != tc.ok {
			t.Errorf("Check(%q, %q) = %v; want ok=%v", tc.source, tc.target, err, tc.ok)
		}
	}
}

func link(target string, watched ...string) store.Link {
	return store.Link{Target: target, Watched: watched, CanBlock: true}
}

func TestValidateLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		ls   []store.Link
		want string // a part of the error; empty: valid
	}{
		{"none", nil, ""},
		{"an empty list", []store.Link{}, ""},
		{"one", []store.Link{link("acme/api", "render/**")}, ""},
		{"no watched paths", []store.Link{link("acme/api")}, ""},
		{"three", []store.Link{link("acme/a"), link("acme/b"), link("acme/c")}, ""},
		{"four", []store.Link{link("acme/a"), link("acme/b"), link("acme/c"), link("acme/d")}, "at most 3"},
		{"itself", []store.Link{link("acme/widgets")}, "itself"},
		{"itself in other case", []store.Link{link("Acme/Widgets")}, "itself"},
		{"a bad glob", []store.Link{link("acme/api", "render/[")}, "acme/api"},
		{"a directory glob", []store.Link{link("acme/api", "render/")}, "render/**"},
		{"an empty glob", []store.Link{link("acme/api", "")}, "empty"},
		{"a duplicate", []store.Link{link("acme/api"), link("acme/other"), link("acme/api", "x/**")}, "twice"},
		{"a duplicate in other case", []store.Link{link("acme/api"), link("ACME/API")}, "twice"},
		{"no owner", []store.Link{link("api")}, "owner/name"},
		{"an empty target", []store.Link{link("")}, "owner/name"},
		{"a sub path", []store.Link{link("acme/api/extra")}, "owner/name"},
		{"a URL", []store.Link{link("https://github.com/acme/api")}, "owner/name"},
		{"a space", []store.Link{link("acme/a pi")}, "owner/name"},
		{"a dot-dot name", []store.Link{link("acme/..")}, "owner/name"},
		{"a dot name", []store.Link{link("acme/.")}, "owner/name"},
		{"a dot-dot owner", []store.Link{link("../x")}, "owner/name"},
		{"a dot owner", []store.Link{link("./x")}, "owner/name"},
		{"a .git suffix", []store.Link{link("acme/api.git")}, "owner/name"},
		{"a .GIT suffix", []store.Link{link("acme/api.GIT")}, "owner/name"},
		{"itself with .git", []store.Link{link("acme/widgets.git")}, "owner/name"},
		{"a name with dots", []store.Link{link("acme/pergent.sh")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate("acme/widgets", tc.ls)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate = %v; want ok", err)
			case tc.want != "" && err == nil:
				t.Errorf("Validate = nil; want an error with %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("Validate = %q; want it to say %q", err, tc.want)
			}
		})
	}
}

func TestMatchLinks(t *testing.T) {
	render := link("acme/pergent.sh", "render/**", "scope/**")
	docs := link("acme/docs", "*.md")
	all := link("acme/all")
	ls := []store.Link{render, docs, all}

	for _, tc := range []struct {
		name  string
		files []string
		want  []store.Link
	}{
		{"watched", []string{"render/comment.go"}, []store.Link{render, all}},
		{"the second glob", []string{"cmd/main.go", "scope/match.go"}, []store.Link{render, all}},
		{"a name pattern at any depth", []string{"docs/guide/README.md"}, []store.Link{docs, all}},
		{"not watched", []string{"internal/x.go"}, []store.Link{all}},
		{"every link, in order", []string{"render/a.go", "b.md"}, []store.Link{render, docs, all}},
		// The caller passes a rename's old path as well as its new one
		// (forge.Paths): moving a watched file out of its folder touches it.
		{"a rename out of a watched path", []string{"elsewhere/comment.go", "render/comment.go"}, []store.Link{render, all}},
		{"no files", nil, nil},
		{"an empty file list", []string{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(ls, tc.files); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Match = %+v; want %+v", got, tc.want)
			}
		})
	}

	if got := Match(nil, []string{"render/a.go"}); got != nil {
		t.Errorf("Match with no links = %+v; want nil", got)
	}
	// A link with no watched paths matches every change, but not no change.
	if got := Match([]store.Link{all}, nil); got != nil {
		t.Errorf("a link with no paths and no files = %+v; want nil", got)
	}
	// The input is not changed.
	if len(ls) != 3 || ls[0].Target != "acme/pergent.sh" {
		t.Errorf("Match changed its input: %+v", ls)
	}
}
