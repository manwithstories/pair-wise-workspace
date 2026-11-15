// Package cryptox wraps AES-256-GCM with deterministic nonce derivation
// and key derivation helpers. Nonces are derived from key id, shard index
// and rotation epoch so a (key, nonce) pair is never reused.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
)

// NonceSize is the AES-GCM standard nonce size.
const NonceSize = 12

// TagSize is the AES-GCM authentication tag size.
const TagSize = 16

// DeriveKey derives a per-version AES-256 key from the master key.
func DeriveKey(master []byte, keyID string) []byte {
	mac := hmac.New(sha256.New, master)
	fmt.Fprintf(mac, "key/%s", keyID)
	return mac.Sum(nil)
}

// SigningKey derives the license HMAC signing key. It is only ever held
// in memory and never written to disk.
func SigningKey(master []byte) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("license-signing"))
	return mac.Sum(nil)
}

// Nonce derives a 12-byte GCM nonce from key id, shard index and epoch.
func Nonce(keyID string, shardIndex, epoch int) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("nonce/%s/%d/%d", keyID, shardIndex, epoch)))
	return sum[:NonceSize]
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with AES-256-GCM binding aad.
func Seal(key, nonce, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, aad), nil
}

// Open decrypts ciphertext produced by Seal.
func Open(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, aad)
}
