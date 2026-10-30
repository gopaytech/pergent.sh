package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func newKey(t *testing.T, n int) string {
	t.Helper()
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestBoxRoundTrip(t *testing.T) {
	b, err := New(newKey(t, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("glpat-secret-deploy-token")
	sealed := b.Seal(plain)
	if bytes.Contains(sealed, plain) {
		t.Fatal("sealed output contains plaintext")
	}
	got, err := b.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q, want %q", got, plain)
	}
	// Two seals of the same plaintext differ (random nonce).
	if bytes.Equal(sealed, b.Seal(plain)) {
		t.Fatal("seal is deterministic")
	}
}

func TestBoxRejectsShortKey(t *testing.T) {
	if _, err := New(newKey(t, 16)); err == nil {
		t.Fatal("16-byte key accepted")
	}
	if _, err := New("not base64 !!"); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	if _, err := New(""); err == nil {
		t.Fatal("empty key accepted")
	}
}

func TestBoxOpenTampered(t *testing.T) {
	b, _ := New(newKey(t, 32))
	sealed := b.Seal([]byte("hello"))
	sealed[len(sealed)-1] ^= 0xff
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if _, err := b.Open([]byte("short")); err == nil {
		t.Fatal("short input opened")
	}
	other, _ := New(newKey(t, 32))
	if _, err := other.Open(b.Seal([]byte("x"))); err == nil {
		t.Fatal("opened with wrong key")
	}
}
