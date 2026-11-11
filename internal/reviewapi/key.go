// Package reviewapi is the review API, v1: the /v1/ routes an outside system
// calls with a bearer key to ask for reviews and read their results.
package reviewapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	keyScheme      = "pgk_"
	callbackScheme = "pgs_"
	// prefixLen is the length of a key's prefix: 4 random bytes in hex.
	prefixLen = 8
)

// NewKey makes a client key, "pgk_<prefix>_<secret>": prefix is 8 lowercase hex
// characters that find the key's row, secret is 32 random bytes in base64url.
// hash is what is stored; the key itself is shown once.
func NewKey() (key, prefix string, hash []byte, err error) {
	p := make([]byte, prefixLen/2)
	s := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		return "", "", nil, err
	}
	if _, err := rand.Read(s); err != nil {
		return "", "", nil, err
	}
	prefix = hex.EncodeToString(p)
	key = keyScheme + prefix + "_" + base64.RawURLEncoding.EncodeToString(s)
	return key, prefix, HashKey(key), nil
}

// ParseKey returns the prefix of a key in the shape NewKey makes, and false
// for anything else. It does not tell whether the key is real.
func ParseKey(key string) (prefix string, ok bool) {
	rest, found := strings.CutPrefix(key, keyScheme)
	if !found || len(rest) < prefixLen+2 || rest[prefixLen] != '_' {
		return "", false
	}
	for _, c := range []byte(rest[:prefixLen]) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return rest[:prefixLen], true
}

// HashKey is the SHA-256 of the whole key, the form that is stored.
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// NewCallbackSecret makes the secret that signs a client's callbacks:
// "pgs_" and 32 random bytes in base64url.
func NewCallbackSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return callbackScheme + base64.RawURLEncoding.EncodeToString(b), nil
}
