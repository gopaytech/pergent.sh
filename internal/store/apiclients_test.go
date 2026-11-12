package store

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"
)

func newAPIClient(t *testing.T, s *Store, name string) APIClient {
	t.Helper()
	c, err := s.CreateAPIClient(ctx, APIClient{Name: name, Skills: []string{"gateway"}, CallbackSecretSealed: []byte("sealed-" + name)})
	if err != nil {
		t.Fatalf("CreateAPIClient(%s): %v", name, err)
	}
	return c
}

func newAPIKey(t *testing.T, s *Store, clientID int64, name, prefix string) APIKey {
	t.Helper()
	k, err := s.CreateAPIKey(ctx, APIKey{ClientID: clientID, Name: name, Prefix: prefix, Hash: hash(prefix)})
	if err != nil {
		t.Fatalf("CreateAPIKey(%s): %v", name, err)
	}
	return k
}

func TestCreateAPIClientRoundTrip(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	if c.ID == 0 || c.CreatedAt.IsZero() {
		t.Fatalf("created client has no ID or time: %+v", c)
	}
	if c.Limits != DefaultAPILimits {
		t.Errorf("limits = %+v, want the defaults %+v", c.Limits, DefaultAPILimits)
	}

	got, found, err := s.APIClientByName(ctx, "fastgateway")
	if err != nil || !found {
		t.Fatalf("APIClientByName: found=%v err=%v", found, err)
	}
	if !slices.Equal(got.Skills, []string{"gateway"}) {
		t.Errorf("skills = %v", got.Skills)
	}
	if !bytes.Equal(got.CallbackSecretSealed, []byte("sealed-fastgateway")) {
		t.Errorf("sealed secret = %q", got.CallbackSecretSealed)
	}
	if got.ModelID != "" || got.CallbackURL != "" || got.PreviousSecretSealed != nil || !got.PreviousSecretUntil.IsZero() {
		t.Errorf("unset fields are not empty: %+v", got)
	}
	if got.Limits != DefaultAPILimits {
		t.Errorf("stored limits = %+v, want the defaults", got.Limits)
	}
	byID, found, err := s.APIClientByID(ctx, got.ID)
	if err != nil || !found || byID.Name != "fastgateway" {
		t.Errorf("APIClientByID = %+v, %v, %v", byID, found, err)
	}
	if _, found, err := s.APIClientByName(ctx, "nobody"); err != nil || found {
		t.Errorf("APIClientByName(nobody): found=%v err=%v", found, err)
	}

	if _, err := s.CreateAPIClient(ctx, APIClient{Name: "fastgateway", CallbackSecretSealed: []byte("x")}); !errors.Is(err, ErrAPIClientExists) {
		t.Errorf("second create = %v, want ErrAPIClientExists", err)
	}
}

func TestAPIClientsByName(t *testing.T) {
	s := newStore(t)
	newAPIClient(t, s, "zeta")
	newAPIClient(t, s, "alpha")
	list, err := s.APIClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Errorf("APIClients = %+v, want alpha then zeta", list)
	}
}

func TestUpdateAPIClientKeepsName(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	got, found, err := s.UpdateAPIClient(ctx, "fastgateway", func(c *APIClient) {
		c.Name = "x"
		c.ID = 999
		c.CreatedAt = t0
		c.Skills = []string{"a", "b"}
		c.ModelID = "m1"
		c.Limits.MaxFiles = 10
	})
	if err != nil || !found {
		t.Fatalf("UpdateAPIClient: found=%v err=%v", found, err)
	}
	if got.Name != "fastgateway" || got.ID != c.ID || !got.CreatedAt.Equal(c.CreatedAt) {
		t.Errorf("name, ID or CreatedAt changed: %+v", got)
	}
	if !got.UpdatedAt.After(c.UpdatedAt) {
		t.Errorf("UpdatedAt %v did not move past %v", got.UpdatedAt, c.UpdatedAt)
	}
	stored, _, err := s.APIClientByName(ctx, "fastgateway")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stored.Skills, []string{"a", "b"}) || stored.ModelID != "m1" || stored.Limits.MaxFiles != 10 {
		t.Errorf("changes not saved: %+v", stored)
	}
	if _, found, err := s.APIClientByName(ctx, "x"); err != nil || found {
		t.Errorf("a client named x exists: found=%v err=%v", found, err)
	}
	if _, found, err := s.UpdateAPIClient(ctx, "nobody", func(*APIClient) {}); err != nil || found {
		t.Errorf("update of a missing client: found=%v err=%v", found, err)
	}
}

