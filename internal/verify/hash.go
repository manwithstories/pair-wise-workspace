// Package verify computes per-segment SHA-256 hashes and locates
// mismatches by segment index.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Sum256 returns the hex SHA-256 of b.
func Sum256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Mismatch locates one segment whose hash does not match.
type Mismatch struct {
	Segment int
	Want    string
	Got     string
}

func (m Mismatch) Error() string {
	return fmt.Sprintf("verify: segment %d hash mismatch: want %s got %s",
		m.Segment, m.Want, m.Got)
}

// HashSegments hashes each segment's content in order.
func HashSegments(contents [][]byte) []string {
	out := make([]string, len(contents))
	for i, c := range contents {
		out[i] = Sum256(c)
	}
	return out
}

// CheckSegments compares recomputed hashes against the expected list and
// returns every mismatch with its segment index.
func CheckSegments(contents [][]byte, want []string) []Mismatch {
	var bad []Mismatch
	for i, c := range contents {
		if i >= len(want) {
			bad = append(bad, Mismatch{Segment: i, Want: "<missing>", Got: Sum256(c)})
			continue
		}
		if got := Sum256(c); got != want[i] {
			bad = append(bad, Mismatch{Segment: i, Want: want[i], Got: got})
		}
	}
	return bad
}
