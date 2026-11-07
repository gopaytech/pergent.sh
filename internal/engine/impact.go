package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/links"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// holdImpact is the publisher's step for the linked repositories' check, run
// on every publish, a republish after a dismissal included. It returns the run
// carrying the result it stored, if it changed it.
//
//   - A review that was started with linked repositories always has an impact
//     skill. One that lacks it came from an image that does not know the
//     feature (the links file is ignored), so a failed skill is added: the
//     check never reads as a pass.
//   - Whatever the check found is posted on this repository's changes, and
//     visibility can change while a review runs. So the rule is checked again,
//     on fresh visibilities, before anything is posted (impactAllowed). If it
//     fails, or can't be read, what the check found is dropped and the skill is
//     failed with a fixed message; the other skills publish as they are.
//
// An error is a failure to store, or shutdown: the publish is retried.
func (e *Engine) holdImpact(ctx context.Context, run store.Run, log *slog.Logger) (store.Run, error) {
	if run.FailureReason != "" || run.Result == nil || !impactIsCheck(run) {
		// A skill named impact that the repository dispatched is its own: it
		// is held to no rule here.
		return run, nil
	}
	r, err := result.Parse(run.Result)
	if err != nil || r.Status != "completed" {
		return run, nil
	}
	data := run.Result
	if len(run.LinkedRepos) > 0 && !r.HoldsImpact() {
		out, added, err := result.AddFailedImpact(data, result.ImpactMessage(result.ImpactNotRunMessage, linkTargets(run)))
		if err != nil {
			return run, fmt.Errorf("add the failed impact check: %w", err)
		}
		if added {
			log.Warn("the review was started with linked repositories and came back without the impact check; it is failed")
			data = out
		}
	}
	if probe, perr := result.Parse(data); perr == nil && probe.HoldsImpact() {
		probeRun := run
		probeRun.Result = data
		if err := e.impactAllowed(ctx, probeRun); err != nil {
			if ctx.Err() != nil {
				return run, ctx.Err()
			}
			log.Warn("the impact check may not be posted; it is failed and its findings are left out", "reason", e.redact(err.Error()))
			out, _, ferr := result.FailImpact(data, result.ImpactWithheldMessage)
			if ferr != nil {
				return run, fmt.Errorf("fail the impact check: %w", ferr)
			}
			data = out
		}
	}
	if sameJSON(data, run.Result) {
		return run, nil
	}
	if err := e.Store.StoreMergedResult(ctx, run.ID, data); err != nil {
		return run, fmt.Errorf("store the result with its impact check settled: %w", err)
	}
	run.Result = data
	return run, nil
}

// sameJSON reports whether a and b are the same bytes. A result that is
// changed is written back whole, so there is no need to compare structure:
// FailImpact and AddFailedImpact return their input as it is when they change
// nothing.
func sameJSON(a, b []byte) bool { return string(a) == string(b) }

// impactAllowed checks the visibility rule again for the run's impact check
// on fresh visibilities, before anything of it is posted: the repository must
// be as closed as every repository the check looked into, or more. It is an
// error when the rule fails and also when it can't be read (a platform error,
// a repository the App can't see, a platform that can't tell): never a pass.
//
// The repositories are those the run was started with, those of the run it was
// merged with (a kept impact check is theirs), the repository's links now, and
// any a finding names. A finding that names a repository none of the others
// do is an error.
func (e *Engine) impactAllowed(ctx context.Context, run store.Run) error {
	plat, ok := e.Platforms.Get(run.MR.Instance)
	if !ok || plat.Forge == nil {
		return fmt.Errorf("no bots for instance %q", run.MR.Instance)
	}
	known := append([]store.Link{}, run.LinkedRepos...)
	if run.BaseRunID != nil {
		if base, found, err := e.Store.Run(ctx, *run.BaseRunID); err == nil && found {
			known = append(known, base.LinkedRepos...)
		}
	}
	if repo, found, err := e.settings().Get(ctx, run.MR.Instance, run.ProjectPath); err == nil && found {
		known = append(known, repo.LinkedRepos...)
	}
	named, err := impactRepos(run.Result)
	if err != nil {
		return err
	}
	targets, err := impactTargets(known, named)
	if err != nil {
		return err
	}
	return checkImpactTargets(ctx, run.ProjectPath, targets, plat.Forge.Visibility)
}

// impactRepos are the repositories the findings of the result's impact skill
// name as the place of a caller.
func impactRepos(data []byte) ([]string, error) {
	r, err := result.Parse(data)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Skills {
		if s.Name != result.ImpactSkill {
			continue
		}
		for _, raw := range s.Findings {
			var f result.Finding
			if json.Unmarshal(raw, &f) != nil {
				return nil, errors.New("an impact finding can't be read")
			}
			if f.Linked != nil {
				out = append(out, f.Linked.Repo)
			}
		}
	}
	return out, nil
}

// impactTargets are the distinct repositories (GitHub ignores case) the check
// has to be proven against: the links known, which must be there, and
// that every repository a finding names is one of.
func impactTargets(known []store.Link, named []string) ([]string, error) {
	var out []string
	have := map[string]bool{}
	for _, l := range known {
		if k := strings.ToLower(l.Target); !have[k] {
			have[k] = true
			out = append(out, l.Target)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no linked repository is known for the impact check")
	}
	for _, n := range named {
		if !have[strings.ToLower(n)] {
			return nil, errors.New("an impact finding names a repository that is not linked")
		}
	}
	return out, nil
}

// checkImpactTargets reads the visibility of source (a path, owner/name) and
// of each target once and applies the rule to each pair, source first. It
// stops at the first failure. A target that is not a plain owner/name, or is
// the source, is a failure, as is every read error.
func checkImpactTargets(ctx context.Context, source string, targets []string, visibility func(ctx context.Context, repo string) (forge.Visibility, error)) error {
	for _, t := range targets {
		if err := links.Validate(source, []store.Link{{Target: t}}); err != nil {
			return fmt.Errorf("the linked repository is not valid: %w", err)
		}
	}
	src, err := visibility(ctx, source)
	if err != nil {
		return fmt.Errorf("read the visibility of the repository: %w", err)
	}
	for _, t := range targets {
		tgt, err := visibility(ctx, t)
		if err != nil {
			return fmt.Errorf("read the visibility of a linked repository: %w", err)
		}
		if err := links.Check(src, tgt); err != nil {
			return err
		}
	}
	return nil
}
