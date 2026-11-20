package model

// TrackKind identifies the media carried by an RTP stream.
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
)

// Packet is one parsed RTP packet after sequence extension, expressed in
// the stream's own 32-bit clock. It is the unit that flows through repair,
// timeline and render stages.
type Packet struct {
	SSRC        uint32    // synchronization source
	Sequence    uint16    // raw 16-bit RTP sequence number
	ExtSequence int64     // signed-extended monotonic sequence number
	Timestamp   uint32    // raw 32-bit RTP timestamp (per-clock-rate)
	PayloadType byte      // RTP payload type
	Marker      bool      // RTP marker bit
	Payload     []byte    // deterministic, locally generated payload
	Keyframe    bool      // derived marker: this packet starts a key frame
	Track       TrackKind // track this SSRC belongs to
	ClockRate   uint32    // RTP clock rate in Hz for this payload type
}

// LostRange is a run of consecutively missing extended sequence numbers
// for one SSRC that outlived the reorder window. Ranges are recorded, never
// silently filled with fabricated frames.
type LostRange struct {
	SSRC      uint32
	Start     int64 // first missing extended sequence number
	End       int64 // last missing extended sequence number (inclusive)
	Track     TrackKind
	TimeNanos int64 // timeline position of the gap (best effort)
}

// Len returns the number of lost packets in the range.
func (r LostRange) Len() int64 { return r.End - r.Start + 1 }

// SenderReport is a parsed RTCP SR: it binds a 32-bit RTP timestamp to a
// wall-clock instant so a stream can be mapped onto a shared nanosecond
// timeline.
type SenderReport struct {
	SSRC        uint32
	RTPTime     uint32 // RTP timestamp in the report
	NTPTime     uint64 // NTP-format wall clock (seconds<<32 | fraction)
	PacketCount uint32 // sender's packet counter
	OctetCount  uint32 // sender's octet counter
	Valid       bool   // false marks a deliberately corrupted report
}

// NTPToNanos converts an NTP-format timestamp to integer nanoseconds since
// the NTP epoch (1900-01-01).
func NTPToNanos(ntp uint64) int64 {
	secs := int64(ntp >> 32)
	frac := ntp & 0xffffffff
	nanos := int64(float64(frac) * 1e9 / (1 << 32))
	return secs*1_000_000_000 + nanos
}

// Frame is one offscreen-rendered timeline record. It carries enough
// provenance to be independently audited against the source packets.
type Frame struct {
	Index        int64     // monotonic frame index within the segment
	Track        TrackKind // owning track
	TimeNanos    int64     // shared-timeline presentation time
	SourceStart  int64     // first source extended sequence number
	SourceEnd    int64     // last source extended sequence number (inclusive)
	SSRC         uint32
	Keyframe     bool
	ClockBasis   string // human-readable clock conversion basis
	PayloadHash  [32]byte
	SegmentIndex int
}

// Segment is a rendered, hash-committed slice of the unified timeline.
type Segment struct {
	Index       int
	TimeStart   int64
	TimeEnd     int64
	Frames      int
	SHA256      [32]byte // whole-file hash, including the sequence header
	ContentSHA  [32]byte // hash of the encoded frame records alone
	PacketStart int64    // global extended packet ordinal at segment start
	PacketEnd   int64    // global ordinal at segment end (inclusive)
}