func TestAPIClientRotationFieldsRoundTrip(t *testing.T) {
	s := newStore(t)
	newAPIClient(t, s, "fastgateway")
	until := t0.Add(time.Hour)
	if _, _, err := s.UpdateAPIClient(ctx, "fastgateway", func(c *APIClient) {
		c.PreviousSecretSealed = []byte("old")
		c.PreviousSecretUntil = until
		c.CallbackURL = "https://example.com/hook"
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.APIClientByName(ctx, "fastgateway")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.PreviousSecretSealed) != "old" || !got.PreviousSecretUntil.Equal(until) || got.CallbackURL != "https://example.com/hook" {
		t.Errorf("rotation fields = %+v", got)
	}
}

func TestAPIKeyLookup(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	k := newAPIKey(t, s, c.ID, "CI", "0a1b2c3d")
	if k.ID == 0 || k.CreatedAt.IsZero() {
		t.Fatalf("created key has no ID or time: %+v", k)
	}

	gotKey, gotClient, found, err := s.APIKeyByPrefix(ctx, "0a1b2c3d")
	if err != nil || !found {
		t.Fatalf("APIKeyByPrefix: found=%v err=%v", found, err)
	}
	if gotKey.ID != k.ID || gotKey.ClientID != c.ID || !bytes.Equal(gotKey.Hash, hash("0a1b2c3d")) {
		t.Errorf("key = %+v", gotKey)
	}
	if gotClient.ID != c.ID || gotClient.Name != "fastgateway" || gotClient.Limits != DefaultAPILimits {
		t.Errorf("client = %+v", gotClient)
	}
	if _, _, found, err := s.APIKeyByPrefix(ctx, "ffffffff"); err != nil || found {
		t.Errorf("APIKeyByPrefix(ffffffff): found=%v err=%v", found, err)
	}

	other := newAPIClient(t, s, "other")
	if _, err := s.CreateAPIKey(ctx, APIKey{ClientID: other.ID, Name: "x", Prefix: "0a1b2c3d", Hash: hash("y")}); !errors.Is(err, ErrAPIPrefixTaken) {
		t.Errorf("same prefix = %v, want ErrAPIPrefixTaken", err)
	}
	if _, err := s.CreateAPIKey(ctx, APIKey{ClientID: c.ID, Name: "ci", Prefix: "11111111", Hash: hash("y")}); !errors.Is(err, ErrAPIKeyExists) {
		t.Errorf("same name in another case = %v, want ErrAPIKeyExists", err)
	}
	// The same name under another client is fine.
	newAPIKey(t, s, other.ID, "CI", "22222222")
}

func TestAPIKeysNewestFirst(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	exp := t0.Add(24 * time.Hour)
	first := newAPIKey(t, s, c.ID, "first", "00000001")
	second, err := s.CreateAPIKey(ctx, APIKey{ClientID: c.ID, Name: "second", Prefix: "00000002", Hash: hash("2"), ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.APIKeys(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].ID != second.ID || keys[1].ID != first.ID {
		t.Fatalf("APIKeys = %+v, want second then first", keys)
	}
	if keys[0].ExpiresAt == nil || !keys[0].ExpiresAt.Equal(exp) || keys[1].ExpiresAt != nil || keys[1].LastUsedAt != nil {
		t.Errorf("expiry or last use wrong: %+v", keys)
	}
}

func TestDeleteAPIKeyStopsLookup(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	other := newAPIClient(t, s, "other")
	k := newAPIKey(t, s, c.ID, "CI", "0a1b2c3d")

	if _, found, err := s.DeleteAPIKey(ctx, other.ID, k.ID); err != nil || found {
		t.Fatalf("delete through another client: found=%v err=%v", found, err)
	}
	if _, _, found, _ := s.APIKeyByPrefix(ctx, "0a1b2c3d"); !found {
		t.Fatal("another client's delete removed the key")
	}
	gone, found, err := s.DeleteAPIKey(ctx, c.ID, k.ID)
	if err != nil || !found || gone.ID != k.ID || gone.Prefix != "0a1b2c3d" {
		t.Fatalf("DeleteAPIKey = %+v, %v, %v", gone, found, err)
	}
	if _, _, found, err := s.APIKeyByPrefix(ctx, "0a1b2c3d"); err != nil || found {
		t.Errorf("lookup after delete: found=%v err=%v", found, err)
	}
	if _, found, err := s.DeleteAPIKey(ctx, c.ID, k.ID); err != nil || found {
		t.Errorf("second delete: found=%v err=%v", found, err)
	}
}

func TestTouchAPIKeyThrottles(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	k := newAPIKey(t, s, c.ID, "CI", "0a1b2c3d")
	lastUsed := func() time.Time {
		t.Helper()
		got, _, found, err := s.APIKeyByPrefix(ctx, "0a1b2c3d")
		if err != nil || !found {
			t.Fatalf("APIKeyByPrefix: found=%v err=%v", found, err)
		}
		if got.LastUsedAt == nil {
			t.Fatal("last_used_at is not set")
		}
		return *got.LastUsedAt
	}

	if err := s.TouchAPIKey(ctx, k.ID, t0); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(t0) {
		t.Fatalf("first touch = %v, want %v", got, t0)
	}
	if err := s.TouchAPIKey(ctx, k.ID, t0.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(t0) {
		t.Errorf("touch after 30s = %v, want it kept at %v", got, t0)
	}
	if err := s.TouchAPIKey(ctx, k.ID, t0.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(t0.Add(61 * time.Second)) {
		t.Errorf("touch after 61s = %v, want it moved", got)
	}
}

func TestDeleteAPIClientCascadesKeys(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	newAPIKey(t, s, c.ID, "CI", "0a1b2c3d")

	jobs, found, err := s.DeleteAPIClient(ctx, "fastgateway")
	if err != nil || !found || jobs != nil {
		t.Fatalf("DeleteAPIClient = %v, %v, %v", jobs, found, err)
	}
	if _, _, found, err := s.APIKeyByPrefix(ctx, "0a1b2c3d"); err != nil || found {
		t.Errorf("key survived its client: found=%v err=%v", found, err)
	}
	if _, found, err := s.APIClientByName(ctx, "fastgateway"); err != nil || found {
		t.Errorf("client survived: found=%v err=%v", found, err)
	}
	if _, found, err := s.DeleteAPIClient(ctx, "fastgateway"); err != nil || found {
		t.Errorf("second delete: found=%v err=%v", found, err)
	}
}
