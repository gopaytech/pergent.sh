package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Why a push is reviewed in full, as the log says it.
const (
	fullSettingOff   = "setting off"
	fullNoEarlier    = "no earlier review"
	fullTarget       = "target branch changed"
	fullSameHead     = "same head"
	fullNoCompare    = "the platform can't compare commits"
	fullUnreadable   = "the last review can't be read"
	fullOlderPergent = "older pergent"
	fullPartial      = "the last review is partial"
	fullNothing      = "nothing to carry"
	fullTooLarge     = "inputs too large"
	fullImpactOpen   = "an open linked-repositories check can't be repeated"
	fullModelChanged = "the model changed"
)

// Why a skill carries no files, as the log says it.
const (
	carryChanged     = "changed"
	carryOutside     = "finding outside reviewed files"
	carryOpenOutside = "open blocking finding outside reviewed files"
	carryUnchanged   = "nothing unchanged"
)

// planCarried decides whether the push run reviews only what changed since
// the last review, and which files each skill carries over from it. A full
// review has no files and the reason, for the log; an error is the store's.
// The cheap conditions come first, so the platform is asked about the
// changed files only for a push that can be incremental.
func (d *dispatcher) planCarried(ctx context.Context, plat Platform, repo settings.Effective, last store.Run, haveLast bool, in *inputs.Inputs) (map[string][]string, string, error) {
	run := d.run
	switch {
	case !repo.IncrementalReviews:
		return nil, fullSettingOff, nil
	case !haveLast:
		return nil, fullNoEarlier, nil
	case last.TargetBranch != run.TargetBranch:
		return nil, fullTarget, nil
	case last.HeadSHA == run.HeadSHA:
		return nil, fullSameHead, nil
	case modelChanged(last, d.provider, d.model):
		// What the files carried over were reviewed by is not what reviews
		// the rest, and the findings of one model are not the other's.
		return nil, fullModelChanged, nil
	case imagePinnedBeforeCarried(d.e.Config.Jobs.Image):
		return nil, fullOlderPergent, nil
	}
	inline, ok := plat.Forge.(forge.Inline)
	if !ok {
		return nil, fullNoCompare, nil
	}
	lastResult, err := parsedFor(last, last.Result)
	switch {
	case err != nil:
		return nil, fullUnreadable, nil
	case !lastResult.SupportsCarried():
		return nil, fullOlderPergent, nil
	case lastResult.MergeNote != "":
		return nil, fullPartial, nil
	}
	changed, err := inline.ChangedSince(ctx, run.MR.ProjectID, last.HeadSHA, run.HeadSHA)
	if err != nil {
		// Not an ancestor (a rebase or a force-push), a list that may be
		// partial, or the platform's trouble: the review is a full one.
		return nil, d.redact(err.Error()), nil
	}
	ds, err := d.e.dismissals(ctx, run.MR)
	if err != nil {
		return nil, "", err
	}
	carried := carriedFiles(lastResult, last.SkillHashes, skillHashes(in.Skills), changed, ds, d.log)
	if len(carried) == 0 {
		return nil, fullNothing, nil
	}
	return carried, "", nil
}

// modelChanged reports whether a review on provider and model differs from
// last, which ran on the pair it recorded. A run that recorded no model (it
// predates the list, or ended before a model was chosen) and an unresolved
// pair are no change. A run from before the provider was recorded has a model
// only: it is compared by the model, so an upgrade does not turn the next push
// into a full review when the model is the same.
func modelChanged(last store.Run, provider, model string) bool {
	if last.Model == nil || (provider == "" && model == "") {
		return false
	}
	if *last.Model != model {
		return true
	}
	return last.Provider != nil && *last.Provider != provider
}

// imagePinnedBeforeCarried reports whether the Jobs image is tagged with a
// pergent version that doesn't take --carried, which it would stop at. An
// operator can pin the image back after running a newer one, and the last
// result alone then says nothing about the pergent that runs next. An image
// with no tag (a digest) or a tag that is not a version is not known to be
// older.
func imagePinnedBeforeCarried(image string) bool {
	supports, known := result.CarriedSupport(imageTag(image))
	return known && !supports
}

// imageTag is the tag of an image reference, or "" when it has none. A digest
// after the tag is ignored; a colon before the last slash is a registry port,
// not a tag.
func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return ""
	}
	return image[i+1:]
}

// setIncremental records, once the inputs are fitted, whether the run reviews
// only what changed since last: it does when it still has carried files. It is
// called for every push, so a run that is dispatched again is decided afresh.
// hadCarried says the run had files to carry before the inputs were fitted.
func (d *dispatcher) setIncremental(ctx context.Context, last store.Run, in *inputs.Inputs, hadCarried bool) error {
	var base *string
	switch {
	case len(in.Carried) > 0:
		base = &last.ID
	case hadCarried:
		d.log.Info("full review", "reason", fullTooLarge)
	}
	if err := d.e.Store.SetIncremental(ctx, d.run.ID, base); err != nil {
		return fmt.Errorf("record the review as incremental: %w", err)
	}
	return nil
}

