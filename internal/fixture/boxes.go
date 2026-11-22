package fixture

import (
	"encoding/binary"
	"math"

	"mediad/internal/clock"
)

func buildInit(rate int, trackID uint32) []byte {
	ftyp := box("ftyp", concat([]byte("isom"), be32(512), []byte("isomiso6mp41")))
	mvhd := box("mvhd", concat(
		[]byte{0, 0, 0, 0}, be32(0), be32(0), be32(uint32(rate)), be32(0),
		be32(0x00010000), be16(0x0100), be16(0),
		make([]byte, 8), make([]byte, 36), make([]byte, 24), be32(2),
	))
	mdhd := box("mdhd", concat(
		[]byte{0, 0, 0, 0}, be32(0), be32(0), be32(uint32(rate)), be32(0),
		be16(0x55c4), be16(0),
	))
	hdlr := box("hdlr", concat([]byte{0, 0, 0, 0}, be32(0), []byte("soun"), make([]byte, 12), []byte("Sound\x00")))
	smhd := box("smhd", concat([]byte{0, 0, 0, 0}, be16(0), be16(0)))
	mp4a := box("mp4a", concat(
		make([]byte, 6), be16(1), be16(0), be16(0), be16(2), be16(16),
		be16(0), be16(0), be32(uint32(rate)<<16),
	))
	stsd := box("stsd", concat([]byte{0, 0, 0, 0}, be32(1), mp4a))
	stbl := box("stbl", stsd)
	minf := box("minf", concat(smhd, stbl))
	mdia := box("mdia", concat(mdhd, hdlr, minf))
	tkhd := box("tkhd", concat(
		[]byte{0, 0, 0, 7}, be32(0), be32(0), be32(trackID), be32(0), be32(0),
		make([]byte, 8), be16(0), be16(0), be16(0x0100), be16(0),
		make([]byte, 36), be32(0), be32(0),
	))
	trak := box("trak", concat(tkhd, mdia))
	trex := box("trex", concat([]byte{0, 0, 0, 0}, be32(trackID), be32(1), be32(0), be32(0), be32(0)))
	mvex := box("mvex", trex)
	moov := box("moov", concat(mvhd, trak, mvex))
	return concat(ftyp, moov)
}

// buildMedia creates styp + moof(mfhd/traf(tfhd,tfdt,trun)) + mdat with
// deterministic PCM payload derived from the start tick and seed-free math.
func buildMedia(start, dur int64, rate int, seq uint64, keyframe bool) []byte {
	frames := int(dur * int64(rate) / int64(clock.DefaultTimescale))
	if frames <= 0 {
		frames = 1
	}
	// 1000-frame samples divide a 96 000-frame two-second block exactly, so
	// native sample durations sum to the declared segment duration.
	const sampleFrames = 1000
	samples := (frames + sampleFrames - 1) / sampleFrames
	styp := box("styp", concat([]byte("msdh"), be32(0), []byte("msdhmsix")))
	mfhd := box("mfhd", concat([]byte{0, 0, 0, 0}, be32(uint32(seq))))
	tfhd := box("tfhd", concat([]byte{0, 0x02, 0, 0}, be32(1)))
	// tfdt is expressed in the track's native timescale, not the canonical
	// clock, so the declared media timeline stays self-consistent.
	nativeStart := start * int64(rate) / int64(clock.DefaultTimescale)
	tfdt := box("tfdt", concat([]byte{1, 0, 0, 0}, be64(uint64(nativeStart))))
	firstFlags := uint32(0x01010000)
	if keyframe {
		firstFlags = 0x02000000
	}
	trunPayload := concat(
		[]byte{0, 0x00, 0x07, 0x05}, be32(uint32(samples)), be32(0), be32(firstFlags),
	)
	samples64 := int64(samples)
	if samples64 <= 0 {
		samples64 = 1
	}
	per := int64(dur) * int64(rate) / int64(clock.DefaultTimescale) / samples64
	if per <= 0 {
		per = 1
	}
	for i := 0; i < samples; i++ {
		flags := uint32(0x01010000)
		if i == 0 && keyframe {
			flags = 0x02000000
		}
		trunPayload = concat(trunPayload, be32(uint32(per)), be32(sampleFrames*2*2), be32(flags))
	}
	trun := box("trun", trunPayload)
	traf := box("traf", concat(tfhd, tfdt, trun))
	moof := box("moof", concat(mfhd, traf))
	payload := make([]byte, samples*sampleFrames*2*2)
	for i := 0; i < samples; i++ {
		for f := 0; f < sampleFrames; f++ {
			phase := float64(start+int64(i*sampleFrames+f)) / float64(rate)
			v := int16(6000 * math.Sin(2*math.Pi*440*phase))
			binary.LittleEndian.PutUint16(payload[(i*sampleFrames+f)*4:], uint16(v))
			binary.LittleEndian.PutUint16(payload[(i*sampleFrames+f)*4+2:], uint16(v))
		}
	}
	mdat := box("mdat", payload)
	return concat(styp, moof, mdat)
}

// corruptSecondBox overwrites the moof size with a value larger than the
// remaining file, producing a box overflow at a deterministic offset.
func corruptSecondBox(data []byte, rng *rng) []byte {
	out := append([]byte(nil), data...)
	offset := 0
	for i := 0; i < 2; i++ {
		if offset+8 > len(out) {
			break
		}
		size := int(binary.BigEndian.Uint32(out[offset:]))
		if i == 1 {
			binary.BigEndian.PutUint32(out[offset:], uint32(len(out)+rng.intn(1024)+1))
			return out
		}
		if size <= 0 {
			break
		}
		offset += size
	}
	return out
}

type rng struct{ state uint64 }

func newRNG(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9e3779b97f4a7c15
	}
	return &rng{state: s}
}

func (r *rng) next() uint64 {
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return r.state
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// digest is exposed for tests that need a stable content fingerprint.
