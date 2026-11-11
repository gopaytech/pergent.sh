package reviewapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

type fakeStore struct {
	keys     map[string]fakeKey
	touched  []int64
	touchErr error
	lookErr  error

	// The review API's side. library holds the skills that exist, reviews the
	// API reviews by ID, enqueued every request that made one.
	library  map[string]bool
	reviews  map[string]store.APIReview
	hashes   map[string][]byte // "<client>/<idempotency key>" -> request hash
	keyed    map[string]string // "<client>/<idempotency key>" -> review ID
	enqueued []store.NewAPIReview
	// readErr fails APIReview.
	readErr error
	// dismissals by "<client>/<tenant>/<subject>/<fingerprint>", and every
	// dismiss or restore that changed one (see dismiss_test.go).
	dismissals map[string]store.APIDismissal
	events     []fakeEvent
	// dismissErr fails DismissAPIFinding and UndismissAPIFinding.
	dismissErr error
}

type fakeKey struct {
	key    store.APIKey
	client store.APIClient
}

func (f *fakeStore) APIKeyByPrefix(_ context.Context, prefix string) (store.APIKey, store.APIClient, bool, error) {
	if f.lookErr != nil {
		return store.APIKey{}, store.APIClient{}, false, f.lookErr
	}
	k, ok := f.keys[prefix]
	return k.key, k.client, ok, nil
}

func (f *fakeStore) TouchAPIKey(_ context.Context, id int64, _ time.Time) error {
	f.touched = append(f.touched, id)
	return f.touchErr
}

func (f *fakeStore) ResolveLibrary(_ context.Context, names []string) (store.ResolvedLibrary, error) {
	out := store.ResolvedLibrary{Skills: map[string]store.LibrarySkill{}}
	for _, n := range names {
		if f.library[n] {
			out.Skills[n] = store.LibrarySkill{Name: n}
		} else {
			out.Missing = append(out.Missing, n)
		}
	}
	return out, nil
}

func (f *fakeStore) EnqueueAPIReview(_ context.Context, n store.NewAPIReview) (string, bool, error) {
	slot := fmt.Sprintf("%d/%s", n.ClientID, n.IdempotencyKey)
	if n.IdempotencyKey != "" {
		if id, ok := f.keyed[slot]; ok {
			if string(f.hashes[slot]) != string(n.RequestHash) {
				return "", false, store.ErrIdempotencyMismatch
			}
			return id, false, nil
		}
	}
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.enqueued)+1)
	f.enqueued = append(f.enqueued, n)
	if f.reviews == nil {
		f.reviews = map[string]store.APIReview{}
	}
	f.reviews[id] = store.APIReview{
		Run: store.Run{ID: id, State: store.Queued}, ClientID: n.ClientID, Tenant: n.Tenant, Subject: n.Subject,
		Title: n.Title, KeyName: n.KeyName, HasInputs: true, CreatedAt: testNow,
	}
	if n.IdempotencyKey != "" {
		if f.keyed == nil {
			f.keyed, f.hashes = map[string]string{}, map[string][]byte{}
		}
		f.keyed[slot], f.hashes[slot] = id, n.RequestHash
	}
	return id, true, nil
}

// APIReview is the real one's contract: another client's review, an unknown ID
// and a malformed one are all not found.
func (f *fakeStore) APIReview(_ context.Context, clientID int64, id string) (store.APIReview, bool, error) {
	if f.readErr != nil {
		return store.APIReview{}, false, f.readErr
	}
	r, ok := f.reviews[id]
	if !ok || r.ClientID != clientID {
		return store.APIReview{}, false, nil
	}
	return r, true, nil
}

func (f *fakeStore) APIReviewByKey(ctx context.Context, clientID int64, key string) (store.APIReview, []byte, bool, error) {
	slot := fmt.Sprintf("%d/%s", clientID, key)
	id, ok := f.keyed[slot]
	if !ok {
		return store.APIReview{}, nil, false, nil
	}
	r, found, err := f.APIReview(ctx, clientID, id)
	return r, f.hashes[slot], found, err
}

type fakeModels struct {
	byID map[string]instances.Model
	def  *instances.Model
}

func (m fakeModels) ModelByID(id string) (instances.Model, bool) {
	v, ok := m.byID[id]
	return v, ok
}

func (m fakeModels) DefaultModel() (instances.Model, bool) {
	if m.def == nil {
		return instances.Model{}, false
	}
	return *m.def, true
}

var testNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// newHandler makes a handler over a store that holds one good key, and returns
// the key.
func newHandler(t *testing.T) (*Handler, *fakeStore, string, *http.ServeMux) {
	t.Helper()
	key, prefix, hash, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{
		keys: map[string]fakeKey{prefix: {
			key:    store.APIKey{ID: 7, ClientID: 3, Name: "ci", Prefix: prefix, Hash: hash},
			client: store.APIClient{ID: 3, Name: "fastgateway", Skills: []string{"security"}, Limits: store.DefaultAPILimits},
		}},
		library: map[string]bool{"security": true, "style": true},
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: fs, Box: box, Now: func() time.Time { return testNow },
		Models: fakeModels{def: &instances.Model{ID: "default", Default: true}}}
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, fs, key, mux
}