// carriedFiles is, for each skill, the files the new run leaves out of its
// review because the last review covers them: the files in the skill's
// files.reviewed that did not change between the two heads and have no open
// blocking finding (one that was dismissed is not open). A file that was not
// reviewed last time, or whose batch failed, is reviewed again.
//
// A skill has no entry when its content is not what the last review used
// (its hash is missing on either side, or differs), when one of its findings
// is on a file outside its files.reviewed (nothing keeps a model's finding
// inside the diff, and carrying the other files would lose it on the next
// push), and when nothing is left to carry. Dismissed findings count for the
// last rule, except a reference finding (see referenceFinding): pergent's
// --check-deleted raises those outside the diff on purpose. A reference
// finding that is a suggestion or was dismissed lets the skill carry, and is
// lost from the merged result unless the new run reports it again. One that
// is open and blocking does not: a carried deleted file gets no new reference
// check, so losing it could approve the change. The paths are sorted.
//
// Each skill that carries nothing is logged with the reason, so one that
// falls back to a full review while the others carry can be told: "changed"
// (a new or edited skill, or one it can't tell), "finding outside reviewed
// files", "open blocking finding outside reviewed files" or "nothing
// unchanged".
func carriedFiles(last *result.Result, lastHashes, hashes map[string]string, changed []string, ds []render.Dismissal, log *slog.Logger) map[string][]string {
	changedSet := map[string]bool{}
	for _, p := range changed {
		changedSet[p] = true
	}
	names := map[string]int{}
	for _, s := range last.Skills {
		names[s.Name]++
	}
	// ToRender and ApplyDismissals keep the order of last.Skills.
	open := render.ApplyDismissals(result.ToRender(last), ds)
	gone := gonePaths(last)

	nothing := func(skill, reason string) {
		log.Info("nothing carried for a skill", "skill", skill, "reason", reason)
	}
	out := map[string][]string{}
	for i, s := range last.Skills {
		if last.IsCheck(s.Name) {
			// The linked repositories' check has no content to hash and no file
			// to carry: dispatch decides whether it runs again.
			continue
		}
		now, was := hashes[s.Name], lastHashes[s.Name]
		if names[s.Name] != 1 || now == "" || now != was {
			nothing(s.Name, carryChanged)
			continue
		}
		reviewed := map[string]bool{}
		for _, p := range s.Files.Reviewed {
			reviewed[p] = true
		}
		inside := true
		for _, raw := range s.Findings {
			var f result.Finding
			if json.Unmarshal(raw, &f) != nil || !reviewed[f.File] && !referenceFinding(s, f.File, gone) {
				inside = false
				break
			}
		}
		if !inside {
			nothing(s.Name, carryOutside)
			continue
		}
		blocked := map[string]bool{}
		openOutside := false
		for _, f := range open[i].Findings {
			if f.Severity == render.Blocking {
				blocked[f.File] = true
				openOutside = openOutside || !reviewed[f.File]
			}
		}
		if openOutside {
			nothing(s.Name, carryOpenOutside)
			continue
		}
		var files []string
		for p := range reviewed {
			if !changedSet[p] && !blocked[p] {
				files = append(files, p)
			}
		}
		if len(files) == 0 {
			nothing(s.Name, carryUnchanged)
			continue
		}
		slices.Sort(files)
		out[s.Name] = files
	}
	// A skill the last review lacks is new: it reviews all its files.
	for _, name := range slices.Sorted(maps.Keys(hashes)) {
		if names[name] == 0 {
			nothing(name, carryChanged)
		}
	}
	return out
}

// referenceFinding reports whether a finding on file, outside skill s's
// files.reviewed, is a reference finding: one on a remaining file that still
// uses a file the change deleted or renamed. It is one when s lists deleted or
// renamed paths (pergent's --check-deleted checked references for them) and
// file names a file that can still be there: not empty and not in gone. With
// neither list, as with the switch off or an older pergent, no finding is one.
func referenceFinding(s result.Skill, file string, gone map[string]bool) bool {
	return len(s.Files.Deleted)+len(s.Files.Renamed) > 0 && file != "" && !gone[file]
}

// gonePaths is every path the last review knows the change removed, in any
// skill: the reviewed deletions, the old paths of renames, and the files
// skipped as deleted. A finding on one is about the file itself, never a
// reference to it.
func gonePaths(last *result.Result) map[string]bool {
	gone := map[string]bool{}
	for _, s := range last.Skills {
		for _, p := range s.Files.Deleted {
			gone[p] = true
		}
		for _, r := range s.Files.Renamed {
			gone[r.From] = true
		}
		for _, f := range s.Files.Skipped {
			if f.Rule == "deleted" {
				gone[f.Path] = true
			}
		}
	}
	return gone
}

// skillHashes is the SHA-256 of each skill's content, in hex. A run records
// them, and the next push carries nothing for a skill whose hash changed.
func skillHashes(skills map[string][]byte) map[string]string {
	out := make(map[string]string, len(skills))
	for name, content := range skills {
		sum := sha256.Sum256(content)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out
}
