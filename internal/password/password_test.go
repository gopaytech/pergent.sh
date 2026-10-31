package password

import (
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(h, "correct horse battery") {
		t.Error("Verify(right password) = false")
	}
	if Verify(h, "correct horse battery!") {
		t.Error("Verify(wrong password) = true")
	}
	for _, bad := range []string{
		"", "plain", "$argon2id$v=19$m=19456,t=2,p=1$", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=x,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$",
		"$argon2id$v=19$m=99999999,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaA",
		h + "$extra",
	} {
		if Verify(bad, "correct horse battery") {
			t.Errorf("Verify(%q) = true, want false for a malformed hash", bad)
		}
	}
	h2, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if h == h2 {
		t.Error("two hashes of one password are equal: no salt?")
	}
}

func TestHashFormat(t *testing.T) {
	h, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("Hash = %q, want the argon2id PHC prefix", h)
	}
	parts := strings.Split(h, "$")
	// "", "argon2id", "v=19", "m=...", salt, key
	if len(parts) != 6 || len(parts[4]) != 22 || len(parts[5]) != 43 {
		t.Errorf("Hash = %q, want a 16-byte salt and a 32-byte key in unpadded base64", h)
	}
}

func TestGenerate(t *testing.T) {
	a, b := Generate(), Generate()
	if len(a) != 24 {
		t.Errorf("len(Generate()) = %d, want 24", len(a))
	}
	for _, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			t.Errorf("Generate() = %q holds %q, want only letters and digits", a, r)
		}
	}
	if a == b {
		t.Errorf("two Generate() calls gave %q", a)
	}
	if len(a) < MinLength {
		t.Errorf("Generate() is shorter than MinLength")
	}
}
