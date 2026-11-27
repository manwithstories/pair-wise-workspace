// Package verify independently checks repaired output and resume consistency.
//
// Verification never trusts the values the pipeline reports about itself: it
// re-scans the repaired bytes, recomputes record checksums, compares damage
// offsets with the reported ranges, checks the segment index, re-hashes the
// proxy frames and confirms that a resumed run produced the same digest as an
// uninterrupted one.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"relayaudit/internal/transform"
	"relayaudit/internal/wire"
)

// Result is the audit outcome printed by the command.
type Result struct {
	// DamageRanges is the number of isolated source spans.
	DamageRanges int
	// CorruptOffsetDelta is |reported corrupt offset - injected offset| in
	// bytes; zero means exact localisation.
	CorruptOffsetDelta int64
	// TruncatedTailBytes is the size of the truncated tail span.
	TruncatedTailBytes int64
	// IndexRebuilt counts rebuilds performed during the run.
	IndexRebuilt int
	// ResumeMillis is the wall time spent on the resumed portion.
	ResumeMillis int64
	// OutputSHA256 is the digest of the final repaired file.
	OutputSHA256 string
	// RepeatSHA256Equal is 1 when a repeated run reproduced the digest.
	RepeatSHA256Equal int
	// Kept and Dropped record counts.
	Kept, Dropped int
	// Ranges is the isolated spans with their timeline annotations.
	Ranges []transform.Range
	// Frames and tracks seen in the source.
	Frames int
	Tracks []uint8
	// KeyIntervalTicks is the mean IDR interval per video track.
	KeyIntervalTicks map[uint8]uint64
	// ProxyHash is the combined proxy pixel hash.
	ProxyHash string
	// Problems lists every failed check; empty means success.
	Problems []string
	// ExitCode is 0, 3 or 4 depending on the failure class.
	ExitCode int
}

// SHA256Hex returns the hex SHA-256 of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Expected describes the damage injected into the fixture, so verification can
// measure localisation error against ground truth.
type Expected struct {
	CorruptOffset   int64
	CorruptInjected bool
	TruncateTail    int
	DamageRanges    int
	TrackCount      int
	SourceSize      int64
	SourceSHA256    string
}

func (r *Result) fail(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// countKept counts source records that survive isolation.
func countKept(scan *wire.ScanResult, ranges []transform.Range) (kept, dropped int) {
	for _, rec := range scan.Records {
		if _, hit := transform.Overlaps(rec, ranges); hit {
			dropped++
			continue
		}
		kept++
	}
	// Damaged records that failed to parse never appear in scan.Records but
	// still count toward the declared total.
	if declared := int(scan.Header.RecordCount); declared > kept+dropped {
		dropped += declared - kept - dropped
	}
	return kept, dropped
}
