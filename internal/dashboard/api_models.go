package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	maxLLMField = 256

	msgBadModelBody   = `the body must be {"name": ..., "provider": ..., "model": ..., "baseURL": ..., "apiKey": ..., "teamsMayUse": ..., "contextWindow": ..., "maxOutput": ...}`
	msgBadModelEdit   = `the body must be {"baseURL": ..., "apiKey": ..., "name": ..., "teamsMayUse": ..., "contextWindow": ..., "maxOutput": ...}: an entry's provider and model never change, add another entry instead`
	msgLLMRequired    = "provider and model are required, each at most 256 characters"
	msgBadBaseURL     = "baseURL must be empty or an absolute http or https URL"
	msgKeyRequired    = "apiKey is required"
	msgBadKey         = "apiKey must not be empty, and at most 16 KiB"
	msgConfigLLM      = "the config file has an llm: block, which is the default; change it there"
	msgConfigModel    = "this model is set in the config file; change it there"
	msgModelIsDefault = "this is the default model: make another one the default first"
	// The default goes to position 1 of a saved global chain, and a full
	// chain has no room for an entry it does not hold.
	msgGlobalChainFull = "the global chain has 5 entries and this model is not one of them: remove one from the chain first"

	msgModelInChain = "a repository's model is its model chain now: PUT .../chain, or POST .../chain/reset to inherit"
	msgUnknownModel = "a model the repository names is not in the list"
)

// modelLabel is how a provider and model are shown and audited. It holds
// neither a base URL nor a key.
func modelLabel(provider, model string) string { return provider + " / " + model }

// checkBaseURL reports whether s is empty or an absolute http or https URL
// without credentials.
func checkBaseURL(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && len(s) <= 2048
}

// modelUsers lists, for each entry of the list, the repositories that name
// it, and counts those that name none and so follow the default: the
// repositories of the config file and the dashboard's that chose no model.
// A paused repository counts: it can't be deleted from under it either.
func (d *Dashboard) modelUsers(ctx context.Context) (users map[string][]RepoRefView, following int, err error) {
	repos, err := d.Repos.ListAll(ctx)
	if err != nil {
		return nil, 0, err
	}
	users = map[string][]RepoRefView{}
	for _, repo := range repos {
		if repo.ModelID == "" {
			following++
			continue
		}
		users[repo.ModelID] = append(users[repo.ModelID], RepoRefView{Instance: repo.Instance, Project: repo.Project})
	}
	for _, list := range users {
		sortRepoRefs(list)
	}
	return users, following, nil
}

func sortRepoRefs(list []RepoRefView) {
	slices.SortFunc(list, func(a, b RepoRefView) int {
		return strings.Compare(a.Instance+"\x00"+lower(a.Project), b.Instance+"\x00"+lower(b.Project))
	})
}

// modelLists is what every model view reads besides the entry: the
// repositories whose model setting names each entry, the chains that name
// it, and the marks.
type modelLists struct {
	users  map[string][]RepoRefView
	usedIn map[store.EntryRef][]string
	marks  map[store.EntryRef]store.Mark
	teams  map[int64]string
	limits []store.ModelLimit
	usage  quota.EntryUsage
}

func (d *Dashboard) modelLists(ctx context.Context) (l modelLists, following int, err error) {
	if l.users, following, err = d.modelUsers(ctx); err != nil {
		return modelLists{}, 0, err
	}
	teams, err := d.teamNames(ctx)
	if err != nil {
		return modelLists{}, 0, err
	}
	l.teams = teams
	if l.usedIn, err = d.usedIn(ctx, teams); err != nil {
		return modelLists{}, 0, err
	}
	if l.marks, err = d.marksByRef(ctx); err != nil {
		return modelLists{}, 0, err
	}
	if l.limits, err = d.Store.ModelLimits(ctx); err != nil {
		return modelLists{}, 0, err
	}
	if l.usage, err = quota.ReadEntryUsage(ctx, d.Store, d.now()); err != nil {
		return modelLists{}, 0, err
	}
	return l, following, nil
}

