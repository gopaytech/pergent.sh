package result

import (
	"encoding/json"
	"strings"
)

// StepLimitAdvice is the hint pergent adds to a step-limit failure (see
// internal/runner/outcome.go in the pergent repository). It names a flag
// that pergent.sh sets, so a reader of the comment is sent to the setting
// instead; the two texts must change together.
const StepLimitAdvice = "(raise --max-turns to allow more steps)"

// What replaces StepLimitAdvice, by where the repository's settings live.
const (
	AdviceDashboard = "(raise Max steps in the repository's Settings to allow more steps)"
	AdviceConfig    = "(raise max_turns for this repository in pergent.sh's config to allow more steps)"
)

// ReplaceStepAdvice is data with every StepLimitAdvice that pergent wrote
// replaced by advice: in each skill's failure messages, failure cause
// messages, notes and the causes of its files not reviewed. Findings are the
// model's text and are never touched. Like Merge, it keeps every field it
// does not edit. Data that does not decode, or holds nothing to replace, is
// returned as it is, with changed false.
func ReplaceStepAdvice(data []byte, advice string) (out []byte, changed bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return data, false
	}
	text := replaceIn(advice)
	skills, ok := each(object(func(skill map[string]json.RawMessage) bool {
		failures := field(skill, "failures", each(object(func(failure map[string]json.RawMessage) bool {
			message := field(failure, "message", text)
			cause := field(failure, "cause", object(func(cause map[string]json.RawMessage) bool {
				return field(cause, "message", text)
			}))
			return message || cause
		})))
		notes := field(skill, "notes", each(text))
		files := field(skill, "files", object(func(files map[string]json.RawMessage) bool {
			return field(files, "not_reviewed", each(object(func(entry map[string]json.RawMessage) bool {
				return field(entry, "cause", text)
			})))
		}))
		return failures || notes || files
	}))(top["skills"])
	if !ok {
		return data, false
	}
	top["skills"] = skills
	out, err := json.Marshal(top)
	if err != nil {
		return data, false
	}
	return out, true
}

// edit changes a JSON value and says whether it did. A value it can't
// decode, or has nothing to change in, is returned as it is.
type edit func(raw json.RawMessage) (json.RawMessage, bool)

// replaceIn replaces StepLimitAdvice in a JSON string.
func replaceIn(advice string) edit {
	return func(raw json.RawMessage) (json.RawMessage, bool) {
		var s string
		if json.Unmarshal(raw, &s) != nil || !strings.Contains(s, StepLimitAdvice) {
			return raw, false
		}
		b, err := json.Marshal(strings.ReplaceAll(s, StepLimitAdvice, advice))
		if err != nil {
			return raw, false
		}
		return b, true
	}
}

// object applies f to the fields of a JSON object, which f edits in place and
// reports on.
func object(f func(map[string]json.RawMessage) bool) edit {
	return func(raw json.RawMessage) (json.RawMessage, bool) {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil || !f(m) {
			return raw, false
		}
		b, err := json.Marshal(m)
		if err != nil {
			return raw, false
		}
		return b, true
	}
}

// each applies e to every element of a JSON array.
func each(e edit) edit {
	return func(raw json.RawMessage) (json.RawMessage, bool) {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return raw, false
		}
		changed := false
		for i, item := range items {
			if edited, ok := e(item); ok {
				items[i], changed = edited, true
			}
		}
		if !changed {
			return raw, false
		}
		b, err := json.Marshal(items)
		if err != nil {
			return raw, false
		}
		return b, true
	}
}

// field applies e to m[key], when m has it, and reports whether it changed.
func field(m map[string]json.RawMessage, key string, e edit) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	edited, changed := e(raw)
	if changed {
		m[key] = edited
	}
	return changed
}
