// Package secrets encrypts per-admin secrets at rest and resolves them for
// rule templates. Values are write-only from the API's point of view: only
// the action runner ever sees plaintext.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Box is AES-256-GCM with a key supplied out of band (Y6IFTTT_SECRETS_KEY),
// so the database alone (or a backup of it) reveals nothing.
type Box struct{ aead cipher.AEAD }

// ParseKey accepts 32 bytes as hex (64 chars) or standard/URL base64.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("Y6IFTTT_SECRETS_KEY must be 32 random bytes as hex (openssl rand -hex 32) or base64")
}

func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// aad binds a ciphertext to its owner and name, so rows can't be swapped
// between users or renamed in the database to borrow someone else's value.
func aad(ownerID, name string) []byte {
	return []byte("y6ifttt/secret/v1\x00" + ownerID + "\x00" + name)
}

// Seal returns nonce||ciphertext.
func (b *Box) Seal(ownerID, name, plaintext string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), aad(ownerID, name)), nil
}

func (b *Box) Open(ownerID, name string, sealed []byte) (string, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("ciphertext too short")
	}
	pt, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad(ownerID, name))
	if err != nil {
		return "", errors.New("secret could not be decrypted (wrong Y6IFTTT_SECRETS_KEY or tampered data)")
	}
	return string(pt), nil
}
