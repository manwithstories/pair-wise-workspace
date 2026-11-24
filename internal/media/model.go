// Package media defines the shared time base, media data model and error
// taxonomy used by every relay subsystem (demux, clock, mapping, recovery,
// transaction and metrics).
package media

import "errors"

// Kind identifies how a sample's encoded bytes should be interpreted.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindVideo
	KindAudio
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	default:
		return "unknown"
	}
}

// UnifiedTimeBase is the single time base every mapped sample is expressed in:
// nanoseconds since the run-local epoch (anchor sample of each stream = 0 ns).
const UnifiedTimeBase = "nanoseconds"

// Sample limits guard locally generated fixtures and reject oversized input.
const (
	MaxFrameBytes   = 1 << 20 // 1 MiB per single encoded frame / RTP payload
	MaxPacketBytes  = 65_535  // length-prefixed record cap (u16 length field)
	MaxTotalSamples = 10_000  // whole run, all tracks combined
	MaxSources      = 16
)

// FrameMeta is the capture metadata that travels with every image frame or
// audio packet. Hash is the SHA-256 digest of the encoded sample bytes only;
// it must never cover framing, headers or container overhead.
type FrameMeta struct {
	SourceID       uint32
	Sequence       uint64
	ClockTicks     uint64 // raw source clock value, already unwrapped if needed
	TimescaleHz   uint64 // ticks per second on the source clock
	PresentationNS int64  // filled by the clock stage: unified nanosecond PTS
	Width          uint32
	Height         uint32
	Size           uint32 // encoded payload length in bytes
	SHA256         [32]byte
}

// RawFrame is one encoded sample together with its capture metadata. The
// Payload bytes are treated as opaque: remuxing must preserve them exactly.
type RawFrame struct {
	Meta    FrameMeta
	Kind    Kind
	Payload []byte
}

// MediaError is the structured error model shared by all stages. The error
// kinds below are sentinel values wrapped by concrete failures.
type MediaError struct {
	Kind   string
	Source uint32
	Detail string
}

func (e *MediaError) Error() string {
	if e.Source != 0 {
		return "media error: " + e.Kind + ": source=" + uitoa(e.Source) + ": " + e.Detail
	}
	return "media error: " + e.Kind + ": " + e.Detail
}

// Error kinds emitted across the pipeline.
const (
	ErrChecksumMismatch = "checksum_mismatch"
	ErrLengthMismatch   = "length_mismatch"
	ErrMetadata         = "metadata_invalid"
	ErrTruncated        = "record_truncated"
	ErrLimitExceeded    = "limit_exceeded"
	ErrMapping          = "mapping_invalid"
	ErrGap              = "packet_gap"
	ErrWAL              = "wal_corrupt"
)

// NewError builds a structured MediaError.
func NewError(kind string, source uint32, detail string) *MediaError {
	return &MediaError{Kind: kind, Source: source, Detail: detail}
}

// AsMediaError extracts a *MediaError from err, or nil when the error is of a
// different type.
func AsMediaError(err error) *MediaError {
	var me *MediaError
	if errors.As(err, &me) {
		return me
	}
	return nil
}

func uitoa(u uint32) string {
	if u == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}
