package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// --- the rule library: GET /api/rule-sets, /api/rule-sets/{name},
// /api/built-in-rules, and the admin writes ---
//
// Everyone signed in reads the rule sets and the built-in rules, as the skill
// library; admins write. A set's text is for a <pre> or a <textarea>, never
// HTML. A config-file set is read-only here and wins over a stored one of the
// same name.

const (
	// maxRuleSetBody bounds a rule-set write's body; the set itself is
	// checked against rule.MaxFileBytes.
	maxRuleSetBody = 1 << 20

	msgBadRuleSetBody   = `the body must be {"name": ..., "content": ...}`
	msgBadRuleSetEdit   = `the body must be {"content": ..., "version": the version you opened}`
	msgBadRuleSetName   = "invalid rule-set name: use letters, digits, '.', '_' and '-', starting with a letter or digit"
	msgRuleSetNoText    = "the rule set has no text"
	msgRuleSetSecret    = "the rule set contains a secret pergent.sh holds"
	msgRuleSetExists    = "name already in the rule library"
	msgRuleSetStale     = "this rule set changed since you opened it"
	msgRuleSetConfig    = "this rule set is defined in the config file and is read-only here"
	msgRuleSetInUse     = "the rule set is in use: take it off the repositories and the default list that name it first"
	sourceRuleSetStored = "dashboard"
	sourceRuleSetConfig = "config"
)

func msgRuleLibraryFull() string {
	return fmt.Sprintf("the rule library holds %d rule sets, the most it can", store.MaxRuleSets)
}

// msgRuleSetMissing is the 400 for a rule set a save names that is in neither
// the library nor the config file.
func msgRuleSetMissing(name string) string {
	return fmt.Sprintf("rule set %q is not in the rule library", name)
}

func (d *Dashboard) ruleSetRoutes(mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, d.Auth.Require(h)) }
	get("/api/rule-sets", d.ruleSets)
	get("/api/rule-sets/{name}", d.ruleSet)
	get("/api/built-in-rules", d.builtInRulesView)
	d.adminRoute(mux, "POST /api/admin/rule-sets", d.createRuleSet)
	d.adminRoute(mux, "PUT /api/admin/rule-sets/{name}", d.updateRuleSet)
	d.adminRoute(mux, "DELETE /api/admin/rule-sets/{name}", d.deleteRuleSet)
}

// ruleSetEntries is how many entries a set's text has; 0 when it no longer
// loads (dispatch fails the run that uses it and names it).
func ruleSetEntries(name, content string) int {
	entries, err := rule.Load([]byte(content), rule.DashboardSource(name))
	if err != nil {
		return 0
	}
	return len(entries)
}

func (d *Dashboard) ruleSets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := d.Store.RuleSets(ctx)
	if err != nil {
		d.failed(w, r, "listing the rule library", err)
		return
	}
	def, err := d.Store.RepoDefaults(ctx)
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	out := make([]RuleSetView, 0, len(list)+len(d.cfg.RuleSets))
	for _, cs := range d.cfg.RuleSets {
		out = append(out, RuleSetView{Name: cs.Name, Entries: len(cs.Entries), Source: sourceRuleSetConfig,
			InDefaults: slices.Contains(def.RuleSets, cs.Name)})
	}
	for _, rs := range list {
		if _, inConfig := d.cfg.RuleSet(rs.Name); inConfig {
			continue
		}
		out = append(out, RuleSetView{Name: rs.Name, Version: rs.Version, UpdatedAt: rs.UpdatedAt.UTC(),
			Entries: ruleSetEntries(rs.Name, rs.Content), Source: sourceRuleSetStored, InDefaults: slices.Contains(def.RuleSets, rs.Name)})
	}
	slices.SortFunc(out, func(a, b RuleSetView) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

func (d *Dashboard) ruleSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	if !inputs.ValidName(name) {
		notFound(w)
		return
	}
	def, err := d.Store.RepoDefaults(ctx)
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	if cs, ok := d.cfg.RuleSet(name); ok {
		writeJSON(w, http.StatusOK, RuleSetDetailView{
			RuleSetView: RuleSetView{Name: name, Entries: len(cs.Entries), Source: sourceRuleSetConfig, InDefaults: slices.Contains(def.RuleSets, name)},
			Content:     cs.Content, Notes: d.ruleSetNotes(name, cs.Content)})
		return
	}
	rs, err := d.Store.RuleSet(ctx, name)
	if errors.Is(err, store.ErrRuleSetNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		d.failed(w, r, "reading a rule set", err)
		return
	}
	writeJSON(w, http.StatusOK, RuleSetDetailView{
		RuleSetView: RuleSetView{Name: name, Version: rs.Version, UpdatedAt: rs.UpdatedAt.UTC(), Entries: ruleSetEntries(name, rs.Content),
			Source: sourceRuleSetStored, InDefaults: slices.Contains(def.RuleSets, name)},
		Content: rs.Content, Notes: d.ruleSetNotes(name, rs.Content)})
}

func (d *Dashboard) builtInRulesView(w http.ResponseWriter, _ *http.Request) {
	out := []BuiltInRuleView{}
	for _, e := range d.builtInRules() {
		out = append(out, BuiltInRuleView{Name: e.Name, Path: e.Path, Off: e.Off, Rule: e.Rule})
	}
	writeJSON(w, http.StatusOK, out)
}

