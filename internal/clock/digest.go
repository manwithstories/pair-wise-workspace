package clock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Digest returns a stable SHA-256 hex digest of the mapping. Running the same
// input through MapSamples twice yields the same digest, which is what the
// pipeline's reproducibility check compares.
func (m Mapping) Digest() string {
	var b strings.Builder
	fmt.Fprintf(&b, "timescale=%d shift=%d origin=%d\n", m.Timescale, m.Shift, m.Origin)
	for _, s := range m.Samples {
		b.WriteString("s=")
		b.WriteString(strconv.Itoa(s.Index))
		b.WriteString(",")
		b.WriteString(strconv.FormatInt(s.DTS, 10))
		b.WriteString(",")
		b.WriteString(strconv.FormatInt(s.PTS, 10))
		b.WriteString(",")
		b.WriteString(strconv.FormatInt(s.Duration, 10))
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
