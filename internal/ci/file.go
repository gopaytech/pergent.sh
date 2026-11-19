package ci

import (
	"strings"
)

const (
	fileLimit = 64 << 10
	leftOut   = "_Text left out: the file's 64 KiB limit._\n"
)

// Build writes ci-results.md: a heading per job and, for a failed (not passed)
// log job or a report job, the text in a fence. Every text is cleaned and
// redacted again here. A job read as status has no text. Past 64 KiB every
// heading stays and the later texts are left out, with a note.
func Build(s Snapshot) []byte {
	var out strings.Builder
	out.WriteString("# CI job results\n\nHead commit: " + oneLine(s.Head) + "\n")

	type part struct{ head, text string }
	parts := make([]part, len(s.Jobs))
	// min[i] is what job i needs at least: its heading and, with text, the note.
	min := make([]int, len(s.Jobs)+1)
	for i, j := range s.Jobs {
		parts[i] = part{head: heading(j), text: block(j)}
		min[i] = len(parts[i].head)
		if parts[i].text != "" {
			min[i] += len(leftOut) + 1
		}
	}
	for i := len(s.Jobs) - 1; i >= 0; i-- {
		min[i] += min[i+1]
	}
	omit := false
	for i, p := range parts {
		out.WriteString(p.head)
		if p.text == "" {
			continue
		}
		if !omit && out.Len()+len(p.text)+min[i+1] > fileLimit {
			omit = true
		}
		if omit {
			out.WriteString("\n" + leftOut)
			continue
		}
		out.WriteString(p.text)
	}
	return []byte(out.String())
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(clean(s), "\n", " "))
}

func heading(j Job) string {
	h := "\n## " + oneLine(j.Name) + ": " + string(j.Result)
	if j.Reason != "" {
		h += " (" + oneLine(j.Reason) + ")"
	}
	if j.Blocks && j.Result != Passed {
		h += " (blocks approval)"
	}
	return h + "\n"
}

// block is a job's label, fence and cut note, or "" when it has no text.
func block(j Job) string {
	var label, text, note string
	switch {
	case j.Read == "log" && j.Result != Passed:
		label, text, note = "Log tail:", j.Log, "_Cut to the last 16 KiB._"
	case j.Read == "report":
		label, text, note = "Report:", j.Report, "_Cut to the first 16 KiB._"
	}
	text = strings.TrimRight(clean(text), "\n")
	if text == "" {
		return ""
	}
	fence := strings.Repeat("`", max(3, longestRun(text, '`')+1))
	b := "\n" + label + "\n\n" + fence + "text\n" + text + "\n" + fence + "\n"
	if j.Cut {
		b += "\n" + note + "\n"
	}
	return b
}

func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}
