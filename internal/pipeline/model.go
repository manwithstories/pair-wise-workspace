// Package pipeline incrementally schedules transport, parsing, timeline,
// cache, checkpoint and manifest stages.
package pipeline

import (
	"github.com/pairwise/relay/internal/cache"
	"github.com/pairwise/relay/internal/checkpoint"
	"github.com/pairwise/relay/internal/media"
	"github.com/pairwise/relay/internal/transport"
)

// Duration90k is the common 2-second segment duration on the 90 kHz clock.
const Duration90k int64 = 180_000

// DamageSample is a known-bad parser fixture and its expected first fault.
type DamageSample struct {
	Name       string
	Data       []byte
	Rendition  string
	Segment    uint64
	WantCode   string
	WantOffset int64
}

// ForgeCacheEntry plants an untrusted cache reference before processing.
type ForgeCacheEntry struct {
	Key  cache.Key
	Data []byte
}

// RungInput is one representation's deterministic source material.
type RungInput struct {
	ID         string
	SourceNode string
	Timescale  uint32
	Bandwidth  uint32
	Average    uint32
	Resolution string
	Codecs     string
	FrameRate  float64

	// Packets are in generated arrival order and may contain duplicates and
	// reordering. Omitted sequences model burst loss.
	Packets []transport.Packet
	// Intact supplies a successful refetch copy for missing sequences.
	Intact map[uint64][]byte
	// Unavailable marks sequences whose sources cannot refetch the packet.
	Unavailable map[uint64]bool
}

// Input is the complete deterministic demo/library request.
type Input struct {
	Seed        int64
	Segments    int
	Rungs       int
	Duration90k int64

	StreamID string
	Window   uint64

	RungInputs []RungInput
	Bandwidth  transport.BandwidthTrace
	Budget     transport.RecoveryBudget
	Damage     []DamageSample
	ForgeCache []ForgeCacheEntry

	// InterruptAfter asks a fresh run to stop after this common segment.
	InterruptAfter int
	// Resume loads and continues from the atomic checkpoint in WorkDir.
	Resume bool
}

// Result summarizes one completed or interrupted pipeline invocation.
type Result struct {
	WorkDir           string
	InputSHA256       string
	ArtifactSHA256    string
	Artifacts         []checkpoint.Artifact
	Aligned           int
	Segments          int
	Renditions        int
	DamageChecked     int
	DamageOffsetDelta int64
	CacheStats        cache.Stats
	TransportStats    transport.Stats
	GapSegments       []uint64
	Discontinuities   []uint64
	MFHDGaps          int
	TFDTRepaired      int
	Faults            []*media.Fault
	Checkpoint        *checkpoint.Checkpoint
	Interrupted       bool
}
