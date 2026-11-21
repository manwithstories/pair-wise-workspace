// Package mp4test builds small, valid MP4 fixtures for tests.  It lives in
// the package's test binary so production code never carries fixture logic.
package mp4

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

// Box is a minimal writer for test fixtures.
type Box struct {
	Type     string
	Payload  []byte
	Children []*Box
}

// NewBox starts a container or leaf box.
func NewBox(typ string) *Box { return &Box{Type: typ} }

// Add appends a child.
func (b *Box) Add(child *Box) *Box {
	b.Children = append(b.Children, child)
	return b
}

// Bytes renders the box with correct nested sizes.
func (b *Box) Bytes() []byte {
	body := b.Payload
	for _, c := range b.Children {
		body = append(body, c.Bytes()...)
	}
	out := make([]byte, 8, len(body)+8)
	binary.BigEndian.PutUint32(out, uint32(len(body)+8))
	copy(out[4:], b.Type)
	return append(out, body...)
}

// Full prepends a version and flags word.
func Full(version uint8, flags uint32, body []byte) []byte {
	out := make([]byte, 4, len(body)+4)
	out[0] = version
	out[1] = byte(flags >> 16)
	out[2] = byte(flags >> 8)
	out[3] = byte(flags)
	return append(out, body...)
}

// MarshalBox serializes a go-mp4 box payload.
func MarshalBox(box mp4.IBox) []byte {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, box, mp4.Context{}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Video is the fixture's video track shape.
type Video struct {
	TrackID   uint32
	Timescale uint32
	Width     uint16
	Height    uint16
	Deltas    []uint32
	Keyframes []uint32
	Color     *mp4.Colr
}

// Audio is the fixture's audio track shape.
type Audio struct {
	TrackID    uint32
	Timescale  uint32
	Channels   uint16
	SampleRate uint32
	Deltas     []uint32
}

// Options configures the fixture.
type Options struct {
	Video *Video
	Audio *Audio
	// Extra appends an unknown top-level box with the given type and bytes.
	ExtraType string
	ExtraData []byte
	// Mdat appends an mdat box with the given bytes.
	Mdat []byte
}

// Build returns a complete MP4 with one video track, one audio track, and an
// optional unknown box.
func Build(opt Options) []byte {
	ftyp := NewBox("ftyp")
	ftyp.Payload = []byte{'i', 's', 'o', '6', 0, 0, 2, 0, 'i', 's', 'o', '6', 'm', 'p', '4', '2'}

	var traks []*Box
	if opt.Video != nil {
		traks = append(traks, buildVideo(opt.Video))
	}
	if opt.Audio != nil {
		traks = append(traks, buildAudio(opt.Audio))
	}

	mvhd := &mp4.Mvhd{Timescale: 90000, DurationV0: 90000, Rate: 0x10000, Volume: 0x100,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, NextTrackID: 3}
	mvhd.SetVersion(0)
	moov := NewBox("moov").Add(NewBox("mvhd"))
	moov.Children[0].Payload = MarshalBox(mvhd)
	for _, t := range traks {
		moov.Add(t)
	}

	out := append(ftyp.Bytes(), moov.Bytes()...)
	if len(opt.Mdat) > 0 {
		mdat := NewBox("mdat")
		mdat.Payload = opt.Mdat
		out = append(out, mdat.Bytes()...)
	}
	if opt.ExtraType != "" {
		extra := NewBox(opt.ExtraType)
		extra.Payload = opt.ExtraData
		out = append(out, extra.Bytes()...)
	}
	return out
}

func buildVideo(v *Video) *Box {
	width := uint32(v.Width) << 16
	height := uint32(v.Height) << 16
	tkhd := &mp4.Tkhd{TrackID: v.TrackID, DurationV0: 90000,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
		Width:  width, Height: height}
	tkhd.SetVersion(0)
	tkhd.SetFlags(3)
	mdhd := &mp4.Mdhd{Timescale: v.Timescale, DurationV0: sum(v.Deltas), Language: [3]byte{'u', 'n', 'd'}}
	mdhd.SetVersion(0)
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "VideoHandler"}
	vmhd := &mp4.Vmhd{}
	vmhd.SetFlags(1)
	stts := &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(len(v.Deltas)), SampleDelta: v.Deltas[0]}}}
	sizes := make([]uint32, len(v.Deltas))
	for i := range sizes {
		sizes[i] = 4
	}
	stsz := &mp4.Stsz{SampleCount: uint32(len(v.Deltas)), EntrySize: sizes}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}
	stco := &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{100}}
	stss := &mp4.Stss{EntryCount: uint32(len(v.Keyframes)), SampleNumber: v.Keyframes}

	stsd := NewBox("stsd")
	stsd.Payload = Full(0, 0, u32(1))
	entry := NewBox("avc1")
	entry.Payload = visualEntry(v.Width, v.Height)
	if v.Color != nil {
		colr := NewBox("colr")
		colr.Payload = MarshalBox(v.Color)
		entry.Add(colr)
	}
	stsd.Add(entry)

	stbl := NewBox("stbl").
		Add(stsd).
		Add(payloadBox("stts", MarshalBox(stts))).
		Add(payloadBox("stsc", MarshalBox(stsc))).
		Add(payloadBox("stsz", MarshalBox(stsz))).
		Add(payloadBox("stco", MarshalBox(stco))).
		Add(payloadBox("stss", MarshalBox(stss)))
	minf := NewBox("minf").Add(payloadBox("vmhd", MarshalBox(vmhd))).Add(stbl)
	mdia := NewBox("mdia").
		Add(payloadBox("mdhd", MarshalBox(mdhd))).
		Add(payloadBox("hdlr", MarshalBox(hdlr))).
		Add(minf)
	return NewBox("trak").
		Add(payloadBox("tkhd", MarshalBox(tkhd))).
		Add(mdia)
}

