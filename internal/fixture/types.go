// Package fixture produces fully deterministic, seed-driven synthetic media
// packets, metadata events and transport damage. No external assets are used:
// every byte derives from a rand.Rand seeded by the caller.
package fixture

import (
	"math/rand/v2"
)

// Kind identifies a track's media role.
type Kind uint8

const (
	KindVideo Kind = iota + 1
	KindAudio
	KindSubtitle
	KindChapter
	KindMetadata // ID3 / emsg / SEI carrier
)

func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindSubtitle:
		return "subtitle"
	case KindChapter:
		return "chapter"
	case KindMetadata:
		return "metadata"
	default:
		return "unknown"
	}
}

// EventKind enumerates the timed side-data the demo carries.
type EventKind uint8

const (
	EventSubtitle EventKind = iota + 1
	EventChapter
	EventID3
	EventEmsg
	EventSEI
)

func (e EventKind) String() string {
	switch e {
	case EventSubtitle:
		return "subtitle"
	case EventChapter:
		return "chapter"
	case EventID3:
		return "id3"
	case EventEmsg:
		return "emsg"
	case EventSEI:
		return "sei"
	default:
		return "unknown"
	}
}

// Track describes one generated track's clock and shape.
type Track struct {
	ID       uint32
	Kind     Kind
	Timebase NumDen // native rational time base
	// DriftPPM is the linear clock drift in parts per million applied to the
	// generated raw timestamps (audio/video only).
	DriftPPM int
	// WrapModulus > 0 makes raw timestamps wrap at this boundary.
	WrapModulus int64
	PayloadLen  int
}

// NumDen is a plain numerator/denominator pair kept serializable for fixtures.
type NumDen struct {
	Num int64
	Den int64
}

// Packet is one generated transport packet with its native raw timestamp.
type Packet struct {
	Track    uint32
	Seq      uint32 // RTP-style per-track sequence number
	RawTS    int64  // native timebase ticks, possibly wrapped / drifted
	TrueUS   int64  // ground-truth normalized position in microseconds
	Duration int64  // frame duration in native ticks (VFR for video)
	Payload  []byte // deterministic synthetic content
	// FEC group index; parity packets share the group with 8 media packets.
	FECGroup int
	IsParity bool
	// Recovered is set by the transport layer when a media packet is rebuilt
	// from the group parity packet rather than received directly.
	Recovered bool
	// Marked by the damage injector.
	Dropped     bool
	Duplicated  bool
	ReorderedTo int // target order position when shuffled, 0 when stable
}

// Event is a timed metadata / subtitle / chapter sample.
type Event struct {
	Track    uint32
	Kind     EventKind
	RawTS    int64
	TrueUS   int64
	Duration int64 // microseconds, zero for point events
	Payload  []byte
	Truncated bool // damage injector cuts the payload short
}

// Clip is one generated fixture: ordered packets plus timed events.
type Clip struct {
	Seed    int64
	Tracks  []Track
	Packets []Packet
	Events  []Event
	// Damage summary applied during generation.
	Spec DamageSpec
}

// DamageSpec configures the transport / metadata damage injector.
type DamageSpec struct {
	// DropSingles are FEC group indices where exactly one media packet is
	// dropped (recoverable by the parity packet).
	DropSingles []int
	// DropMultiples are FEC group indices with >=2 media packets dropped.
	DropMultiples map[int]int
	// DuplicateSeq lists per-track sequence numbers emitted twice.
	DuplicateSeq map[uint32][]uint32
	// ReorderWindow is the shuffle window for out-of-order delivery.
	ReorderWindow int
	// TruncateEvents are event indices whose payload is cut short.
	TruncateEvents []int
}

// Config controls fixture generation. Hard bounds keep demo inputs small.
type Config struct {
	Seed         int64
	VideoFrames  int
	AudioPackets int
	Subtitles    int
	Chapters     int
	MetaEvents   int
	// VFR makes video frame durations vary within a bounded factor.
	VFR bool
	// Base video frame rate numerator/denominator.
	VideoFPS NumDen
	Damage   DamageSpec
}

// DefaultConfig returns the small bounded shape used by the demo cases.
func DefaultConfig(seed int64) Config {
	return Config{
		Seed:         seed,
		VideoFrames:  240, // ~8-10 s depending on VFR
		AudioPackets: 480, // 1024-sample AAC-style packets at 48 kHz ~= 10.24 s
		Subtitles:    12,
		Chapters:     4,
		MetaEvents:   18,
		VFR:          true,
		VideoFPS:     NumDen{Num: 25, Den: 1},
	}
}

// newRand returns a fresh deterministic RNG local to one generation pass.
func newRand(seed int64, salt int64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed)^0x9e3779b97f4a7c15, uint64(salt)))
}

// deterministicPayload fills b with a reproducible byte pattern derived from
// track, sequence and seed so identical reruns hash identically.
func deterministicPayload(b []byte, seed int64, track uint32, seq uint32) {
	r := newRand(seed, int64(track)*1_000_003+int64(seq))
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
}
