// Package security reads what a security scan Job sends back: the envelope,
// its SARIF and its usage. It also builds what a scan needs on the way in: the
// prior file and the review input. It has no database and no network.
package security

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The size limits and the defaults of a security scan.
const (
	MaxBodyBytes      = 12 << 20
	MaxStateBytes     = 512 << 10 // in all, and per file
	MaxSARIFBytes     = 8 << 20
	MaxPriorBytes     = 800 << 10
	MaxFindings       = 5000
	MaxDismissedBytes = 256 << 10
	ReviewInputLimit  = 32 << 10

	DefaultMaxSteps         = 600
	DefaultSubagentMaxSteps = 100
	DefaultTimeout          = 3 * time.Hour
	MinTimeout              = 10 * time.Minute
	MaxTimeout              = 6 * time.Hour

	// UsageSkill is the skill name of a scan's usage row.
	UsageSkill = "security-scan"
	// Withheld replaces a text that held a secret pergent.sh knows.
	Withheld = "[withheld: it held a secret pergent.sh knows]"
)

// Envelope is the body pergent sends when a scan ends. Skill is nil, or has
// an empty name, when the scan ended before the skill was known. Coverage is
// nil when the body has none. StateDropped counts the state files
// ParseEnvelope did not keep, over MaxStateBytes.
type Envelope struct {
	Kind     string            `json:"kind"`
	Status   string            `json:"status"`
	Reason   string            `json:"reason"`
	Commit   string            `json:"commit"`
	Skill    *Skill            `json:"skill"`
	Usage    Usage             `json:"usage"`
	Coverage *Coverage         `json:"coverage"`
	State    map[string]string `json:"state"`
	SARIF    json.RawMessage   `json:"sarif"`

	StateDropped int `json:"-"`
}

// Skill names the skill that made the scan and its pinned commit.
type Skill struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

// Coverage is how many of the planned parts the scan finished.
type Coverage struct {
	Done    int `json:"done"`
	Planned int `json:"planned"`
}

// Usage is what the scan used. Source is stats, stats_poll or stream.
type Usage struct {
	Source    string       `json:"source"`
	Sessions  int          `json:"sessions"`
	Subagents int          `json:"subagents"`
	Steps     int          `json:"steps"`
	Tokens    Tokens       `json:"tokens"`
	Models    []ModelUsage `json:"models"`
}

// ModelUsage is one model's share of the usage.
type ModelUsage struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Steps    int    `json:"steps"`
	Tokens   Tokens `json:"tokens"`
}

// Tokens are token counts.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ParseEnvelope reads a scan result body. Unknown fields are ignored. Every
// text has its NUL characters removed, because Postgres refuses them in text
// and jsonb and one would fail the whole result.
func ParseEnvelope(body []byte) (Envelope, error) {
	var env Envelope
	if len(body) > MaxBodyBytes {
		return env, errors.New("the body is too big")
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("the body is not a valid envelope: %w", err)
	}
	switch {
	case env.Kind != "security":
		return Envelope{}, errors.New(`kind must be "security"`)
	case env.Status != "complete" && env.Status != "partial" && env.Status != "failed":
		return Envelope{}, errors.New("status must be complete, partial or failed")
	case !commitRe.MatchString(env.Commit):
		return Envelope{}, errors.New("commit must be a 40-character lowercase hex SHA")
	case env.Usage.Source != "stats" && env.Usage.Source != "stats_poll" && env.Usage.Source != "stream":
		return Envelope{}, errors.New("usage.source must be stats, stats_poll or stream")
	}
	if err := checkUsage(env.Usage); err != nil {
		return Envelope{}, err
	}
	if c := env.Coverage; c != nil && (c.Done < 0 || c.Planned < 0) {
		return Envelope{}, errors.New("coverage must not be negative")
	}

	env.Reason = noNUL(env.Reason)
	if env.Skill != nil {
		env.Skill = &Skill{Name: noNUL(env.Skill.Name), Commit: noNUL(env.Skill.Commit)}
	}
	env.Usage.Source = noNUL(env.Usage.Source)
	for i, m := range env.Usage.Models {
		env.Usage.Models[i].Provider, env.Usage.Models[i].Model = noNUL(m.Provider), noNUL(m.Model)
	}
	if env.State != nil {
		// State is only the prior of the next scan, the findings are the
		// product: a file over a cap is dropped, never the whole result. The
		// keys are read in sorted order so the same body keeps the same files.
		keys := make([]string, 0, len(env.State))
		for k := range env.State {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		state := make(map[string]string, len(env.State))
		total := 0
		for _, k := range keys {
			v := env.State[k]
			if len(v) > MaxStateBytes || total+len(k)+len(v) > MaxStateBytes {
				env.StateDropped++
				continue
			}
			total += len(k) + len(v)
			state[noNUL(k)] = noNUL(v)
		}
		env.State = state
	}
	env.SARIF = stripNULEscapes(env.SARIF)
	return env, nil
}

func checkUsage(u Usage) error {
	if u.Sessions < 0 || u.Subagents < 0 || u.Steps < 0 || negative(u.Tokens) {
		return errors.New("usage must not be negative")
	}
	for _, m := range u.Models {
		if m.Steps < 0 || negative(m.Tokens) {
			return errors.New("usage must not be negative")
		}
	}
	return nil
}

func negative(t Tokens) bool {
	return t.Input < 0 || t.Output < 0 || t.Reasoning < 0 || t.CacheRead < 0 || t.CacheWrite < 0
}

// noNUL removes NUL characters.
func noNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// stripNULEscapes removes every \u0000 escape from JSON text. It reads the
// escapes pair by pair, so the text `\\u0000` (an escaped backslash and then
// u0000) stays as it is.
func stripNULEscapes(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			if bytes.HasPrefix(raw[i:], []byte(`\u0000`)) {
				i += 5
				continue
			}
			out = append(out, raw[i], raw[i+1])
			i++
			continue
		}
		out = append(out, raw[i])
	}
	return out
}

// cut returns s cut to at most n bytes, never inside a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// firstLine returns the text up to the first line break.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
