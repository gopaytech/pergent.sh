package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/links"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// impactSkill is the name pergent gives the linked repositories' check in the
// result.
const impactSkill = "impact"

// What a link's entry says when it can't be cloned. pergent shows the text in
// the comment, so each one is fixed and short: nothing from the platform, the
// link or an error reaches it.
const (
	linkErrInvalid     = "the links of this repository are not valid"
	linkErrVisibility  = "the visibility could not be read"
	linkErrNotReadable = "the pergent review App can't read this repository"
	linkErrRefused     = "a linked repository must be as open as this one or more"
	linkErrLookup      = "the repository could not be looked up"
	linkErrInstall     = "it is in another installation of the review App"
	linkErrURL         = "the clone address is not https"
)

// linkedEntry is one entry of linked-repos.json, the file pergent reads. A
// non-empty Error, or a Path that was not cloned, makes the check failed.
type linkedEntry struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Watched  []string `json:"watched"`
	CanBlock bool     `json:"can_block"`
	Error    string   `json:"error"`
}

// linkPlan is what a review does about its linked repositories. The zero
// plan is none: nothing about the Job changes.
type linkPlan struct {
	// Matched are the links the change touched, recorded on the run.
	Matched []store.Link
	Entries []linkedEntry
	// Clones are the entries that can be cloned, RepoIDs their repositories
	// (the clone token covers them next to the checkout).
	Clones  []kube.LinkedClone
	RepoIDs []int64
}

// linkInput is what planLinks decides from.
type linkInput struct {
	// Source is the reviewed repository's path, SourceInstallation the review
	// App's installation on it and RepoURL its clone URL.
	Source             string
	SourceInstallation int64
	RepoURL            string
	Links              []store.Link
	// All makes every link count as matched; otherwise Files (every path the
	// change touches, a rename's old path too) decide.
	All   bool
	Files []string

	Visibility func(ctx context.Context, repo string) (forge.Visibility, error)
	Target     func(ctx context.Context, repo string) (githubforge.Target, error)
}

// planLinks matches the links against the change and decides, for each match,
// whether it may be cloned. The rules are checked again here, whatever was
// checked when the links were saved: visibility changes, and a platform that
// can't be asked is an error entry, never a pass.
func planLinks(ctx context.Context, in linkInput) linkPlan {
	var matched []store.Link
	if in.All {
		matched = in.Links
	} else {
		matched = links.Match(in.Links, in.Files)
	}
	if len(matched) == 0 {
		return linkPlan{}
	}
	if len(matched) > links.Max {
		// Only an invalid set has more; its entries all carry an error.
		matched = matched[:links.Max]
	}
	plan := linkPlan{Matched: matched}

	invalid := links.Validate(in.Source, in.Links) != nil
	var (
		srcVis    forge.Visibility
		srcVisErr error
		srcRead   bool
	)
	base, baseErr := cloneBase(in.RepoURL)

	for i, l := range matched {
		dir := "linked-" + strconv.Itoa(i+1)
		entry := linkedEntry{
			Name: l.Target, Path: "/work/" + dir, CanBlock: l.CanBlock,
			Watched: append([]string{}, l.Watched...),
		}
		var target githubforge.Target
		switch {
		case invalid:
			entry.Error = linkErrInvalid
		default:
			if !srcRead {
				srcVis, srcVisErr = in.Visibility(ctx, in.Source)
				srcRead = true
			}
			tgtVis, err := in.Visibility(ctx, l.Target)
			switch {
			case errors.Is(err, forge.ErrSourceNotFound):
				entry.Error = linkErrNotReadable
			case err != nil || srcVisErr != nil:
				entry.Error = linkErrVisibility
			case links.Check(srcVis, tgtVis) != nil:
				entry.Error = linkErrRefused
			}
		}
		if entry.Error == "" {
			var err error
			target, err = in.Target(ctx, l.Target)
			switch {
			case errors.Is(err, forge.ErrSourceNotFound):
				entry.Error = linkErrNotReadable
			case err != nil:
				entry.Error = linkErrLookup
			case target.InstallationID != in.SourceInstallation:
				// One token covers the checkout and the targets, and a token
				// belongs to one installation.
				entry.Error = linkErrInstall
			case baseErr != nil:
				entry.Error = linkErrURL
			}
		}
		if entry.Error == "" {
			plan.Clones = append(plan.Clones, kube.LinkedClone{Dir: dir, CloneURL: base + "/" + l.Target + ".git"})
			plan.RepoIDs = append(plan.RepoIDs, target.RepoID)
		}
		plan.Entries = append(plan.Entries, entry)
	}
	return plan
}

