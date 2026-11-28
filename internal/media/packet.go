package media

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

// TrackKind is the coarse media kind carried by a track.
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
)

// ColorInfo is the color description attached to a video track. Each field is
// a canonical FFmpeg/ISO token such as "bt709", "smpte170m", "tv" or "pc";
// an empty field means "unspecified".
type ColorInfo struct {
	Primaries string `json:"primaries,omitempty"`
	Transfer  string `json:"transfer,omitempty"`
	Space     string `json:"space,omitempty"`
	Range     string `json:"range,omitempty"`
}

// CropRect is an integer display crop expressed in coded sample coordinates.
// X and Y are the top-left origin; W and H the width and height.
type CropRect struct {
	W int `json:"w"`
	H int `json:"h"`
	X int `json:"x"`
	Y int `json:"y"`
}

// DisplayInfo is the resolved display description of a video track, together
// with the provenance of each decision. Provenance maps a field name to the
// source that won ("explicit", "track", "container", "bitstream"). Conflicts
// keeps every rejected candidate in "field:source=value" form so the terminal
// and the manifest can show why the receiver would have disagreed.
type DisplayInfo struct {
	Rotation  int               `json:"rotation"`
	Crop      *CropRect         `json:"crop,omitempty"`
	Color     ColorInfo         `json:"color"`
	Provenance map[string]string `json:"provenance,omitempty"`
	Conflicts []string          `json:"conflicts,omitempty"`
	// Invalid is set when a declared value (rotation or crop) is illegal.
	// Invalid tracks are disabled rather than silently "fixed".
	Invalid       bool   `json:"invalid,omitempty"`
	InvalidReason string `json:"invalid_reason,omitempty"`
}

// Track is the stable identity and default timing of one demuxed stream.
type Track struct {
	ID       string    `json:"id"`
	Source   int       `json:"source_index"`
	Kind     TrackKind `json:"kind"`
	Codec    string    `json:"codec"`
	TimeBase Rational  `json:"time_base"`
	// DeclaredWidth/Height are the coded dimensions from the container.
	DeclaredWidth  int  `json:"width,omitempty"`
	DeclaredHeight int  `json:"height,omitempty"`
	// TimestampBits is the wrap width applied to DTS, 0 when unwrapping is
	// disabled.
	TimestampBits int         `json:"timestamp_bits,omitempty"`
	Display       DisplayInfo `json:"display"`
	Language      string      `json:"language,omitempty"`
}

// Packet is one encoded packet after demuxing and clock recovery. Data is a
// private copy of the encoded bytes; timestamps are in TimeBase ticks.
type Packet struct {
	TrackID  string   `json:"track_id"`
	Seq      int64    `json:"seq"`        // per-track source order
	Arrival  int64    `json:"arrival"`    // global arrival order
	PTS      int64    `json:"pts"`        // unwrapped presentation timestamp
	DTS      int64    `json:"dts"`        // unwrapped decode timestamp
	RawPTS   int64    `json:"raw_pts"`
	RawDTS   int64    `json:"raw_dts"`
	Duration int64    `json:"duration"`
	Key      bool     `json:"key"`
	Corrupt  bool     `json:"corrupt"`
	Data     []byte   `json:"-"`
	TimeBase Rational `json:"time_base"`
	Origin   Origin   `json:"origin"`
}

// HasDTS reports whether the source carried a real decode timestamp. Missing
// timestamps are represented with DTS == NoTimestamp.
const NoTimestamp int64 = -1 << 63

// HasDTS reports whether the packet has a usable decode timestamp.
func (p Packet) HasDTS() bool { return p.DTS != NoTimestamp }

// HasPTS reports whether the packet has a usable presentation timestamp.
func (p Packet) HasPTS() bool { return p.PTS != NoTimestamp }

// PTSSeconds returns the presentation time in exact seconds.
func (p Packet) PTSSeconds() (Rational, error) {
	if !p.HasPTS() || !p.TimeBase.Valid() {
		return Rational{}, fmt.Errorf("media: packet has no presentation timestamp")
	}
	return Time{Ticks: p.PTS, Base: p.TimeBase}.Seconds()
}

// DTSSeconds returns the decode time in exact seconds.
func (p Packet) DTSSeconds() (Rational, error) {
	if !p.HasDTS() || !p.TimeBase.Valid() {
		return Rational{}, fmt.Errorf("media: packet has no decode timestamp")
	}
	return Time{Ticks: p.DTS, Base: p.TimeBase}.Seconds()
}

