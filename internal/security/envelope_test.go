package security

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const goodCommit = "0123456789abcdef0123456789abcdef01234567"

// body makes a small valid envelope; edit replaces text in it.
func body(edit ...string) []byte {
	s := `{"kind":"security","status":"complete","commit":"` + goodCommit + `","usage":{"source":"stats","tokens":{"input":1}},"sarif":{"version":"2.1.0","runs":[]}}`
	for i := 0; i+1 < len(edit); i += 2 {
		s = strings.Replace(s, edit[0+i], edit[1+i], 1)
	}
	return []byte(s)
}

func TestParseEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		body    []byte
		status  string
		source  string
		skill   string
		wantErr bool
	}{
		{name: "complete", body: fixture(t, "complete.json"), status: "complete", source: "stats", skill: "security-audit"},
		{name: "partial", body: fixture(t, "partial.json"), status: "partial", source: "stats_poll", skill: "security-audit"},
		{name: "failed with an empty skill", body: fixture(t, "failed.json"), status: "failed", source: "stream", skill: ""},
		{name: "unknown top-level field", body: body(`"kind"`, `"extra":1,"kind"`), status: "complete", source: "stats"},
		{name: "no skill key", body: body(), status: "complete", source: "stats"},
		{name: "kind review", body: body(`"security"`, `"review"`), wantErr: true},
		{name: "status done", body: body(`"complete"`, `"done"`), wantErr: true},
		{name: "source other", body: body(`"stats"`, `"other"`), wantErr: true},
		{name: "short commit", body: body(goodCommit, "0123abc"), wantErr: true},
		{name: "upper-case commit", body: body(goodCommit, strings.ToUpper(goodCommit)), wantErr: true},
		{name: "negative input", body: body(`"input":1`, `"input":-1`), wantErr: true},
		{name: "negative steps", body: body(`"source":"stats"`, `"source":"stats","steps":-2`), wantErr: true},
		{name: "negative coverage", body: body(`"sarif"`, `"coverage":{"done":-1,"planned":2},"sarif"`), wantErr: true},
		{name: "negative model tokens", body: body(`"source":"stats"`, `"source":"stats","models":[{"provider":"a","model":"b","steps":1,"tokens":{"output":-3}}]`), wantErr: true},
		{name: "not json", body: []byte("nope"), wantErr: true},
		{name: "too big", body: append(body(), make([]byte, MaxBodyBytes)...), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := ParseEnvelope(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", env)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if env.Status != tc.status || env.Usage.Source != tc.source || env.Commit != goodCommit {
				t.Fatalf("got %+v", env)
			}
			got := ""
			if env.Skill != nil {
				got = env.Skill.Name
			}
			if got != tc.skill {
				t.Fatalf("skill = %q, want %q", got, tc.skill)
			}
		})
	}
}

func TestParseEnvelopeFields(t *testing.T) {
	t.Parallel()
	env, err := ParseEnvelope(fixture(t, "complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	if env.Coverage == nil || env.Coverage.Done != 3 || env.Coverage.Planned != 3 {
		t.Fatalf("coverage = %+v", env.Coverage)
	}
	u := env.Usage
	if u.Subagents != 2 || u.Steps != 40 || u.Tokens.CacheRead != 500 || u.Tokens.CacheWrite != 50 || len(u.Models) != 1 || u.Models[0].Tokens.Reasoning != 10 {
		t.Fatalf("usage = %+v", u)
	}
	if env.State["findings.json"] != "[]" || env.Skill.Commit != "abc123" || len(env.SARIF) == 0 {
		t.Fatalf("env = %+v", env)
	}
}

func TestParseEnvelopeStripsNUL(t *testing.T) {
	t.Parallel()
	b := body(`"complete"`, `"partial","reason":"a\u0000b"`, `"sarif"`, `"state":{"k\u0000":"v\u0000w"},"skill":{"name":"s\u0000n","commit":"c"},"sarif"`,
		`"runs":[]`, `"runs":[{"x":"p\u0000q","y":"\\u0000z"}]`)
	env, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if env.Reason != "ab" || env.State["k"] != "vw" || env.Skill.Name != "sn" {
		t.Fatalf("env = %+v", env)
	}
	// The raw SARIF reaches a jsonb column: no \u0000 escape may stay, and an
	// escaped backslash before "u0000" must stay a valid string.
	raw := string(env.SARIF)
	if strings.Contains(raw, `"p\u0000q"`) || !strings.Contains(raw, `"pq"`) || !strings.Contains(raw, `"\\u0000z"`) {
		t.Fatalf("sarif = %s", raw)
	}
}

func TestParseEnvelopeDropsBigState(t *testing.T) {
	t.Parallel()
	// "a" is over the file cap. "b" and "c" fit alone but not together with
	// "b" in front (sorted order), so "c" is dropped and "d" stays. The findings stay.
	half := strings.Repeat("x", MaxStateBytes/2)
	b := body(`"sarif"`, `"state":{"c":"`+half+`","a":"`+strings.Repeat("x", MaxStateBytes+1)+`","b":"`+half+`","d":"ok"},"sarif"`,
		`"runs":[]`, `"runs":[{"tool":{"driver":{"name":"s"}},"results":[{"message":{"text":"t"}}]}]`)
	env, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted walk: a is over the file cap, b fits, c would pass the total, d fits.
	if _, a := env.State["a"]; len(env.State) != 2 || env.State["b"] == "" || env.State["d"] != "ok" || a {
		t.Fatalf("state keys = %d", len(env.State))
	}
	if p, err := ReadSARIF(env.SARIF); err != nil || len(p.Findings) != 1 {
		t.Fatalf("findings = %v %v", p.Findings, err)
	}
}
