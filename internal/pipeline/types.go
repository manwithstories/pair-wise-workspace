// Package pipeline streams input through read → analyze → caption
// align → summarize stages connected by bounded channels, with
// backpressure, cancellation and checkpointed resume.
package pipeline

import (
	"mediacheck/internal/fault"
)

// ParseBufferBytes is the fixed per-run parse buffer budget reported
// alongside the queue-bound peak (acceptance: peak ≤ queue*chunk + 4MiB).
const ParseBufferBytes = 4 << 20

// Config bounds the pipeline. QueueCap is 1..64 chunks total in flight.
type Config struct {
	QueueCap  int
	ChunkSize int64
}

func (c Config) validate() error {
	if c.QueueCap < 1 || c.QueueCap > 64 {
		return fault.New(fault.CatPipeline, -1, false,
			"queue capacity %d outside 1..64", c.QueueCap)
	}
	if c.ChunkSize < 1 {
		return fault.New(fault.CatPipeline, -1, false, "chunk size must be > 0")
	}
	return nil
}

// SegmentHash is one committed chunk boundary and its digest.
type SegmentHash struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

// Result summarizes one pipeline run.
type Result struct {
	Segments        []SegmentHash `json:"segments"`
	TotalBytes      int64         `json:"total_bytes"`
	PeakBufferBytes int64         `json:"peak_buffer_bytes"`
	ResumeMatches   bool          `json:"resume_matches"`
	Resumed         bool          `json:"resumed"`
	Faults          *fault.List   `json:"faults"`
}

// SegmentHashesEqual reports whether two runs produced identical
// segment boundaries and hashes.
func SegmentHashesEqual(a, b []SegmentHash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
