package ci

import (
	"strings"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func blocks(j store.CIJobResult) bool { return j.Blocks && j.Result != string(Passed) }

// Line is the comment's one CI line, "" when there are no jobs.
func Line(jobs []store.CIJobResult) string {
	if len(jobs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(jobs))
	for _, j := range jobs {
		p := j.Name + " " + j.Result
		if j.Reason != "" {
			p += " (" + j.Reason + ")"
		}
		if blocks(j) {
			p += " (blocks approval)"
		}
		parts = append(parts, p)
	}
	return "CI: " + strings.Join(parts, " · ")
}

// Blocking is the jobs that stop an approval: chosen as blocking and not passed.
func Blocking(jobs []store.CIJobResult) []store.CIJobResult {
	var out []store.CIJobResult
	for _, j := range jobs {
		if blocks(j) {
			out = append(out, j)
		}
	}
	return out
}
