// Package fixture generates deterministic synthetic bare streams, manifests
// and PCM material entirely in-process. No external files or encoders are
// used: every byte derives from a math/rand/v2 source seeded by the demo
// seed, so a rerun with the same seed is byte-identical.
package fixture

import (
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// GOP is the structural IDR/keyframe interval in access units (six frames at
// 30 fps = a structural keyframe every 0.2 s).
const GOP = 6

// KeyframeMarkStride makes the manifest index one in every six structural
// keyframes, so one 24 s, 30 fps stream (120 structural keyframes) exposes
// 20 indexed keyframes. Across two cameras and three renditions that is
// exactly 120 indexed keyframes, while segment boundaries still snap to the
// denser structural keyframe grid (one every 0.2 s).
const KeyframeMarkStride = 6

// FrameRate of the synthetic camera.
const FrameRate = 30

// Params describes one rendition's synthetic bare stream.
type Params struct {
	Camera    string
	Rendition string
	Codec     ingest.Codec
	Units     int
	TimeBase  uint64
	Seed      uint64
}

// BareStream is a generated stream plus its correct, uncorrupted manifest.
type BareStream struct {
	Params   Params
	Data     []byte
	Manifest *ingest.Manifest
	// ColorTags carries the conflicting labels the demo policy resolves.
	ColorTags []ColorSource
}

// ColorSource is one color-label claim with its provenance.
type ColorSource struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	Source    string
}

// startCode3 is the three-byte Annex B start code prefix.
var startCode3 = []byte{0, 0, 1}

// Generate builds an Annex B / low-overhead bitstream and a matching
// manifest. Keyframe units carry codec-valid IDR/IRAP/key OBU headers so the
// ingest scanners recognize them; inter frames carry slice/frame headers.
func Generate(p Params) *BareStream {
	rng := newRNG(p.Seed)
	m := &ingest.Manifest{
		Camera: p.Camera, Rendition: p.Rendition, Codec: p.Codec,
		TimeBase: p.TimeBase, Entries: make([]ingest.IndexEntry, 0, p.Units),
	}
	var data []byte
	keyOrdinal := 0
	for i := 0; i < p.Units; i++ {
		structuralKey := i%GOP == 0
		// The bitstream still carries an IDR/key frame at every GOP; only
		// the manifest's keyframe marking is subsampled.
		unit := encodeUnit(p.Codec, i, structuralKey, rng)
		marked := false
		if structuralKey {
			marked = keyOrdinal%KeyframeMarkStride == 0
			keyOrdinal++
		}
		off := int64(len(data))
		data = append(data, unit...)
		m.Entries = append(m.Entries, ingest.IndexEntry{
			Camera: p.Camera, Rendition: p.Rendition, Codec: p.Codec,
			TimeBase: p.TimeBase, PTS: uint64(i) * p.TimeBase / FrameRate,
			Offset: off, Length: len(unit), Keyframe: marked,
		})
	}
	return &BareStream{
		Params: p, Data: data, Manifest: m,
		ColorTags: []ColorSource{
			{Primaries: 9, Transfer: 16, Matrix: 9, Source: "bitstream"},
			{Primaries: 1, Transfer: 1, Matrix: 1, Source: "manifest"},
			{Primaries: 9, Transfer: 16, Matrix: 9, Source: "policy"},
		},
	}
}

// encodeUnit produces one structurally valid access unit with deterministic
// payload bytes.
func encodeUnit(codec ingest.Codec, index int, key bool, rng *rngSource) []byte {
	payloadLen := 24 + rng.intn(40)
	body := make([]byte, payloadLen)
	for i := range body {
		body[i] = byte(rng.intn(256))
	}
	switch codec {
	case ingest.CodecH264:
		nalType := byte(1) // non-IDR slice
		if key {
			nalType = 5 // IDR
		}
		header := append([]byte{}, startCode3...)
		header = append(header, nalType&0x1f)
		return append(header, body...)
	case ingest.CodecH265:
		nalType := byte(0) // TRAIL_N
		if key {
			nalType = 19 // IDR_W_RADL
		}
		header := append([]byte{}, startCode3...)
		header = append(header, (nalType&0x3f)<<1)
		return append(header, body...)
	case ingest.CodecAV1:
		return encodeAV1Unit(index, key, body)
	}
	return append(append([]byte{}, startCode3...), body...)
}

// encodeAV1Unit emits a low-overhead temporal unit: one Annex B start code
// followed by a sequence header OBU (on keyframes) and a frame header OBU,
// concatenated without an inner start code (per AV1 low-overhead format).
func encodeAV1Unit(index int, key bool, body []byte) []byte {
	var out []byte
	out = append(out, startCode3...)
	if key {
		out = append(out, av1OBU(1, []byte{0x42, 0x0a})...) // minimal sequence header
	}
	// Frame header OBU. The uncompressed header opens with three bits of
	// show_existing_frame then two bits of frame_type; KEY_FRAME is 0b00.
	// The byte-level value keeps show_existing_frame clear and encodes
	// frame_type in its low two bits: 0x00 key, 0x02 inter.
	fh := []byte{0x02}
	if key {
		fh[0] = 0x00
	}
	fh = append(fh, byte(index), byte(index>>8))
	out = append(out, av1OBU(3, append(fh, body...))...)
	return out
}

// av1OBU wraps an OBU payload with header and LEB128 size.
func av1OBU(obType uint8, payload []byte) []byte {
	hdr := (obType & 0x0f) << 3
	hdr |= 0x02 // obu_has_size_field
	out := []byte{hdr}
	out = appendLEB128(out, uint64(len(payload)))
	return append(out, payload...)
}

func appendLEB128(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
		} else {
			return append(b, c)
		}
	}
}

// rngSource is a small deterministic PCG-style generator avoiding global
// runtime state so concurrent streams never interleave randomness.
type rngSource struct{ state uint64 }

func newRNG(seed uint64) *rngSource {
	return &rngSource{state: seed*6364136223846793005 + 1442695040888963407}
}

func (r *rngSource) next() uint64 {
	old := r.state
	r.state = old*6364136223846793005 + 1442695040888963407
	xorshifted := ((old >> 18) ^ old) >> 27
	rot := old >> 59
	return (xorshifted >> rot) | (xorshifted << ((-rot) & 31))
}

func (r *rngSource) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}
