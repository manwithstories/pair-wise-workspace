// Package clock defines the unified sample timeline, the shared block type
// carried through every pipeline stage and the classified error model used by
// all subsystems.
package clock

import "fmt"

// SampleRate is the common clock rate of the whole pipeline, in Hz.
const SampleRate = 48000

// MaxBlockBytes limits a single encoded block payload (spec: 64 KiB).
const MaxBlockBytes = 64 * 1024

// FrameRate is the export frame rate, in Hz.
const FrameRate = 30

// Source marks where a block's samples came from.
type Source uint8

const (
	// SourceOriginal marks samples received intact from the network.
	SourceOriginal Source = iota
	// SourceInterpolated marks samples reconstructed by linear interpolation.
	SourceInterpolated
	// SourceSilence marks unrecoverable gaps filled with silence.
	SourceSilence
)

// Block is an immutable-ish unit of PCM data positioned on the unified sample
// timeline. Samples are interleaved int32 PCM in the range of the source
// bit depth; all timestamps are in SampleRate ticks (zero based).
type Block struct {
	TrackID   uint32  // logical track / channel group
	SSRC      uint32  // RTP synchronization source
	Seq       uint16  // RTP sequence number
	StartTick int64   // first sample's position on the unified timeline
	Channels  int     // interleaved channels per sample frame
	Samples   []int32 // interleaved PCM, length = Frames*Channels
	Source    Source  // provenance of the payload
}

// Frames returns the number of sample frames (one frame = Channels samples).
func (b *Block) Frames() int {
	if b.Channels <= 0 {
		return 0
	}
	return len(b.Samples) / b.Channels
}

// EndTick returns the tick immediately after the block's last frame.
func (b *Block) EndTick() int64 {
	return b.StartTick + int64(b.Frames())
}

// ByteSize reports the encoded payload size in bytes (4 bytes per sample).
func (b *Block) ByteSize() int {
	return len(b.Samples) * 4
}

// Kind classifies pipeline errors so callers can map them to exit codes.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindTruncatedWAV   // truncated or malformed WAV container
	KindBadBlock       // illegal block length / oversized allocation
	KindRateMismatch   // sample rate differs from the unified clock
	KindLate           // packet arrived outside the reorder window
	KindSync           // cross-track timeline misalignment
	KindQueueFull      // backpressure / queue resource boundary
	KindArgs           // invalid command line arguments
)

// Error is a classified pipeline error carrying a byte offset where relevant.
type Error struct {
	Kind   Kind
	Offset int64 // byte offset into the offending input, -1 when not applicable
	Msg    string
}

func (e *Error) Error() string {
	if e.Offset >= 0 {
		return fmt.Sprintf("%s at offset %d: %s", e.Kind, e.Offset, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

// String renders the machine-readable kind token used in CLI output.
func (k Kind) String() string {
	switch k {
	case KindTruncatedWAV:
		return "truncated_wav"
	case KindBadBlock:
		return "bad_block"
	case KindRateMismatch:
		return "rate_mismatch"
	case KindLate:
		return "late_packet"
	case KindSync:
		return "sync_error"
	case KindQueueFull:
		return "queue_full"
	case KindArgs:
		return "bad_args"
	default:
		return "unknown"
	}
}

// NewError builds a classified error without a byte offset.
func NewError(k Kind, format string, args ...any) *Error {
	return &Error{Kind: k, Offset: -1, Msg: fmt.Sprintf(format, args...)}
}

// NewErrorAt builds a classified error anchored at a byte offset.
func NewErrorAt(k Kind, offset int64, format string, args ...any) *Error {
	return &Error{Kind: k, Offset: offset, Msg: fmt.Sprintf(format, args...)}
}

// TicksPerFrame is the frame boundary period in sample ticks.
func TicksPerFrame() int64 {
	return SampleRate / FrameRate
}

// AlignToFrame rounds a tick count down to the nearest 30 fps frame boundary.
func AlignToFrame(tick int64) int64 {
	p := TicksPerFrame()
	return tick - tick%p
}
