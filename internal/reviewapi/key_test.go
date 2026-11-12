package reviewapi

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewKeyParses(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		key, prefix, hash, err := NewKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(key, "pgk_"+prefix+"_") || len(prefix) != 8 {
			t.Fatalf("key %q does not have the shape pgk_<8 hex>_<secret> for prefix %q", key, prefix)
		}
		got, ok := ParseKey(key)
		if !ok || got != prefix {
			t.Fatalf("ParseKey(%q) = %q, %v; want %q, true", key, got, ok, prefix)
		}
		if !bytes.Equal(HashKey(key), hash) {
			t.Fatalf("HashKey(key) differs from the hash NewKey returned")
		}
		if seen[key] {
			t.Fatalf("key %q was made twice", key)
		}
		seen[key] = true
	}
}

func TestParseKeyRefuses(t *testing.T) {
	for _, key := range []string{
		"", "pgk_", "pgk_0a1b2c3d", "pgk_0A1B2C3D_x", "pgx_0a1b2c3d_x", "pgk_0a1b2c3_xx",
	} {
		if prefix, ok := ParseKey(key); ok {
			t.Errorf("ParseKey(%q) = %q, true; want false", key, prefix)
		}
	}
}

func TestNewCallbackSecret(t *testing.T) {
	a, err := NewCallbackSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewCallbackSecret()
	if !strings.HasPrefix(a, "pgs_") || len(a) != len("pgs_")+43 || a == b {
		t.Errorf("secrets %q and %q: want pgs_ and 43 base64url characters, never the same", a, b)
	}
}