// cloneBase is "https://host" of the reviewed repository's clone URL: a
// linked repository lives on the same host. Anything else than a plain https
// URL is refused.
func cloneBase(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("not a plain https clone URL")
	}
	return "https://" + u.Host, nil
}

// impactOpen reports whether the earlier review leaves the linked
// repositories' check open: an impact skill that failed, or one with a
// blocking finding nobody dismissed.
func impactOpen(r *result.Result, ds []render.Dismissal) bool {
	for i, s := range render.ApplyDismissals(result.ToRender(r), ds) {
		if r.Skills[i].Name != impactSkill {
			continue
		}
		if s.Verdict == "failed" {
			return true
		}
		for _, f := range s.Findings {
			if f.Severity == render.Blocking {
				return true
			}
		}
	}
	return false
}

// linkedRepos decides what this run does about linked repositories. It is
// none (the zero plan, no request, no write) unless the repository has the
// setting on, has links and is on GitHub.
//
// Every link counts as matched while the earlier review's impact check is
// open: a push that touches no watched path must not lose it, as the result
// of an incremental review keeps only the skills it ran. An earlier result
// that can't be read counts as open. Otherwise the files that count are those
// the run reviews: since the earlier review's head for an incremental review
// (in.Carried set), since the merge base for any other. A list that can't be
// had matches every link.
func (d *dispatcher) linkedRepos(ctx context.Context, plat Platform, repo settings.Effective, project store.Project, mr forge.Change, last store.Run, haveLast bool, in *inputs.Inputs) linkPlan {
	if !repo.LinkedReposEnabled || len(repo.LinkedRepos) == 0 || !plat.isGitHub() {
		return linkPlan{}
	}
	finder, ok := plat.Forge.(interface {
		LinkTarget(ctx context.Context, repo string) (githubforge.Target, error)
	})
	source := project.Path
	if source == "" {
		source = d.run.ProjectPath
	}
	all := d.impactIsOpen(ctx, last, haveLast)
	var files []string
	if !all {
		var err error
		if files, err = d.changedForLinks(ctx, plat, mr, last, in); err != nil {
			d.log.Warn("every linked repository counts as touched: the changed files can't be listed", "error", d.redact(err.Error()))
			all = true
		}
	}
	target := func(context.Context, string) (githubforge.Target, error) {
		return githubforge.Target{}, errors.New("this platform can't look up a linked repository")
	}
	if ok {
		target = finder.LinkTarget
	}
	plan := planLinks(ctx, linkInput{
		Source: source, SourceInstallation: project.InstallationID, RepoURL: project.HTTPURLToRepo,
		Links: repo.LinkedRepos, All: all, Files: files,
		Visibility: plat.Forge.Visibility, Target: target,
	})
	for _, e := range plan.Entries {
		if e.Error != "" {
			d.log.Warn("a linked repository can't be cloned", "link", e.Name, "reason", e.Error)
		}
	}
	return plan
}

// impactIsOpen reads the earlier review for an open impact check. Nothing it
// can't read counts as closed.
func (d *dispatcher) impactIsOpen(ctx context.Context, last store.Run, haveLast bool) bool {
	if !haveLast {
		return false
	}
	r, err := result.Parse(last.Result)
	if err != nil {
		d.log.Warn("every linked repository counts as touched: the last result can't be read", "error", err)
		return true
	}
	for _, sk := range r.Skills {
		if sk.Name == impactSkill && sk.Verdict == "failed" {
			// No dismissal closes a failed check.
			return true
		}
	}
	if !impactOpen(r, nil) {
		// A dismissal only closes a finding: with nothing open before them,
		// there is nothing to look up.
		return false
	}
	ds, err := d.e.dismissals(ctx, d.run.MR)
	if err != nil {
		// Without the dismissals every blocking finding is open: the safe side.
		d.log.Warn("reading the impact check without dismissals", "error", d.redact(err.Error()))
		ds = nil
	}
	return impactOpen(r, ds)
}

// changedForLinks lists the paths the run reviews, a rename's old path too.
func (d *dispatcher) changedForLinks(ctx context.Context, plat Platform, mr forge.Change, last store.Run, in *inputs.Inputs) ([]string, error) {
	inline, ok := plat.Forge.(forge.Inline)
	if !ok {
		return nil, errors.New("the platform can't compare commits")
	}
	from := mr.BaseSHA
	if len(in.Carried) > 0 {
		from = last.HeadSHA
	}
	return inline.ChangedSince(ctx, d.run.MR.ProjectID, from, d.run.HeadSHA)
}

// encode is linked-repos.json for the plan; nil for none.
func (p linkPlan) encode() ([]byte, error) {
	if len(p.Entries) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(p.Entries)
	if err != nil {
		return nil, fmt.Errorf("encode the linked repositories: %w", err)
	}
	return b, nil
}
