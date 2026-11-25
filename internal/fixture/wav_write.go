package fixture

import (
	"encoding/binary"
	"os"

	"mediaaudit/internal/wav"
)

// rng is a deterministic xorshift64* PRNG so fixture bytes depend only on seed.
type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9E3779B97F4A7C15
	}
	return &rng{state: s}
}

func (r *rng) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}

// writeWAV emits a little-endian PCM WAVE with fmt, data and crCC chunks. The
// bit depth is fixed at 24-bit for generated multi-channel fixtures.
func writeWAV(path string, o Options, bits uint16, data []byte, crcs []uint32) error {
	frameBytes := o.Channels * int(bits) / 8
	byteRate := uint32(o.SampleRate) * uint32(frameBytes)
	blockAlign := uint16(frameBytes)

	crcPayload := make([]byte, 4*len(crcs))
	for i, c := range crcs {
		binary.LittleEndian.PutUint32(crcPayload[i*4:], c)
	}
	riffSize := 4 + (8+16) + (8+len(data)+len(data)%2) + (8 + len(crcPayload))

	var b []byte
	put4 := func(buf []byte, at int, s string) { copy(buf[at:], []byte(s)) }
	head := make([]byte, 12)
	copy(head[0:4], "RIFF")
	binary.LittleEndian.PutUint32(head[4:], uint32(riffSize))
	copy(head[8:], "WAVE")
	b = append(b, head...)

	fmtc := make([]byte, 8+16)
	put4(fmtc, 0, "fmt ")
	binary.LittleEndian.PutUint32(fmtc[4:], 16)
	binary.LittleEndian.PutUint16(fmtc[8:], 1) // PCM
	binary.LittleEndian.PutUint16(fmtc[10:], uint16(o.Channels))
	binary.LittleEndian.PutUint32(fmtc[12:], uint32(o.SampleRate))
	binary.LittleEndian.PutUint32(fmtc[16:], byteRate)
	binary.LittleEndian.PutUint16(fmtc[20:], blockAlign)
	binary.LittleEndian.PutUint16(fmtc[22:], bits)
	b = append(b, fmtc...)

	dh := make([]byte, 8)
	put4(dh, 0, "data")
	binary.LittleEndian.PutUint32(dh[4:], uint32(len(data)))
	b = append(b, dh...)
	b = append(b, data...)
	if len(data)%2 == 1 {
		b = append(b, 0)
	}

	ch := make([]byte, 8+len(crcPayload))
	put4(ch, 0, wav.CRCChunkID)
	binary.LittleEndian.PutUint32(ch[4:], uint32(len(crcPayload)))
	copy(ch[8:], crcPayload)
	b = append(b, ch...)

	return os.WriteFile(path, b, 0o644)
}
