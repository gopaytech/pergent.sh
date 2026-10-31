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
	"sort"

	"github.com/gopaytech/pergent/render"
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

// Skill is the outcome of one skill. Findings stay raw: pergent.sh stores and
// forwards them (see Trim) and decodes them where it needs them (see Finding
// and ToRender). The other fields are what re-rendering the comment needs.
type Skill struct {
	Name     string            `json:"name"`
	Verdict  string            `json:"verdict"`
	Findings []json.RawMessage `json:"findings"`
	Files    Files             `json:"files"`

	Failures        []Failure     `json:"failures,omitempty"`
	Notes           []string      `json:"notes,omitempty"`
	Summaries       []Summary     `json:"summaries,omitempty"`
	Checks          []BatchChecks `json:"checks,omitempty"`
	Batches         int           `json:"batches,omitempty"`
	BatchesFinished int           `json:"batches_finished,omitempty"`
}

// Finding is one finding of a skill, as the result stores it.
type Finding struct {
	ID           string `json:"id"`
	Fingerprint  string `json:"fingerprint"`
	File         string `json:"file"`
	Line         int    `json:"line,omitempty"`
	Severity     string `json:"severity"`
	Title        string `json:"title"`
	Explanation  string `json:"explanation"`
	SuggestedFix string `json:"suggested_fix,omitempty"`
}

// Failure is a batch of a skill that did not finish, with the plain text of
// why.
type Failure struct {
	Files   []string `json:"files"`
	Message string   `json:"message"`
}

// Summary is the model's summary of one batch.
type Summary struct {
	Batch int    `json:"batch"`
	Text  string `json:"text"`
}

// BatchChecks is the checklist the model reported for one batch.
type BatchChecks struct {
	Batch    int     `json:"batch"`
	Dropped  int     `json:"dropped"`
	Findings int     `json:"findings"`
	Checks   []Check `json:"checks"`
}

// Check is one checklist item.
type Check struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

// notReviewed is the part of a files.not_reviewed entry pergent.sh reads.
type notReviewed struct {
	Path string `json:"path"`
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
	for _, s := range r.Skills {
		for _, raw := range s.Findings {
			var f Finding
			if err := json.Unmarshal(raw, &f); err != nil {
				return nil, fmt.Errorf("decode a finding of skill %q: %w", s.Name, err)
			}
		}
	}
	return &r, nil
}

// ToRender is the result's skills in the form the comment renderer takes,
// with the verdicts as the result gives them. A finding without an ID gets
// one assigned, unique among the result's findings. A finding that does not
// decode is left out (Parse rejects such a result).
func ToRender(r *Result) []render.Skill {
	findings := make([][]Finding, len(r.Skills))
	var fps []string
	for i, s := range r.Skills {
		for _, raw := range s.Findings {
			var f Finding
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			findings[i] = append(findings[i], f)
			fps = append(fps, f.Fingerprint)
		}
	}
	ids := render.AssignIDs(fps)

	out := make([]render.Skill, len(r.Skills))
	for i, s := range r.Skills {
		rs := render.Skill{
			Name: s.Name, Verdict: s.Verdict, Reviewed: s.Files.Reviewed,
			Notes: s.Notes, Batches: s.Batches, BatchesFinished: s.BatchesFinished,
			Findings: []render.Finding{},
		}
		for _, f := range findings[i] {
			id := f.ID
			if id == "" {
				id = ids[f.Fingerprint]
			}
			rs.Findings = append(rs.Findings, render.Finding{
				ID: id, Fingerprint: f.Fingerprint, File: f.File, Line: f.Line,
				Severity: render.Severity(f.Severity), Title: f.Title,
				Explanation: f.Explanation, SuggestedFix: f.SuggestedFix,
			})
		}
		for _, f := range s.Failures {
			rs.Failures = append(rs.Failures, render.Failure{Files: f.Files, Message: f.Message})
		}
		for _, f := range s.Files.Skipped {
			rs.Skipped = append(rs.Skipped, render.Skipped{Path: f.Path, Rule: f.Rule})
		}
		for _, sum := range s.Summaries {
			rs.Summaries = append(rs.Summaries, render.Summary{Batch: sum.Batch, Text: sum.Text})
		}
		for _, bc := range s.Checks {
			rb := render.BatchChecks{Batch: bc.Batch, Dropped: bc.Dropped, Findings: bc.Findings}
			for _, c := range bc.Checks {
				rb.Checks = append(rb.Checks, render.Check{Section: c.Section, Name: c.Name, Status: c.Status, Detail: c.Detail})
			}
			rs.Checks = append(rs.Checks, rb)
		}
		out[i] = rs
	}
	return out
}

// ChangedFiles is the paths of every file any skill reviewed, skipped or
// could not review, sorted: the changed files of the change, as far as the
// result knows them.
func ChangedFiles(r *Result) []string {
	seen := map[string]bool{}
	for _, s := range r.Skills {
		for _, p := range s.Files.Reviewed {
			seen[p] = true
		}
		for _, f := range s.Files.Skipped {
			seen[f.Path] = true
		}
		for _, raw := range s.Files.NotReviewed {
			var n notReviewed
			if json.Unmarshal(raw, &n) == nil && n.Path != "" {
				seen[n.Path] = true
			}
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

type trimmed struct {
	SchemaVersion int            `json:"schema_version"`
	Status        string         `json:"status"`
	Skills        []trimmedSkill `json:"skills"`
}

type trimmedSkill struct {
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
	t := trimmed{SchemaVersion: r.SchemaVersion, Status: r.Status, Skills: make([]trimmedSkill, 0, len(r.Skills))}
	for _, s := range r.Skills {
		f := s.Findings
		if f == nil {
			f = []json.RawMessage{}
		}
		t.Skills = append(t.Skills, trimmedSkill{Name: s.Name, Findings: f})
	}
	return json.Marshal(t)
}
