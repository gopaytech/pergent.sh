// Package ci holds the rules for reading CI job results as review input. It
// imports no config package: config imports it.
package ci

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	MinWait     = 5 * time.Minute
	MaxWait     = 2 * time.Hour
	DefaultWait = 30 * time.Minute
	MaxJobs     = 10

	maxNameBytes = 255
)

// Validate checks a repository's ci_jobs: a wait from MinWait to MaxWait, at
// most MaxJobs jobs with unique names (never "pergent", in any case; no
// control characters, no space at either end), a read of
// status, log or report, and a report only with read: report, with a relative
// path with no "..", and on GitHub an artifact. No jobs is valid. The error
// names the field, such as ci_jobs.jobs[1].report.path.
func Validate(c store.CIJobs, github bool) error {
	if w := c.Wait(); w < MinWait || w > MaxWait {
		return fmt.Errorf("ci_jobs.wait must be from %s to %s, got %s", MinWait, MaxWait, w)
	}
	if len(c.Jobs) > MaxJobs {
		return fmt.Errorf("ci_jobs.jobs has at most %d jobs, got %d", MaxJobs, len(c.Jobs))
	}
	seen := make(map[string]bool, len(c.Jobs))
	for i, j := range c.Jobs {
		field := fmt.Sprintf("ci_jobs.jobs[%d]", i)
		switch {
		case j.Name == "":
			return fmt.Errorf("%s.name must not be empty", field)
		case len(j.Name) > maxNameBytes:
			return fmt.Errorf("%s.name must be at most %d bytes, got %d", field, maxNameBytes, len(j.Name))
		case strings.TrimSpace(j.Name) != j.Name:
			return fmt.Errorf("%s.name must not start or end with a space, got %q", field, j.Name)
		case strings.IndexFunc(j.Name, unicode.IsControl) >= 0:
			return fmt.Errorf("%s.name must not hold control characters, got %q", field, j.Name)
		case strings.EqualFold(j.Name, "pergent"):
			return fmt.Errorf("%s.name must not be pergent, which is pergent's own status", field)
		case seen[j.Name]:
			return fmt.Errorf("%s.name %q is used twice", field, j.Name)
		}
		seen[j.Name] = true
		switch j.Read {
		case "status", "log":
			if j.Report != nil {
				return fmt.Errorf("%s.report is only for read: report, got read: %s", field, j.Read)
			}
		case "report":
			if j.Report == nil {
				return fmt.Errorf("%s.report is required for read: report", field)
			}
			if err := validReport(*j.Report, github); err != nil {
				return fmt.Errorf("%s.report.%w", field, err)
			}
		default:
			return fmt.Errorf("%s.read must be status, log or report, got %q", field, j.Read)
		}
	}
	return nil
}

// validReport checks a report; its errors start with the field name.
func validReport(r store.CIReport, github bool) error {
	if github && r.Artifact == "" {
		return fmt.Errorf("artifact is required on GitHub")
	}
	switch {
	case r.Path == "":
		return fmt.Errorf("path must not be empty")
	case strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, `\`):
		return fmt.Errorf("path must be relative, got %q", r.Path)
	}
	for _, part := range strings.FieldsFunc(r.Path, func(c rune) bool { return c == '/' || c == '\\' }) {
		if part == ".." {
			return fmt.Errorf("path must not contain .., got %q", r.Path)
		}
	}
	return nil
}
