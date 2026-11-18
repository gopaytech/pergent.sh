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

// A key whose BEGIN line fell before the start of the text (Tail keeps the
// last 2 MiB) still loses its body: a run of base64 lines at the very start
// that ends in an END PRIVATE KEY line.
func TestRedactKeyBodyWithoutBegin(t *testing.T) {
	for _, in := range []string{
		"QUJDREVGR0g=\nMIIEabcdef\n-----END RSA PRIVATE KEY-----\nnext step",
		"bcdef\n\nMIIEabcdef \n-----END PRIVATE KEY-----\nnext step",
		"MIIEabcdef\n-----END OPENSSH PRIVATE KEY-----",
	} {
		got := Redact(in)
		if strings.Contains(got, "MIIEabc") || strings.Contains(got, "bcdef") || !strings.HasPrefix(got, "[REDACTED]") {
			t.Errorf("Redact(%q) = %q", in, got)
		}
		if strings.Contains(in, "next step") && !strings.HasSuffix(got, "\nnext step") {
			t.Errorf("the text after the key went: %q", got)
		}
		if Redact(got) != got {
			t.Errorf("not idempotent: %q", got)
		}
	}
	// Only at the start, and only base64 lines: prose before an END line stays.
	for _, in := range []string{
		"step 1 said hello\n-----END RSA PRIVATE KEY-----",
		"step 2: ok\nMIIEabcdef\n-----END RSA PRIVATE KEY-----",
	} {
		if got := Redact(in); !strings.HasPrefix(got, strings.SplitN(in, "\n", 2)[0]) {
			t.Errorf("Redact(%q) = %q, want the first line kept", in, got)
		}
	}
}

func TestRedactMore(t *testing.T) {
	cases := []struct{ in, want string }{
		{`Authorization: Basic dXNlcjpwYXNz`, `Authorization: Basic [REDACTED]`},
		{`curl -H "authorization: basic dXNlcjpwYXNzd29yZA==" x`, `curl -H "authorization: basic [REDACTED]" x`},
		{`{"Authorization":"Basic dXNlcjpwYXNz"}`, `{"Authorization":"Basic [REDACTED]"}`},
		{`Basic configuration done`, `Basic configuration done`},
		{`{"password": "a\"b c", "user": "x"}`, `{"password": "[REDACTED]", "user": "x"}`},
		{`{"token":"a\\"}, "next": "ok"`, `{"token":"[REDACTED]"}, "next": "ok"`},
		{`run --password "two words" now`, `run --password [REDACTED] now`},
		{`run --api-key 'two words' now`, `run --api-key [REDACTED] now`},
		{`run --secret "a\" b" now`, `run --secret [REDACTED] now`},
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
