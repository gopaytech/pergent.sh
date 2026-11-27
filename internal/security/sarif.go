package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	maxTextBytes        = 8 << 10
	maxTitleBytes       = 200
	maxFingerprintBytes = 256
	maxLine             = 10_000_000
	maxTags             = 20
	maxTagBytes         = 100
	primaryFingerprint  = "security-audit/v1"
)

// Parsed is what ReadSARIF reads: the skill from the SARIF's driver, the
// findings, and whether the findings were cut at MaxFindings.
type Parsed struct {
	SkillName, SkillCommit string
	Findings               []store.SecurityFindingInput
	Capped                 bool
}

type sarifLog struct {
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				Rules   []struct {
					ID         string `json:"id"`
					Properties struct {
						Tags []string `json:"tags"`
					} `json:"properties"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []sarifResult `json:"results"`
	} `json:"runs"`
}

type sarifResult struct {
	RuleID  string `json:"ruleId"`
	Level   string `json:"level"`
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
	Locations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI string `json:"uri"`
			} `json:"artifactLocation"`
			Region struct {
				StartLine int `json:"startLine"`
			} `json:"region"`
		} `json:"physicalLocation"`
	} `json:"locations"`
	Fingerprints        map[string]string `json:"fingerprints"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Fixes               []struct {
		Description struct {
			Text string `json:"text"`
		} `json:"description"`
	} `json:"fixes"`
	Properties map[string]any `json:"properties"`
}

var (
	cweRe    = regexp.MustCompile(`^CWE-[0-9]+$`)
	schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// ReadSARIF reads the findings of a scan's SARIF. No SARIF, or a null one, is
// no findings. The version must be 2.1.0. Of results with one fingerprint the
// first stays. A result with a file that is not a relative path inside the
// repository stays, with the file "".
func ReadSARIF(raw json.RawMessage) (Parsed, error) {
	var p Parsed
	if len(raw) > MaxSARIFBytes {
		return p, errors.New("the SARIF is too big")
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" {
		return p, nil
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		return p, fmt.Errorf("the SARIF is not valid: %w", err)
	}
	if log.Version != "2.1.0" {
		return p, fmt.Errorf("the SARIF version must be 2.1.0, got %q", cut(noNUL(log.Version), 20))
	}
	if len(log.Runs) == 0 {
		return p, nil
	}
	run := log.Runs[0]
	p.SkillName = cut(noNUL(run.Tool.Driver.Name), maxTitleBytes)
	p.SkillCommit = cut(noNUL(run.Tool.Driver.Version), maxTitleBytes)
	tags := map[string][]string{}
	for _, r := range run.Tool.Driver.Rules {
		tags[r.ID] = r.Properties.Tags
	}
	seen := map[string]bool{}
	for _, r := range run.Results {
		f := finding(r, tags[r.RuleID])
		if seen[f.Fingerprint] {
			continue
		}
		if len(p.Findings) == MaxFindings {
			p.Capped = true
			break
		}
		seen[f.Fingerprint] = true
		p.Findings = append(p.Findings, f)
	}
	return p, nil
}

func finding(r sarifResult, ruleTags []string) store.SecurityFindingInput {
	text := func(s string) string { return cut(noNUL(s), maxTextBytes) }
	prop := func(key string) string {
		s, _ := r.Properties[key].(string)
		return text(s)
	}
	f := store.SecurityFindingInput{
		RuleID:          text(r.RuleID),
		Severity:        severityOf(r),
		Title:           strings.TrimSpace(cut(firstLine(noNUL(r.Message.Text)), maxTitleBytes)),
		Description:     prop("description"),
		Exploit:         prop("exploit_scenario"),
		FixHint:         prop("fix"),
		Confidence:      text(numberText(r.Properties["confidence"])),
		NeedsValidation: r.Properties["needs_validation"] == true,
	}
	if f.Description == "" {
		f.Description = text(r.Message.Text)
	}
	if f.FixHint == "" && len(r.Fixes) > 0 {
		f.FixHint = text(r.Fixes[0].Description.Text)
	}
	if len(r.Locations) > 0 {
		loc := r.Locations[0].PhysicalLocation
		f.File = cleanFile(loc.ArtifactLocation.URI)
		// start_line is a Postgres integer: a line no file has is 0.
		if l := loc.Region.StartLine; l > 0 && l <= maxLine {
			f.StartLine = l
		}
	}
	for _, t := range ruleTags {
		t = cut(noNUL(t), maxTagBytes)
		if f.CWE == "" && cweRe.MatchString(t) {
			f.CWE = t
		}
		if len(f.Tags) < maxTags {
			f.Tags = append(f.Tags, t)
		}
	}
	f.Fingerprint = fingerprintOf(r, f)
	return f
}

// fingerprintOf picks the identity of a finding: the skill's own, else the
// first other value in sorted key order, else a hash of the rule, the file and
// the title (not the line, which moves).
func fingerprintOf(r sarifResult, f store.SecurityFindingInput) string {
	clean := func(s string) string {
		return strings.TrimSpace(cut(firstLine(strings.TrimSpace(noNUL(s))), maxFingerprintBytes))
	}
	if v := clean(r.Fingerprints[primaryFingerprint]); v != "" {
		return v
	}
	for _, m := range []map[string]string{r.Fingerprints, r.PartialFingerprints} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := clean(m[k]); v != "" {
				return v
			}
		}
	}
	h := sha256.Sum256([]byte(f.RuleID + "\x00" + f.File + "\x00" + f.Title))
	return hex.EncodeToString(h[:])
}

// severityOf reads the severity: the skill's own word, else the score, else
// the SARIF level. A result with none of them is medium.
func severityOf(r sarifResult) string {
	word, _ := r.Properties["severity"].(string)
	switch s := strings.ToLower(strings.TrimSpace(word)); s {
	case "critical", "high", "medium", "low":
		return s
	}
	if score, err := strconv.ParseFloat(strings.TrimSpace(numberText(r.Properties["security-severity"])), 64); err == nil && score > 0 {
		switch {
		case score >= 9:
			return "critical"
		case score >= 7:
			return "high"
		case score >= 4:
			return "medium"
		}
		return "low"
	}
	switch r.Level {
	case "error":
		return "high"
	case "note", "none":
		return "low"
	}
	return "medium"
}

// numberText gives a JSON number or string as text.
func numberText(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	}
	return ""
}

// cleanFile returns a relative path with no "..", or "".
func cleanFile(uri string) string {
	uri = strings.TrimPrefix(noNUL(uri), "./")
	if uri == "" || strings.HasPrefix(uri, "/") || strings.Contains(uri, `\`) || schemeRe.MatchString(uri) {
		return ""
	}
	for _, part := range strings.Split(uri, "/") {
		if part == ".." {
			return ""
		}
	}
	return cut(uri, maxTextBytes)
}

// SkillOf returns the skill of a scan: the envelope's when it names one, else
// the SARIF driver's.
func SkillOf(env Envelope, p Parsed) (name, commit string) {
	if env.Skill != nil && env.Skill.Name != "" {
		return env.Skill.Name, env.Skill.Commit
	}
	return p.SkillName, p.SkillCommit
}
