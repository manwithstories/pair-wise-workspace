package fixture

import (
	"crypto/sha256"
	"encoding/binary"
)

// Variant selects the timeline shape to synthesize.
type Variant string

const (
	// VariantElstCttsWrap includes an edit list (empty edit + media_time
	// offset), negative ctts v1 offsets and a uint32 DTS rollover.
	VariantElstCttsWrap Variant = "elst-ctts-wrap"
	// VariantPlain is a straightforward monotonic single/multi-track file.
	VariantPlain Variant = "plain"
)

// Options controls fixture synthesis.
type Options struct {
	Path            string
	Tracks          int
	Frames          int
	Variant         Variant
	MoovAtEnd       bool
	UseCo64         bool // force 64-bit chunk offsets
	CttsV0          bool // emit version-0 ctts (unsigned offsets)
	SamplesPerChunk int  // >1 packs multiple samples into each chunk
}

// wrapTimescale makes the per-frame delta large enough that the cumulative DTS
// crosses the 2^32 boundary three times within the requested frame count while
// keeping a wall-clock duration of ~1 frame per second.
const wrapTimescale uint32 = 10_800_000

// samplePayload builds deterministic, unique sample bytes so frame hashing and
// byte-exact export can be verified. The 32-byte header embeds track/frame ids.
func samplePayload(track uint32, frame uint32, size uint32) []byte {
	buf := make([]byte, size)
	var hdr [32]byte
	binary.BigEndian.PutUint32(hdr[0:], 0x46524d31) // "FRM1"
	binary.BigEndian.PutUint32(hdr[4:], track)
	binary.BigEndian.PutUint32(hdr[8:], frame)
	sum := sha256.Sum256(hdr[:12])
	copy(buf, hdr[:12])
	copy(buf[12:], sum[:])
	for i := 32; i < len(buf); i++ {
		buf[i] = byte(int(frame)*31 + i*7 + int(track))
	}
	return buf
}

func videoSize(frame uint32) uint32 {
	return 9000 + (frame%37)*53
}

func audioSize(frame uint32) uint32 {
	return 320 + (frame%5)*16
}

// trackSpec holds the in-memory plan for one track.
type trackSpec struct {
	handler  string
	trackID  uint32
	ts       uint32
	samples  [][]byte
	sync     []bool
	sizes    []uint32
	offsets  []int64 // chunk offsets (one chunk per sample here)
	deltas   []uint32
	ctts     []int64
	keyEvery int
	perChunk int // samples packed into each chunk
}

func buildVideoSpec(frames int, variant Variant) *trackSpec {
	ts := wrapTimescale
	delta := wrapTimescale
	if variant == VariantPlain {
		ts = 90000 // conventional video timescale, one 1000 ms frame
		delta = 90000
	}
	sp := &trackSpec{
		handler:  "vide",
		trackID:  1,
		ts:       ts,
		keyEvery: 20,
		perChunk: 1,
	}
	for f := 0; f < frames; f++ {
		size := videoSize(uint32(f))
		sp.samples = append(sp.samples, samplePayload(1, uint32(f), size))
		sp.sizes = append(sp.sizes, size)
		sp.sync = append(sp.sync, f%sp.keyEvery == 0)
		sp.deltas = append(sp.deltas, delta) // 1 second per frame
	}
	if variant == VariantElstCttsWrap {
		// Negative composition offsets (B-frame style) including values that
		// would push PTS backwards before normalization.
		for f := range sp.samples {
			switch f % 6 {
			case 1, 4:
				sp.ctts = append(sp.ctts, -int64(wrapTimescale)/2)
			case 2:
				sp.ctts = append(sp.ctts, int64(wrapTimescale)/4)
			default:
				sp.ctts = append(sp.ctts, 0)
			}
		}
	}
	return sp
}

func buildAudioSpec(frames int) *trackSpec {
	sp := &trackSpec{
		handler:  "soun",
		trackID:  2,
		ts:       48000,
		perChunk: 1,
	}
	// roughly four audio chunks per video frame
	audioFrames := frames * 4
	for f := 0; f < audioFrames; f++ {
		size := audioSize(uint32(f))
		sp.samples = append(sp.samples, samplePayload(2, uint32(f), size))
		sp.sizes = append(sp.sizes, size)
		sp.sync = append(sp.sync, true)
		sp.deltas = append(sp.deltas, 1024) // AAC-ish
	}
	return sp
}
