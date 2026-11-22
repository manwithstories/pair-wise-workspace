// Package pipeline orchestrates parse -> track mapping -> RTP validation ->
// mux with bounded in-memory buffering, context cancellation that unblocks
// every goroutine, slow-consumer backpressure, content-addressed caching and
// crash-safe checkpointed resume.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"mediapack/internal/track"
)

// Config controls a pipeline run.
type Config struct {
	SourceID     string
	ChunkBytes   int           // byte budget; also bounds buffered memory
	SlowConsumer time.Duration // artificial per-fragment consumer delay
	InterruptAt  int           // fragments committed before a simulated crash; <1 disables
	WorkDir      string
}

// Metrics is the deterministic run report rendered by the CLI.
type Metrics struct {
	ParseErrorOffset int64  `json:"parse_error_offset"`
	PeakBufferBytes  int64  `json:"peak_buffer_bytes"`
	ResumeMs         int64  `json:"resume_ms"`
	SourceSHA256     string `json:"source_sha256"`
	OutputSHA256     string `json:"output_sha256"`
	CacheRevalidated bool   `json:"cache_revalidated"`
	Packets          int    `json:"packets"`
	Fragments        int    `json:"fragments"`
	Keyframes        int    `json:"keyframes"`
	Resumed          bool   `json:"resumed"`
}

// Result bundles run outputs.
type Result struct {
	Metrics  Metrics
	Timeline track.TimelineSummary
}

// ErrInterrupted marks a deliberate crash simulation; the checkpoint on disk
// is durable and Resume can continue from it.
type ErrInterrupted struct{ Fragments int }

func (e *ErrInterrupted) Error() string {
	return "pipeline interrupted after committed fragment " + itoa(e.Fragments)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// sha256Hex returns the lowercase hex digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// cachedParse is the cacheable result of the parse/map stage.
type cachedParse struct {
	Codec     string                `json:"codec"`
	Timeline  track.TimelineSummary `json:"timeline"`
	Keyframes []int64               `json:"keyframes"`
	NumAU     int                   `json:"num_au"`
}

// fragmentName returns the deterministic file name for a fragment index.
func fragmentName(i int) string {
	return "frag-" + pad4(i) + ".mp4"
}

func pad4(i int) string {
	s := itoa(i)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
