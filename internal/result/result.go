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
	"slices"
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

	// Merged is set by Merge on a result that carries kept skills of an
	// earlier run: its comment covers only the skills that were re-run, so
	// the comment is always rendered again from the skills. MergeNote is set
	// by NoteUnmerged on a re-run whose earlier results could not be kept,
	// and is shown in the comment. Either marks the merge step as done.
	Merged    bool   `json:"merged,omitempty"`
	MergeNote string `json:"merge_note,omitempty"`
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

	// Confidence is the model's own score, 1 to 5 (0 when it gave none);
	// render applies the caps.
	Confidence       int    `json:"confidence,omitempty"`
	ConfidenceReason string `json:"confidence_reason,omitempty"`
}

// Finding is one finding of a skill, as the result stores it.
type Finding struct {
	ID           string `json:"id"`
	Fingerprint  string `json:"fingerprint"`
	File         string `json:"file"`
	Line         int    `json:"line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	Severity     string `json:"severity"`
	Title        string `json:"title"`
	Explanation  string `json:"explanation"`
	SuggestedFix string `json:"suggested_fix,omitempty"`
	Suggestion   string `json:"suggestion,omitempty"`
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
			Confidence: s.Confidence, ConfidenceReason: s.ConfidenceReason,
			Findings: []render.Finding{},
		}
		for _, f := range findings[i] {
			id := f.ID
			if id == "" {
				id = ids[f.Fingerprint]
			}
			rs.Findings = append(rs.Findings, render.Finding{
				ID: id, Fingerprint: f.Fingerprint, File: f.File, Line: f.Line, EndLine: f.EndLine,
				Severity: render.Severity(f.Severity), Title: f.Title,
				Explanation: f.Explanation, SuggestedFix: f.SuggestedFix, Suggestion: f.Suggestion,
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

// FailedSkills names the skills a "review failed" re-run reviews again: those
// whose verdict is "failed" (or one pergent.sh does not know, as the policy
// treats it) and those that are "no_blocking_issues" yet list files they could
// not review. The verdicts are the ones the policy decides on, after
// dismissals ds are applied: a skill whose blocking findings are all
// dismissed is "no_blocking_issues", so it is failed when it also lists files
// it could not review. whole is true when no skill can be singled out: the
// result is an error or has no skills; the whole review is then re-run.
func FailedSkills(r *Result, ds []render.Dismissal) (names []string, whole bool) {
	if r.Status != "completed" || len(r.Skills) == 0 {
		return nil, true
	}
	decided := render.ApplyDismissals(ToRender(r), ds)
	for i, s := range r.Skills {
		switch decided[i].Verdict {
		case "has_blocking_issues":
		case "no_blocking_issues":
			if len(s.Files.NotReviewed) > 0 {
				names = append(names, s.Name)
			}
		default:
			names = append(names, s.Name)
		}
	}
	return names, false
}

// NoteBaseUnreadable is the note of a re-run whose earlier results of the
// other skills could not be kept.
const NoteBaseUnreadable = "earlier results of the other skills could not be read"

// Merge combines the result of a partial re-run with the result it builds on:
// the skills in subset come from partial, every other skill from base. The
// base's order is kept (a subset skill takes the place of its earlier result;
// one the base lacks goes last), a skill of subset that partial lacks is a
// failed skill (never dropped, which would pass it unreviewed), and each skill
// is kept as it was delivered, fields that Result does not declare included. Finding IDs are assigned again over all the findings
// of the merged result, so they stay unique. The envelope and the comment are
// partial's, and the result is marked Merged.
//
// It fails when partial is not a completed result, when base is not a usable
// completed result of the same base and head commits, or when subset is empty:
// the caller then keeps partial alone (see NoteUnmerged).
func Merge(partial, base []byte, subset []string) ([]byte, error) {
	if len(subset) == 0 {
		return nil, errors.New("no skills were re-run")
	}
	pr, err := Parse(partial)
	if err != nil {
		return nil, fmt.Errorf("the re-run result: %w", err)
	}
	br, err := Parse(base)
	if err != nil {
		return nil, fmt.Errorf("the earlier result: %w", err)
	}
	switch {
	case pr.Status != "completed":
		return nil, errors.New("the re-run result is not completed")
	case br.Status != "completed":
		return nil, errors.New("the earlier result is not completed")
	case pr.Base != br.Base || pr.Head != br.Head:
		return nil, errors.New("the earlier result is for another commit")
	}

	var top, baseTop map[string]json.RawMessage
	var partialSkills, baseSkills []json.RawMessage
	if err := unmarshalSkills(partial, &top, &partialSkills); err != nil {
		return nil, err
	}
	if err := unmarshalSkills(base, &baseTop, &baseSkills); err != nil {
		return nil, err
	}

	fresh := map[string]json.RawMessage{}
	var freshOrder []string
	for i, raw := range partialSkills {
		name := pr.Skills[i].Name
		if _, dup := fresh[name]; !dup && slices.Contains(subset, name) {
			fresh[name] = raw
			freshOrder = append(freshOrder, name)
		}
	}
	var skills []json.RawMessage
	used := map[string]bool{}
	for i, raw := range baseSkills {
		name := br.Skills[i].Name
		if !slices.Contains(subset, name) {
			skills = append(skills, raw)
			continue
		}
		if used[name] {
			continue
		}
		used[name] = true
		skills = append(skills, freshOrMissing(fresh, name))
	}
	for _, name := range freshOrder {
		if !used[name] {
			used[name] = true
			skills = append(skills, fresh[name])
		}
	}
	for _, name := range subset {
		if !used[name] {
			used[name] = true
			skills = append(skills, freshOrMissing(fresh, name))
		}
	}

	skills, err = assignIDs(skills)
	if err != nil {
		return nil, err
	}
	if top["skills"], err = json.Marshal(skills); err != nil {
		return nil, err
	}
	top["merged"] = json.RawMessage("true")
	return json.Marshal(top)
}

// failedMessage is why a skill that was asked for is failed in a merge.
const failedMessage = "The re-run returned no result for this skill."

// freshOrMissing is the re-run's result for name, or a failed skill when the
// re-run has none.
func freshOrMissing(fresh map[string]json.RawMessage, name string) json.RawMessage {
	if raw, ok := fresh[name]; ok {
		return raw
	}
	missing, _ := json.Marshal(Skill{
		Name: name, Verdict: "failed", Findings: []json.RawMessage{},
		Files:    Files{Reviewed: []string{}, Skipped: []Skipped{}, NotReviewed: []json.RawMessage{}},
		Failures: []Failure{{Files: []string{}, Message: failedMessage}},
	})
	return missing
}

// unmarshalSkills decodes data into its top-level fields and the raw skills
// among them.
func unmarshalSkills(data []byte, top *map[string]json.RawMessage, skills *[]json.RawMessage) error {
	if err := json.Unmarshal(data, top); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	if raw, ok := (*top)["skills"]; ok {
		if err := json.Unmarshal(raw, skills); err != nil {
			return fmt.Errorf("decode skills: %w", err)
		}
	}
	return nil
}

// assignIDs sets each finding's "id" in skills to the short ID of its
// fingerprint, unique among all the findings (render.AssignIDs). A finding
// without a fingerprint keeps what it has.
func assignIDs(skills []json.RawMessage) ([]json.RawMessage, error) {
	type found struct {
		skill   map[string]json.RawMessage
		finding []map[string]json.RawMessage
	}
	decoded := make([]found, len(skills))
	var fps []string
	for i, raw := range skills {
		if err := json.Unmarshal(raw, &decoded[i].skill); err != nil {
			return nil, fmt.Errorf("decode a skill: %w", err)
		}
		fr, ok := decoded[i].skill["findings"]
		if !ok {
			continue
		}
		if err := json.Unmarshal(fr, &decoded[i].finding); err != nil {
			return nil, fmt.Errorf("decode findings: %w", err)
		}
		for _, f := range decoded[i].finding {
			if fp := fingerprintOf(f); fp != "" {
				fps = append(fps, fp)
			}
		}
	}
	ids := render.AssignIDs(fps)

	out := make([]json.RawMessage, len(skills))
	for i, d := range decoded {
		if len(d.finding) == 0 {
			out[i] = skills[i]
			continue
		}
		for _, f := range d.finding {
			if fp := fingerprintOf(f); fp != "" {
				f["id"], _ = json.Marshal(ids[fp])
			}
		}
		var err error
		if d.skill["findings"], err = json.Marshal(d.finding); err != nil {
			return nil, err
		}
		if out[i], err = json.Marshal(d.skill); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fingerprintOf(f map[string]json.RawMessage) string {
	var fp string
	_ = json.Unmarshal(f["fingerprint"], &fp)
	return fp
}

// NoteUnmerged is data (a partial re-run's result) with note to show in its
// comment, for a re-run whose earlier results could not be merged: the review
// then covers only the skills that were re-run.
func NoteUnmerged(data []byte, note string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	n, err := json.Marshal(note)
	if err != nil {
		return nil, err
	}
	top["merge_note"] = n
	return json.Marshal(top)
}