// globalView adds what l says of the global entry v.ID to v.
func (d *Dashboard) globalView(v ModelAdminView, l modelLists) ModelAdminView {
	ref := store.EntryRef{Source: store.SourceGlobal, ID: v.ID}
	v.Repositories, v.UsedIn = nonNilRefs(l.users[v.ID]), nonNilStrings(l.usedIn[ref])
	mk, ok := l.marks[ref]
	v.Mark = d.markView(mk, ok)
	v.Limit, v.Used, v.TeamLimits = limitOf(l.limits, ref, 0), l.usage.Used(ref, 0), []TeamLimitView{}
	for _, lim := range l.limits {
		if lim.Ref == ref && lim.TeamID != 0 {
			v.TeamLimits = append(v.TeamLimits, TeamLimitView{TeamID: lim.TeamID, Team: l.teams[lim.TeamID],
				Limit: lim.MonthlyTokens, Used: l.usage.Used(ref, lim.TeamID)})
		}
	}
	slices.SortFunc(v.TeamLimits, func(a, b TeamLimitView) int { return strings.Compare(strings.ToLower(a.Team), strings.ToLower(b.Team)) })
	return v
}

// modelView is the view of an entry the dashboard keeps. Its key counts as
// set when one is stored and opens with PERGENT_SH_SECRET_KEY, as the
// registry counts it.
func (d *Dashboard) modelView(m store.LLMModel, l modelLists) ModelAdminView {
	v := ModelAdminView{ID: m.ID, Name: m.Name, Provider: m.Provider, Model: m.Model, BaseURL: m.BaseURL,
		Source: string(instances.SourceDashboard),
		// While the config has an llm: block its entry is the default,
		// whatever the row says.
		Default: m.Default && !d.cfg.HasLLM(), TeamsMayUse: m.TeamsMayUse}
	if len(m.APIKeySealed) > 0 {
		if _, err := d.Auth.box.Open(m.APIKeySealed); err == nil {
			at := m.UpdatedAt
			v.Key = SecretView{Set: true, UpdatedAt: &at}
		}
	}
	v.ContextWindow, v.MaxOutput = tokensOrNull(m.ContextWindow), tokensOrNull(m.MaxOutput)
	return d.globalView(v, l)
}

func nonNilRefs(list []RepoRefView) []RepoRefView {
	if list == nil {
		return []RepoRefView{}
	}
	return list
}

func nonNilStrings(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// modelsView is the whole list: the config's entries first (its llm: block,
// then llm.fallbacks), when there are any, then the dashboard's.
func (d *Dashboard) modelsView(ctx context.Context) (ModelsAdminView, error) {
	rows, err := d.Store.LLMModels(ctx)
	if err != nil {
		return ModelsAdminView{}, err
	}
	l, following, err := d.modelLists(ctx)
	if err != nil {
		return ModelsAdminView{}, err
	}
	v := ModelsAdminView{CanSetDefault: !d.cfg.HasLLM(), FollowingDefault: following, Models: []ModelAdminView{},
		WindowOff: modelWindowOff(d.cfg.Jobs.Image, d.cfg.Jobs.SecurityImage)}
	for _, m := range d.registry.GlobalModels() {
		if m.Source == instances.SourceConfig {
			v.Models = append(v.Models, d.globalView(ModelAdminView{ID: m.ID, Name: m.Name, Provider: m.LLM.Provider, Model: m.LLM.Model,
				BaseURL: m.LLM.BaseURL, Source: string(instances.SourceConfig), Default: m.ID == store.ConfigModelID,
				Key: SecretView{Set: m.Key != ""}, ContextWindow: tokensOrNull(int(m.LLM.ContextWindow)),
				MaxOutput: tokensOrNull(int(m.LLM.MaxOutput))}, l))
		}
	}
	for _, row := range rows {
		v.Models = append(v.Models, d.modelView(row, l))
	}
	return v, nil
}

// modelDetail is how an entry is audited: its name, provider and model,
// never a base URL or a key.
func modelDetail(name, provider, model string) string {
	return name + " (" + modelLabel(provider, model) + ")"
}

// storedModel finds the entry id among the dashboard's.
func (d *Dashboard) storedModel(ctx context.Context, id string) (m store.LLMModel, found bool, err error) {
	rows, err := d.Store.LLMModels(ctx)
	if err != nil {
		return store.LLMModel{}, false, err
	}
	i := slices.IndexFunc(rows, func(m store.LLMModel) bool { return m.ID == id })
	if i < 0 {
		return store.LLMModel{}, false, nil
	}
	return rows[i], true, nil
}

// reloadModels reads the list into the registry again after a change was
// saved. A failure doesn't undo the change: the engine uses the earlier list
// until pergent.sh restarts or the list changes again. It is logged, with any
// key in the message hidden.
func (d *Dashboard) reloadModels(ctx context.Context, hide string) {
	err := d.loadLLM(ctx)
	if err == nil {
		return
	}
	d.Log.Error("dashboard: the models were saved but reading them back failed; changing the list again tries again",
		"error", d.registry.Redact(redactKey(err.Error(), hide)))
}

// redactKey is msg with key, when there is one, replaced by [REDACTED]: a new
// key is not in the registry's set until it is read back.
func redactKey(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "[REDACTED]")
}

