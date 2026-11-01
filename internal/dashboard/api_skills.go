package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

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
// name the library lacks counts for nothing: the save names it. An empty
// list picks nothing and always fits: the store reads a nil list as "the
// default list", which is not what clearing one means.
func (d *Dashboard) checkSkillBudget(r *http.Request, names []string) (msg string, err error) {
	if len(names) == 0 {
		return "", nil
	}
	total, err := d.Store.LibrarySize(r.Context(), names)
	if err != nil {
		return "", err
	}
	return budgetMessage(total), nil
}

// budgetMessage is the 400's message for total bytes of library skills, ""
// when they fit.
func budgetMessage(total int) string {
	if total <= inputs.ConfigMapLimit {
		return ""
	}
	return fmt.Sprintf("the library skills come to %d KiB; a review takes at most %d KiB",
		(total+1023)/1024, inputs.ConfigMapLimit/1024)
}

// --- the library writes: POST/PUT/DELETE /api/admin/skills... ---
//
// Any admin writes the library (D3). Every write is checked as CheckSkill
// says and against the secrets pergent.sh holds (D7), is audited by the
// skill's name and version and never by its text (D10), and drops the kept
// inputs views, which list skills and their state.

const (
	// maxSkillBody bounds a skill write's body: a skill may be MaxSkillBytes,
	// and JSON can spell each byte in up to 6 (\u00XX). A skill over its own
	// limit is answered with the skill checks' message, not "too large".
	maxSkillBody = 1 << 20

	msgBadSkillBody   = `the body must be {"name": ..., "content": ..., "addToDefaults": true or false}`
	msgBadSkillEdit   = `the body must be {"content": ..., "version": the version you opened}`
	msgBadSkillCopy   = `the body must be {"name": the new name}`
	msgSkillNoText    = "the skill has no text"
	msgSkillSecret    = "the skill contains a secret pergent.sh holds"
	msgSkillExists    = "name already in the library"
	msgSkillStale     = "this skill changed since you opened it"
	msgSkillInstalled = "an installed skill can't be edited; copy it"
	msgSkillInUse     = "the skill is in use: take it off the repositories and the default list that name it first"
)

func msgLibraryFull() string {
	return fmt.Sprintf("the library holds %d skills, the most it can", inputs.MaxLibrarySkills)
}

func (d *Dashboard) skillWriteRoutes(mux *http.ServeMux) {
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(requireAdmin(h))) }
	admin("POST /api/admin/skills", d.createSkill)
	admin("PUT /api/admin/skills/{name}", d.updateSkill)
	admin("POST /api/admin/skills/{name}/copy", d.copySkill)
	admin("DELETE /api/admin/skills/{name}", d.deleteSkill)
}

// auditSkill records a library write: the skill's name leads the detail,
// and the entry names no instance or project, so every admin sees it.
func (d *Dashboard) auditSkill(r *http.Request, action, name, detail string) {
	if detail != "" {
		name += ": " + detail
	}
	d.audit(r.Context(), session(r), action, "", "", name)
}

// skillActor is who a version is recorded as written by.
func skillActor(se Session) string {
	return actorLabel(se.Kind, se.Platform, se.Instance, se.Username)
}

// refuseSkill answers 400 with why name and content can't be a library
// skill, and reports whether it did: the skill checks first, then the held
// secrets. The answer names no secret.
func (d *Dashboard) refuseSkill(w http.ResponseWriter, r *http.Request, name, content string) bool {
	if strings.TrimSpace(content) == "" {
		writeError(w, http.StatusBadRequest, msgSkillNoText)
		return true
	}
	if err := inputs.CheckSkill(name, []byte(content)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return true
	}
	held, err := d.heldSecrets(r.Context())
	if err != nil {
		// Fail closed: with the secrets unknown nothing is saved. The answer
		// names no instance or field.
		d.Log.Error("dashboard: reading the held secrets for a skill check failed", "path", r.URL.Path, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return true
	}
	if inputs.ContainsSecret([]byte(content), held) {
		writeError(w, http.StatusBadRequest, msgSkillSecret)
		return true
	}
	return false
}

// createFailed answers a failed create of a skill: a taken name is 409, a
// full library 400, anything else 500.
func (d *Dashboard) createFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrSkillExists):
		writeError(w, http.StatusConflict, msgSkillExists)
	case errors.Is(err, store.ErrLibraryFull):
		writeError(w, http.StatusBadRequest, msgLibraryFull())
	default:
		d.Log.Error("dashboard: creating a skill failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
	}
}

