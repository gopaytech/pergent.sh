package dashboard

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	maxAPIClientName  = 63
	maxAPIKeyName     = 100
	maxCallbackURL    = 2000
	maxInFlightLimit  = 50
	maxRequestsPerHr  = 100000
	minRequestBytes   = 1024
	keyPrefixAttempts = 3

	// previousSecretFor is how long the callback secret before a rotation
	// keeps signing, so a caller can switch over.
	previousSecretFor = 24 * time.Hour
	// jobDeleteTimeout bounds the removal of a deleted client's Jobs.
	jobDeleteTimeout = 30 * time.Second

	msgBadAPIClient     = `the body must be {"name": ..., "skills": [...], "modelId": ..., "callbackUrl": ..., "limits": {...}}`
	msgBadAPIClientEdit = `the body must be {"skills": [...], "modelId": ..., "callbackUrl": ..., "limits": {...}}: the name never changes`
	msgBadAPIKey        = `the body must be {"name": ..., "expiresAt": ...}`
	msgAPIName          = "the name may hold only a-z, 0-9 and -"
	msgAPINameLen       = "the name must be 1 to 63 characters"
	msgAPISkillName     = "a skill name may hold only letters, digits, . _ and -, and starts with a letter or digit"
	msgAPISkillTwice    = "a skill is named twice"
	msgAPIModel         = "no model has this ID"
	msgAPICallback      = "the callback URL must be an http or https URL with no user name or password"
	msgAPICallbackLen   = "the callback URL must be at most 2000 bytes"
	msgAPILimits        = "limits are required"
	msgAPIRequestBytes  = "max request bytes must be from 1024 to 819200"
	msgAPIFiles         = "max files must be from 1 to 1000"
	msgAPIInFlight      = "max in flight must be from 1 to 50"
	msgAPIPerHour       = "requests per hour must be from 1 to 100000"
	msgAPIBudget        = "monthly token budget must be more than 0"
	msgAPIKeyName       = "the key name must be 1 to 100 characters"
	msgAPIKeyExpiry     = "the expiry must be in the future"
	msgAPIClientExists  = "an API client with this name exists"
	msgAPIKeyExists     = "the client has a key with this name"
	msgAPINoCallback    = "this client has no callback URL"
	msgAPITestOff       = "test callbacks are not available"
	// msgTestSecretGone is the error of a test callback that can't be signed;
	// it is the text a real callback records for it.
	msgTestSecretGone = "the client's callback secret can't be opened: rotate it"
)

var apiClientName = regexp.MustCompile(`^[a-z0-9-]+$`)

// apiClientFields are the settings of a client, checked.
type apiClientFields struct {
	skills      []string
	modelID     string
	callbackURL string
	limits      store.APILimits
}

// checkAPIClientFields checks the settings of a create or an update. msg is
// the 400's message, "" when they are fine. A nil limits is the defaults
// unless required.
func (d *Dashboard) checkAPIClientFields(skills []string, modelID, callbackURL string, limits *APILimitsView, required bool) (f apiClientFields, msg string) {
	seen := map[string]bool{}
	for _, s := range skills {
		if !inputs.ValidName(s) {
			return f, msgAPISkillName
		}
		if seen[s] {
			return f, msgAPISkillTwice
		}
		seen[s] = true
	}
	if modelID != "" && !slices.ContainsFunc(d.registry.Models(), func(m instances.Model) bool { return m.ID == modelID }) {
		return f, msgAPIModel
	}
	callbackURL = strings.TrimSpace(callbackURL)
	if len(callbackURL) > maxCallbackURL {
		return f, msgAPICallbackLen
	}
	if !checkCallbackURL(callbackURL) {
		return f, msgAPICallback
	}
	switch {
	case limits != nil:
		l, msg := checkAPILimits(*limits)
		if msg != "" {
			return f, msg
		}
		f.limits = l
	case required:
		return f, msgAPILimits
	}
	f.skills, f.modelID, f.callbackURL = skills, modelID, callbackURL
	if f.skills == nil {
		f.skills = []string{}
	}
	return f, ""
}

