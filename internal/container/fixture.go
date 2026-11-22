// Self-generated MP4/MOV fixtures.
//
// Fixtures are produced by the same code that ships in the binary, so the
// tests and the demo command never depend on media files outside the repo.
// The writer emits one 'sowt' audio track with stts/stsz/stsc/stco/stsd the
// parser understands, plus optional deliberate damage used to exercise the
// error-attribution paths.
package container

import (
	"encoding/binary"
	"math"
	"os"

	"mediacore/internal/media"
)

// FixtureSpec describes a synthetic single-track PCM MP4.
type FixtureSpec struct {
	Rate      int
	Channels  int
	Frames    int64 // total frames
	FrameSize int   // frames per container sample
	CTTSTrick bool  // insert a regressing composition offset
	OddSize   bool  // make the first sample an odd byte count
}

// writeFixture builds a playable MP4 with one 'sowt' audio track.
func WriteFixture(path string, fx FixtureSpec) error {
	if fx.Channels < 1 || fx.Channels > 2 || fx.Rate <= 0 || fx.Frames <= 0 || fx.FrameSize <= 0 {
		return media.NewError(media.CodeUsage, -1, 0, "invalid fixture spec")
	}
	pcm := SynthPCM(fx)
	// Split into fixed-size samples; the last one may be short.
	var sizes []int
	for off := 0; off < len(pcm); off += fx.FrameSize * fx.Channels * 2 {
		n := fx.FrameSize * fx.Channels * 2
		if off+n > len(pcm) {
			n = len(pcm) - off
		}
		sizes = append(sizes, n)
	}
	if fx.OddSize && len(sizes) > 0 {
		sizes[0] = 1023 // not a whole stereo 16-bit frame
	}
	data := make([]byte, 0, len(pcm)+4096)
	data = append(data, boxBytes("ftyp", ftypBody())...)
	moov := buildMoov(fx, sizes, 0)
	mdatStart := len(data) + len(moov) + 8
	moov = buildMoov(fx, sizes, int64(mdatStart))
	data = append(data, moov...)
	data = append(data, boxBytes("mdat", pcm)...)
	return os.WriteFile(path, data, 0o644)
}

func ftypBody() []byte {
	b := []byte("M4A ")
	b = u32Append(b, 0)
	for _, brand := range []string{"M4A ", "isom", "qt  "} {
		b = append(b, brand...)
	}
	return b
}

