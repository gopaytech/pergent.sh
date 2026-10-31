// Package instances is the set of GitLab and GitHub instances pergent.sh
// reviews: those in the config file, whose secrets are env vars, and those
// added in the dashboard, kept in the database with their secrets sealed.
// The registry tells the rest of the code which instances exist and what
// their secrets are; connecting to them is the caller's.
package instances

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The names of an instance's secrets, as stored (instance_secrets.field).
const (
	FieldReviewToken       = "review_token"
	FieldApproveToken      = "approve_token"
	FieldWebhookSecret     = "webhook_secret"
	FieldOAuthClientSecret = "oauth_client_secret"
	FieldReviewAppKey      = "review_app_key"
	FieldApproveAppKey     = "approve_app_key"
)

// Source tells where an instance is set up.
type Source string

const (
	SourceConfig    Source = "config"
	SourceDashboard Source = "dashboard"
)

// Instance is one GitLab or GitHub instance, whichever source it comes from.
// Exactly one of GitLab and GitHub is set, by Platform. For an instance of
// the dashboard the *_env fields are empty, and an empty ApproveApp means
// the approve side is a bot user's token.
type Instance struct {
	Name     string
	Platform string // config.PlatformGitLab or config.PlatformGitHub
	Source   Source
	// Removing is set while the dashboard takes the instance out.
	Removing bool
	GitLab   *config.Instance
	GitHub   *config.GitHubInstance
	// OAuthClientID is the OAuth application used to sign in with the
	// instance; empty means nobody signs in through it. For a config
	// instance it is read from the env var the config names.
	OAuthClientID string
}

// HasOAuth reports whether people can sign in to the dashboard through the
// instance. A config instance is set up for it by naming both env vars; a
// dashboard one by holding a client ID.
func (i Instance) HasOAuth() bool {
	if i.Source == SourceConfig {
		switch {
		case i.GitLab != nil:
			return i.GitLab.HasOAuth()
		case i.GitHub != nil:
			return i.GitHub.HasOAuth()
		}
		return false
	}
	return i.OAuthClientID != ""
}

// Secrets holds an instance's secrets, in the clear. Which are set depends
// on the platform and how the instance approves.
type Secrets struct {
	ReviewToken       string // GitLab review bot token
	ApproveToken      string // GitLab approve bot token; GitHub approve bot user's token
	WebhookSecret     string
	OAuthClientSecret string
	ReviewAppKey      string // GitHub review App private key
	ApproveAppKey     string // GitHub approve App private key
}

// Fields returns the secrets that are set, by Field* name.
func (s Secrets) Fields() map[string]string {
	out := map[string]string{}
	for field, v := range map[string]string{
		FieldReviewToken: s.ReviewToken, FieldApproveToken: s.ApproveToken, FieldWebhookSecret: s.WebhookSecret,
		FieldOAuthClientSecret: s.OAuthClientSecret, FieldReviewAppKey: s.ReviewAppKey, FieldApproveAppKey: s.ApproveAppKey,
	} {
		if v != "" {
			out[field] = v
		}
	}
	return out
}

// Registry holds the instances of the config and of the dashboard. The
// config's never change; the dashboard's change with Put and Remove, which
// the caller calls after writing the store. It is safe for concurrent use.
type Registry struct {
	cfg *config.Config
	st  *store.Store
	box *secretbox.Box

	mu   sync.RWMutex
	dash map[string]Instance

	// llm is the LLM set in the dashboard (the store's row, its key opened);
	// llmSeen is every key held since start, for redaction.
	llmMu   sync.RWMutex
	llm     llmRow
	llmSeen []string
}

// llmRow is the store's LLM row with its key opened. key is "" when the row
// has none or it can't be opened.
type llmRow struct {
	llm       config.LLM
	key       string
	updatedAt time.Time
	found     bool
}

// New returns a Registry over cfg and, for the dashboard's instances and
// their secrets, st and box. A nil st leaves it with the config's instances
// alone; box is only needed to open secrets.
func New(cfg *config.Config, st *store.Store, box *secretbox.Box) *Registry {
	return &Registry{cfg: cfg, st: st, box: box, dash: map[string]Instance{}}
}

// Load reads the dashboard's instances from the store, replacing those
// held. An instance of the same name in the config file is an error: config
// wins, and the two can't both be there.
func (r *Registry) Load(ctx context.Context) error {
	if r.st == nil {
		return nil
	}
	rows, err := r.st.Instances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	dash := make(map[string]Instance, len(rows))
	for _, row := range rows {
		if _, ok := r.config(row.Name); ok {
			return fmt.Errorf("instance %q is in both the config file and the dashboard", row.Name)
		}
		dash[row.Name] = FromRow(row)
	}
	r.mu.Lock()
	r.dash = dash
	r.mu.Unlock()
	return nil
}