// checkCallbackURL reports whether s is empty (no callbacks) or an absolute
// http or https URL with a host, no user name or password and no fragment. A
// private or in-cluster address is allowed: the caller's own, as they asked.
func checkCallbackURL(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil &&
		!strings.Contains(s, "#")
}

// checkAPILimits checks every limit against the ranges the table holds.
func checkAPILimits(v APILimitsView) (store.APILimits, string) {
	switch {
	case v.MaxRequestBytes < minRequestBytes || v.MaxRequestBytes > store.HardMaxRequestBytes:
		return store.APILimits{}, msgAPIRequestBytes
	case v.MaxFiles < 1 || v.MaxFiles > store.HardMaxFiles:
		return store.APILimits{}, msgAPIFiles
	case v.MaxInFlight < 1 || v.MaxInFlight > maxInFlightLimit:
		return store.APILimits{}, msgAPIInFlight
	case v.RequestsPerHour < 1 || v.RequestsPerHour > maxRequestsPerHr:
		return store.APILimits{}, msgAPIPerHour
	case v.MonthlyTokenBudget < 1:
		return store.APILimits{}, msgAPIBudget
	}
	return store.APILimits{MaxRequestBytes: v.MaxRequestBytes, MaxFiles: v.MaxFiles, MaxInFlight: v.MaxInFlight,
		RequestsPerHour: v.RequestsPerHour, MonthlyTokenBudget: v.MonthlyTokenBudget}, ""
}

func apiLimitsView(l store.APILimits) APILimitsView {
	return APILimitsView{MaxRequestBytes: l.MaxRequestBytes, MaxFiles: l.MaxFiles, MaxInFlight: l.MaxInFlight,
		RequestsPerHour: l.RequestsPerHour, MonthlyTokenBudget: l.MonthlyTokenBudget}
}

func apiKeyView(k store.APIKey) APIKeyView {
	return APIKeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt}
}

// apiClientView is a client with its keys. The previous secret counts as
// there only while it still signs.
func (d *Dashboard) apiClientView(c store.APIClient, keys []store.APIKey) APIClientView {
	v := APIClientView{Name: c.Name, Skills: c.Skills, ModelID: c.ModelID, CallbackURL: c.CallbackURL,
		Limits: apiLimitsView(c.Limits), Keys: make([]APIKeyView, 0, len(keys)), CreatedAt: c.CreatedAt}
	if v.Skills == nil {
		v.Skills = []string{}
	}
	if c.PreviousSecretUntil.After(d.now()) {
		until := c.PreviousSecretUntil
		v.PreviousSecretUntil = &until
	}
	for _, k := range keys {
		v.Keys = append(v.Keys, apiKeyView(k))
	}
	return v
}

// keyDetail is the audit detail of a key: the client, then the key's name and
// prefix. Never the key.
func keyDetail(client string, k store.APIKey) string {
	return client + ": " + k.Name + " (" + k.Prefix + ")"
}

func (d *Dashboard) apiClientRoutes(mux *http.ServeMux) {
	d.adminRoute(mux, "GET /api/admin/api-clients", d.listAPIClients)
	d.adminRoute(mux, "POST /api/admin/api-clients", d.createAPIClient)
	d.adminRoute(mux, "GET /api/admin/api-clients/{name}", d.getAPIClient)
	d.adminRoute(mux, "PUT /api/admin/api-clients/{name}", d.updateAPIClient)
	d.adminRoute(mux, "DELETE /api/admin/api-clients/{name}", d.deleteAPIClient)
	d.adminRoute(mux, "POST /api/admin/api-clients/{name}/secret", d.rotateCallbackSecret)
	d.adminRoute(mux, "POST /api/admin/api-clients/{name}/test-callback", d.testCallback)
	d.adminRoute(mux, "POST /api/admin/api-clients/{name}/keys", d.createAPIKey)
	d.adminRoute(mux, "DELETE /api/admin/api-clients/{name}/keys/{id}", d.deleteAPIKey)
}

