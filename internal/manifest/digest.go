package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Canonical returns the deterministic JSON representation used for digests
// and equality checks.
func (m *Manifest) Canonical() ([]byte, error) {
	return json.Marshal(m)
}

// Digest returns the hex SHA-256 of the canonical serialisation. Checkpoints
// use it to detect changed inputs before resuming.
func (m *Manifest) Digest() (string, error) {
	b, err := m.Canonical()
	if err != nil {
		return "", fmt.Errorf("manifest: digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Equal reports whether two manifests carry identical fields and timelines.
func Equal(a, b *Manifest) bool {
	ab, err := a.Canonical()
	if err != nil {
		return false
	}
	bb, err := b.Canonical()
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}
