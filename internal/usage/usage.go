// Package usage reads what a review's skills used out of its result.
package usage

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

type rawResult struct {
	Merged      bool            `json:"merged"`
	Incremental json.RawMessage `json:"incremental"`
	Skills      []rawSkill      `json:"skills"`
}

type rawSkill struct {
	Name     string `json:"name"`
	Verdict  string `json:"verdict"`
	Findings []struct {
		Severity string `json:"severity"`
	} `json:"findings"`
	Tokens *struct {
		Input      int64  `json:"input"`
		Output     int64  `json:"output"`
		Reasoning  *int64 `json:"reasoning"`
		CacheRead  *int64 `json:"cache_read"`
		CacheWrite *int64 `json:"cache_write"`
	} `json:"tokens"`
}

var errNoSkills = errors.New("usage: the result has no skills")

// FromResult returns what each skill of run used, from body: the result the
// Job posted, or the one stored for run (which may be merged). Only skills
// the run itself ran count: a re-run of failed skills counts its subset, and
// a merged incremental result skips the impact skill it may have carried
// from the base run, unless the run was a re-run that named it. A skill
// without a name or with negative counts is left out; a body that isn't a
// result is an error.
func FromResult(body []byte, run store.Run) ([]store.SkillUsage, error) {
	var res rawResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	if res.Skills == nil {
		return nil, errNoSkills
	}
	subset := run.Trigger == store.TriggerRerunFailed && run.SkillSubset != nil
	mergedIncremental := res.Merged && len(res.Incremental) > 0 && string(res.Incremental) != "null"
	var out []store.SkillUsage
	for _, s := range res.Skills {
		switch {
		case s.Name == "":
			continue
		case subset && !slices.Contains(run.SkillSubset, s.Name):
			continue
		case mergedIncremental && !subset && s.Name == result.ImpactSkill:
			continue
		}
		u := store.SkillUsage{Skill: s.Name, Verdict: s.Verdict, Findings: len(s.Findings)}
		for _, f := range s.Findings {
			if f.Severity == "blocking" {
				u.Blocking++
			}
		}
		if t := s.Tokens; t != nil {
			if t.Input < 0 || t.Output < 0 || negative(t.Reasoning) || negative(t.CacheRead) || negative(t.CacheWrite) {
				continue
			}
			u.In, u.Out, u.Reasoning, u.CacheRead, u.CacheWrite = t.Input, t.Output, t.Reasoning, t.CacheRead, t.CacheWrite
		}
		out = append(out, u)
	}
	return out, nil
}

func negative(v *int64) bool { return v != nil && *v < 0 }
