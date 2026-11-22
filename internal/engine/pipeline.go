// Streaming pipeline: decode -> resample -> channel normalise -> limit ->
// framed output blocks -> checkpoint.
//
// Determinism is the central property. Output bytes are a pure function of
// (input bytes, configuration, adapter versions), so a resumed run produces
// byte-identical blocks to an uninterrupted run. The checkpoint stores an
// *input-frame* cursor plus the number of completed blocks; replaying input
// up to the cursor and discarding the already-framed output reproduces the
// resampler state exactly, because the kernel is finite and deterministic.
package engine

import (
	"time"

	"mediacore/internal/codec"
	"mediacore/internal/container"
	"mediacore/internal/store"
)

// Options configures one pipeline run.
type Options struct {
	WorkDir   string
	Config    Config
	Source    *container.Source
	Codecs    *codec.Registry
	InputPath string
	InputSHA  string
	InputSize int64

	// Mode is "demo", "resume" or "verify"; recorded in provenance.
	Mode string
	// InterruptAfter stops cleanly after this many completed blocks. Zero
	// means never.
	InterruptAfter int
	// Fresh discards an existing unfinished checkpoint before starting.
	Fresh bool
	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// Stats is the summary rendered by the command layer.
type Stats struct {
	SamplesIn     int64
	FramesOut     int64
	SamplesOut    int64
	PeakDBFS      float64
	SHA256        string
	Blocks        int
	ResumedBlocks int
	RecoveryMS    int64
	SilentBlocks  int
	ClippedBlocks int
	LimitedBlocks int
	TotalBytes    int64
	RTPFirst      uint32
	RTPLast       uint32
	RTPVerified   bool
}

// Result is the outcome of one run.
type Result struct {
	Stats       Stats
	Interrupted bool
	Checkpoint  *store.Checkpoint
}

const rtpClockRate = 48000
const rtpPayloadType = 111
const rtpSSRC = 0x4D434100 // "MCA\0"

// Run executes the pipeline described by opts.
func statsFromCheckpoint(cp *store.Checkpoint, sha string) Stats {
	var samplesOut, bytes int64
	for _, b := range cp.Blocks {
		samplesOut += int64(b.Frames) * int64(cp.OutputChannels)
		bytes += b.Bytes
	}
	var first, last uint32
	if len(cp.Blocks) > 0 {
		first = cp.Blocks[0].RTPTimestamp
		last = cp.Blocks[len(cp.Blocks)-1].RTPTimestamp
	}
	return Stats{
		SamplesIn:     cp.SamplesIn,
		FramesOut:     cp.FramesOut,
		SamplesOut:    samplesOut,
		PeakDBFS:      cp.PeakDBFS,
		SHA256:        sha,
		Blocks:        len(cp.Blocks),
		RecoveryMS:    0,
		SilentBlocks:  cp.SilentBlocks,
		ClippedBlocks: cp.ClippedBlocks,
		LimitedBlocks: cp.LimitedBlocks,
		TotalBytes:    bytes,
		RTPFirst:      first,
		RTPLast:       last,
	}
}
