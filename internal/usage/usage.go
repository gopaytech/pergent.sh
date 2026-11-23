// Package usage reads what a review's skills used out of its result.
package usage

import (
	"context"
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

// Usage is what to record of one run's result: what each skill used, and
// whether the run is a multi review's judge run.
type Usage struct {
	Skills []store.SkillUsage
	Judge  bool
}

// Recorder is the part of the store a run's usage is written with.
type Recorder interface {
	RecordUsage(ctx context.Context, id string, skills []store.SkillUsage, models []store.ModelUsage) error
	RecordJudgeUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// Rows returns what to record of run's result body: what each skill used
// (FromResult), for a multi review's judge run when judge is true. An
// unreadable result is an error with no skills; the run is still marked
// when it is recorded, so it is not read again.
func Rows(body []byte, run store.Run, judge bool) (Usage, error) {
	skills, err := FromResult(body, run)
	if err != nil {
		skills = nil
	}
	return Usage{Skills: skills, Judge: judge}, err
}

// Record writes u for run id. A judge run's tokens are added to the run's
// rows (RecordJudgeUsage), which hold its reviewers' tokens when it is the
// run that was split, with one judge row per skill. RecordUsage keeps a row
// already there, so it would drop the judge's tokens. Any other run is
// recorded as before, with no model rows.
func (u Usage) Record(ctx context.Context, st Recorder, id string) error {
	if u.Judge {
		return st.RecordJudgeUsage(ctx, id, u.Skills)
	}
	return st.RecordUsage(ctx, id, u.Skills, nil)
}
