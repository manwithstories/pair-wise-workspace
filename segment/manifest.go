package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Canonical renders the manifest as a stable, platform-independent text form.
// Two pipeline runs (full and resumed) produce byte-identical output here when
// their segments agree, which is what ManifestSHA256 certifies.
func (m *Manifest) Canonical() string {
	var b strings.Builder
	b.WriteString("reps=")
	b.WriteString(strings.Join(m.Representations, ","))
	b.WriteString("\ntarget_ms=")
	b.WriteString(strconv.FormatInt(m.TargetMS, 10))
	b.WriteString("\nmax_alignment_error_ms=")
	b.WriteString(strconv.FormatInt(m.MaxAlignmentErrMS, 10))
	b.WriteString("\n")
	for _, s := range m.Segments {
		b.WriteString("seg ")
		b.WriteString(strconv.Itoa(s.Index))
		b.WriteString(" start=")
		b.WriteString(s.Start.String())
		b.WriteString(" end=")
		b.WriteString(s.End.String())
		for _, id := range m.Representations {
			c := s.Cuts[id]
			b.WriteString(" ")
			b.WriteString(id)
			b.WriteString("=")
			b.WriteString(strconv.Itoa(c.FrameIndex))
			b.WriteString("@")
			b.WriteString(c.ActualStart.String())
			b.WriteString(" dev=")
			b.WriteString(strconv.FormatInt(c.DeviationMS, 10))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// SHA256 returns the hex digest of the canonical manifest.
func (m *Manifest) SHA256() string {
	sum := sha256.Sum256([]byte(m.Canonical()))
	return hex.EncodeToString(sum[:])
}

// SegmentCount is a convenience accessor for CLI reporting.
func (m *Manifest) SegmentCount() int { return len(m.Segments) }
