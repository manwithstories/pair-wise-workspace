// Package report aggregates per-frame checksums into a run summary
// hash used to prove that a resumed run matches a complete run.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Summary accumulates frame checksums in order.
type Summary struct {
	sums [][32]byte
}

// AddFrame appends one frame checksum.
func (s *Summary) AddFrame(sum [32]byte) { s.sums = append(s.sums, sum) }

// Frames returns the number of accumulated frames.
func (s *Summary) Frames() int { return len(s.sums) }

// Sums returns the hex-encoded per-frame checksums.
func (s *Summary) Sums() []string {
	out := make([]string, len(s.sums))
	for i, sum := range s.sums {
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// SHA256 returns the summary hash over all frame checksums.
func (s *Summary) SHA256() string {
	h := sha256.New()
	for _, sum := range s.sums {
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FromHex rebuilds a Summary from committed hex checksums, so a
// resumed run continues the same hash chain.
func FromHex(sums []string) (Summary, error) {
	var s Summary
	for i, h := range sums {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			return Summary{}, fmt.Errorf("report: bad frame checksum %d", i)
		}
		var sum [32]byte
		copy(sum[:], b)
		s.sums = append(s.sums, sum)
	}
	return s, nil
}