// --- GET /api/admin/models ---

func (d *Dashboard) getModels(w http.ResponseWriter, r *http.Request) {
	v, err := d.modelsView(r.Context())
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// --- POST /api/admin/models ---

func (d *Dashboard) createModel(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Name        *string `json:"name"`
		Provider    string  `json:"provider"`
		Model       string  `json:"model"`
		BaseURL     string  `json:"baseURL"`
		APIKey      *string `json:"apiKey"`
		TeamsMayUse bool    `json:"teamsMayUse"`

		ContextWindow json.RawMessage `json:"contextWindow"`
		MaxOutput     json.RawMessage `json:"maxOutput"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadModelBody)
		return
	}
	if in.Name == nil {
		writeError(w, http.StatusBadRequest, msgBadModelName)
		return
	}
	name, ok := entryName(*in.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadModelName)
		return
	}
	in.Provider, in.Model, in.BaseURL = strings.TrimSpace(in.Provider), strings.TrimSpace(in.Model), strings.TrimSpace(in.BaseURL)
	if in.Provider == "" || in.Model == "" || len(in.Provider) > maxLLMField || len(in.Model) > maxLLMField {
		writeError(w, http.StatusBadRequest, msgLLMRequired)
		return
	}
	if !checkBaseURL(in.BaseURL) {
		writeError(w, http.StatusBadRequest, msgBadBaseURL)
		return
	}
	if in.APIKey == nil {
		writeError(w, http.StatusBadRequest, msgKeyRequired)
		return
	}
	if strings.TrimSpace(*in.APIKey) == "" || len(*in.APIKey) > maxSecretBytes {
		writeError(w, http.StatusBadRequest, msgBadKey)
		return
	}
	contextWindow, maxOutput, ok := windowBody(w, in.ContextWindow, in.MaxOutput, 0, 0)
	if !ok {
		return
	}
	m, err := d.Store.CreateLLMModel(ctx, store.LLMModel{Name: name, Provider: in.Provider, Model: in.Model, BaseURL: in.BaseURL,
		TeamsMayUse: in.TeamsMayUse, APIKeySealed: d.Auth.box.Seal([]byte(*in.APIKey)),
		ContextWindow: contextWindow, MaxOutput: maxOutput}, d.now())
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, msgNameTaken)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: adding a model failed", "error", d.registry.Redact(redactKey(err.Error(), *in.APIKey)))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.reloadModels(ctx, *in.APIKey)
	detail := modelDetail(m.Name, m.Provider, m.Model)
	d.Log.Info("dashboard: model added", "model_id", m.ID, "model", detail, "by", se.Username)
	d.audit(ctx, se, "model_added", "", "", detail)
	writeJSON(w, http.StatusCreated, d.modelView(m, modelLists{}))
}

// --- PUT /api/admin/models/{id} ---

// updateModel changes an entry's base URL (required, as before names) and,
// when given, its key, name and teams may use switch. The name is checked
// and stored first, so a name taken changes nothing.
func (d *Dashboard) updateModel(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("id")
	if id == store.ConfigModelID || strings.HasPrefix(id, store.ConfigModelID+"-") {
		writeError(w, http.StatusConflict, msgConfigModel)
		return
	}
	// A provider or model in the body is an unknown field: refused, since
	// they would move the chains that name the entry to another model.
	var in struct {
		BaseURL     *string `json:"baseURL"`
		APIKey      *string `json:"apiKey"`
		Name        *string `json:"name"`
		TeamsMayUse *bool   `json:"teamsMayUse"`

		ContextWindow json.RawMessage `json:"contextWindow"`
		MaxOutput     json.RawMessage `json:"maxOutput"`
	}
	if err := decodeStrict(r, &in); err != nil || in.BaseURL == nil {
		writeError(w, http.StatusBadRequest, msgBadModelEdit)
		return
	}
	baseURL := strings.TrimSpace(*in.BaseURL)
	if !checkBaseURL(baseURL) {
		writeError(w, http.StatusBadRequest, msgBadBaseURL)
		return
	}
	var name *string
	if in.Name != nil {
		n, ok := entryName(*in.Name)
		if !ok {
			writeError(w, http.StatusBadRequest, msgBadModelName)
			return
		}
		name = &n
	}
	// hidden is the new key, to keep out of anything logged.
	var sealed []byte
	hidden := ""
	if in.APIKey != nil {
		if strings.TrimSpace(*in.APIKey) == "" || len(*in.APIKey) > maxSecretBytes {
			writeError(w, http.StatusBadRequest, msgBadKey)
			return
		}
		sealed, hidden = d.Auth.box.Seal([]byte(*in.APIKey)), *in.APIKey
	}
	before, found, err := d.storedModel(ctx, id)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	contextWindow, maxOutput, ok := windowBody(w, in.ContextWindow, in.MaxOutput, before.ContextWindow, before.MaxOutput)
	if !ok {
		return
	}
	if name != nil && *name == before.Name {
		name = nil
	}
	if in.TeamsMayUse != nil && *in.TeamsMayUse == before.TeamsMayUse {
		in.TeamsMayUse = nil
	}
	if name != nil || in.TeamsMayUse != nil {
		found, err = d.Store.SetLLMModelMeta(ctx, id, name, in.TeamsMayUse, d.now())
		if errors.Is(err, store.ErrNameTaken) {
			writeError(w, http.StatusConflict, msgNameTaken)
			return
		}
		if err == nil && found {
			found, err = d.Store.UpdateLLMModel(ctx, id, baseURL, sealed, d.now())
		}
	} else {
		found, err = d.Store.UpdateLLMModel(ctx, id, baseURL, sealed, d.now())
	}
	if err != nil {
		d.Log.Error("dashboard: changing a model failed", "model_id", id, "error", d.registry.Redact(redactKey(err.Error(), hidden)))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w) // removed meanwhile
		return
	}
	windowSet := contextWindow != before.ContextWindow || maxOutput != before.MaxOutput
	if windowSet {
		found, err = d.Store.SetLLMModelWindow(ctx, id, contextWindow, maxOutput, d.now())
		if err != nil {
			d.Log.Error("dashboard: changing a model failed", "model_id", id, "error", d.registry.Redact(err.Error()))
			writeError(w, http.StatusInternalServerError, msgWriteFailed)
			return
		}
		if !found {
			notFound(w) // removed meanwhile
			return
		}
	}
	d.reloadModels(ctx, hidden)

	after := before
	after.BaseURL, after.UpdatedAt = baseURL, d.now()
	after.ContextWindow, after.MaxOutput = contextWindow, maxOutput
	if sealed != nil {
		after.APIKeySealed = sealed
	}
	if name != nil {
		after.Name = *name
	}
	if in.TeamsMayUse != nil {
		after.TeamsMayUse = *in.TeamsMayUse
	}
	var changed []string
	if name != nil {
		changed = append(changed, "name")
	}
	if before.BaseURL != baseURL {
		changed = append(changed, "base_url")
	}
	if in.APIKey != nil {
		changed = append(changed, "api_key")
	}
	changed = append(changed, windowChanges(before.ContextWindow, before.MaxOutput, contextWindow, maxOutput)...)
	detail := modelDetail(after.Name, before.Provider, before.Model)
	if len(changed) > 0 {
		d.Log.Info("dashboard: model updated", "model_id", id, "model", detail, "fields", changed, "by", se.Username)
		d.audit(ctx, se, "model_updated", "", "", detail+" ("+strings.Join(changed, ", ")+")")
	}
	if in.TeamsMayUse != nil {
		state := "off"
		if *in.TeamsMayUse {
			state = "on"
		}
		d.Log.Info("dashboard: teams may use changed", "model_id", id, "teams_may_use", state, "by", se.Username)
		d.audit(ctx, se, "teams_may_use_changed", "", "", after.Name+": "+state)
	}
	l, _, err := d.modelLists(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	writeJSON(w, http.StatusOK, d.modelView(after, l))
}

// --- POST /api/admin/models/{id}/default ---

func (d *Dashboard) setDefaultModel(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("id")
	// A config llm: block is the default as long as it is there.
	if d.cfg.HasLLM() {
		writeError(w, http.StatusConflict, msgConfigLLM)
		return
	}
	rows, err := d.Store.LLMModels(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	i := slices.IndexFunc(rows, func(m store.LLMModel) bool { return m.ID == id })
	if i < 0 {
		notFound(w)
		return
	}
	next := rows[i]
	if !next.Default {
		found, err := d.Store.SetDefaultLLMModel(ctx, id, d.now())
		if errors.Is(err, store.ErrChainTooLong) {
			writeError(w, http.StatusConflict, msgGlobalChainFull)
			return
		}
		if err != nil {
			d.Log.Error("dashboard: changing the default model failed", "model_id", id, "error", d.registry.Redact(err.Error()))
			writeError(w, http.StatusInternalServerError, msgWriteFailed)
			return
		}
		if !found {
			notFound(w) // removed meanwhile
			return
		}
		d.reloadModels(ctx, "")
		detail := modelLabel(next.Provider, next.Model)
		if j := slices.IndexFunc(rows, func(m store.LLMModel) bool { return m.Default }); j >= 0 {
			detail += " (was " + modelLabel(rows[j].Provider, rows[j].Model) + ")"
		}
		d.Log.Info("dashboard: default model changed", "model_id", id, "model", modelLabel(next.Provider, next.Model), "by", se.Username)
		d.audit(ctx, se, "model_default_changed", "", "", detail)
	} else {
		// Already the default: nothing to record, but the earlier change may
		// have saved without the engine reading it back, and asking again is
		// how an operator retries that.
		d.reloadModels(ctx, "")
	}
	v, err := d.modelsView(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// --- DELETE /api/admin/models/{id} ---

func (d *Dashboard) deleteModel(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("id")
	if id == store.ConfigModelID || strings.HasPrefix(id, store.ConfigModelID+"-") {
		writeError(w, http.StatusConflict, msgConfigModel)
		return
	}
	before, found, err := d.storedModel(ctx, id)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	// While the config has an llm: block it is the default, and the flag on
	// a dashboard entry is dormant: the entry may go, and the store passes
	// the flag on.
	// Chains that name it keep it and skip it as removed: the views list
	// where it was used (usedIn) and show it as removed there.
	err = d.Store.DeleteLLMModel(ctx, id, d.cfg.HasLLM())
	switch {
	case errors.Is(err, store.ErrModelIsDefault):
		writeError(w, http.StatusConflict, msgModelIsDefault)
		return
	case errors.Is(err, store.ErrModelNotFound):
		notFound(w)
		return
	case err != nil:
		d.Log.Error("dashboard: removing a model failed", "model_id", id, "error", d.registry.Redact(err.Error()))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.reloadModels(ctx, "")
	detail := modelDetail(before.Name, before.Provider, before.Model)
	d.Log.Info("dashboard: model removed", "model_id", id, "model", detail, "by", se.Username)
	d.audit(ctx, se, "model_removed", "", "", detail)
	w.WriteHeader(http.StatusNoContent)
}

// hasConfigModel reports whether the config file's llm: block supplies the
// entry "config".
func (d *Dashboard) hasConfigModel() bool {
	return slices.ContainsFunc(d.registry.Models(), func(m instances.Model) bool { return m.ID == store.ConfigModelID })
}

// defaultModelLabel is "provider / model" of the default entry; "" when there
// is none.
func (d *Dashboard) defaultModelLabel() string {
	for _, m := range d.registry.Models() {
		if m.Default {
			return modelLabel(m.LLM.Provider, m.LLM.Model)
		}
	}
	return ""
}