func buildAudio(a *Audio) *Box {
	tkhd := &mp4.Tkhd{TrackID: a.TrackID, DurationV0: 90000,
		Matrix: [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}, Volume: 0x100}
	tkhd.SetVersion(0)
	tkhd.SetFlags(3)
	mdhd := &mp4.Mdhd{Timescale: a.Timescale, DurationV0: sum(a.Deltas), Language: [3]byte{'u', 'n', 'd'}}
	mdhd.SetVersion(0)
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'}, Name: "SoundHandler"}
	smhd := &mp4.Smhd{}
	stts := &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: uint32(len(a.Deltas)), SampleDelta: a.Deltas[0]}}}
	sizes := make([]uint32, len(a.Deltas))
	for i := range sizes {
		sizes[i] = 8
	}
	stsz := &mp4.Stsz{SampleCount: uint32(len(a.Deltas)), EntrySize: sizes}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}
	stco := &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{200}}

	stsd := NewBox("stsd")
	stsd.Payload = Full(0, 0, u32(1))
	entry := NewBox("mp4a")
	entry.Payload = audioEntry(a.Channels, a.SampleRate)
	stsd.Add(entry)

	stbl := NewBox("stbl").
		Add(stsd).
		Add(payloadBox("stts", MarshalBox(stts))).
		Add(payloadBox("stsc", MarshalBox(stsc))).
		Add(payloadBox("stsz", MarshalBox(stsz))).
		Add(payloadBox("stco", MarshalBox(stco)))
	minf := NewBox("minf").Add(payloadBox("smhd", MarshalBox(smhd))).Add(stbl)
	mdia := NewBox("mdia").
		Add(payloadBox("mdhd", MarshalBox(mdhd))).
		Add(payloadBox("hdlr", MarshalBox(hdlr))).
		Add(minf)
	return NewBox("trak").
		Add(payloadBox("tkhd", MarshalBox(tkhd))).
		Add(mdia)
}

func payloadBox(typ string, payload []byte) *Box {
	b := NewBox(typ)
	b.Payload = payload
	return b
}

func visualEntry(width, height uint16) []byte {
	out := make([]byte, 78)
	binary.BigEndian.PutUint16(out[6:], 1) // data reference index
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

func u32(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

func sum(deltas []uint32) uint32 {
	var total uint32
	for _, d := range deltas {
		total += d
	}
	return total
}

// DefaultVideo returns a ten-frame 30 fps video track.
func DefaultVideo() *Video {
	deltas := make([]uint32, 10)
	for i := range deltas {
		deltas[i] = 3000
	}
	return &Video{TrackID: 1, Timescale: 90000, Width: 1920, Height: 1080,
		Deltas: deltas, Keyframes: []uint32{1, 6},
		Color: &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'}, ColourPrimaries: 1,
			TransferCharacteristics: 1, MatrixCoefficients: 1}}
}

// DefaultAudio returns a five-frame 48 kHz audio track.
func DefaultAudio() *Audio {
	deltas := make([]uint32, 5)
	for i := range deltas {
		deltas[i] = 1024
	}
	return &Audio{TrackID: 2, Timescale: 48000, Channels: 2, SampleRate: 48000, Deltas: deltas}
}
