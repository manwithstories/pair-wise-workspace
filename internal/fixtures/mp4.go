package fixtures

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

// mp4Options configures the synthetic MP4 written for one rate.
type mp4Options struct {
	FrameCount int
	Delta      uint32
	Keyframes  []uint32
	Payloads   map[int][]byte
	Width      uint16
	Height     uint16
	Channels   uint16
	SampleRate uint32
}

// buildMP4 writes a minimal, standards-shaped MP4: ftyp, moov with a video
// track and an audio track, then mdat.  Chunk offsets are computed after the
// ftyp and moov sizes are known, so the file parses cleanly and round-trips.
func buildMP4(opt mp4Options) []byte {
	videoDeltas := make([]uint32, opt.FrameCount)
	audioDeltas := make([]uint32, opt.FrameCount*3)
	for i := range videoDeltas {
		videoDeltas[i] = opt.Delta
	}
	for i := range audioDeltas {
		audioDeltas[i] = 1000
	}

	ftyp := payloadBox("ftyp", []byte{'i', 's', 'o', '6', 0, 0, 2, 0, 'i', 's', 'o', '6', 'm', 'p', '4', '2'})
	mdatLen := 0
	for n := 1; n <= opt.FrameCount; n++ {
		mdatLen += len(opt.Payloads[n])
	}
	audioSampleLen := 360
	mdatLen += len(audioDeltas) * audioSampleLen

	// Offsets depend on the moov size, which depends on offsets.  Integer
	// sizes are stable, so compute with a placeholder then patch.
	videoOffsets := make([]uint32, opt.FrameCount)
	audioOffsets := make([]uint32, len(audioDeltas))
	moov := buildMoov(opt, videoDeltas, audioDeltas, videoOffsets, audioOffsets, 0)
	base := uint32(len(ftyp.bytes()) + len(moov.bytes()) + 8)
	cursor := base
	for n := 1; n <= opt.FrameCount; n++ {
		videoOffsets[n-1] = cursor
		cursor += uint32(len(opt.Payloads[n]))
	}
	for i := range audioOffsets {
		audioOffsets[i] = cursor
		cursor += uint32(audioSampleLen)
	}
	moov = buildMoov(opt, videoDeltas, audioDeltas, videoOffsets, audioOffsets, 0)

	mdat := payloadBox("mdat", make([]byte, mdatLen))
	out := append(ftyp.bytes(), moov.bytes()...)
	out = append(out, mdat.bytes()...)
	return out
}

func buildMoov(opt mp4Options, videoDeltas, audioDeltas []uint32,
	videoOffsets, audioOffsets []uint32, _ uint32) *box {
	mvhd := &mp4.Mvhd{Timescale: 90000, DurationV0: uint32(opt.FrameCount) * opt.Delta,
		Rate: 0x10000, Volume: 0x100,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, NextTrackID: 3}
	mvhd.SetVersion(0)

	moov := newBox("moov").add(payloadBox("mvhd", marshal(mvhd)))
	moov.add(buildVideoTrak(opt, videoDeltas, videoOffsets))
	moov.add(buildAudioTrak(opt, audioDeltas, audioOffsets))
	return moov
}

func buildVideoTrak(opt mp4Options, deltas []uint32, offsets []uint32) *box {
	tkhd := &mp4.Tkhd{TrackID: 1, DurationV0: uint32(opt.FrameCount) * opt.Delta,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
		Width:  uint32(opt.Width) << 16, Height: uint32(opt.Height) << 16}
	tkhd.SetVersion(0)
	tkhd.SetFlags(3)
	mdhd := &mp4.Mdhd{Timescale: 90000, DurationV0: uint32(opt.FrameCount) * opt.Delta, Language: [3]byte{'u', 'n', 'd'}}
	mdhd.SetVersion(0)
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "VideoHandler"}
	vmhd := &mp4.Vmhd{}
	vmhd.SetFlags(1)

	stsd := newBox("stsd")
	stsd.payload = fullBox(0, 0, u32(1))
	entry := payloadBox("avc1", visualEntry(opt.Width, opt.Height))
	colr := &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'}, ColourPrimaries: 1,
		TransferCharacteristics: 1, MatrixCoefficients: 1}
	entry.add(payloadBox("colr", marshal(colr)))
	stsd.add(entry)

	stts := &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(len(deltas)), SampleDelta: deltas[0]}}}
	sizes := make([]uint32, len(deltas))
	for n := range sizes {
		sizes[n] = uint32(len(opt.Payloads[n+1]))
	}
	stsz := &mp4.Stsz{SampleCount: uint32(len(deltas)), EntrySize: sizes}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}
	stco := &mp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets}
	stss := &mp4.Stss{EntryCount: uint32(len(opt.Keyframes)), SampleNumber: opt.Keyframes}

	stbl := newBox("stbl").add(stsd).
		add(payloadBox("stts", marshal(stts))).
		add(payloadBox("stsc", marshal(stsc))).
		add(payloadBox("stsz", marshal(stsz))).
		add(payloadBox("stco", marshal(stco))).
		add(payloadBox("stss", marshal(stss)))
	minf := newBox("minf").add(payloadBox("vmhd", marshal(vmhd))).add(stbl)
	mdia := newBox("mdia").
		add(payloadBox("mdhd", marshal(mdhd))).
		add(payloadBox("hdlr", marshal(hdlr))).
		add(minf)
	return newBox("trak").add(payloadBox("tkhd", marshal(tkhd))).add(mdia)
}

