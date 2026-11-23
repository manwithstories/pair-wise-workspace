package fixture

import (
	"mediacore/internal/sample"
)

// Mode selects how the generated file is damaged.
type Mode string

const (
	ModeOK              Mode = "ok"
	ModeTruncatedTail   Mode = "truncated_tail"   // last frame cut mid-sample
	ModeCorruptSample   Mode = "corrupt_sample"   // one sample's payload byte flipped
	ModeMissingIndex    Mode = "missing_index"    // stco/co64 box removed
	ModeIndexOutOfRange Mode = "index_out_range"  // chunk offset points past EOF
	ModeUnknownCodec    Mode = "unknown_codec"    // unsupported required codec config
	ModeTruncatedHeader Mode = "truncated_header" // final box header cut to 4 bytes
)

// TrackSpec describes one synthetic track.
type TrackSpec struct {
	Codec     sample.Codec
	Timescale uint32
	Samples   int
	// Delta is the per-sample duration in the track's own timescale.
	Delta uint32
	// Every KeyEvery-th sample (1-based) is a sync sample; 0 disables stss.
	KeyEvery int
}

// Spec is the complete generation request.
type Spec struct {
	Seed    int64
	Variant int // 0 = isom/moov-first/stco; 1 = iso5/mdat-first/co64
	Tracks  []TrackSpec
	Mode    Mode
	// CorruptIndex is the sample ordinal damaged for ModeCorruptSample.
	CorruptIndex int
	// DropStco removes the chunk offset table (ModeMissingIndex).
	DropStco bool
}

// Artifact is a generated input plus the ground truth used to verify it.
type Artifact struct {
	Bytes       []byte
	Variant     int
	Tracks      []TrackTruth
	// MdatOffset is the absolute offset of the mdat payload.
	MdatOffset int64
	// CorruptOffset is the exact byte flipped/claimed, or -1.
	CorruptOffset int64
	// TruncatedAt is the file length before truncation (-1 if none).
	OriginalLength int64
}

// TrackTruth records what the demuxer must recover for one track.
type TrackTruth struct {
	ID          uint32
	Codec       sample.Codec
	Timescale   uint32
	SampleCount int
	// SampleOffsets are absolute frame-header offsets in mdat order.
	SampleOffsets []int64
	SampleSizes   []int
	Deltas        []uint32
}

// DefaultSpec returns the 3-track (H.264 + AAC + Opus) demo spec.
func DefaultSpec(seed int64, variant int) Spec {
	return Spec{
		Seed:    seed,
		Variant: variant,
		Tracks: []TrackSpec{
			{Codec: sample.CodecH264, Timescale: 90000, Samples: 24, Delta: 3000, KeyEvery: 6},
			{Codec: sample.CodecAAC, Timescale: 44100, Samples: 24, Delta: 1024, KeyEvery: 0},
			{Codec: sample.CodecOpus, Timescale: 48000, Samples: 24, Delta: 960, KeyEvery: 0},
		},
		Mode:          ModeOK,
		CorruptIndex:  10,
	}
}

// rng is a tiny deterministic LCG so fixture bytes are reproducible by seed.
type rng struct{ state uint64 }

func newRNG(seed int64) *rng { return &rng{state: uint64(seed)*6364136223846793005 + 1442695040888963407} }
func (r *rng) byte() byte     { r.state = r.state*6364136223846793005 + 1442695040888963407; return byte(r.state >> 33) }

// payload builds deterministic, seed-dependent compressed sample bytes.
func payload(rng *rng, codec sample.Codec, seq int) []byte {
	size := 24 + (seq*7+int(rng.state>>40))%40
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = rng.byte()
	}
	// Leading nibble hints at codec without making the data decodable.
	switch codec {
	case sample.CodecH264:
		buf[0] = (buf[0] & 0x0F) | 0x60
	case sample.CodecAAC:
		buf[0] = (buf[0] & 0x0F) | 0xA0
	case sample.CodecOpus:
		buf[0] = (buf[0] & 0x0F) | 0x40
	}
	return buf
}
