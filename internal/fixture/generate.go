// Package fixture generates the deterministic, fully in-memory drill
// footage used by the demo and the integration tests. Every random choice
// is driven by a seeded PRNG so a given (seed, segments) pair always
// produces byte-identical inputs, including the injected fault blobs.
package fixture

import (
	"encoding/binary"
	"fmt"

	"fastpost/internal/metadata"
)

// FaultSpec names an injected fault and, where meaningful, the absolute
// byte offset at which the corrupted blob must be cut.
type FaultSpec struct {
	Kind   string
	Offset int64
}

// ParseFaultSpec turns a CLI token such as "sei-truncate:4096" into a
// FaultSpec. An empty token means "no fault".
func ParseFaultSpec(token string) (FaultSpec, error) {
	if token == "" {
		return FaultSpec{}, nil
	}
	var kind string
	var off int64
	colon := -1
	for i, c := range token {
		if c == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return FaultSpec{}, fmt.Errorf("fault %q must be kind:offset", token)
	}
	kind = token[:colon]
	rest := token[colon+1:]
	for _, c := range rest {
		if c < '0' || c > '9' {
			return FaultSpec{}, fmt.Errorf("fault offset %q must be numeric", rest)
		}
		off = off*10 + int64(c-'0')
	}
	switch kind {
	case "sei-truncate":
	default:
		return FaultSpec{}, fmt.Errorf("unknown fault kind %q", kind)
	}
	return FaultSpec{Kind: kind, Offset: off}, nil
}

// Track is one generated camera angle with all of its associated blobs.
type Track struct {
	ID         string
	RateNum    int64
	RateDen    int64
	Origin     int64 // common-tick clock skew absorbed by the timebase
	Channels   int
	SampleRate int
	Color      ColorMark
	// Blobs hold the raw, parseable event streams.
	SEI       []byte
	Subtitles []byte
	Chapters  []byte
	ID3       []byte
	EMSG      []byte
	// SegmentTicks are the native cut points in common 90 kHz ticks.
	SegmentTicks []int64
}

// ColorMark is the per-camera color signaling.
type ColorMark struct {
	Primaries  string
	Transfer   string
	Matrix     string
	VideoRange string
}

// rng is a tiny deterministic LCG, avoiding any global/runtime seed.
type rng struct{ state uint64 }

func newRNG(seed uint64) *rng { return &rng{state: seed | 1} }

func (r *rng) next() uint64 {
	// Constants from Knuth's MMIX.
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next()>>33) % n
}

// rate is the fractional video clock shared by the generated cameras.
var rate = metadata.RationalLike{Num: 30000, Den: 1001}

// Set is the full generated workspace for one demo run.
type Set struct {
	Seed     uint64
	Segments int
	Tracks   []Track
	Fault    FaultSpec
	// FaultTrack is the track that carries the injected fault blob.
	FaultTrack string
}

// Generate builds the deterministic fixture set. When fault.Kind is
// sei-truncate, the named track's SEI blob is cut at fault.Offset.
func Generate(seed uint64, segments int, fault FaultSpec) *Set {
	r := newRNG(seed)
	frameTicks := int64(3003) // one 29.97 fps frame on the 90 kHz clock
	segDur := frameTicks * 10 // ten frames per segment

	cameraIDs := []string{"cam-a", "cam-b"}
	set := &Set{Seed: seed, Segments: segments, Fault: fault}

	for ci, id := range cameraIDs {
		// Each camera gets a distinct but deterministic clock origin so the
		// pipeline can demonstrate skew absorption down to zero.
		origin := int64(r.intn(5)) * frameTicks
		tr := Track{
			ID: id, RateNum: rate.Num, RateDen: rate.Den, Origin: origin,
			SampleRate: 48000,
			Color: ColorMark{
				Primaries: "bt709", Transfer: "iec61966-2-1",
				Matrix: "bt709", VideoRange: "SDR",
			},
		}
		// Camera A is stereo, camera B is 5.1 to exercise audio groups.
		if ci == 0 {
			tr.Channels = 2
		} else {
			tr.Channels = 6
		}

		// Native segment cuts on the common timeline.
		cuts := make([]int64, segments+1)
		for s := 0; s <= segments; s++ {
			cuts[s] = int64(s) * segDur
		}
		tr.SegmentTicks = cuts

		// SEI: one timing message per segment, with one event placed exactly
		// on a boundary to exercise boundary-event assignment.
		tr.SEI = buildSEIBlob(r, segments, segDur)
		tr.Subtitles = buildSubtitleBlob(segments, segDur)
		tr.Chapters = buildChapterBlob(segments, segDur)
		tr.ID3 = buildID3Blob(segments, segDur)
		tr.EMSG = buildEMSGBlob(segments, segDur)

		set.Tracks = append(set.Tracks, tr)
	}

	if fault.Kind == "sei-truncate" {
		set.FaultTrack = cameraIDs[0]
		cut := int(fault.Offset)
		if cut < len(set.Tracks[0].SEI) {
			set.Tracks[0].SEI = set.Tracks[0].SEI[:cut]
		}
	}
	return set
}