// Get returns the instance called name.
func (r *Registry) Get(name string) (Instance, bool) {
	if inst, ok := r.config(name); ok {
		return inst, true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.dash[name]
	return inst, ok
}

// Platform reports the platform of the instance called name:
// config.PlatformGitLab or config.PlatformGitHub.
func (r *Registry) Platform(name string) (string, bool) {
	inst, ok := r.Get(name)
	return inst.Platform, ok
}

// All returns every instance: the config's first, then the dashboard's,
// each by name.
func (r *Registry) All() []Instance {
	var out []Instance
	for _, inst := range r.cfg.GitLab {
		out = append(out, r.fromGitLab(inst))
	}
	for _, inst := range r.cfg.GitHub {
		out = append(out, r.fromGitHub(inst))
	}
	byName := func(a, b Instance) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(out, byName)

	r.mu.RLock()
	dash := slices.Collect(maps.Values(r.dash))
	r.mu.RUnlock()
	slices.SortFunc(dash, byName)
	return append(out, dash...)
}

// Put adds or replaces an instance of the dashboard, after it was written to
// the store.
func (r *Registry) Put(inst Instance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dash[inst.Name] = inst
}

// SetRemoving marks the instance of the dashboard called name as being
// removed, in place, and returns it; ok is false when there is none. A change
// made at the same time (SetOAuthClientID) never clears it.
func (r *Registry) SetRemoving(name string) (Instance, bool) {
	return r.change(name, func(inst *Instance) { inst.Removing = true })
}

// SetOAuthClientID sets the OAuth client ID of the instance of the dashboard
// called name, in place, and returns it; ok is false when there is none.
func (r *Registry) SetOAuthClientID(name, clientID string) (Instance, bool) {
	return r.change(name, func(inst *Instance) { inst.OAuthClientID = clientID })
}

func (r *Registry) change(name string, f func(*Instance)) (Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.dash[name]
	if !ok {
		return Instance{}, false
	}
	f(&inst)
	r.dash[name] = inst
	return inst, true
}

// Remove forgets an instance of the dashboard, after it was deleted from the
// store.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dash, name)
}

// config returns the config file's instance called name.
func (r *Registry) config(name string) (Instance, bool) {
	if inst, ok := r.cfg.Instance(name); ok {
		return r.fromGitLab(inst), true
	}
	if inst, ok := r.cfg.GitHubInstance(name); ok {
		return r.fromGitHub(inst), true
	}
	return Instance{}, false
}

func (r *Registry) fromGitLab(inst config.Instance) Instance {
	return Instance{Name: inst.Name, Platform: config.PlatformGitLab, Source: SourceConfig,
		GitLab: &inst, OAuthClientID: envValue(inst.OAuthClientIDEnv)}
}

func (r *Registry) fromGitHub(inst config.GitHubInstance) Instance {
	return Instance{Name: inst.Name, Platform: config.PlatformGitHub, Source: SourceConfig,
		GitHub: &inst, OAuthClientID: envValue(inst.OAuthClientIDEnv)}
}