// buildMoov builds moov with stco offsets based on mdatStart.
func buildMoov(fx FixtureSpec, sizes []int, mdatStart int64) []byte {
	duration := uint32(fx.Frames)
	hdr := func(tag string, body []byte) []byte { return boxBytes(tag, body) }
	full := func(tag string, version uint8, body []byte) []byte {
		payload := []byte{version, 0, 0, 0}
		payload = append(payload, body...)
		return hdr(tag, payload)
	}

	mvhd := make([]byte, 0, 96)
	mvhd = append(mvhd, u32(0)...)    // creation
	mvhd = append(mvhd, u32(0)...)    // modification
	mvhd = append(mvhd, u32(1000)...) // timescale
	mvhd = append(mvhd, u32(duration)...)
	mvhd = append(mvhd, u32(0x00010000)...) // rate
	mvhd = append(mvhd, 0x01, 0x00)         // volume
	mvhd = append(mvhd, 0, 0, 0, 0, 0, 0, 0, 0)
	mvhd = append(mvhd, matrix()...)
	mvhd = append(mvhd, make([]byte, 24)...) // pre_defined
	mvhd = append(mvhd, u32(2)...)           // next track id

	tkhd := make([]byte, 0, 80)
	tkhd = append(tkhd, u32(0)...)
	tkhd = append(tkhd, u32(0)...)
	tkhd = append(tkhd, u32(1)...) // track id
	tkhd = append(tkhd, u32(0)...)
	tkhd = append(tkhd, u32(duration)...)
	tkhd = append(tkhd, make([]byte, 8)...)
	tkhd = append(tkhd, 0, 0)       // layer
	tkhd = append(tkhd, 0, 0)       // alternate group
	tkhd = append(tkhd, 0x01, 0x00) // volume
	tkhd = append(tkhd, 0, 0)
	tkhd = append(tkhd, matrix()...)
	tkhd = append(tkhd, u32(0)...) // width
	tkhd = append(tkhd, u32(0)...) // height

	mdhd := make([]byte, 0, 20)
	mdhd = append(mdhd, u32(0)...)
	mdhd = append(mdhd, u32(0)...)
	mdhd = append(mdhd, u32(uint32(fx.Rate))...)
	mdhd = append(mdhd, u32(duration)...)
	mdhd = append(mdhd, 0x55, 0xc4) // language 'und'
	mdhd = append(mdhd, 0, 0)

	hdlr := append(u32(0), []byte("soun")...)
	hdlr = append(hdlr, make([]byte, 12)...)
	hdlr = append(hdlr, append([]byte("SoundHandler"), 0)...)

	smhd := []byte{0, 0}      // balance
	smhd = append(smhd, 0, 0) // reserved

	entry := make([]byte, 0, 36)
	entry = append(entry, make([]byte, 6)...)
	entry = append(entry, 0, 1) // data reference index
	entry = append(entry, make([]byte, 8)...)
	entry = append(entry, byte(fx.Channels>>8), byte(fx.Channels))
	entry = append(entry, 0, 16) // sample size
	entry = append(entry, 0, 0)  // compression id
	entry = append(entry, 0, 0)  // packet size
	entry = append(entry, u32(uint32(fx.Rate)<<16)...)
	stsdBody := append([]byte{0, 0, 0, 0}, u32(1)...)
	stsdBody = append(stsdBody, boxBytes("sowt", entry)...)
	stsd := boxBytes("stsd", stsdBody)

	sttsBody := append(u32(1), u32(uint32(len(sizes)))...)
	sttsBody = append(sttsBody, u32(uint32(fx.FrameSize))...)
	// The final sample may be shorter; stts is about duration, so all
	// samples keep the nominal delta and the last one is simply short.
	stts := full("stts", 0, sttsBody)

	szBody := append(u32(0), u32(uint32(len(sizes)))...)
	for _, s := range sizes {
		szBody = append(szBody, u32(uint32(s))...)
	}
	stsz := full("stsz", 0, szBody)

	stscBody := append(u32(1), u32(1)...)
	stscBody = append(stscBody, u32(1)...)
	stscBody = append(stscBody, u32(1)...)
	stsc := full("stsc", 0, stscBody)

	stcoBody := u32(uint32(len(sizes)))
	chunk := int64(fx.FrameSize * fx.Channels * 2)
	for i := range sizes {
		stcoBody = append(stcoBody, u32(uint32(mdatStart+int64(i)*chunk))...)
	}
	stco := full("stco", 0, stcoBody)

	stblParts := [][]byte{stsd, stts}
	if fx.CTTSTrick {
		// Two entries: first sample offset 0, the rest a large negative
		// offset, which makes PTS regress on the second sample.
		body := append(u32(2), u32(1)...)
		body = append(body, u32(0)...)
		body = append(body, u32(uint32(len(sizes)-1))...)
		body = append(body, u32(uint32(uint32(0)-uint32(fx.FrameSize)-1))...)
		stblParts = append(stblParts, full("ctts", 1, body))
	}
	stblParts = append(stblParts, stsz, stsc, stco)
	stbl := hdr("stbl", concat(stblParts))

	minf := hdr("minf", concat([][]byte{full("smhd", 0, smhd), stbl}))
	mdia := hdr("mdia", concat([][]byte{full("mdhd", 0, mdhd), full("hdlr", 0, hdlr), minf}))
	trak := hdr("trak", concat([][]byte{full("tkhd", 0, tkhd), mdia}))
	moov := hdr("moov", concat([][]byte{full("mvhd", 0, mvhd), trak}))
	return moov
}

// synthPCM produces a deterministic signal that exercises every loudness
// classifier: tone, silence, a clipped passage and a loud but clean passage.
func SynthPCM(fx FixtureSpec) []byte {
	n := int(fx.Frames)
	out := make([]byte, n*fx.Channels*2)
	phase := 0.0
	for i := 0; i < n; i++ {
		// Four quarters of the signal: clean, silent, clipped, loud.
		quarter := n / 4
		var amp float64
		switch {
		case i < quarter:
			amp = 0.35
		case i < 2*quarter:
			amp = 0
		case i < 3*quarter:
			amp = 1.20
		default:
			amp = 0.80
		}
		phase += 2 * 3.141592653589793 * 440 / float64(fx.Rate)
		v := amp * math.Sin(phase)
		s := int16(0)
		if v > 0.999969 {
			s = 32767
		} else if v < -1 {
			s = -32768
		} else {
			s = int16(v * 32767)
		}
		for c := 0; c < fx.Channels; c++ {
			binary.LittleEndian.PutUint16(out[2*(i*fx.Channels+c):], uint16(s))
		}
	}
	return out
}

func matrix() []byte {
	m := make([]byte, 36)
	binary.BigEndian.PutUint32(m[0:], 0x00010000)
	binary.BigEndian.PutUint32(m[16:], 0x00010000)
	binary.BigEndian.PutUint32(m[32:], 0x40000000)
	return m
}

func boxBytes(tag string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	copy(out[4:8], tag)
	return append(out, body...)
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u32Append(dst []byte, v uint32) []byte { return append(dst, u32(v)...) }

func concat(parts [][]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
