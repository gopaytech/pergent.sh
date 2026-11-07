package result

import (
	"encoding/json"
	"fmt"
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
