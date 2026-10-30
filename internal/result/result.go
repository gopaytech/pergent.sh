// Package result reads the versioned JSON result that `pergent agent` delivers.
//
// Only the fields pergent.sh uses are declared; unknown fields are ignored, as
// the schema promises version 1 only grows. The schema is documented in the
// pergent repository, docs/agent-result-schema.md.
package result

import (
	"encoding/json"
	"errors"
	"fmt"
)

// SchemaVersion is the only result schema version this package reads.
const SchemaVersion = 1

// Result is one delivered review result.
type Result struct {
	SchemaVersion  int     `json:"schema_version"`
	Status         string  `json:"status"` // "completed" or "error"
	PergentVersion string  `json:"pergent_version"`
	Base           string  `json:"base"`
	Head           string  `json:"head"`
	Skills         []Skill `json:"skills"`
	Error          *Error  `json:"error"`
	Comment        Comment `json:"comment"`
}

// Skill is the outcome of one skill. Findings stay raw: pergent.sh only stores
// and forwards them (see Trim).
type Skill struct {
	Name     string            `json:"name"`
	Verdict  string            `json:"verdict"`
	Findings []json.RawMessage `json:"findings"`
	Files    Files             `json:"files"`
}

// Files records what a skill looked at. Its verdict covers Reviewed only.
type Files struct {
	Reviewed    []string          `json:"reviewed"`
	Skipped     []Skipped         `json:"skipped"`
	NotReviewed []json.RawMessage `json:"not_reviewed"`
}

// Skipped is a file left out of a review, and the rule that left it out.
type Skipped struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// Error is the reason an error result carries.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Comment is the rendered comment, ready to post.
type Comment struct {
	Markdown      string `json:"markdown"`
	FindingsBlock string `json:"findings_block"`
}

// Parse decodes and validates a result. It rejects another schema version, a
// status other than "completed" or "error", an empty comment and a completed
// result without skills, so a malformed result is never mistaken for a pass.
func Parse(data []byte) (*Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	if r.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported result schema_version %d (want %d)", r.SchemaVersion, SchemaVersion)
	}
	switch r.Status {
	case "completed":
		if len(r.Skills) == 0 {
			return nil, errors.New("completed result has no skills")
		}
	case "error":
	default:
		return nil, fmt.Errorf("unknown result status %q", r.Status)
	}
	if r.Comment.Markdown == "" {
		return nil, errors.New("result has no comment.markdown")
	}
	return &r, nil
}

type trimmed struct {
	SchemaVersion int           `json:"schema_version"`
	Status        string        `json:"status"`
	Skills        []trimmedSkil `json:"skills"`
}

type trimmedSkil struct {
	Name     string            `json:"name"`
	Findings []json.RawMessage `json:"findings"`
}

// Trim reduces a result to what a later run needs as --previous: the schema
// version, the status and each skill's name and findings, which are kept
// verbatim.
func Trim(data []byte) ([]byte, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	t := trimmed{SchemaVersion: r.SchemaVersion, Status: r.Status, Skills: make([]trimmedSkil, 0, len(r.Skills))}
	for _, s := range r.Skills {
		f := s.Findings
		if f == nil {
			f = []json.RawMessage{}
		}
		t.Skills = append(t.Skills, trimmedSkil{Name: s.Name, Findings: f})
	}
	return json.Marshal(t)
}