// Clone returns a packet with an independent copy of the payload.
func (p Packet) Clone() Packet {
	cp := p
	if p.Data != nil {
		cp.Data = make([]byte, len(p.Data))
		copy(cp.Data, p.Data)
	}
	return cp
}

// Hash is the canonical content digest of a packet. It covers the identity,
// unwrapped and raw timing, flags and encoded bytes, but never the arrival
// order, so a re-run over the same source always yields the same digest. This
// is the digest the audit layer uses for roundtrip and replay equality.
func (p Packet) Hash() string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	write(p.TrackID)
	write(itoa(p.Seq))
	write(itoa(p.PTS))
	write(itoa(p.DTS))
	write(itoa(p.RawPTS))
	write(itoa(p.RawDTS))
	write(itoa(p.Duration))
	var flags byte
	if p.Key {
		flags |= 1
	}
	if p.Corrupt {
		flags |= 2
	}
	_, _ = h.Write([]byte{flags})
	write(p.TimeBase.String())
	_, _ = h.Write(p.Data)
	return hex.EncodeToString(h.Sum(nil))
}

// PayloadHash is the SHA-256 of the encoded bytes alone, used to assert that a
// remux preserved the bitstream exactly.
func (p Packet) PayloadHash() string {
	sum := sha256.Sum256(p.Data)
	return hex.EncodeToString(sum[:])
}

// Fingerprint is a compact identity used by tests and logs.
func (p Packet) Fingerprint() string {
	return fmt.Sprintf("%s#%d@%d/%d", p.TrackID, p.Seq, p.PTS, p.DTS)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ByteCount is a helper used by manifests.
func (p Packet) ByteCount() int { return len(p.Data) }

// Segment is an ordered, auditable group of packets covering [Start, End) in
// seconds. Packets is the exact output order.
type Segment struct {
	Index      int      `json:"index"`
	Start      Rational `json:"start"`
	End        Rational `json:"end"`
	FirstKey   bool     `json:"first_key"`
	Hash       string   `json:"hash"`
	Packets    int      `json:"packets"`
	Bytes      int64    `json:"bytes"`
	Reasons    []string `json:"reasons,omitempty"`
}

// ComputeSegmentHash digests the ordered packet content of a segment.
//
// The digest covers the segment index and, for every packet in emission
// order, the stable track ID and the SHA-256 of the encoded bytes. It
// deliberately excludes container-level representation (time base, raw tick
// values, muxer chunking): those legitimately change when the same packets
// are written to a different container, while the encoded payload and its
// track association must not. This is what lets audit verification recompute
// a segment from a produced container and compare it to the manifest, and it
// is what proves a re-run reproduced the full run exactly.
//
// Timestamps are audited separately by the merge ordering guarantees and the
// segment's Start/End records; they are not part of the content digest.
func ComputeSegmentHash(index int, pkts []Packet) string {
	h := sha256.New()
	_, _ = h.Write([]byte(itoa(int64(index))))
	h.Write([]byte{0})
	for _, p := range pkts {
		_, _ = h.Write([]byte(p.TrackID))
		h.Write([]byte{0})
		_, _ = h.Write([]byte(p.PayloadHash()))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// u32 encodes a uint32 big-endian; retained for callers building canonical
// digests over structured records.
func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// ComparePresentation orders two packets by exact presentation time, then by
// stable track ID, then by per-track source sequence. It is the single
// canonical output order used by the merge stage and by audit verification,
// so re-demuxing a produced container yields the same order it was written in
// regardless of how the muxer interleaved chunks on disk.
func ComparePresentation(a, b Packet) int {
	sa, errA := a.PTSSeconds()
	sb, errB := b.PTSSeconds()
	if errA == nil && errB == nil {
		if c := sa.Cmp(sb); c != 0 {
			return c
		}
	} else {
		// Fall back to raw PTS, which is only comparable within one base.
		switch {
		case a.PTS < b.PTS:
			return -1
		case a.PTS > b.PTS:
			return 1
		}
	}
	if a.TrackID != b.TrackID {
		if a.TrackID < b.TrackID {
			return -1
		}
		return 1
	}
	switch {
	case a.Seq < b.Seq:
		return -1
	case a.Seq > b.Seq:
		return 1
	}
	return 0
}

// SortPresentation sorts packets into the canonical output order in place.
func SortPresentation(pkts []Packet) {
	sort.SliceStable(pkts, func(i, j int) bool { return ComparePresentation(pkts[i], pkts[j]) < 0 })
}