func buildSEIBlob(r *rng, segments int, segDur int64) []byte {
	var blob []byte
	for s := 0; s < segments; s++ {
		// frameNum, skew, position on the fractional clock.
		frame := int32(s * 10)
		skew := int64(r.intn(100))
		pos := int64(s) * segDur
		blob = append(blob, encodeTimingNAL(frame, skew, pos)...)
	}
	return blob
}

// timingBodySize is the fixed per-message timing payload. The leading
// 16 bytes carry frame/skew/position; the remainder is deterministic
// padding so a 12-segment blob comfortably exceeds the 4096-byte fault
// injection point used by the demo.
const timingBodySize = 384

// encodeTimingNAL mirrors the parser's expected Annex-B layout.
func encodeTimingNAL(frame int32, skew, pos int64) []byte {
	body := make([]byte, timingBodySize)
	binary.BigEndian.PutUint32(body[0:4], uint32(frame))
	binary.BigEndian.PutUint32(body[4:8], uint32(uint64(skew)))
	binary.BigEndian.PutUint64(body[8:16], uint64(pos))
	// Deterministic padding; never zero-filled for long runs so escaping
	// stays bounded and blob length stays predictable.
	for i := 16; i < timingBodySize; i++ {
		body[i] = byte(i*31 + 7)
	}
	payload := []byte{0x67}
	payload = append(payload, encodeSEILen(len(body))...)
	payload = append(payload, body...)
	payload = append(payload, 0x80)
	nal := []byte{0, 0, 0, 1, 0x06}
	return append(nal, escape(payload)...)
}

// encodeSEILen writes a length using the SEI multi-byte convention:
// 0xFF continuation bytes followed by the remainder.
func encodeSEILen(n int) []byte {
	var out []byte
	for n >= 0xFF {
		out = append(out, 0xFF)
		n -= 0xFF
	}
	out = append(out, byte(n))
	return out
}

// escape applies the same Annex-B emulation prevention as the test codec.
func escape(in []byte) []byte {
	var out []byte
	zeros := 0
	for _, b := range in {
		if zeros >= 2 && b <= 3 {
			out = append(out, 0x03)
			zeros = 2
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

func buildSubtitleBlob(segments int, segDur int64) []byte {
	var blob []byte
	for s := 0; s < segments; s++ {
		text := fmt.Sprintf("cue-%d", s)
		blob = append(blob, be32(uint32(int64(s)*segDur))...)
		blob = append(blob, be32(uint32(segDur/2))...)
		blob = append(blob, be16(uint16(len(text)))...)
		blob = append(blob, text...)
	}
	return blob
}

func buildChapterBlob(segments int, segDur int64) []byte {
	var blob []byte
	for s := 0; s < segments; s++ {
		title := fmt.Sprintf("stage-%d", s)
		blob = append(blob, be32(uint32(int64(s)*segDur))...)
		blob = append(blob, be32(uint32(segDur))...) // strictly positive
		blob = append(blob, be16(uint16(len(title)))...)
		blob = append(blob, title...)
	}
	return blob
}

func buildID3Blob(segments int, segDur int64) []byte {
	var frames []byte
	for s := 0; s < segments; s++ {
		tag := fmt.Sprintf("tag-%d", s)
		var payload []byte
		payload = append(payload, be32(uint32(int64(s)*segDur))...)
		payload = append(payload, be32(100)...)
		payload = append(payload, tag...)
		frames = append(frames, []byte("TXXX")...)
		frames = append(frames, be32(uint32(len(payload)))...)
		frames = append(frames, 0, 0)
		frames = append(frames, payload...)
	}
	out := []byte("ID3")
	out = append(out, 3, 0, 0)
	sz := uint32(len(frames))
	out = append(out, byte(sz>>21), byte(sz>>14), byte(sz>>7), byte(sz))
	return append(out, frames...)
}

func buildEMSGBlob(segments int, segDur int64) []byte {
	var blob []byte
	for s := 0; s < segments; s++ {
		blob = append(blob, 0, 0, 0, 0)
		blob = append(blob, "urn:drill"...)
		blob = append(blob, 0)
		blob = append(blob, "stage"...)
		blob = append(blob, 0)
		blob = append(blob, be32(1000)...)
		blob = append(blob, be64(uint64(s))...)
		blob = append(blob, be64(uint64(segDur))...)
	}
	return blob
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}
func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}
func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
