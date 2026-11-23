// Package report turns merged scan results into a deterministic textual
// summary and a SHA-256 digest. The digest is computed over a canonical
// serialization whose byte layout depends only on the merged findings and
// run parameters, never on map iteration, goroutine scheduling or clocks,
// so identical inputs always yield identical hashes.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"mediascan/internal/fault"
)

// Params is the subset of run configuration that influences results. It is
// part of the canonical digest so changing any parameter changes the hash.
type Params struct {
	Seed       int64  `json:"seed"`
	Tracks     int    `json:"tracks"`
	Frames     int    `json:"frames"`
	CorruptAt  int64  `json:"corrupt_at"`
	Timescale  int64  `json:"timescale"`
	MaxKeyGap  int64  `json:"max_key_gap_ticks"`
	DriftLimit int64  `json:"drift_limit_ticks"`
}

// Summary is the stable, human-readable scan outcome.
type Summary struct {
	Tracks        int    `json:"tracks"`
	Frames        int    `json:"frames"`
	FaultCount    int    `json:"fault_count"`
	FirstBadFrame int64  `json:"first_bad_frame"`
	OffsetBytes   int64  `json:"offset_bytes"`
	// LocalizationErrorFrames is always zero on a correctly located fault:
	// the reported frame must equal the injected corruption point.
	LocalizationErrorFrames int64 `json:"localization_error_frames"`
	// ResumeMs counts time spent recomputing after a checkpoint resume;
	// zero on a full run.
	ResumeMs       int64          `json:"resume_ms"`
	RecomputedFrames int64        `json:"recomputed_frames"`
	TopFaults      []fault.Fault  `json:"top_faults"`
	SHA256         string         `json:"sha256"`
}

// Canonical renders params plus the ordered fault slice into stable bytes.
// Fields are emitted in a fixed order and faults use the already-merged
// total order, so there is no nondeterministic map or slice ordering here.
func Canonical(p Params, faults []fault.Fault) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "params seed=%d tracks=%d frames=%d corrupt=%d timescale=%d keygap=%d drift=%d\n",
		p.Seed, p.Tracks, p.Frames, p.CorruptAt, p.Timescale, p.MaxKeyGap, p.DriftLimit)
	fmt.Fprintf(&b, "faults count=%d\n", len(faults))
	for i, f := range faults {
		fmt.Fprintf(&b, "%d|kind=%s|sev=%d|track=%d|frame=%d|offset=%d|ticks=%d|spanf=%d|spanb=%d|name=%s|msg=%s\n",
			i, f.Kind, f.Severity, f.TrackID, f.FrameIndex, f.OffsetBytes,
			f.Ticks, f.SpanFrames, f.SpanBytes, f.TrackName, f.Message)
	}
	return []byte(b.String())
}

// Digest returns the hex SHA-256 of the canonical representation.
func Digest(p Params, faults []fault.Fault) string {
	sum := sha256.Sum256(Canonical(p, faults))
	return hex.EncodeToString(sum[:])
}

// Build assembles the summary, attaching the deterministic digest.
func Build(p Params, tracks, frames int, faults []fault.Fault,
	resumeMs, recomputed int64) Summary {
	s := Summary{
		Tracks:           tracks,
		Frames:           frames,
		FaultCount:       len(faults),
		FirstBadFrame:    -1,
		OffsetBytes:      -1,
		ResumeMs:         resumeMs,
		RecomputedFrames: recomputed,
		TopFaults:        topN(faults, 10),
	}
	if f, ok := fault.FirstByKind(faults, fault.KindTruncatedBox); ok {
		// Truncation faults carry the localized byte offset. The frame is
		// attached by the adapter; here we only surface the byte position.
		s.OffsetBytes = f.OffsetBytes
		s.FirstBadFrame = f.FrameIndex
	}
	s.SHA256 = Digest(p, faults)
	return s
}

func topN(faults []fault.Fault, n int) []fault.Fault {
	if len(faults) <= n {
		out := make([]fault.Fault, len(faults))
		copy(out, faults)
		return out
	}
	out := make([]fault.Fault, n)
	copy(out, faults[:n])
	return out
}

// Render produces the stable terminal lines used by the demo adapter. The
// key=value layout is part of the acceptance contract.
func Render(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tracks=%d frames=%d\n", s.Tracks, s.Frames)
	if s.FirstBadFrame >= 0 {
		fmt.Fprintf(&b, "first_bad_frame=%d offset_bytes=%d localization_error_frames=%d\n",
			s.FirstBadFrame, s.OffsetBytes, s.LocalizationErrorFrames)
	}
	if s.ResumeMs > 0 || s.RecomputedFrames > 0 {
		fmt.Fprintf(&b, "resume_ms=%d recomputed_frames=%d sha256=%s\n",
			s.ResumeMs, s.RecomputedFrames, s.SHA256)
	} else {
		fmt.Fprintf(&b, "sha256=%s\n", s.SHA256)
	}
	if s.FaultCount > 0 {
		fmt.Fprintf(&b, "fault_count=%d\n", s.FaultCount)
	}
	return b.String()
}
