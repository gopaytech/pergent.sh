package result

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImpactSkill is the name of the linked repositories' check in a result. It is
// not one of the repository's skills: pergent runs it, with a prompt of its
// own, when the Job was given linked repositories, and then always reports it.
const ImpactSkill = "impact"

// What the impact skill says when pergent.sh wrote it, not pergent. Both are
// fixed text: nothing from a linked repository or the model reaches them.
const (
	// ImpactNotRunMessage is the failure of a review that was started with
	// linked repositories and came back without an impact skill: the image
	// that ran it does not know the feature.
	ImpactNotRunMessage = "Not checked: the review returned no result for the linked repositories, so their callers were not looked for."
	// ImpactWithheldMessage is the failure of an impact skill whose findings
	// were left out because a linked repository could not be shown to be as
	// open as this one when the review was posted.
	ImpactWithheldMessage = "Not checked: a linked repository could not be shown to be as open as this one, so what the check found is left out."
)

// ImpactWithheldComment is what stays of the comment pergent wrote when the
// check's findings were withheld: that text may quote them, so it is replaced.
// The comment is rendered again from the skills whenever it is posted.
const ImpactWithheldComment = "The comment pergent wrote was dropped: part of it was left out because of the linked repositories' check."

// ImpactMessage is message with the linked repositories it covers named, as
// the Job was given them. The names are pergent.sh's own (the links an admin
// saved, which passed the rule for owner/name), never text from a clone or the
// model.
func ImpactMessage(message string, targets []string) string {
	if len(targets) == 0 {
		return message
	}
	return message + " Linked repositories: " + strings.Join(targets, ", ") + "."
}

// IsImpactCheck reports whether a skill called name in the result of a run is
// the linked repositories' check. A skill named impact is the repository's own,
// an ordinary skill with nothing special about it, when it is among the skills
// the run was dispatched with (sources, by name). A run that recorded none
// (nil: an older run, or the recording failed) can't say, and impact is the
// check there, as it always was. The name is reserved only while the check
// runs: dispatch refuses a run that would have both.
func IsImpactCheck(name string, sources map[string]string) bool {
	if name != ImpactSkill {
		return false
	}
	_, own := sources[name]
	return !own
}

// ForRun marks r with what IsImpactCheck says for the run it is the result of
// (sources: the skills the run was dispatched with), and returns it: after that
// IsCheck is the test every reader of the result uses. A result nobody marked
// reads as one whose impact skill is the check.
func (r *Result) ForRun(sources map[string]string) *Result {
	r.OwnImpact = !IsImpactCheck(ImpactSkill, sources)
	return r
}

// IsCheck reports whether the skill called name in r is the linked
// repositories' check (see IsImpactCheck).
func (r *Result) IsCheck(name string) bool { return name == ImpactSkill && !r.OwnImpact }

// HoldsImpact reports whether the result has a skill named impact.
func (r *Result) HoldsImpact() bool {
	for _, s := range r.Skills {
		if s.Name == ImpactSkill {
			return true
		}
	}
	return false
}

// impactIsOpen reports whether an earlier impact skill is still open: it
// failed, it has a blocking finding, or its verdict is not the clean one.
// Dismissals are not known here, so a dismissed blocking finding counts: the
// comment decides the verdict again.
func impactIsOpen(s Skill) bool {
	return s.Verdict != "no_blocking_issues" || hasBlocking(s.Findings)
}

// failedImpact is the impact skill pergent.sh writes when the check did not
// happen or may not be shown: failed, with no finding, no file and message as
// its one failure.
func failedImpact(message string) (json.RawMessage, error) {
	return json.Marshal(Skill{
		Name: ImpactSkill, Verdict: "failed", Findings: []json.RawMessage{},
		Files:    Files{Reviewed: []string{}, Skipped: []Skipped{}, NotReviewed: []json.RawMessage{}},
		Failures: []Failure{{Files: []string{}, Message: message}},
	})
}

// isFailedImpact reports whether s is the skill failedImpact(message) writes
// (decoded, since a database may respace the JSON).
func isFailedImpact(s Skill, message string) bool {
	return s.Verdict == "failed" && len(s.Findings) == 0 && len(s.Notes) == 0 && len(s.Summaries) == 0 &&
		len(s.Checks) == 0 && len(s.Failures) == 1 && s.Failures[0].Message == message &&
		len(s.Files.Reviewed) == 0 && len(s.Files.Carried) == 0 && len(s.Files.NotReviewed) == 0 &&
		s.Confidence == 0 && s.ConfidenceReason == ""
}

// FailImpact is data with every impact skill replaced by a failed one that has
// message as its only failure: nothing else of the check stays (findings,
// notes, summaries, checks), as any of it may quote a repository that may not
// be shown. Like Merge, it keeps every field of the result it does not edit.
// changed is false, and data is returned as it is, when it has no impact skill
// or its impact is that failed skill already.
func FailImpact(data []byte, message string) (out []byte, changed bool, err error) {
	var top map[string]json.RawMessage
	var raws []json.RawMessage
	if err := unmarshalSkills(data, &top, &raws); err != nil {
		return data, false, err
	}
	var skills []Skill
	if err := json.Unmarshal(top["skills"], &skills); err != nil {
		return data, false, fmt.Errorf("decode skills: %w", err)
	}
	for i, s := range skills {
		if s.Name != ImpactSkill || isFailedImpact(s, message) {
			continue
		}
		if raws[i], err = failedImpact(message); err != nil {
			return data, false, err
		}
		changed = true
	}
	if !changed {
		return data, false, nil
	}
	if top["skills"], err = json.Marshal(raws); err != nil {
		return data, false, err
	}
	// The comment pergent wrote may quote what was withheld, and the stored
	// result must not keep it. Parse wants some text, so a fixed one stays.
	if top["comment"], err = json.Marshal(Comment{Markdown: ImpactWithheldComment}); err != nil {
		return data, false, err
	}
	if out, err = json.Marshal(top); err != nil {
		return data, false, err
	}
	return out, true, nil
}

// AddFailedImpact is data with a failed impact skill added last when it is a
// completed result that has none, with message as its only failure. added is
// false, and data is returned as it is, for any other result. A review that
// was started with linked repositories always has an impact skill: one that
// lacks it came from an image that does not know the feature, and must not
// read as a pass.
func AddFailedImpact(data []byte, message string) (out []byte, added bool, err error) {
	r, err := Parse(data)
	if err != nil {
		return data, false, err
	}
	if r.Status != "completed" || r.HoldsImpact() {
		return data, false, nil
	}
	var top map[string]json.RawMessage
	var raws []json.RawMessage
	if err := unmarshalSkills(data, &top, &raws); err != nil {
		return data, false, err
	}
	skill, err := failedImpact(message)
	if err != nil {
		return data, false, err
	}
	if top["skills"], err = json.Marshal(append(raws, skill)); err != nil {
		return data, false, err
	}
	if out, err = json.Marshal(top); err != nil {
		return data, false, err
	}
	return out, true, nil
}
