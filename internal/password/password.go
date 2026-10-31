// Package password hashes and checks the passwords of the dashboard's
// password accounts, with argon2id, and makes random ones.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MinLength is the fewest characters a password may have.
const MinLength = 12

// The argon2id parameters of new hashes: 19 MiB, 2 passes, one lane (the
// OWASP minimum), a 16-byte salt and a 32-byte key.
const (
	memoryKiB = 19456
	passes    = 2
	lanes     = 1
	saltBytes = 16
	keyBytes  = 32
)

// Bounds on the parameters Verify accepts from a stored hash, so a
// malformed one can't make it use unbounded memory or time.
const (
	maxMemoryKiB = 256 << 10
	maxPasses    = 16
	maxLanes     = 16
)

// generated is the alphabet of Generate; generatedLength its length.
const (
	generated       = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	generatedLength = 24
)

var b64 = base64.RawStdEncoding

// Hash returns the argon2id hash of pw as a PHC string:
// "$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>", salt and key in unpadded
// base64.
func Hash(pw string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: reading random bytes: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, passes, memoryKiB, lanes, keyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, passes, lanes, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether pw is the password encoded hashes, comparing in
// constant time. A malformed hash is false.
func Verify(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	var m, t uint32
	var p uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || n != 3 ||
		parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, p) {
		return false
	}
	if t < 1 || t > maxPasses || p < 1 || p > maxLanes || m < 8*uint32(p) || m > maxMemoryKiB {
		return false
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(key)))
	return subtle.ConstantTimeCompare(got, key) == 1
}

// Generate returns a random password of 24 letters and digits, read from
// crypto/rand.
func Generate() string {
	// 248 is the largest multiple of len(generated) below 256: bytes from it
	// up are dropped, so every character is equally likely.
	const limit = 256 - 256%len(generated)
	out := make([]byte, 0, generatedLength)
	buf := make([]byte, 2*generatedLength)
	for len(out) < generatedLength {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand failing means the platform is broken; never fall back.
			panic("password: reading random bytes: " + err.Error())
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < generatedLength {
				out = append(out, generated[int(b)%len(generated)])
			}
		}
	}
	return string(out)
}
