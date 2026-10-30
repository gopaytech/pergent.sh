package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func pkcs1PEM(t *testing.T) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t))}))
}

func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

var appNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// testApp returns an App for app id 123 against a recording server, with a
// clock the test can move and sleeps skipped.
func testApp(t *testing.T, h http.HandlerFunc) (*App, *recorder, *time.Time) {
	t.Helper()
	srv, rec := server(t, h)
	a, err := NewApp(srv.URL, 123, pkcs1PEM(t), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := appNow
	var mu sync.Mutex
	a.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a.sleep = func(context.Context, time.Duration) error { return nil }
	return a, rec, &now
}

// verifyJWT checks the signature against the test key and returns the
// decoded header and claims.
func verifyJWT(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&rsaKey(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var hdr, claims map[string]any
	for i, out := range []*map[string]any{&hdr, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	return hdr, claims
}

func TestJWT(t *testing.T) {
	a, _, _ := testApp(t, jsonReply(nil))
	tok, err := a.JWT(appNow)
	if err != nil {
		t.Fatal(err)
	}
	hdr, claims := verifyJWT(t, tok)
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" || len(hdr) != 2 {
		t.Fatalf("header = %v", hdr)
	}
	want := map[string]any{
		"iat": float64(appNow.Add(-60 * time.Second).Unix()),
		"exp": float64(appNow.Add(9 * time.Minute).Unix()),
		"iss": "123",
	}
	if len(claims) != len(want) {
		t.Fatalf("claims = %v", claims)
	}
	for k, v := range want {
		if claims[k] != v {
			t.Errorf("%s = %v, want %v", k, claims[k], v)
		}
	}
}

func TestNewAppAcceptsPKCS8RSA(t *testing.T) {
	a, err := NewApp("", 123, pkcs8PEM(t, rsaKey(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.JWT(appNow)
	if err != nil {
		t.Fatal(err)
	}
	verifyJWT(t, tok)
	if a.base != "https://api.github.com" {
		t.Fatalf("default base = %q", a.base)
	}
}

func TestNewAppRejectsNonRSAAndBadID(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewApp("", 123, pkcs8PEM(t, ec), nil); err == nil {
		t.Fatal("accepted an EC key")
	}
	if _, err := NewApp("", 0, pkcs1PEM(t), nil); err == nil {
		t.Fatal("accepted app id 0")
	}
}

// No error from a bad key may carry any of the key's text.
func TestBadPEMErrorHasNoKey(t *testing.T) {
	good := pkcs1PEM(t)
	lines := strings.Split(strings.TrimSpace(good), "\n")
	body := strings.Join(lines[1:len(lines)-1], "")
	// Corrupt the DER: valid PEM framing, garbage inside.
	der, _ := base64.StdEncoding.DecodeString(body)
	der[10] ^= 0xff
	corrupt := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
	cases := map[string]string{
		"no PEM":         body,
		"truncated":      strings.Join(lines[:len(lines)/2], "\n"),
		"corrupt DER":    corrupt,
		"wrong type":     strings.Replace(good, "RSA PRIVATE KEY", "EC PRIVATE KEY", 2),
		"PKCS#8 garbage": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"empty":          "",
	}
	for name, key := range cases {
		_, err := NewApp("", 123, key, nil)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		for _, l := range lines[1 : len(lines)-1] {
			if len(l) >= 8 && strings.Contains(msg, l[:8]) {
				t.Errorf("%s: error carries key text: %q", name, msg)
			}
		}
		if strings.Contains(msg, "MII") || strings.Contains(msg, "BEGIN") {
			t.Errorf("%s: error carries key text: %q", name, msg)
		}
	}
}

func TestSlugUsesJWT(t *testing.T) {
	a, rec, _ := testApp(t, jsonReply(map[string]any{"id": 123, "slug": "acme-pergent-review"}))
	slug, err := a.Slug(ctx)
	if err != nil || slug != "acme-pergent-review" {
		t.Fatalf("Slug = %q, %v", slug, err)
	}
	got := rec.get(0)
	expect(t, got, "GET", "/app", "")
	auth := got.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("Authorization = %q", auth)
	}
	_, claims := verifyJWT(t, strings.TrimPrefix(auth, "Bearer "))
	if claims["iss"] != "123" {
		t.Fatalf("claims = %v", claims)
	}
	if got.Header.Get("X-Github-Api-Version") != "2022-11-28" || got.Header.Get("User-Agent") != "pergent-sh" {
		t.Fatalf("headers = %v", got.Header)
	}
}

// The bot user is public: looked up without credentials (a JWT is not
// accepted outside the /app endpoints).
func TestBotUserID(t *testing.T) {
	a, rec, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app" {
			jsonReply(map[string]any{"id": 123, "slug": "acme-pergent-review"})(w, r)
			return
		}
		jsonReply(map[string]any{"id": 4242, "login": "acme-pergent-review[bot]"})(w, r)
	})
	id, err := a.BotUserID(ctx)
	if err != nil || id != 4242 {
		t.Fatalf("BotUserID = %d, %v", id, err)
	}
	expect(t, rec.get(0), "GET", "/app", "")
	got := rec.get(1)
	expect(t, got, "GET", "/users/acme-pergent-review%5Bbot%5D", "")
	if got.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q", got.Header.Get("Authorization"))
	}
}

