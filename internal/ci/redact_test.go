package ci

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct{ name, in, secret string }{
		{"bearer", "Authorization: Bearer abcdefgh123", "abcdefgh123"},
		{"url", "clone https://user:pass@host/x now", "user:pass"},
		{"private key", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIEabc\ndef\n-----END RSA PRIVATE KEY-----\nb", "MIIEabc"},
		{"aws", "key AKIAABCDEFGHIJKLMNOP end", "AKIAABCDEFGHIJKLMNOP"},
		{"ghp", "t ghp_" + strings.Repeat("a", 36), "ghp_aaaa"},
		{"github_pat", "t github_pat_" + strings.Repeat("B", 30) + "_x", "github_pat_BBBB"},
		{"glpat", "t glpat-" + strings.Repeat("c", 20), "glpat-cccc"},
		{"slack", "t xoxb-1234567890-abcdefghij", "xoxb-1234567890"},
		{"password", "password=hunter2", "hunter2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if strings.Contains(got, c.secret) || !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("got %q", got)
			}
		})
	}
	if got := Redact("password=hunter2"); got != "password=[REDACTED]" {
		t.Errorf("key name must stay: %q", got)
	}
	if got := Redact("https://user:pass@host/x"); !strings.Contains(got, "host/x") {
		t.Errorf("host must stay: %q", got)
	}
	if got := Redact("nothing secret here"); got != "nothing secret here" {
		t.Errorf("changed plain text: %q", got)
	}
	if got := Redact(Redact("password=hunter2")); got != "password=[REDACTED]" {
		t.Errorf("not idempotent: %q", got)
	}
}

func TestRedactForms(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"password": "hunter2", "api_key":"abc"}`, `{"password": "[REDACTED]", "api_key":"[REDACTED]"}`},
		{`password: "x y"`, `password: "[REDACTED]"`},
		{`'secret': 'abc'`, `'secret': '[REDACTED]'`},
		{`run --password hunter2 now`, `run --password [REDACTED] now`},
		{`run --token=abc now`, `run --token=[REDACTED] now`},
		{`https://u:p@ss@host/x`, `https://[REDACTED]@host/x`},
		{`https://u:p/ss@host`, `https://u:p/ss@host`},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q; want %q", c.in, got, c.want)
		}
		if got := Redact(Redact(c.in)); got != c.want {
			t.Errorf("not idempotent for %q: %q", c.in, got)
		}
	}
}
