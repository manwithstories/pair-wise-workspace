// Package report renders deterministic scan reports. Given identical inputs
// and parameters, the serialized bytes — and therefore the SHA-256 — are
// identical across runs, including interrupted-and-resumed runs.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"mediascan/internal/fault"
	"mediascan/internal/pipeline"
)

// Summary is the stable, byte-deterministic rendering of a scan Result.
type Summary struct {
	Text   string
	SHA256 string
}

// KeyLines returns the machine-readable "key=value" lines required by the
// acceptance contract. resumeMS is the elapsed milliseconds reported by the
// caller; it is intentionally NOT part of the hashed text so resume timing
// cannot perturb the checksum.
func KeyLines(r *pipeline.Result, resumeMS int64, damageOffset int64) []string {
	return []string{
		fmt.Sprintf("segments=%d", len(r.Segments)),
		fmt.Sprintf("keyframe_gaps=%d", r.KeyframeGapTotal),
		fmt.Sprintf("damage_offset=%d", damageOffset),
		fmt.Sprintf("recovered_segments=%d", r.RecoveredSegments),
		fmt.Sprintf("resume_ms=%d", resumeMS),
		fmt.Sprintf("sha256=%s", Build(r).SHA256),
	}
}

// canonicalBytes serializes every result field in a fixed order. Maps are
// flattened through sorted keys, and all slices are already ordered by the
// pipeline, so no map iteration order can leak into the bytes.
func canonicalBytes(r *pipeline.Result) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "params=%s\n", r.Params.String())
	fmt.Fprintf(&b, "interrupted=%v\n", r.Interrupted)
	fmt.Fprintf(&b, "segments=%d\n", len(r.Segments))
	fmt.Fprintf(&b, "recovered_total=%d\n", r.RecoveredSegments)
	fmt.Fprintf(&b, "keyframe_gap_total=%d\n", r.KeyframeGapTotal)

	keys := make([]string, 0, len(r.Fingerprints))
	for k := range r.Fingerprints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "fingerprint:%s=%s\n", k, r.Fingerprints[k])
	}

	for _, s := range r.Segments {
		fmt.Fprintf(&b, "segment:%d|%s|%s|bytes=%d|events=%d|boxes=%d|packets=%d|kfgaps=%d|rtpgaps=%d|damage=%d|recovered=%v|span=%d-%d\n",
			s.Index, s.Source, s.Kind, s.Bytes, s.EventCount, s.BoxCount,
			s.PacketCount, s.KeyframeGaps, s.RTPGaps, s.DamageOffset,
			s.Recovered, s.Start, s.End)
	}
	for _, f := range r.Faults {
		fmt.Fprintf(&b, "fault:%s|off=%d|stage=%s|recoverable=%v|%s\n",
			f.Source, f.Offset, f.Stage, f.Recoverable, f.Message)
	}
	return []byte(b.String())
}

// Build produces the canonical text and its 64-hex-character SHA-256.
func Build(r *pipeline.Result) Summary {
	raw := canonicalBytes(r)
	sum := sha256.Sum256(raw)
	return Summary{Text: string(raw), SHA256: hex.EncodeToString(sum[:])}
}

// FirstDamageOffset returns the smallest byte-anchored damage offset across
// all faults, or -1 when no file damage exists. Network-only faults
// (offset -1) are ignored.
func FirstDamageOffset(faults []fault.Fault) int64 {
	first := int64(-1)
	for _, f := range faults {
		if f.Offset < 0 {
			continue
		}
		if first < 0 || f.Offset < first {
			first = f.Offset
		}
	}
	return first
}
