package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	maxLLMField = 256

	msgBadModelBody   = `the body must be {"provider": ..., "model": ..., "baseURL": ..., "apiKey": ...}`
	msgBadModelEdit   = `the body must be {"baseURL": ..., "apiKey": ...}: an entry's provider and model never change, add another entry instead`
	msgLLMRequired    = "provider and model are required, each at most 256 characters"
	msgBadBaseURL     = "baseURL must be empty or an absolute http or https URL"
	msgKeyRequired    = "apiKey is required"
	msgBadKey         = "apiKey must not be empty, and at most 16 KiB"
	msgModelExists    = "a model with this provider, model and base URL is in the list already"
	msgConfigLLM      = "the config file has an llm: block, which is the default; change it there"
	msgConfigModel    = "this model is set in the config file; change it there"
	msgModelIsDefault = "this is the default model: make another one the default first"
	msgModelInUse     = "the model is in use: change the model of the repositories that name it first"

	msgNeedsAdminModel = "only admins of this repository change its model"
	msgUnknownModel    = "model must be the ID of a model in the list, or null to follow the default"
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
func (d *Dashboard) modelUsers(ctx context.Context) (users map[string][]RepoRefView, following int, err error) {
	repos, err := d.Repos.List(ctx)
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

// modelView is the view of an entry the dashboard keeps. Its key counts as
// set when one is stored and opens with PERGENT_SH_SECRET_KEY, as the
// registry counts it.
func (d *Dashboard) modelView(m store.LLMModel, repos []RepoRefView) ModelAdminView {
	v := ModelAdminView{ID: m.ID, Provider: m.Provider, Model: m.Model, BaseURL: m.BaseURL,
		Source: string(instances.SourceDashboard),
		// While the config has an llm: block its entry is the default,
		// whatever the row says.
		Default:      m.Default && !d.cfg.HasLLM(),
		Repositories: nonNilRefs(repos)}
	if len(m.APIKeySealed) > 0 {
		if _, err := d.Auth.box.Open(m.APIKeySealed); err == nil {
			at := m.UpdatedAt
			v.Key = SecretView{Set: true, UpdatedAt: &at}
		}
	}
	return v
}

func nonNilRefs(list []RepoRefView) []RepoRefView {
	if list == nil {
		return []RepoRefView{}
	}
	return list
}

// modelsView is the whole list: the config's entry first, when there is one,
// then the dashboard's.
func (d *Dashboard) modelsView(ctx context.Context) (ModelsAdminView, error) {
	rows, err := d.Store.LLMModels(ctx)
	if err != nil {
		return ModelsAdminView{}, err
	}
	users, following, err := d.modelUsers(ctx)
	if err != nil {
		return ModelsAdminView{}, err
	}
	v := ModelsAdminView{Editable: !d.cfg.HasLLM(), FollowingDefault: following, Models: []ModelAdminView{}}
	for _, m := range d.registry.Models() {
		if m.ID == store.ConfigModelID {
			v.Models = append(v.Models, ModelAdminView{ID: m.ID, Provider: m.LLM.Provider, Model: m.LLM.Model, BaseURL: m.LLM.BaseURL,
				Source: string(instances.SourceConfig), Default: true, Key: SecretView{Set: m.Key != ""},
				Repositories: nonNilRefs(users[m.ID])})
		}
	}
	for _, row := range rows {
		v.Models = append(v.Models, d.modelView(row, users[row.ID]))
	}
	return v, nil
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
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		BaseURL  string  `json:"baseURL"`
		APIKey   *string `json:"apiKey"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadModelBody)
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
	m, err := d.Store.CreateLLMModel(ctx, store.LLMModel{Provider: in.Provider, Model: in.Model, BaseURL: in.BaseURL,
		APIKeySealed: d.Auth.box.Seal([]byte(*in.APIKey))}, d.now())
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, msgModelExists)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: adding a model failed", "error", d.registry.Redact(redactKey(err.Error(), *in.APIKey)))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.reloadModels(ctx, *in.APIKey)
	label := modelLabel(m.Provider, m.Model)
	d.Log.Info("dashboard: model added", "model_id", m.ID, "model", label, "by", se.Username)
	d.audit(ctx, se, "model_added", "", "", label)
	writeJSON(w, http.StatusCreated, d.modelView(m, nil))
}

// --- PUT /api/admin/models/{id} ---

func (d *Dashboard) updateModel(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("id")
	if id == store.ConfigModelID {
		writeError(w, http.StatusConflict, msgConfigModel)
		return
	}
	// A provider or model in the body is an unknown field: refused, since
	// they would move the repositories that name the entry to another model.
	var in struct {
		BaseURL *string `json:"baseURL"`
		APIKey  *string `json:"apiKey"`
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
	found, err = d.Store.UpdateLLMModel(ctx, id, baseURL, sealed, d.now())
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, msgModelExists)
		return
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
	d.reloadModels(ctx, hidden)

	var changed []string
	if before.BaseURL != baseURL {
		changed = append(changed, "base_url")
	}
	if in.APIKey != nil {
		changed = append(changed, "api_key")
	}
	label := modelLabel(before.Provider, before.Model)
	detail := label
	if len(changed) > 0 {
		detail += " (" + strings.Join(changed, ", ") + ")"
	}
	d.Log.Info("dashboard: model updated", "model_id", id, "model", label, "fields", changed, "by", se.Username)
	d.audit(ctx, se, "model_updated", "", "", detail)

	after := before
	after.BaseURL, after.UpdatedAt = baseURL, d.now()
	if sealed != nil {
		after.APIKeySealed = sealed
	}
	users, _, err := d.modelUsers(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	writeJSON(w, http.StatusOK, d.modelView(after, users[id]))
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
	if id == store.ConfigModelID {
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
	err = d.Store.DeleteLLMModel(ctx, id)
	var inUse *store.ModelInUseError
	switch {
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, modelInUseView(inUse))
		return
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
	label := modelLabel(before.Provider, before.Model)
	d.Log.Info("dashboard: model removed", "model_id", id, "model", label, "by", se.Username)
	d.audit(ctx, se, "model_removed", "", "", label)
	w.WriteHeader(http.StatusNoContent)
}

// modelInUseView is the 409's body: every repository that names the model.
func modelInUseView(e *store.ModelInUseError) ModelInUseView {
	v := ModelInUseView{Error: msgModelInUse, Repositories: []RepoRefView{}}
	for _, repo := range e.Repos {
		v.Repositories = append(v.Repositories, RepoRefView{Instance: repo.Instance, Project: repo.Project})
	}
	sortRepoRefs(v.Repositories)
	return v
}

// --- a repository's model ---

// effectiveModel is "provider / model" for the entry a repository runs on:
// the one it names, else the default; "" when there is none (the entry was
// removed from the config file, or nothing is set up).
func (d *Dashboard) effectiveModel(modelID string) string {
	for _, m := range d.registry.Models() {
		if (modelID == "" && m.Default) || (modelID != "" && m.ID == modelID) {
			return modelLabel(m.LLM.Provider, m.LLM.Model)
		}
	}
	return ""
}

// modelChoices are the entries a repository can be given, for an admin of
// the repository: an ID and a name each, never a base URL or a key. Anyone
// else gets an empty list.
func (d *Dashboard) modelChoices(role Role) []ModelChoiceView {
	out := []ModelChoiceView{}
	if role < RoleAdmin {
		return out
	}
	for _, m := range d.registry.Models() {
		out = append(out, ModelChoiceView{ID: m.ID, Label: modelLabel(m.LLM.Provider, m.LLM.Model)})
	}
	return out
}

// hasConfigModel reports whether the config file's llm: block supplies the
// entry "config".
func (d *Dashboard) hasConfigModel() bool {
	return slices.ContainsFunc(d.registry.Models(), func(m instances.Model) bool { return m.ID == store.ConfigModelID })
}

// modelValue is a repository's model setting as the API shows it: the entry's
// ID, or null for the default.
func modelValue(repo settings.Effective) any {
	if repo.ModelID == "" {
		return nil
	}
	return repo.ModelID
}

// isModelKey reports whether key is the model setting, which only an admin
// of the repository changes: it decides what the reviews cost.
func isModelKey(key string) bool { return key == "model" }

// effectiveModelOrID is effectiveModel, or the ID when the entry is gone.
func (d *Dashboard) effectiveModelOrID(id string) string {
	if label := d.effectiveModel(id); label != "" {
		return label
	}
	return id
}

// modelSettingDetail is the audit detail of a change to the model setting,
// naming the provider and model it was given, or "default".
func (d *Dashboard) modelSettingDetail(id *string) string {
	if id == nil {
		return "model (default)"
	}
	return "model (" + d.effectiveModelOrID(*id) + ")"
}