func buildAudioTrak(opt mp4Options, deltas []uint32, offsets []uint32) *box {
	rate := opt.SampleRate
	if rate == 0 {
		rate = 48000
	}
	channels := opt.Channels
	if channels == 0 {
		channels = 2
	}
	duration := uint32(0)
	for _, d := range deltas {
		duration += d
	}
	tkhd := &mp4.Tkhd{TrackID: 2, DurationV0: duration,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, Volume: 0x100}
	tkhd.SetVersion(0)
	tkhd.SetFlags(3)
	mdhd := &mp4.Mdhd{Timescale: rate, DurationV0: duration, Language: [3]byte{'u', 'n', 'd'}}
	mdhd.SetVersion(0)
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'}, Name: "SoundHandler"}
	smhd := &mp4.Smhd{}

	stsd := newBox("stsd")
	stsd.payload = fullBox(0, 0, u32(1))
	stsd.add(payloadBox("mp4a", audioEntry(channels, rate)))

	stts := &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(len(deltas)), SampleDelta: deltas[0]}}}
	sizes := make([]uint32, len(deltas))
	for i := range sizes {
		sizes[i] = 360
	}
	stsz := &mp4.Stsz{SampleCount: uint32(len(deltas)), EntrySize: sizes}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}
	stco := &mp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets}

	stbl := newBox("stbl").add(stsd).
		add(payloadBox("stts", marshal(stts))).
		add(payloadBox("stsc", marshal(stsc))).
		add(payloadBox("stsz", marshal(stsz))).
		add(payloadBox("stco", marshal(stco)))
	minf := newBox("minf").add(payloadBox("smhd", marshal(smhd))).add(stbl)
	mdia := newBox("mdia").
		add(payloadBox("mdhd", marshal(mdhd))).
		add(payloadBox("hdlr", marshal(hdlr))).
		add(minf)
	return newBox("trak").add(payloadBox("tkhd", marshal(tkhd))).add(mdia)
}

// box is a minimal writer used to assemble fixture files.
type box struct {
	typ      string
	payload  []byte
	children []*box
}

func newBox(typ string) *box { return &box{typ: typ} }

func (b *box) add(child *box) *box {
	b.children = append(b.children, child)
	return b
}

func (b *box) bytes() []byte {
	body := append([]byte(nil), b.payload...)
	for _, c := range b.children {
		body = append(body, c.bytes()...)
	}
	out := make([]byte, 8, len(body)+8)
	binary.BigEndian.PutUint32(out, uint32(len(body)+8))
	copy(out[4:], b.typ)
	return append(out, body...)
}

func payloadBox(typ string, payload []byte) *box {
	b := newBox(typ)
	b.payload = payload
	return b
}

func rawBox(typ string, payload []byte) []byte {
	return payloadBox(typ, payload).bytes()
}

func marshal(b mp4.IBox) []byte {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func fullBox(version uint8, flags uint32, body []byte) []byte {
	out := make([]byte, 4, len(body)+4)
	out[0] = version
	out[1] = byte(flags >> 16)
	out[2] = byte(flags >> 8)
	out[3] = byte(flags)
	return append(out, body...)
}

func u32(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

func visualEntry(width, height uint16) []byte {
	out := make([]byte, 78)
	binary.BigEndian.PutUint16(out[6:], 1)
	binary.BigEndian.PutUint16(out[24:], width)
	binary.BigEndian.PutUint16(out[26:], height)
	binary.BigEndian.PutUint32(out[28:], 0x00480000)
	binary.BigEndian.PutUint32(out[32:], 0x00480000)
	binary.BigEndian.PutUint16(out[40:], 1)
	binary.BigEndian.PutUint16(out[74:], 0x0018)
	binary.BigEndian.PutUint16(out[76:], 0xffff)
	return out
}

func audioEntry(channels uint16, rate uint32) []byte {
	out := make([]byte, 28)
	binary.BigEndian.PutUint16(out[6:], 1)
	binary.BigEndian.PutUint16(out[16:], channels)
	binary.BigEndian.PutUint16(out[18:], 16)
	binary.BigEndian.PutUint32(out[24:], rate<<16)
	return out
}