func call(mux *http.ServeMux, method, path, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestRequireKey401(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	prefix, _ := ParseKey(key)
	// The right prefix with a wrong secret: the good key with its first secret
	// character changed to another one, so it can never equal the key.
	head := "pgk_" + prefix + "_"
	first := "A"
	if key[len(head)] == 'A' {
		first = "B"
	}
	wrong := head + first + key[len(head)+1:]
	// An expired key: it expires at the instant of the call.
	expiredKey, expiredPrefix, expiredHash, _ := NewKey()
	expiry := testNow
	fs.keys[expiredPrefix] = fakeKey{
		key:    store.APIKey{ID: 8, ClientID: 3, Prefix: expiredPrefix, Hash: expiredHash, ExpiresAt: &expiry},
		client: store.APIClient{ID: 3, Name: "fastgateway"},
	}
	unknown, _, _, _ := NewKey()

	const want = `{"code":"unauthorized","message":"missing, unknown, revoked or expired key"}` + "\n"
	for name, authorization := range map[string]string{
		"no header":    "",
		"basic":        "Basic x",
		"unknown":      "Bearer " + unknown,
		"wrong secret": "Bearer " + wrong,
		"expired":      "Bearer " + expiredKey,
		"not a key":    "Bearer nonsense",
		"empty bearer": "Bearer ",
	} {
		rec := call(mux, "GET", "/v1/client", authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
		if rec.Body.String() != want {
			t.Errorf("%s: body %q, want %q", name, rec.Body.String(), want)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", name, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q, want nosniff", name, got)
		}
	}
	if len(fs.touched) != 0 {
		t.Errorf("a refused key was touched: %v", fs.touched)
	}
}

func TestRequireKeyAcceptsKeyNotYetExpired(t *testing.T) {
	_, fs, _, mux := newHandler(t)
	key, prefix, hash, _ := NewKey()
	later := testNow.Add(time.Second)
	fs.keys[prefix] = fakeKey{
		key:    store.APIKey{ID: 9, ClientID: 3, Prefix: prefix, Hash: hash, ExpiresAt: &later},
		client: store.APIClient{ID: 3, Name: "fastgateway", Limits: store.DefaultAPILimits},
	}
	if rec := call(mux, "GET", "/v1/client", "Bearer "+key); rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200", rec.Code)
	}
}

func TestRequireKeyTouches(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	rec := call(mux, "GET", "/v1/client", "Bearer "+key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if len(fs.touched) != 1 || fs.touched[0] != 7 {
		t.Errorf("touched %v, want [7]", fs.touched)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
}

func TestRequireKeyIgnoresTouchFailure(t *testing.T) {
	h, fs, key, mux := newHandler(t)
	h.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	fs.touchErr = errors.New("database down")
	if rec := call(mux, "GET", "/v1/client", "Bearer "+key); rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200: the touch is bookkeeping", rec.Code)
	}
}

func TestRequireKeyStoreFailureIs500Internal(t *testing.T) {
	h, fs, key, mux := newHandler(t)
	h.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	fs.lookErr = errors.New("database down")
	rec := call(mux, "GET", "/v1/client", "Bearer "+key)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
	if want := `{"code":"internal","message":"internal error"}` + "\n"; rec.Body.String() != want {
		t.Errorf("body %q, want %q", rec.Body.String(), want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
}

func TestRequireKeyPutsKeyAndClientInContext(t *testing.T) {
	h, _, key, _ := newHandler(t)
	var gotKey store.APIKey
	var gotClient store.APIClient
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotKey, gotClient = keyOf(r), clientOf(r)
	})
	req := httptest.NewRequest("GET", "/v1/anything", nil)
	req.Header.Set("Authorization", "bearer "+key)
	h.RequireKey(next).ServeHTTP(httptest.NewRecorder(), req)
	if gotKey.ID != 7 || gotClient.ID != 3 {
		t.Errorf("context holds key %d and client %d, want 7 and 3", gotKey.ID, gotClient.ID)
	}
}

func TestClientBodyShowsLimits(t *testing.T) {
	_, _, key, mux := newHandler(t)
	rec := call(mux, "GET", "/v1/client", "Bearer "+key)
	const want = `{"name":"fastgateway","limits":{"max_request_bytes":524288,"max_files":200,"max_in_flight":5,"requests_per_hour":600,"monthly_token_budget":10000000}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body %q, want %q", rec.Body.String(), want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, want application/json", got)
	}
}

func TestUnknownV1PathIs404JSON(t *testing.T) {
	_, _, key, mux := newHandler(t)
	rec := call(mux, "GET", "/v1/nope", "Bearer "+key)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
	if want := `{"code":"not_found","message":"not found"}` + "\n"; rec.Body.String() != want {
		t.Errorf("body %q, want %q", rec.Body.String(), want)
	}
	// Without a key the same path is the 401, not a hint that it is unknown.
	if rec := call(mux, "GET", "/v1/nope", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", rec.Code)
	}
}
