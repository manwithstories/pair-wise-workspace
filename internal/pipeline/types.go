// Package pipeline orchestrates the end-to-end processing of a generated
// fixture: parse manifests, probe and normalise tracks, run silence and
// fingerprint analysis, plan cuts, recover missing slots from backup
// representations, and write a deterministic, resumable block index.
//
// Concurrency and per-block memory are bounded, inputs are opened read-only,
// and all writes stay inside the configured working directory.
package pipeline

import (
	"runtime"

	"mediadelivery/internal/fault"
	"mediadelivery/internal/fixture"
	"mediadelivery/internal/index"
)

// Config controls a pipeline run.
type Config struct {
	WorkDir        string
	Format         string // hls | dash | both
	Cameras        int
	DurationSec    float64
	Profile        fixture.Profile
	RunID          string
	// MaxWorkers bounds goroutine concurrency.
	MaxWorkers int
	// MaxBlockBytes bounds the largest segment payload accepted in memory.
	MaxBlockBytes int
	// InterruptAfterBlock > 0 stops processing after that many blocks are
	// indexed, simulating an interruption for resume testing.
	InterruptAfterBlock int
}

// DefaultMaxBlockBytes caps a single block at 64 MiB.
const DefaultMaxBlockBytes = 64 << 20

func (c *Config) applyDefaults() {
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = runtime.NumCPU()
		if c.MaxWorkers > 4 {
			c.MaxWorkers = 4 // hard concurrency cap
		}
	}
	if c.MaxBlockBytes <= 0 {
		c.MaxBlockBytes = DefaultMaxBlockBytes
	}
	if c.RunID == "" {
		c.RunID = "run"
	}
}

// Metrics is the acceptance-facing metric set.
type Metrics struct {
	AVSyncMaxMs      int     `json:"av_sync_max_ms"`
	LostSlots        int     `json:"lost_slots"`
	RecoveredSlots   int     `json:"recovered_slots"`
	LossRecoveryRate float64 `json:"loss_recovery_rate"`
	SegmentP95Ms     int64   `json:"segment_p95_ms"`
	ResumeHashEqual  bool    `json:"resume_hash_equal"`
	DuplicateHits    int     `json:"duplicate_hits"`
	// ProcessedBlocks counts blocks fully indexed.
	ProcessedBlocks int `json:"processed_blocks"`
	// HasUnrecoverable is true when a slot could not be filled.
	HasUnrecoverable bool `json:"has_unrecoverable"`
}

// block is one unit of work: a segment slot for one camera/representation.
type block struct {
	camera   string
	role     string
	sequence int
	// path is the resolved on-disk segment file.
	path string
	// missing indicates the primary slot file is absent.
	missing bool
	// truncated indicates the primary payload was cut mid-packet.
	truncated bool
	// start90k/duration90k place the block on the shared clock.
	start90k    int64
	duration90k int64
}

// result is a processed block's analytics output.
type result struct {
	block       block
	contentHash string
	fileHash    string
	silence     []silenceSpan
	avSyncMs    int
	segmentMs   int64
	recovered   bool
	fingerprint fpLike
	entry       index.Entry
}

type silenceSpan struct {
	Start90k int64
	End90k   int64
}

// fpLike is the minimal fingerprint shape the dedup step needs.
type fpLike struct {
	videoHash     uint64
	videoBits     int
	audioEnvelope []float32
}

// recorder alias keeps fault wiring readable.
type faultRecorder = fault.Recorder