func TestInstallation(t *testing.T) {
	a, rec, _ := testApp(t, jsonReply(map[string]any{"id": 42, "app_id": 123}))
	id, err := a.Installation(ctx, "acme", "widgets")
	if err != nil || id != 42 {
		t.Fatalf("Installation = %d, %v", id, err)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets/installation", "")
	if !strings.HasPrefix(rec.get(0).Header.Get("Authorization"), "Bearer ey") {
		t.Fatal("installation lookup not authenticated with the JWT")
	}
}

func TestInstallationNotFound(t *testing.T) {
	a, _, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"Not Found"}`, 404) })
	if _, err := a.Installation(ctx, "acme", "widgets"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// tokenServer mints "inst-<n>" tokens that expire an hour after appNow and
// answers every other request with {"id":1}.
func tokenServer(mints *atomic.Int32, expires time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			n := mints.Add(1)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":       "ghs_inst-" + string(rune('0'+n)),
				"expires_at":  expires.Format(time.RFC3339),
				"permissions": map[string]string{"contents": "read", "metadata": "read"},
			})
			return
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	}
}

func TestInstallationTokenCached(t *testing.T) {
	var mints atomic.Int32
	expires := appNow.Add(time.Hour)
	a, rec, now := testApp(t, tokenServer(&mints, expires))
	c := a.Client(42)
	for i := 0; i < 2; i++ {
		if _, err := c.Repo(ctx, "acme", "widgets"); err != nil {
			t.Fatal(err)
		}
	}
	// A second Client for the same installation shares the cache.
	if _, err := a.Client(42).Repo(ctx, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 1 {
		t.Fatalf("mints = %d, want 1", mints.Load())
	}
	all := rec.all()
	expect(t, all[0], "POST", "/app/installations/42/access_tokens", "")
	if !strings.HasPrefix(all[0].Header.Get("Authorization"), "Bearer ey") {
		t.Fatal("token minted without the JWT")
	}
	for _, r := range all[1:] {
		if r.Header.Get("Authorization") != "Bearer ghs_inst-1" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
	}

	// More than 5 minutes left: still cached.
	*now = expires.Add(-5*time.Minute - time.Second)
	if _, err := c.Repo(ctx, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 1 {
		t.Fatalf("mints = %d, want 1 with 5m01s left", mints.Load())
	}
	// Fewer than 5 minutes left: a new token.
	*now = expires.Add(-5*time.Minute + time.Second)
	if _, err := c.Repo(ctx, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 2 {
		t.Fatalf("mints = %d, want 2 with 4m59s left", mints.Load())
	}
	all = rec.all()
	if got := all[len(all)-1].Header.Get("Authorization"); got != "Bearer ghs_inst-2" {
		t.Fatalf("Authorization after refresh = %q", got)
	}
}

func TestInstallationTokenPerInstallation(t *testing.T) {
	var mints atomic.Int32
	a, rec, _ := testApp(t, tokenServer(&mints, appNow.Add(time.Hour)))
	if _, err := a.Client(42).Repo(ctx, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Client(43).Repo(ctx, "acme", "gadgets"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 2 {
		t.Fatalf("mints = %d", mints.Load())
	}
	expect(t, rec.get(2), "POST", "/app/installations/43/access_tokens", "")
}

func TestInstallationTokenConcurrentMintsOnce(t *testing.T) {
	var mints atomic.Int32
	a, _, _ := testApp(t, tokenServer(&mints, appNow.Add(time.Hour)))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Client(42).Repo(ctx, "acme", "widgets")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if mints.Load() != 1 {
		t.Fatalf("mints = %d, want 1", mints.Load())
	}
}

// A failed mint is not cached: the next call mints again.
func TestInstallationTokenMintFailureNotCached(t *testing.T) {
	var mints atomic.Int32
	fail := true
	var mu sync.Mutex
	ok := tokenServer(&mints, appNow.Add(time.Hour))
	a, _, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.Error(w, `{"message":"Bad credentials"}`, 401)
			return
		}
		ok(w, r)
	})
	if _, err := a.Client(42).Repo(ctx, "acme", "widgets"); err == nil {
		t.Fatal("expected the mint error")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := a.Client(42).Repo(ctx, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}
}

func TestCloneTokenScoped(t *testing.T) {
	var mints atomic.Int32
	expires := appNow.Add(time.Hour)
	a, rec, _ := testApp(t, tokenServer(&mints, expires))
	for i := 1; i <= 2; i++ {
		tok, exp, err := a.CloneToken(ctx, 42, 99)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "ghs_inst-"+string(rune('0'+i)) || !exp.Equal(expires) {
			t.Fatalf("CloneToken = %q, %v", tok, exp)
		}
	}
	if mints.Load() != 2 {
		t.Fatalf("mints = %d, want 2 (never cached)", mints.Load())
	}
	got := rec.get(0)
	expect(t, got, "POST", "/app/installations/42/access_tokens", "")
	jsonEq(t, got.Body, `{"repository_ids":[99],"permissions":{"contents":"read"}}`)
	// A clone token doesn't feed the installation-token cache.
	if _, err := a.Client(42).Repo(ctx, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 3 {
		t.Fatalf("mints = %d, want 3", mints.Load())
	}
}

// GitHub answering with more than read access means the scope wasn't applied:
// the token is not handed out.
func TestCloneTokenRefusesWiderPermissions(t *testing.T) {
	a, _, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"token":"ghs_wide","expires_at":"2026-10-30T13:00:00Z","permissions":{"contents":"write","metadata":"read"}}`)
	})
	tok, _, err := a.CloneToken(ctx, 42, 99)
	if err == nil || tok != "" {
		t.Fatalf("CloneToken = %q, %v", tok, err)
	}
	if strings.Contains(err.Error(), "ghs_wide") {
		t.Fatalf("err = %v", err)
	}
}

