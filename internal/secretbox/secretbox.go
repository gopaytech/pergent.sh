// Package secretbox seals small secrets (stored deploy tokens) with AES-256-GCM.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Box seals and opens values with one 32-byte key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a standard-base64 encoded 32-byte key.
func New(keyBase64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, errors.New("secretbox: key is not valid standard base64")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secretbox: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal returns nonce || ciphertext. A fresh random nonce is used every call.
func (b *Box) Seal(plain []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand failing means the platform is broken; never fall back.
		panic("secretbox: reading random nonce: " + err.Error())
	}
	return b.aead.Seal(nonce, nonce, plain, nil)
}

// Open reverses Seal. It fails on any modification or a different key.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, errors.New("secretbox: sealed value too short")
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, errors.New("secretbox: cannot open sealed value")
	}
	return plain, nil
}
