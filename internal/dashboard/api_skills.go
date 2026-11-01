package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// --- GET /api/skills, /api/skills/{name} and /api/skills/{name}/versions/{v} ---
//
// Every signed-in user reads the library and its history (D11); who wrote
// each version shows to admins only. A skill's text is for a <pre> or a
// <textarea>, never HTML.

func (d *Dashboard) skills(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	list, err := d.Store.LibrarySkills(ctx)
	if err != nil {
		d.failed(w, r, "listing the skill library", err)
		return
	}
	def, err := d.Store.RepoDefaults(ctx)
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	out := make([]LibrarySkillView, 0, len(list))
	for _, sk := range list {
		out = append(out, skillView(se, sk, slices.Contains(def.Skills, sk.Name)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Dashboard) skill(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	name := r.PathValue("name")
	if !inputs.ValidName(name) {
		notFound(w)
		return
	}
	sk, versions, err := d.Store.LibrarySkill(ctx, name)
	if errors.Is(err, store.ErrSkillNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		d.failed(w, r, "reading a skill", err)
		return
	}
	def, err := d.Store.RepoDefaults(ctx)
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	view := SkillDetailView{LibrarySkillView: skillView(se, sk, slices.Contains(def.Skills, name)),
		Content: sk.Content, Versions: make([]SkillVersionView, 0, len(versions))}
	for _, v := range versions {
		view.Versions = append(view.Versions, versionView(se, v))
	}
	writeJSON(w, http.StatusOK, view)
}

func (d *Dashboard) skillVersion(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	n, err := strconv.Atoi(r.PathValue("v"))
	if !inputs.ValidName(name) || err != nil || n < 1 {
		notFound(w)
		return
	}
	v, err := d.Store.LibrarySkillVersion(r.Context(), name, n)
	if errors.Is(err, store.ErrSkillNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		d.failed(w, r, "reading a skill version", err)
		return
	}
	writeJSON(w, http.StatusOK, SkillVersionDetailView{SkillVersionView: versionView(session(r), v), Content: v.Content})
}

// skillView is sk as se may see it: who last wrote it only for admins, the
// source repository's path only for operators.
func skillView(se Session, sk store.LibrarySkill, inDefaults bool) LibrarySkillView {
	v := LibrarySkillView{Name: sk.Name, Version: sk.Version, Size: sk.Size, UpdatedAt: sk.UpdatedAt.UTC(),
		InDefaults: inDefaults, Source: sourceView(se, sk.Source)}
	if se.Admin {
		v.UpdatedBy = sk.UpdatedBy
	}
	return v
}

func versionView(se Session, v store.LibrarySkillVersion) SkillVersionView {
	out := SkillVersionView{Version: v.Version, At: v.At.UTC(), Commit: v.Commit}
	if se.Admin {
		out.Actor = v.Actor
	}
	return out
}

// sourceView hides the repository an instance skill was read from from all
// but operators: Task 10 widens this to those who can see the repository.
func sourceView(se Session, s *store.SkillSource) *SkillSourceView {
	if s == nil {
		return nil
	}
	v := &SkillSourceView{Kind: s.Kind, Ref: s.Ref, Commit: s.Commit}
	switch s.Kind {
	case store.SourceKindInstance:
		v.Instance = s.Instance
		if se.Operator {
			v.Repo = s.Repo
		} else {
			v.Hidden = true
		}
	case store.SourceKindURL:
		v.URL = s.URL
	}
	return v
}

// --- the library's limits on a save of a skill list ---

// msgSkillMissing is the 400 for a skill the library lacks.
func msgSkillMissing(name string) string {
	return fmt.Sprintf("skill %q is not in the skill library", name)
}

// skillMissing maps the store's "not in the library" error, from any save
// that names skills, to the 400's message naming the skill.
func skillMissing(err error) (string, bool) {
	if !errors.Is(err, store.ErrSkillNotFound) {
		return "", false
	}
	var missing *store.MissingSkillError
	if errors.As(err, &missing) {
		return msgSkillMissing(missing.Name), true
	}
	return "the skill is not in the skill library", true
}

// checkSkillBudget says (as the 400's message) when the texts of the named
// library skills alone are over what a review takes; "" when they fit. A
// name the library lacks counts for nothing: the save names it.
func (d *Dashboard) checkSkillBudget(r *http.Request, names []string) (msg string, err error) {
	total, err := d.Store.LibrarySize(r.Context(), names)
	if err != nil {
		return "", err
	}
	if total <= inputs.ConfigMapLimit {
		return "", nil
	}
	return fmt.Sprintf("the library skills come to %d KiB; a review takes at most %d KiB",
		(total+1023)/1024, inputs.ConfigMapLimit/1024), nil
}
