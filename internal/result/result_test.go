package result

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParse(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		r, err := Parse(fixture(t, "completed.json"))
		if err != nil {
			t.Fatal(err)
		}
		if r.SchemaVersion != 1 || r.Status != "completed" || r.PergentVersion != "0.1.0" {
			t.Errorf("envelope: %+v", r)
		}
		if r.Base != "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b" || r.Head != "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b" {
			t.Errorf("base/head: %q %q", r.Base, r.Head)
		}
		if len(r.Skills) != 2 {
			t.Fatalf("skills: %d", len(r.Skills))
		}
		s := r.Skills[0]
		if s.Name != "code-review" || s.Verdict != "has_blocking_issues" || len(s.Findings) != 2 {
			t.Errorf("skill 0: %+v", s)
		}
		if len(s.Files.Reviewed) != 2 || len(s.Files.Skipped) != 1 || s.Files.Skipped[0] != (Skipped{Path: "go.sum", Rule: "lockfile"}) {
			t.Errorf("files: %+v", s.Files)
		}
		s1 := r.Skills[1]
		if s1.Verdict != "failed" || len(s1.Files.NotReviewed) != 1 {
			t.Errorf("skill 1: %+v", s1)
		}
		if !strings.HasPrefix(r.Comment.Markdown, "### code-review") || !strings.HasPrefix(r.Comment.FindingsBlock, "<!-- pergent:findings") {
			t.Errorf("comment: %+v", r.Comment)
		}
	})
	t.Run("error", func(t *testing.T) {
		r, err := Parse(fixture(t, "error.json"))
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "error" || r.Error == nil || r.Error.Code != "commit_missing" || r.Error.Message == "" {
			t.Errorf("error result: %+v", r)
		}
		if !strings.HasPrefix(r.Comment.Markdown, "<!-- pergent -->") {
			t.Errorf("comment: %q", r.Comment.Markdown)
		}
	})
	t.Run("unknown fields are accepted", func(t *testing.T) {
		in := `{"schema_version":1,"status":"error","future":{"a":1},"comment":{"markdown":"m","extra":2}}`
		if _, err := Parse([]byte(in)); err != nil {
			t.Fatal(err)
		}
	})
	rejects := map[string]string{
		"schema version 2":     `{"schema_version":2,"status":"error","comment":{"markdown":"m"}}`,
		"missing version":      `{"status":"error","comment":{"markdown":"m"}}`,
		"missing markdown":     `{"schema_version":1,"status":"error","comment":{}}`,
		"no comment":           `{"schema_version":1,"status":"error"}`,
		"unknown status":       `{"schema_version":1,"status":"partial","comment":{"markdown":"m"}}`,
		"empty status":         `{"schema_version":1,"comment":{"markdown":"m"}}`,
		"completed no skills":  `{"schema_version":1,"status":"completed","comment":{"markdown":"m"}}`,
		"completed empty list": `{"schema_version":1,"status":"completed","skills":[],"comment":{"markdown":"m"}}`,
		"not json":             `nope`,
		"empty":                ``,
	}
	for name, in := range rejects {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestTrimKeepsNamesAndFindingsOnly(t *testing.T) {
	out, err := Trim(fixture(t, "completed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("top-level keys: %v", got)
	}
	if string(got["schema_version"]) != "1" || string(got["status"]) != `"completed"` {
		t.Errorf("envelope: %s", out)
	}
	var skills []map[string]json.RawMessage
	if err := json.Unmarshal(got["skills"], &skills); err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills: %d", len(skills))
	}
	for _, s := range skills {
		if len(s) != 2 {
			t.Errorf("skill keys: %v", s)
		}
	}
	var findings []map[string]any
	if err := json.Unmarshal(skills[0]["findings"], &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 || findings[0]["fingerprint"] != "a1b2c3d4e5f6" || findings[0]["suggested_fix"] == nil {
		t.Errorf("findings must be kept verbatim: %v", findings)
	}
	// a skill without findings still has a findings array, never null
	if string(skills[1]["findings"]) != "[]" {
		t.Errorf("empty findings: %s", skills[1]["findings"])
	}
}

func TestTrimRejectsInvalid(t *testing.T) {
	if _, err := Trim([]byte("nope")); err == nil {
		t.Fatal("expected an error")
	}
}
