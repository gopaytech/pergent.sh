package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// res makes one SARIF result; extra is spliced in before the closing brace.
func res(extra string) string {
	return `{"ruleId":"injection","message":{"text":"Title one\nmore"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"a/b.go"},"region":{"startLine":7}}}]` + extra + `}`
}

func sarifOf(results ...string) json.RawMessage {
	return json.RawMessage(`{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"sk","version":"v1","rules":[{"id":"injection","properties":{"tags":["security","injection"]}},{"id":"sqli","properties":{"tags":["CWE-89"]}}]}},"results":[` + strings.Join(results, ",") + `]}]}`)
}

func one(t *testing.T, raw json.RawMessage) (f struct {
	Fingerprint, Severity, Title, File string
}) {
	t.Helper()
	p, err := ReadSARIF(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Findings) != 1 {
		t.Fatalf("findings = %d", len(p.Findings))
	}
	g := p.Findings[0]
	f.Fingerprint, f.Severity, f.Title, f.File = g.Fingerprint, g.Severity, g.Title, g.File
	return f
}

func TestReadSARIF(t *testing.T) {
	t.Parallel()
	sum := func(rule, file, title string) string {
		h := sha256.Sum256([]byte(rule + "\x00" + file + "\x00" + title))
		return hex.EncodeToString(h[:])
	}
	t.Run("fingerprint", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, extra, want string }{
			{"v1 first", `,"fingerprints":{"a":"x","security-audit/v1":"v1"},"partialFingerprints":{"b":"p"}`, "v1"},
			{"fingerprints before partial", `,"fingerprints":{"b":"fb","a":"fa"},"partialFingerprints":{"a":"pa"}`, "fa"},
			{"empty v1 is skipped", `,"fingerprints":{"security-audit/v1":"","z":"fz"}`, "fz"},
			{"partial when fingerprints are empty", `,"fingerprints":{"a":""},"partialFingerprints":{"b":"pb","a":"pa"}`, "pa"},
			{"derived", ``, sum("injection", "a/b.go", "Title one")},
			{"cut to 256 and one line", `,"fingerprints":{"security-audit/v1":"` + strings.Repeat("f", 300) + `"}`, strings.Repeat("f", 256)},
			{"first line only", `,"fingerprints":{"security-audit/v1":"one\ntwo"}`, "one"},
		} {
			if got := one(t, sarifOf(res(tc.extra))).Fingerprint; got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		}
	})
	t.Run("derived fingerprint ignores the line", func(t *testing.T) {
		t.Parallel()
		other := strings.Replace(res(""), `"startLine":7`, `"startLine":90`, 1)
		if a, b := one(t, sarifOf(res(""))).Fingerprint, one(t, sarifOf(other)).Fingerprint; a != b {
			t.Fatalf("%q != %q", a, b)
		}
	})
	t.Run("severity", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, extra, want string }{
			{"severity over score", `,"properties":{"severity":"high","security-severity":"3.0"}`, "high"},
			{"critical", `,"properties":{"security-severity":"9.8"}`, "critical"},
			{"high", `,"properties":{"security-severity":"7"}`, "high"},
			{"medium", `,"properties":{"security-severity":"4.0"}`, "medium"},
			{"low", `,"properties":{"security-severity":"0.1"}`, "low"},
			{"unknown severity uses the score", `,"properties":{"severity":"urgent","security-severity":"8"}`, "high"},
			{"warning", `,"level":"warning"`, "medium"},
			{"error with no score", `,"level":"error"`, "high"},
			{"note", `,"level":"note"`, "low"},
			{"none", `,"level":"none"`, "low"},
			{"zero score uses the level", `,"properties":{"security-severity":"0"},"level":"error"`, "high"},
		} {
			if got := one(t, sarifOf(res(tc.extra))).Severity; got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		}
	})
	t.Run("properties", func(t *testing.T) {
		t.Parallel()
		p, err := ReadSARIF(sarifOf(res(`,"properties":{"description":"desc","exploit_scenario":"exp","fix":"fixit","confidence":0.75,"needs_validation":true}`)))
		if err != nil {
			t.Fatal(err)
		}
		f := p.Findings[0]
		if f.Description != "desc" || f.Exploit != "exp" || f.FixHint != "fixit" || f.Confidence != "0.75" || !f.NeedsValidation {
			t.Fatalf("%+v", f)
		}
		if f.Title != "Title one" || f.RuleID != "injection" || f.File != "a/b.go" || f.StartLine != 7 {
			t.Fatalf("%+v", f)
		}
		if p.SkillName != "sk" || p.SkillCommit != "v1" {
			t.Fatalf("skill = %q %q", p.SkillName, p.SkillCommit)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		p, err := ReadSARIF(sarifOf(res(`,"fixes":[{"description":{"text":"from fixes"}}]`)))
		if err != nil {
			t.Fatal(err)
		}
		f := p.Findings[0]
		if f.Description != "Title one\nmore" || f.FixHint != "from fixes" || f.NeedsValidation || f.Exploit != "" {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("cwe", func(t *testing.T) {
		t.Parallel()
		if p, _ := ReadSARIF(sarifOf(res(``))); p.Findings[0].CWE != "" || strings.Join(p.Findings[0].Tags, ",") != "security,injection" {
			t.Fatalf("%+v", p.Findings[0])
		}
		r := strings.Replace(res(``), `"injection"`, `"sqli"`, 1)
		if p, _ := ReadSARIF(sarifOf(r)); p.Findings[0].CWE != "CWE-89" {
			t.Fatalf("%+v", p.Findings[0])
		}
	})
	t.Run("file", func(t *testing.T) {
		t.Parallel()
		for uri, want := range map[string]string{"../x": "", "a/../../x": "", "/etc/passwd": "", "file:///x": "", "a\\b": "", "./a/b.go": "a/b.go", "a/b c.go": "a/b c.go"} {
			r := strings.Replace(res(``), `a/b.go`, strings.ReplaceAll(uri, `\`, `\\`), 1)
			if got := one(t, sarifOf(r)).File; got != want {
				t.Errorf("%q: got %q, want %q", uri, got, want)
			}
		}
	})
	t.Run("no location", func(t *testing.T) {
		t.Parallel()
		p, err := ReadSARIF(sarifOf(`{"ruleId":"r","message":{"text":"t"}}`))
		if err != nil || p.Findings[0].File != "" || p.Findings[0].StartLine != 0 {
			t.Fatalf("%v %+v", err, p.Findings)
		}
	})
	t.Run("duplicate fingerprints keep the first", func(t *testing.T) {
		t.Parallel()
		a := res(`,"fingerprints":{"security-audit/v1":"same"}`)
		b := strings.Replace(a, "Title one", "Other", 1)
		p, err := ReadSARIF(sarifOf(a, b))
		if err != nil || len(p.Findings) != 1 || p.Findings[0].Title != "Title one" {
			t.Fatalf("%v %+v", err, p.Findings)
		}
	})
	t.Run("long texts are cut and NUL is gone", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("é", 9000)
		r := strings.Replace(res(`,"properties":{"description":"`+long+`","fix":"a\u0000b"}`), "Title one", strings.Repeat("t", 300), 1)
		p, err := ReadSARIF(sarifOf(r))
		if err != nil {
			t.Fatal(err)
		}
		f := p.Findings[0]
		if len(f.Title) != 200 || len(f.Description) > 8<<10 || len(f.Description) < 8<<10-1 || f.FixHint != "ab" {
			t.Fatalf("title %d, description %d, fix %q", len(f.Title), len(f.Description), f.FixHint)
		}
		if strings.ContainsRune(f.Description, '�') {
			t.Fatal("cut in the middle of a character")
		}
	})
	t.Run("version", func(t *testing.T) {
		t.Parallel()
		if _, err := ReadSARIF(json.RawMessage(`{"version":"2.0.0","runs":[]}`)); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("not json", func(t *testing.T) {
		t.Parallel()
		if _, err := ReadSARIF(json.RawMessage(`[1`)); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("too big", func(t *testing.T) {
		t.Parallel()
		big := `{"version":"2.1.0","runs":[],"x":"` + strings.Repeat("a", MaxSARIFBytes) + `"}`
		if _, err := ReadSARIF(json.RawMessage(big)); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{``, `null`, `{"version":"2.1.0","runs":[]}`} {
			p, err := ReadSARIF(json.RawMessage(raw))
			if err != nil || len(p.Findings) != 0 {
				t.Errorf("%q: %v %+v", raw, err, p)
			}
		}
	})
}

func TestReadSARIFCap(t *testing.T) {
	t.Parallel()
	rs := make([]string, 0, MaxFindings+1)
	for i := 0; i <= MaxFindings; i++ {
		rs = append(rs, res(fmt.Sprintf(`,"fingerprints":{"security-audit/v1":"f%d"}`, i)))
	}
	p, err := ReadSARIF(sarifOf(rs...))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Findings) != MaxFindings || !p.Capped {
		t.Fatalf("%d findings, capped %v", len(p.Findings), p.Capped)
	}
	p, err = ReadSARIF(sarifOf(rs[:MaxFindings]...))
	if err != nil || len(p.Findings) != MaxFindings || p.Capped {
		t.Fatalf("exactly the max: %v %d %v", err, len(p.Findings), p.Capped)
	}
}

func TestSkillOf(t *testing.T) {
	t.Parallel()
	p := Parsed{SkillName: "from-sarif", SkillCommit: "c2"}
	for _, tc := range []struct {
		name         string
		skill        *Skill
		wantN, wantC string
	}{
		{"envelope wins", &Skill{Name: "from-env", Commit: "c1"}, "from-env", "c1"},
		{"no skill", nil, "from-sarif", "c2"},
		{"empty skill", &Skill{}, "from-sarif", "c2"},
	} {
		n, c := SkillOf(Envelope{Skill: tc.skill}, p)
		if n != tc.wantN || c != tc.wantC {
			t.Errorf("%s: got %q %q", tc.name, n, c)
		}
	}
}
