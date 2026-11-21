// Package fixture synthesizes deterministic MP4/MOV inputs and their damaged
// variants. Everything is generated in-process; no external media is read.
package fixture

import (
	"math/rand"

	"mediamerge/internal/mp4util"
)

// TrackSpec describes one generated track.
type TrackSpec struct {
	ID         uint32
	Media      string // "video" or "audio"
	Codec      string // "h264", "hevc", "aac", "alac"
	Timescale  uint32
	Samples    int
	PayloadMin int
	PayloadMax int
	Width      uint16
	Height     uint16
	Channels   uint16
}

// Plan is a full deterministic input description.
type Plan struct {
	Tracks  []TrackSpec
	Seed    int64
	Padding int // free-space bytes inserted after each sample
}

// DefaultPlan builds n tracks cycling through common codec/timescale pairs.
// With n=3 and 240 samples the produced file exceeds 10 MB.
func DefaultPlan(n, samples int) Plan {
	if n < 1 {
		n = 1
	}
	codecs := []struct {
		media string
		codec string
		ts    uint32
		min   int
		max   int
	}{
		{"video", "h264", 30000, 16000, 28000},
		{"audio", "aac", 48000, 2400, 5200},
		{"video", "hevc", 24000, 14000, 26000},
		{"audio", "alac", 44100, 3000, 6000},
	}
	p := Plan{Seed: 0x5eed, Tracks: make([]TrackSpec, n)}
	for i := 0; i < n; i++ {
		c := codecs[i%len(codecs)]
		p.Tracks[i] = TrackSpec{
			ID: uint32(i + 1), Media: c.media, Codec: c.codec, Timescale: c.ts,
			Samples: samples, PayloadMin: c.min, PayloadMax: c.max,
			Width: 320, Height: 240, Channels: 2,
		}
	}
	return p
}

func ftypBox() []byte {
	return mp4util.Box("ftyp", mp4util.Cat(
		mp4util.FourCC("isom"), mp4util.U32(0x200),
		mp4util.FourCC("isom"), mp4util.FourCC("iso2"),
		mp4util.FourCC("avc1"), mp4util.FourCC("mp41"),
	))
}

type layout struct {
	offsets [][]uint64
	sizes   [][]uint32
}

func (p Plan) layout(base uint64) layout {
	rnd := rand.New(rand.NewSource(p.Seed))
	l := layout{offsets: make([][]uint64, len(p.Tracks)), sizes: make([][]uint32, len(p.Tracks))}
	for i, t := range p.Tracks {
		l.sizes[i] = make([]uint32, t.Samples)
		l.offsets[i] = make([]uint64, t.Samples)
		for s := range l.sizes[i] {
			l.sizes[i][s] = uint32(t.PayloadMin + rnd.Intn(t.PayloadMax-t.PayloadMin+1))
		}
	}
	for i := range p.Tracks {
		pos := base
		for s := range l.offsets[i] {
			l.offsets[i][s] = pos
			pos += uint64(l.sizes[i][s]) + uint64(p.Padding)
		}
	}
	return l
}

// Build renders the plan as a complete MP4 file.
func (p Plan) Build() []byte {
	// Chunk offsets are fixed-width 64-bit values, so moov length does not
	// depend on their values. Compute the length with zero offsets, then
	// rebuild with real offsets.
	zero := p.layout(0)
	header := len(ftypBox()) + len(mp4util.Box("free", mp4util.Zeros(8))) + len(p.moov(zero))
	l := p.layout(uint64(header + 8))
	return mp4util.Cat(ftypBox(), mp4util.Box("free", mp4util.Zeros(8)),
		p.moov(l), p.mdatBox(l))
}

// Sizes returns the per-track sample sizes of the built file.
func (p Plan) Sizes() [][]uint32 { return p.layout(0).sizes }

func (p Plan) mdatBox(l layout) []byte {
	rnd := rand.New(rand.NewSource(p.Seed ^ 0x1234))
	total := 0
	for i := range l.sizes {
		for _, n := range l.sizes[i] {
			total += int(n) + p.Padding
		}
	}
	body := make([]byte, 0, total)
	for i, t := range p.Tracks {
		for s := 0; s < t.Samples; s++ {
			buf := make([]byte, int(l.sizes[i][s]))
			rnd.Read(buf)
			buf[0] = byte(t.ID)
			if len(buf) > 1 {
				buf[1] = byte(s)
			}
			body = append(body, buf...)
			body = append(body, make([]byte, p.Padding)...)
		}
	}
	return mp4util.Box("mdat", body)
}

func (p Plan) moov(l layout) []byte {
	var duration uint64
	for _, t := range p.Tracks {
		if d := uint64(t.Samples); d > duration {
			duration = d
		}
	}
	mvhd := mp4util.FullBox("mvhd", 0, 0, mp4util.Cat(
		mp4util.U32(0), mp4util.U32(0), mp4util.U32(1000), mp4util.U32(uint32(duration)),
		mp4util.U32(0x00010000), mp4util.U16(0x0100), mp4util.U16(0), mp4util.Zeros(8),
		mp4util.Matrix(), mp4util.Zeros(24), mp4util.U32(uint32(len(p.Tracks)+1)),
	))
	parts := [][]byte{mvhd}
	for i, t := range p.Tracks {
		parts = append(parts, p.trak(t, l.offsets[i], l.sizes[i]))
	}
	return mp4util.Box("moov", parts...)
}

