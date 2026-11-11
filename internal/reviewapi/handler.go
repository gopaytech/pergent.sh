package reviewapi

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Store is the part of the database the review API uses. *store.Store
// satisfies it.
type Store interface {
	APIKeyByPrefix(ctx context.Context, prefix string) (store.APIKey, store.APIClient, bool, error)
	TouchAPIKey(ctx context.Context, id int64, now time.Time) error

	EnqueueAPIReview(ctx context.Context, n store.NewAPIReview) (id string, created bool, err error)
	APIReview(ctx context.Context, clientID int64, id string) (store.APIReview, bool, error)
	// APIReviewByKey finds the client's review that was asked for with an
	// Idempotency-Key, and the hash of the request it came with.
	APIReviewByKey(ctx context.Context, clientID int64, key string) (store.APIReview, []byte, bool, error)
	ResolveLibrary(ctx context.Context, names []string) (store.ResolvedLibrary, error)
}

// ModelSource tells which models exist. *instances.Registry satisfies it.
type ModelSource interface {
	ModelByID(id string) (instances.Model, bool)
	DefaultModel() (instances.Model, bool)
}

// Handler serves the /v1/ routes. Every route works on the client of the key
// it was called with, and no other.
type Handler struct {
	Store Store
	// Box seals and opens the client's callback secrets.
	Box    *secretbox.Box
	Models ModelSource
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Wake nudges the engine's loops. May be nil.
	Wake func()
	Log  *slog.Logger
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// Routes registers the /v1/ routes and, last, a catch-all that answers an
// unknown path with a JSON 404 (after the key check, so a stranger learns no
// more from it than from any other call).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("GET /v1/client", h.RequireKey(http.HandlerFunc(h.client)))
	mux.Handle("POST /v1/reviews", h.RequireKey(http.HandlerFunc(h.createReview)))
	mux.Handle("GET /v1/reviews/{id}", h.RequireKey(http.HandlerFunc(h.getReview)))
	mux.Handle("/v1/", h.RequireKey(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "not found")
	})))
}

type ctxKey int

const (
	keyCtx ctxKey = iota
	clientCtx
)

// keyOf is the key a request passed RequireKey with.
func keyOf(r *http.Request) store.APIKey {
	k, _ := r.Context().Value(keyCtx).(store.APIKey)
	return k
}

// clientOf is the client of the key a request passed RequireKey with.
func clientOf(r *http.Request) store.APIClient {
	c, _ := r.Context().Value(clientCtx).(store.APIClient)
	return c
}

const unauthorizedMessage = "missing, unknown, revoked or expired key"

// RequireKey lets a request through only with a bearer key that is known, not
// revoked and not expired. Every refusal is the same 401 body, so a caller
// cannot tell a key that never existed from one that was revoked. The key and
// its client go in the request context. Every response is uncacheable and not
// content-sniffed.
func (h *Handler) RequireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		deny := func() { writeError(w, http.StatusUnauthorized, CodeUnauthorized, unauthorizedMessage) }
		key, ok := bearer(r)
		if !ok {
			deny()
			return
		}
		// An unknown prefix still hashes the key, so the refusal takes as long
		// as one for a wrong secret.
		hash := HashKey(key)
		prefix, ok := ParseKey(key)
		if !ok {
			deny()
			return
		}
		k, c, found, err := h.Store.APIKeyByPrefix(r.Context(), prefix)
		if err != nil {
			h.log().Error("review API: reading a key failed", "err", err)
			writeInternal(w)
			return
		}
		if !found || subtle.ConstantTimeCompare(k.Hash, hash) != 1 {
			deny()
			return
		}
		now := h.now()
		if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
			deny()
			return
		}
		// The touch is bookkeeping: a failure to write it never fails the call.
		if err := h.Store.TouchAPIKey(r.Context(), k.ID, now); err != nil {
			h.log().Warn("review API: recording a key's use failed", "key_id", k.ID, "err", err)
		}
		ctx := context.WithValue(r.Context(), keyCtx, k)
		ctx = context.WithValue(ctx, clientCtx, c)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearer reads the key from "Authorization: Bearer <key>". The scheme is not
// case-sensitive.
func bearer(r *http.Request) (string, bool) {
	scheme, key, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || key == "" {
		return "", false
	}
	return key, true
}

// LimitsBody is a client's caps, as GET /v1/client shows them.
type LimitsBody struct {
	MaxRequestBytes    int   `json:"max_request_bytes"`
	MaxFiles           int   `json:"max_files"`
	MaxInFlight        int   `json:"max_in_flight"`
	RequestsPerHour    int   `json:"requests_per_hour"`
	MonthlyTokenBudget int64 `json:"monthly_token_budget"`
}

// ClientBody is the body of GET /v1/client.
type ClientBody struct {
	Name   string     `json:"name"`
	Limits LimitsBody `json:"limits"`
}

func (h *Handler) client(w http.ResponseWriter, r *http.Request) {
	c := clientOf(r)
	writeJSON(w, http.StatusOK, ClientBody{
		Name: c.Name,
		Limits: LimitsBody{
			MaxRequestBytes:    c.Limits.MaxRequestBytes,
			MaxFiles:           c.Limits.MaxFiles,
			MaxInFlight:        c.Limits.MaxInFlight,
			RequestsPerHour:    c.Limits.RequestsPerHour,
			MonthlyTokenBudget: c.Limits.MonthlyTokenBudget,
		},
	})
}