func (d *Dashboard) createSkill(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Name          string `json:"name"`
		Content       string `json:"content"`
		AddToDefaults bool   `json:"addToDefaults"`
	}
	if decodeStrictLimit(r, &in, maxSkillBody) != nil {
		writeError(w, http.StatusBadRequest, msgBadSkillBody)
		return
	}
	if d.refuseSkill(w, r, in.Name, in.Content) {
		return
	}
	var defaults store.RepoDefaults
	if in.AddToDefaults {
		// Over the budget the create is refused whole: nothing is written, and
		// the skill goes in with the box unticked.
		var err error
		if defaults, err = d.Store.RepoDefaults(ctx); err != nil {
			d.failed(w, r, "reading the defaults", err)
			return
		}
		var total int // an empty list is nil to LibrarySize, which would read the default list: also empty, but say so
		if len(defaults.Skills) > 0 {
			if total, err = d.Store.LibrarySize(ctx, defaults.Skills); err != nil {
				d.failed(w, r, "checking the skill budget", err)
				return
			}
		}
		if msg := budgetMessage(total + len(in.Content)); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	version, err := d.Store.CreateLibrarySkill(ctx, store.NewSkill{Name: in.Name, Content: in.Content,
		Actor: skillActor(se), At: d.now()})
	if err != nil {
		d.createFailed(w, err)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: skill created", "skill", in.Name, "version", version, "by", se.Username)
	d.auditSkill(r, "skill_created", in.Name, "v"+strconv.Itoa(version))
	if in.AddToDefaults {
		defaults.Skills = append(slices.Clone(defaults.Skills), in.Name)
		if err := d.Store.PutRepoDefaults(ctx, defaults); err != nil {
			// The skill is in the library already; it can be added from Settings.
			d.Log.Error("dashboard: adding a new skill to the default skills failed", "skill", in.Name, "error", err.Error())
			writeError(w, http.StatusInternalServerError, msgWriteFailed)
			return
		}
		d.inputs.invalidate()
		d.Log.Info("dashboard: defaults updated", "fields", []string{"skills"}, "by", se.Username)
		d.audit(ctx, se, "defaults_updated", "", "", "skills")
	}
	writeJSON(w, http.StatusCreated, SkillWrittenView{Version: version})
}

func (d *Dashboard) updateSkill(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	name := r.PathValue("name")
	if !inputs.ValidName(name) {
		notFound(w)
		return
	}
	var in struct {
		Content string `json:"content"`
		Version int    `json:"version"`
	}
	if decodeStrictLimit(r, &in, maxSkillBody) != nil || in.Version < 1 {
		writeError(w, http.StatusBadRequest, msgBadSkillEdit)
		return
	}
	if d.refuseSkill(w, r, name, in.Content) {
		return
	}
	version, err := d.Store.UpdateLibrarySkill(ctx, name, in.Content, skillActor(se), in.Version, d.now())
	switch {
	case errors.Is(err, store.ErrSkillNotFound):
		notFound(w)
		return
	case errors.Is(err, store.ErrStaleVersion):
		writeError(w, http.StatusConflict, msgSkillStale)
		return
	case errors.Is(err, store.ErrSkillInstalled):
		writeError(w, http.StatusConflict, msgSkillInstalled)
		return
	case err != nil:
		d.Log.Error("dashboard: saving a skill failed", "skill", name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: skill updated", "skill", name, "version", version, "by", se.Username)
	d.auditSkill(r, "skill_updated", name, fmt.Sprintf("v%d -> v%d", in.Version, version))
	writeJSON(w, http.StatusOK, SkillWrittenView{Version: version})
}

func (d *Dashboard) copySkill(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	from := r.PathValue("name")
	if !inputs.ValidName(from) {
		notFound(w)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if decodeStrict(r, &in) != nil {
		writeError(w, http.StatusBadRequest, msgBadSkillCopy)
		return
	}
	src, _, err := d.Store.LibrarySkill(ctx, from)
	if errors.Is(err, store.ErrSkillNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		d.failed(w, r, "reading a skill", err)
		return
	}
	content := withFrontmatterName(src.Content, in.Name)
	if d.refuseSkill(w, r, in.Name, content) {
		return
	}
	version, err := d.Store.CreateLibrarySkill(ctx, store.NewSkill{Name: in.Name, Content: content,
		Actor: skillActor(se), At: d.now()})
	if err != nil {
		d.createFailed(w, err)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: skill copied", "skill", in.Name, "from", from, "by", se.Username)
	d.auditSkill(r, "skill_copied", in.Name, "from "+from)
	writeJSON(w, http.StatusCreated, SkillWrittenView{Version: version})
}

func (d *Dashboard) deleteSkill(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	name := r.PathValue("name")
	if !inputs.ValidName(name) {
		notFound(w)
		return
	}
	err := d.Store.DeleteLibrarySkill(ctx, name, d.configReposNaming(name))
	var inUse *store.SkillInUseError
	switch {
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, skillInUseView(se, inUse))
		return
	case errors.Is(err, store.ErrSkillNotFound):
		notFound(w)
		return
	case err != nil:
		d.Log.Error("dashboard: deleting a skill failed", "skill", name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: skill deleted", "skill", name, "by", se.Username)
	d.auditSkill(r, "skill_deleted", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// configReposNaming lists the repositories of the config file whose own
// skills list names the skill. (The default list is the store's to say.)
func (d *Dashboard) configReposNaming(name string) []store.RepoRef {
	var out []store.RepoRef
	for _, repo := range d.cfg.Repositories {
		if !slices.Contains(repo.Skills, name) {
			continue
		}
		instance := repo.GitLab
		if repo.GitHub != "" {
			instance = repo.GitHub
		}
		out = append(out, store.RepoRef{Instance: instance, Project: repo.Project})
	}
	return out
}

// skillInUseView is the 409's body for se: the repositories it can see,
// by instance and project, and a count of those it can't.
func skillInUseView(se Session, e *store.SkillInUseError) SkillInUseView {
	v := SkillInUseView{Error: msgSkillInUse, Repositories: []SkillUserRepoView{}, InDefaults: e.InDefaults}
	for _, repo := range e.Repos {
		if se.CanSee(repo.Instance, repo.Project) {
			v.Repositories = append(v.Repositories, SkillUserRepoView{Instance: repo.Instance, Project: repo.Project})
		} else {
			v.Hidden++
		}
	}
	slices.SortFunc(v.Repositories, func(a, b SkillUserRepoView) int {
		return strings.Compare(a.Instance+"\x00"+lower(a.Project), b.Instance+"\x00"+lower(b.Project))
	})
	return v
}

// withFrontmatterName is content with the `name` of its frontmatter set to
// name, quoted so a name like 1.5 stays text. Content without a frontmatter
// name is returned as it is: the name is optional (D4). A name line the
// skill checks still disagree with is caught by them, after.
func withFrontmatterName(content, name string) string {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return content
	}
	lines := strings.SplitAfter(content, "\n")
	for i := 1; i < len(lines); i++ {
		bare := strings.TrimRight(lines[i], "\r\n")
		if bare == "---" {
			break
		}
		if strings.HasPrefix(bare, "name:") {
			lines[i] = `name: "` + name + `"` + lines[i][len(bare):]
			return strings.Join(lines, "")
		}
	}
	return content
}