// ruleSetNotes is the editor's notes on a set's text: one for each entry that
// comes before a built-in rule its files may also match, so that built-in no
// longer applies to them. Allowed, but worth knowing. None for text that
// doesn't load.
func (d *Dashboard) ruleSetNotes(name, content string) []string {
	notes := []string{}
	entries, err := rule.Load([]byte(content), rule.DashboardSource(name))
	if err != nil {
		return notes
	}
	builtins := d.builtInRules()
	for i, e := range entries {
		for _, b := range rule.Hides(e, builtins) {
			notes = append(notes, fmt.Sprintf("entry %d: This entry hides the built-in rule %s for matching files.", i+1, b))
		}
	}
	return notes
}

// refuseRuleSet answers 400 with why content can't be the rule set called
// name, and reports whether it did: pergent's loader first, then the held
// secrets. The answer names no secret and never quotes the text.
func (d *Dashboard) refuseRuleSet(w http.ResponseWriter, r *http.Request, name, content string) bool {
	if !inputs.ValidName(name) {
		writeError(w, http.StatusBadRequest, msgBadRuleSetName)
		return true
	}
	if strings.TrimSpace(content) == "" {
		writeError(w, http.StatusBadRequest, msgRuleSetNoText)
		return true
	}
	if _, err := rule.Load([]byte(content), rule.DashboardSource(name)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return true
	}
	held, err := d.heldSecrets(r.Context())
	if err != nil {
		// Fail closed: with the secrets unknown nothing is saved.
		d.Log.Error("dashboard: reading the held secrets for a rule-set check failed", "path", r.URL.Path, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return true
	}
	if inputs.ContainsSecret([]byte(content), held) {
		writeError(w, http.StatusBadRequest, msgRuleSetSecret)
		return true
	}
	return false
}

func (d *Dashboard) createRuleSet(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if decodeStrictLimit(r, &in, maxRuleSetBody) != nil {
		writeError(w, http.StatusBadRequest, msgBadRuleSetBody)
		return
	}
	if _, inConfig := d.cfg.RuleSet(in.Name); inConfig {
		writeError(w, http.StatusConflict, msgRuleSetConfig)
		return
	}
	if d.refuseRuleSet(w, r, in.Name, in.Content) {
		return
	}
	err := d.Store.CreateRuleSet(ctx, in.Name, in.Content, d.now())
	switch {
	case errors.Is(err, store.ErrRuleSetExists):
		writeError(w, http.StatusConflict, msgRuleSetExists)
		return
	case errors.Is(err, store.ErrRuleSetsFull):
		writeError(w, http.StatusBadRequest, msgRuleLibraryFull())
		return
	case err != nil:
		d.Log.Error("dashboard: creating a rule set failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: rule set created", "rule_set", in.Name, "by", se.Username)
	d.auditSkill(r, "rule_set_created", in.Name, "v1")
	writeJSON(w, http.StatusCreated, RuleSetWrittenView{Version: 1, Notes: d.ruleSetNotes(in.Name, in.Content)})
}

func (d *Dashboard) updateRuleSet(w http.ResponseWriter, r *http.Request) {
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
	if decodeStrictLimit(r, &in, maxRuleSetBody) != nil || in.Version < 1 {
		writeError(w, http.StatusBadRequest, msgBadRuleSetEdit)
		return
	}
	if _, inConfig := d.cfg.RuleSet(name); inConfig {
		writeError(w, http.StatusConflict, msgRuleSetConfig)
		return
	}
	if d.refuseRuleSet(w, r, name, in.Content) {
		return
	}
	version, err := d.Store.UpdateRuleSet(ctx, name, in.Content, in.Version, d.now())
	switch {
	case errors.Is(err, store.ErrRuleSetNotFound):
		notFound(w)
		return
	case errors.Is(err, store.ErrRuleSetVersion):
		writeError(w, http.StatusConflict, msgRuleSetStale)
		return
	case err != nil:
		d.Log.Error("dashboard: saving a rule set failed", "rule_set", name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: rule set updated", "rule_set", name, "version", version, "by", se.Username)
	d.auditSkill(r, "rule_set_updated", name, fmt.Sprintf("v%d -> v%d", in.Version, version))
	writeJSON(w, http.StatusOK, RuleSetWrittenView{Version: version, Notes: d.ruleSetNotes(name, in.Content)})
}

func (d *Dashboard) deleteRuleSet(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	name := r.PathValue("name")
	if !inputs.ValidName(name) {
		notFound(w)
		return
	}
	if _, inConfig := d.cfg.RuleSet(name); inConfig {
		writeError(w, http.StatusConflict, msgRuleSetConfig)
		return
	}
	err := d.Store.DeleteRuleSet(ctx, name, d.configReposNamingRuleSet(name))
	var inUse *store.RuleSetInUseError
	switch {
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, ruleSetInUseView(inUse))
		return
	case errors.Is(err, store.ErrRuleSetNotFound):
		notFound(w)
		return
	case err != nil:
		d.Log.Error("dashboard: deleting a rule set failed", "rule_set", name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.inputs.invalidate()
	d.Log.Info("dashboard: rule set deleted", "rule_set", name, "by", se.Username)
	d.auditSkill(r, "rule_set_deleted", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// configReposNamingRuleSet lists the config-file repositories whose own
// rule_sets names the set.
func (d *Dashboard) configReposNamingRuleSet(name string) []store.RepoRef {
	var out []store.RepoRef
	for _, repo := range d.cfg.Repositories {
		if !slices.Contains(repo.RuleSets, name) {
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

func ruleSetInUseView(e *store.RuleSetInUseError) SkillInUseView {
	v := skillInUseView(&store.SkillInUseError{Repos: e.Repos, InDefaults: e.InDefaults})
	v.Error = msgRuleSetInUse
	return v
}
