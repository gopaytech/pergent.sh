package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/policy"
)

func TestCheckFooterNote(t *testing.T) {
	long := strings.Repeat("a", maxFooterNote)
	for in, want := range map[string]string{
		"":                             "",
		"  Managed by the team  ":      "Managed by the team",
		"contact @team for onboarding": "contact @team for onboarding",
		"[team](https://example.test)": "[team](https://example.test)",
		long:                           long,
	} {
		if got, msg := checkFooterNote(in); got != want || msg != "" {
			t.Errorf("checkFooterNote(%q) = %q, %q; want %q", in, got, msg, want)
		}
	}
	for _, in := range []string{
		long + "a",
		"two\nlines",
		"a\ttab",
		"a <!-- comment",
		"<img src=x>",
		"![tracker](https://example.test/p.png)",
	} {
		if got, msg := checkFooterNote(in); msg == "" {
			t.Errorf("checkFooterNote(%q) = %q, want it refused", in, got)
		}
	}
}

func TestCommentFooterAPI(t *testing.T) {
	e := newEnv(t, nil)
	admin, user := e.sessionFor(e.ssoAdmin("boss@example.com")), e.sessionFor(e.sso("waiting@example.com"))
	const path = "/api/admin/comment-footer"

	wantCode(t, e.get(user, path), "user GET", http.StatusNotFound)
	wantCode(t, e.send(user, "PUT", path, `{"note":"x"}`), "user PUT", http.StatusNotFound)

	v := getJSON[CommentFooterView](e, admin, path)
	if v.Note != "" || v.Default != policy.DefaultFooterNote {
		t.Errorf("unset: %+v", v)
	}
	v = decode[CommentFooterView](t, e.send(admin, "PUT", path, `{"note":" Run by the team "}`), "PUT", http.StatusOK)
	if v.Note != "Run by the team" {
		t.Errorf("after PUT: %+v", v)
	}
	if got := e.auditOf("comment_footer_updated"); len(got) != 1 {
		t.Errorf("audit = %+v", got)
	}
	wantCode(t, e.send(admin, "PUT", path, `{"note":"<b>"}`), "bad text", http.StatusBadRequest)
	// A body with no note must not clear the text by accident.
	wantCode(t, e.send(admin, "PUT", path, `{}`), "no note", http.StatusBadRequest)
	if v := getJSON[CommentFooterView](e, admin, path); v.Note != "Run by the team" {
		t.Errorf("after refusals: %+v", v)
	}
	v = decode[CommentFooterView](t, e.send(admin, "PUT", path, `{"note":""}`), "clear", http.StatusOK)
	if v.Note != "" {
		t.Errorf("after clearing: %+v", v)
	}
}