func envValue(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

// FromRow is the instance of the dashboard a store row describes.
func FromRow(row store.InstanceRow) Instance {
	inst := Instance{Name: row.Name, Platform: row.Platform, Source: SourceDashboard,
		Removing: row.Removing, OAuthClientID: row.OAuthClientID}
	if row.Platform == config.PlatformGitHub {
		gh := &config.GitHubInstance{Name: row.Name, APIURL: row.APIURL, ReviewApp: config.App{AppID: row.ReviewAppID}}
		if row.ApproveAppID != 0 {
			gh.ApproveApp = &config.App{AppID: row.ApproveAppID}
		}
		inst.GitHub = gh
	} else {
		inst.GitLab = &config.Instance{Name: row.Name, URL: row.URL}
	}
	return inst
}

// Secrets returns the secrets of the instance called name. A config
// instance's come from the env vars the config names: one unset is an
// error naming it (the OAuth client secret is read only with the dashboard
// on). A dashboard instance's are opened from the store; a value that
// can't be opened, because PERGENT_SH_SECRET_KEY changed, is left empty and
// its field returned in unopened: it counts as not set.
func (r *Registry) Secrets(ctx context.Context, name string) (s Secrets, unopened []string, err error) {
	inst, ok := r.Get(name)
	if !ok {
		return Secrets{}, nil, fmt.Errorf("no instance %q", name)
	}
	if inst.Source == SourceConfig {
		s, err = r.configSecrets(inst)
		return s, nil, err
	}
	if r.st == nil || r.box == nil {
		return Secrets{}, nil, fmt.Errorf("instance %q: no store to read its secrets from", name)
	}
	sealed, err := r.st.InstanceSecrets(ctx, name)
	if err != nil {
		return Secrets{}, nil, fmt.Errorf("read the secrets of instance %q: %w", name, err)
	}
	for _, field := range slices.Sorted(maps.Keys(sealed)) {
		plain, err := r.box.Open(sealed[field].Value)
		if err != nil {
			unopened = append(unopened, field)
			continue
		}
		v := string(plain)
		switch field {
		case FieldReviewToken:
			s.ReviewToken = v
		case FieldApproveToken:
			s.ApproveToken = v
		case FieldWebhookSecret:
			s.WebhookSecret = v
		case FieldOAuthClientSecret:
			s.OAuthClientSecret = v
		case FieldReviewAppKey:
			s.ReviewAppKey = v
		case FieldApproveAppKey:
			s.ApproveAppKey = v
		}
	}
	return s, unopened, nil
}

func (r *Registry) configSecrets(inst Instance) (Secrets, error) {
	var s Secrets
	var err error
	switch {
	case inst.GitLab != nil:
		s.ReviewToken, s.ApproveToken, s.WebhookSecret, err = inst.GitLab.Secrets()
	case inst.GitHub != nil:
		var approve string
		s.ReviewAppKey, approve, s.WebhookSecret, err = inst.GitHub.Secrets()
		if inst.GitHub.ApproveApp != nil {
			s.ApproveAppKey = approve
		} else {
			s.ApproveToken = approve
		}
	}
	if err != nil {
		return Secrets{}, err
	}
	if r.cfg.DashboardEnabled() && inst.HasOAuth() {
		var oauth func() (string, string, error)
		if inst.GitLab != nil {
			oauth = inst.GitLab.OAuthClient
		} else {
			oauth = inst.GitHub.OAuthClient
		}
		if _, s.OAuthClientSecret, err = oauth(); err != nil {
			return Secrets{}, err
		}
	}
	return s, nil
}

// LLMState is the LLM as the dashboard shows it. Source is SourceConfig when
// the config file has an llm: block (then the dashboard can't change it) and
// SourceDashboard otherwise, even when nothing is set yet. KeySet is false
// when the key is missing, the env var empty or the stored key can't be
// opened. UpdatedAt is when the dashboard last saved it; zero for the
// config's.
type LLMState struct {
	Source    Source
	LLM       config.LLM
	KeySet    bool
	UpdatedAt time.Time
}

// LLM returns the LLM reviews use: the config's llm: block when it has one,
// with its key from the env var it names, otherwise the one set in the
// dashboard. ok is false when there is none, or its key is missing or can't
// be opened. The model and the key are read together, so a LoadLLM at the
// same time can't pair the old model with the new key.
func (r *Registry) LLM() (llm config.LLM, key string, source Source, ok bool) {
	st, key := r.llmState()
	if key == "" {
		return st.LLM, "", st.Source, false
	}
	return st.LLM, key, st.Source, true
}

// LLMStatus describes the LLM without its key (see LLMState).
func (r *Registry) LLMStatus() LLMState {
	st, _ := r.llmState()
	return st
}

// llmState is the LLM and its key, read as one.
func (r *Registry) llmState() (LLMState, string) {
	if r.cfg.HasLLM() {
		key := envValue(r.cfg.LLM.APIKeyEnv)
		r.noteKey(key)
		return LLMState{Source: SourceConfig, LLM: r.cfg.LLM, KeySet: key != ""}, key
	}
	r.llmMu.RLock()
	defer r.llmMu.RUnlock()
	return LLMState{Source: SourceDashboard, LLM: r.llm.llm, KeySet: r.llm.key != "", UpdatedAt: r.llm.updatedAt}, r.llm.key
}

// LoadLLM reads the LLM row from the store again: at startup, and after the
// dashboard saved it. A key that can't be opened, because
// PERGENT_SH_SECRET_KEY changed, counts as not set.
func (r *Registry) LoadLLM(ctx context.Context) error {
	if r.st == nil {
		return nil
	}
	row, found, err := r.st.LLM(ctx)
	if err != nil {
		return fmt.Errorf("read the LLM: %w", err)
	}
	next := llmRow{found: found}
	if found {
		next.llm = config.LLM{Provider: row.Provider, Model: row.Model, BaseURL: row.BaseURL}
		next.updatedAt = row.UpdatedAt
		if len(row.APIKeySealed) > 0 && r.box != nil {
			if plain, err := r.box.Open(row.APIKeySealed); err == nil {
				next.key = string(plain)
			}
		}
	}
	r.noteKey(next.key)
	r.llmMu.Lock()
	r.llm = next
	r.llmMu.Unlock()
	return nil
}

// LLMKeys returns every LLM key held since start, the one in use and those
// it replaced, for redacting them from anything bound for a log or a page.
func (r *Registry) LLMKeys() []string {
	if r.cfg.HasLLM() {
		r.noteKey(envValue(r.cfg.LLM.APIKeyEnv))
	}
	r.llmMu.RLock()
	defer r.llmMu.RUnlock()
	return slices.Sorted(slices.Values(r.llmSeen))
}

func (r *Registry) noteKey(key string) {
	if key == "" {
		return
	}
	r.llmMu.Lock()
	defer r.llmMu.Unlock()
	if !slices.Contains(r.llmSeen, key) {
		r.llmSeen = append(r.llmSeen, key)
	}
}