// namedAPIClient reads the client the path names; it answers 404 or 500
// itself and returns false.
func (d *Dashboard) namedAPIClient(w http.ResponseWriter, r *http.Request) (store.APIClient, bool) {
	c, found, err := d.Store.APIClientByName(r.Context(), r.PathValue("name"))
	if err != nil {
		d.failed(w, r, "reading an API client", err)
		return store.APIClient{}, false
	}
	if !found {
		notFound(w)
	}
	return c, found
}

// --- GET /api/admin/api-clients ---

func (d *Dashboard) listAPIClients(w http.ResponseWriter, r *http.Request) {
	clients, err := d.Store.APIClients(r.Context())
	if err != nil {
		d.failed(w, r, "listing the API clients", err)
		return
	}
	stats, err := d.Store.APIClientStats(r.Context(), monthStart(d.now()))
	if err != nil {
		d.failed(w, r, "counting the API clients' reviews", err)
		return
	}
	out := APIClientListView{Items: make([]APIClientRowView, 0, len(clients))}
	for _, c := range clients {
		st := stats[c.ID]
		out.Items = append(out.Items, APIClientRowView{Name: c.Name, Keys: int(st.Keys),
			ReviewsThisMonth: st.Reviews, FailedThisMonth: st.Failed, TokensThisMonth: st.Tokens})
	}
	writeJSON(w, http.StatusOK, out)
}

// monthStart is midnight UTC on the 1st of the month t is in, UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// --- POST /api/admin/api-clients ---

func (d *Dashboard) createAPIClient(w http.ResponseWriter, r *http.Request) {
	var in APIClientInput
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadAPIClient)
		return
	}
	if in.Name == "" || len(in.Name) > maxAPIClientName {
		writeError(w, http.StatusBadRequest, msgAPINameLen)
		return
	}
	if !apiClientName.MatchString(in.Name) {
		writeError(w, http.StatusBadRequest, msgAPIName)
		return
	}
	f, msg := d.checkAPIClientFields(in.Skills, in.ModelID, in.CallbackURL, in.Limits, false)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	ctx, se := r.Context(), session(r)
	secret, err := reviewapi.NewCallbackSecret()
	if err != nil {
		d.failed(w, r, "making a callback secret", err)
		return
	}
	c, err := d.Store.CreateAPIClient(ctx, store.APIClient{Name: in.Name, Skills: f.skills, ModelID: f.modelID,
		CallbackURL: f.callbackURL, CallbackSecretSealed: d.Auth.box.Seal([]byte(secret)), Limits: f.limits})
	if errors.Is(err, store.ErrAPIClientExists) {
		writeError(w, http.StatusConflict, msgAPIClientExists)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: adding an API client failed", "client", in.Name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: API client added", "client", c.Name, "by", se.Username)
	d.audit(ctx, se, "api_client_created", "", "", c.Name)
	writeJSON(w, http.StatusCreated, APIClientCreatedView{Client: d.apiClientView(c, nil), CallbackSecret: secret})
}

// --- GET /api/admin/api-clients/{name} ---

func (d *Dashboard) getAPIClient(w http.ResponseWriter, r *http.Request) {
	c, ok := d.namedAPIClient(w, r)
	if !ok {
		return
	}
	keys, err := d.Store.APIKeys(r.Context(), c.ID)
	if err != nil {
		d.failed(w, r, "listing the API keys", err)
		return
	}
	writeJSON(w, http.StatusOK, d.apiClientView(c, keys))
}

// --- PUT /api/admin/api-clients/{name} ---

func (d *Dashboard) updateAPIClient(w http.ResponseWriter, r *http.Request) {
	var in APIClientEdit
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadAPIClientEdit)
		return
	}
	f, msg := d.checkAPIClientFields(in.Skills, in.ModelID, in.CallbackURL, in.Limits, true)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	ctx, se := r.Context(), session(r)
	c, found, err := d.Store.UpdateAPIClient(ctx, r.PathValue("name"), func(c *store.APIClient) {
		c.Skills, c.ModelID, c.CallbackURL, c.Limits = f.skills, f.modelID, f.callbackURL, f.limits
	})
	if err != nil {
		d.Log.Error("dashboard: changing an API client failed", "client", r.PathValue("name"), "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w)
		return
	}
	keys, err := d.Store.APIKeys(ctx, c.ID)
	if err != nil {
		d.failed(w, r, "listing the API keys", err)
		return
	}
	d.Log.Info("dashboard: API client updated", "client", c.Name, "by", se.Username)
	d.audit(ctx, se, "api_client_updated", "", "", c.Name)
	writeJSON(w, http.StatusOK, d.apiClientView(c, keys))
}