func (p Plan) trak(t TrackSpec, offsets []uint64, sizes []uint32) []byte {
	tkhd := mp4util.FullBox("tkhd", 0, 0x7, mp4util.Cat(
		mp4util.U32(0), mp4util.U32(0), mp4util.U32(t.ID), mp4util.U32(0),
		mp4util.U32(uint32(t.Samples)), mp4util.Zeros(8), mp4util.U16(0), mp4util.U16(0),
		mp4util.U16(0), mp4util.U16(0), mp4util.Matrix(),
		mp4util.U32(uint32(t.Width)<<16), mp4util.U32(uint32(t.Height)<<16),
	))
	mdhd := mp4util.FullBox("mdhd", 0, 0, mp4util.Cat(
		mp4util.U32(0), mp4util.U32(0), mp4util.U32(t.Timescale),
		mp4util.U32(uint32(t.Samples)), mp4util.U16(mp4util.Language("und")), mp4util.U16(0),
	))
	handler := map[string]string{"video": "vide", "audio": "soun"}[t.Media]
	hdlr := mp4util.FullBox("hdlr", 0, 0, mp4util.Cat(
		mp4util.U32(0), mp4util.FourCC(handler), mp4util.Zeros(12), []byte("MediaHandler\x00"),
	))
	dinf := mp4util.Box("dinf", mp4util.FullBox("dref", 0, 0, mp4util.U32(1),
		mp4util.FullBox("url ", 0, 1)))
	var entry []byte
	switch {
	case t.Media == "video" && t.Codec == "h264":
		entry = visualSampleEntry("avc1", t.Width, t.Height, avcC())
	case t.Media == "video":
		entry = visualSampleEntry("hvc1", t.Width, t.Height, hvcC())
	case t.Codec == "aac":
		entry = audioSampleEntry("mp4a", t.Channels, 16, t.Timescale, esds())
	default:
		entry = audioSampleEntry("alac", t.Channels, 16, t.Timescale, alacCookie(t.Channels, t.Timescale))
	}
	stsd := mp4util.FullBox("stsd", 0, 0, mp4util.U32(1), entry)
	stts := mp4util.FullBox("stts", 0, 0, mp4util.U32(1),
		mp4util.U32(uint32(t.Samples)), mp4util.U32(1))
	cttsDelta := uint32(0)
	if t.Media == "video" {
		cttsDelta = 2
	}
	ctts := mp4util.FullBox("ctts", 0, 0, mp4util.U32(uint32(t.Samples)),
		bytesRepeat(mp4util.Cat(mp4util.U32(1), mp4util.U32(cttsDelta)), t.Samples))
	stsc := mp4util.FullBox("stsc", 0, 0, mp4util.U32(1),
		mp4util.U32(1), mp4util.U32(1), mp4util.U32(1))
	stsz := mp4util.FullBox("stsz", 0, 0, mp4util.U32(0), mp4util.U32(uint32(len(sizes))),
		u32s(sizes))
	co64 := mp4util.FullBox("co64", 0, 0, mp4util.U32(uint32(len(offsets))), u64s(offsets))
	stblParts := [][]byte{stsd, stts, ctts, stsc, stsz, co64}
	if t.Media == "video" {
		var sync []uint32
		for s := 0; s < t.Samples; s++ {
			if s%12 == 0 {
				sync = append(sync, uint32(s+1))
			}
		}
		stblParts = append(stblParts,
			mp4util.FullBox("stss", 0, 0, mp4util.U32(uint32(len(sync))), u32s(sync)))
	}
	var mediaHeader []byte
	if t.Media == "audio" {
		mediaHeader = mp4util.FullBox("smhd", 0, 0, mp4util.U16(0), mp4util.U16(0))
	} else {
		mediaHeader = mp4util.FullBox("vmhd", 0, 1,
			mp4util.U16(0), mp4util.U16(0), mp4util.U16(0), mp4util.U16(0))
	}
	minf := mp4util.Box("minf", mediaHeader, dinf, mp4util.Box("stbl", stblParts...))
	return mp4util.Box("trak", tkhd, mp4util.Box("mdia", mdhd, hdlr, minf))
}

func bytesRepeat(b []byte, n int) []byte {
	out := make([]byte, 0, len(b)*n)
	for i := 0; i < n; i++ {
		out = append(out, b...)
	}
	return out
}

func u32s(v []uint32) []byte {
	out := make([]byte, 0, len(v)*4)
	for _, x := range v {
		out = append(out, mp4util.U32(x)...)
	}
	return out
}

func u64s(v []uint64) []byte {
	out := make([]byte, 0, len(v)*8)
	for _, x := range v {
		out = append(out, mp4util.U64(x)...)
	}
	return out
}

// hvcC builds a plausible HEVC decoder configuration record.
func hvcC() []byte {
	return mp4util.Box("hvcC",
		mp4util.Cat(mp4util.U8(1), mp4util.U8(1), mp4util.U32(0x60000000), mp4util.U32(0),
			mp4util.Zeros(6), mp4util.U8(0x00), mp4util.U16(0xf000), mp4util.Zeros(2),
			mp4util.U8(0xfc), mp4util.U8(0xfc), mp4util.U8(0xfc), mp4util.U8(0xf8),
			mp4util.U16(0), mp4util.U8(0x0f), mp4util.U8(0)),
	)
}