// Neither the JWT nor an installation token may reach an error.
func TestAppTokensNeverInErrors(t *testing.T) {
	var mints atomic.Int32
	echo := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, "echo: "+r.Header.Get("Authorization"))
	}
	ok := tokenServer(&mints, appNow.Add(time.Hour))
	var seen []string
	var mu sync.Mutex
	a, _, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/access_tokens") && r.URL.Path != "/app/installations/1/access_tokens" {
			ok(w, r)
			return
		}
		echo(w, r)
	})
	var errs []error
	_, err := a.Slug(ctx)
	errs = append(errs, err)
	_, _, err = a.CloneToken(ctx, 1, 99)
	errs = append(errs, err)
	_, err = a.Client(42).Repo(ctx, "acme", "widgets")
	errs = append(errs, err)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("requests = %d", len(seen))
	}
	for i, err := range errs {
		if err == nil {
			t.Fatalf("call %d: expected an error", i)
		}
		for _, tok := range seen {
			if tok != "" && strings.Contains(err.Error(), tok) {
				t.Errorf("call %d: error contains a token: %v", i, err)
			}
		}
		if !strings.Contains(err.Error(), "echo: Bearer ***") {
			t.Errorf("call %d: err = %v, want the token masked", i, err)
		}
	}
}

// A token that can't be minted (404: the installation is gone) is an
// installation error and never ErrNotFound: callers would read it as the
// repository, file or pull request they asked for being missing.
func TestMintFailureIsInstallationErrorNotNotFound(t *testing.T) {
	a, _, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	_, err := a.Client(42).Repo(ctx, "acme", "widgets")
	_, _, cerr := a.CloneToken(ctx, 42, 900)
	for name, err := range map[string]error{"installation token": err, "clone token": cerr} {
		if err == nil || errors.Is(err, ErrNotFound) || !errors.Is(err, ErrInstallation) {
			t.Errorf("%s: err = %v; want ErrInstallation and not ErrNotFound", name, err)
		}
		var ae *APIError
		if errors.As(err, &ae) {
			t.Errorf("%s: err unwraps to the APIError %v", name, ae)
		}
		if !InstallationGone(err) {
			t.Errorf("%s: InstallationGone(%v) = false, want true", name, err)
		}
	}
	if InstallationGone(&APIError{Status: 404}) || InstallationGone(nil) {
		t.Error("InstallationGone matches a plain 404 or nil")
	}
}

// A mint refused for another reason is an installation error that isn't
// "gone".
func TestMintFailureOtherStatus(t *testing.T) {
	a, _, _ := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"suspended"}`)
	})
	_, err := a.Client(42).Repo(ctx, "acme", "widgets")
	if !errors.Is(err, ErrInstallation) || InstallationGone(err) {
		t.Fatalf("err = %v; want ErrInstallation, not gone", err)
	}
}

func TestInstallations(t *testing.T) {
	a, rec, _ := testApp(t, jsonReply([]map[string]any{{"id": 42}, {"id": 43}}))
	ids, err := a.Installations(ctx)
	if err != nil || len(ids) != 2 || ids[0] != 42 || ids[1] != 43 {
		t.Fatalf("Installations = %v, %v", ids, err)
	}
	expect(t, rec.get(0), "GET", "/app/installations", "per_page=100")
	if !strings.HasPrefix(rec.get(0).Header.Get("Authorization"), "Bearer ey") {
		t.Fatal("installations listing not authenticated with the JWT")
	}
}