// --- DELETE /api/admin/api-clients/{name} ---

func (d *Dashboard) deleteAPIClient(w http.ResponseWriter, r *http.Request) {
	ctx, se, name := r.Context(), session(r), r.PathValue("name")
	jobs, found, err := d.Store.DeleteAPIClient(ctx, name)
	if err != nil {
		d.Log.Error("dashboard: removing an API client failed", "client", name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w)
		return
	}
	d.removeJobs(ctx, name, jobs)
	d.Log.Info("dashboard: API client removed", "client", name, "by", se.Username)
	d.audit(ctx, se, "api_client_removed", "", "", name)
	w.WriteHeader(http.StatusNoContent)
}

// removeJobs deletes the Jobs of a removed client's live reviews. It is best
// effort: a Job that is not deleted is logged, and the rest still are.
func (d *Dashboard) removeJobs(ctx context.Context, client string, jobs []string) {
	if d.deleteJob == nil || len(jobs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobDeleteTimeout)
	defer cancel()
	for _, job := range jobs {
		if err := d.deleteJob(ctx, job); err != nil {
			d.Log.Error("dashboard: deleting the Job of a removed API client's review failed",
				"client", client, "job", job, "error", err.Error())
		}
	}
}

// --- POST /api/admin/api-clients/{name}/secret ---

