// Package license defines the license document, its HMAC signature and
// issuance helpers.
package license

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"
)

// License authorizes decryption of a shard range with a key version.
type License struct {
	Version   string    `json:"version"`
	ShardFrom int       `json:"shard_from"`
	ShardTo   int       `json:"shard_to"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Nonce     string    `json:"nonce"`
	Machine   string    `json:"machine"`
	Signature string    `json:"signature"`
}

// payload is the signed portion of a license.
type payload struct {
	Version   string    `json:"version"`
	ShardFrom int       `json:"shard_from"`
	ShardTo   int       `json:"shard_to"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Nonce     string    `json:"nonce"`
	Machine   string    `json:"machine"`
}

func (l *License) payloadBytes() []byte {
	raw, _ := json.Marshal(payload{
		Version:   l.Version,
		ShardFrom: l.ShardFrom,
		ShardTo:   l.ShardTo,
		NotBefore: l.NotBefore,
		NotAfter:  l.NotAfter,
		Nonce:     l.Nonce,
		Machine:   l.Machine,
	})
	return raw
}

// Sign computes the hex HMAC-SHA256 signature over the license payload.
func Sign(key []byte, l *License) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(l.payloadBytes())
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks the license signature in constant time.
func VerifySignature(key []byte, l *License) bool {
	got, err := hex.DecodeString(l.Signature)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(Sign(key, l))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// MachineFingerprint derives a stable fingerprint of this machine.
func MachineFingerprint() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	sum := sha256.Sum256([]byte("machine/" + host))
	return hex.EncodeToString(sum[:8])
}

// Load reads a license from disk.
func Load(path string) (*License, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l License
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// Save writes the license as indented JSON.
func (l *License) Save(path string) error {
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