func (d *Dashboard) rotateCallbackSecret(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	secret, err := reviewapi.NewCallbackSecret()
	if err != nil {
		d.failed(w, r, "making a callback secret", err)
		return
	}
	sealed := d.Auth.box.Seal([]byte(secret))
	until := d.now().Add(previousSecretFor)
	c, found, err := d.Store.UpdateAPIClient(ctx, r.PathValue("name"), func(c *store.APIClient) {
		c.PreviousSecretSealed, c.PreviousSecretUntil = c.CallbackSecretSealed, until
		c.CallbackSecretSealed = sealed
	})
	if err != nil {
		d.Log.Error("dashboard: rotating a callback secret failed", "client", r.PathValue("name"), "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w)
		return
	}
	d.Log.Info("dashboard: callback secret rotated", "client", c.Name, "by", se.Username)
	d.audit(ctx, se, "api_callback_secret_rotated", "", "", c.Name)
	writeJSON(w, http.StatusOK, APICallbackSecretView{CallbackSecret: secret})
}

// --- POST /api/admin/api-clients/{name}/test-callback ---

// testCallback sends the client's callback URL a test review, signed and sent
// as a real callback is, and answers with what came back. Its verdict is
// nothing_reviewed and its body says test: true, so it is never taken for a
// pass. It is not recorded as a callback of any review. The error in the
// answer is already redacted: the URL's path and query and the secrets are
// not in it.
func (d *Dashboard) testCallback(w http.ResponseWriter, r *http.Request) {
	c, ok := d.namedAPIClient(w, r)
	if !ok {
		return
	}
	if c.CallbackURL == "" {
		writeError(w, http.StatusConflict, msgAPINoCallback)
		return
	}
	if d.sendCallback == nil {
		writeError(w, http.StatusServiceUnavailable, msgAPITestOff)
		return
	}
	ctx, se := r.Context(), session(r)
	d.audit(ctx, se, "api_callback_tested", "", "", c.Name)
	secrets, ok := d.callbackSecrets(c)
	if !ok {
		d.Log.Warn("dashboard: a client's callback secret can't be opened", "client", c.Name)
		writeJSON(w, http.StatusOK, APICallbackTestView{Error: msgTestSecretGone})
		return
	}
	body, err := reviewapi.Encode(reviewapi.TestReview(d.now()))
	if err != nil {
		d.failed(w, r, "making a test callback", err)
		return
	}
	delivery, err := newDeliveryID()
	if err != nil {
		d.failed(w, r, "making a delivery ID", err)
		return
	}
	a := d.sendCallback(ctx, c.CallbackURL, body, delivery, secrets...)
	d.Log.Info("dashboard: test callback sent", "client", c.Name, "by", se.Username, "status", a.StatusCode,
		"took_ms", a.Duration.Milliseconds())
	writeJSON(w, http.StatusOK, APICallbackTestView{StatusCode: a.StatusCode, Error: a.Error, DurationMs: a.Duration.Milliseconds()})
}

// callbackSecrets opens the client's callback secret and, while the
// rotation's overlap lasts, the previous one after it: the secrets a real
// callback is signed with now. ok is false when the current one can't be
// opened; a previous one that can't is left out.
func (d *Dashboard) callbackSecrets(c store.APIClient) ([]string, bool) {
	cur, err := d.Auth.box.Open(c.CallbackSecretSealed)
	if err != nil {
		return nil, false
	}
	secrets := []string{string(cur)}
	if len(c.PreviousSecretSealed) > 0 && c.PreviousSecretUntil.After(d.now()) {
		if prev, err := d.Auth.box.Open(c.PreviousSecretSealed); err == nil {
			secrets = append(secrets, string(prev))
		}
	}
	return secrets, true
}

// newDeliveryID is a random version 4 UUID, the X-Pergent-Delivery of a test
// callback.
func newDeliveryID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// --- POST /api/admin/api-clients/{name}/keys ---

func (d *Dashboard) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var in APIKeyInput
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadAPIKey)
		return
	}
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxAPIKeyName {
		writeError(w, http.StatusBadRequest, msgAPIKeyName)
		return
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(d.now()) {
		writeError(w, http.StatusBadRequest, msgAPIKeyExpiry)
		return
	}
	c, ok := d.namedAPIClient(w, r)
	if !ok {
		return
	}
	ctx, se := r.Context(), session(r)
	k, key, err := d.storeNewAPIKey(ctx, c.ID, name, in.ExpiresAt)
	if errors.Is(err, store.ErrAPIKeyExists) {
		writeError(w, http.StatusConflict, msgAPIKeyExists)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: adding an API key failed", "client", c.Name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: API key added", "client", c.Name, "key", k.Name, "prefix", k.Prefix, "by", se.Username)
	d.audit(ctx, se, "api_key_created", "", "", keyDetail(c.Name, k))
	writeJSON(w, http.StatusCreated, APIKeyCreatedView{Key: apiKeyView(k), Secret: key})
}

// storeNewAPIKey makes a key and stores its hash, with a new prefix when the
// one it drew is taken. It returns the row and the key itself.
func (d *Dashboard) storeNewAPIKey(ctx context.Context, clientID int64, name string, expiresAt *time.Time) (store.APIKey, string, error) {
	for range keyPrefixAttempts {
		key, prefix, hash, err := reviewapi.NewKey()
		if err != nil {
			return store.APIKey{}, "", err
		}
		k, err := d.Store.CreateAPIKey(ctx, store.APIKey{ClientID: clientID, Name: name, Prefix: prefix, Hash: hash, ExpiresAt: expiresAt})
		if errors.Is(err, store.ErrAPIPrefixTaken) {
			continue
		}
		return k, key, err
	}
	return store.APIKey{}, "", store.ErrAPIPrefixTaken
}

// --- DELETE /api/admin/api-clients/{name}/keys/{id} ---

func (d *Dashboard) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	c, ok := d.namedAPIClient(w, r)
	if !ok {
		return
	}
	ctx, se := r.Context(), session(r)
	k, found, err := d.Store.DeleteAPIKey(ctx, c.ID, id)
	if err != nil {
		d.Log.Error("dashboard: revoking an API key failed", "client", c.Name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w)
		return
	}
	d.Log.Info("dashboard: API key revoked", "client", c.Name, "key", k.Name, "prefix", k.Prefix, "by", se.Username)
	d.audit(ctx, se, "api_key_revoked", "", "", keyDetail(c.Name, k))
	w.WriteHeader(http.StatusNoContent)
}
