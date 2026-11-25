package fixture

import (
	"encoding/binary"
	"fmt"

	"relay.local/relay/internal/model"
)

// ColrSpec is a colour description box value.
type ColrSpec struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
}

// SampleSpec is one sample to mux.
type SampleSpec struct {
	Payload  []byte
	Duration int64 // in the track timescale
	Sync     bool
}

// MP4TrackSpec describes one track to write.
type MP4TrackSpec struct {
	ID        uint32
	Kind      model.TrackKind
	FourCC    string
	Timescale uint32
	Width     int
	Height    int
	Rotation  int // 0/90/180/270, encoded in the tkhd matrix
	ColrBox   *ColrSpec
	// ColrSampleEntry places a second colr box inside the visual sample
	// entry, i.e. the bitstream description layer.
	ColrSampleEntry *ColrSpec
	// Aperture, when non-zero, adds a clap box to the sample entry.
	ApertureW int
	ApertureH int
	Samples   []SampleSpec
}

// MP4Spec is a complete file to write.
type MP4Spec struct {
	MajorBrand string
	Tracks     []MP4TrackSpec
}

// rotationMatrix encodes a clockwise rotation as a 3x3 fixed-point matrix.
func rotationMatrix(deg int) [9]int32 {
	const one = 1 << 16
	switch deg {
	case 90:
		return [9]int32{0, one, 0, -one, 0, 0, 0, 0, 1 << 30}
	case 180:
		return [9]int32{-one, 0, 0, 0, -one, 0, 0, 0, 1 << 30}
	case 270:
		return [9]int32{0, -one, 0, one, 0, 0, 0, 0, 1 << 30}
	}
	return [9]int32{one, 0, 0, 0, one, 0, 0, 0, 1 << 30}
}

func matrixBytes(m [9]int32) []byte {
	out := make([]byte, 36)
	for i, v := range m {
		binary.BigEndian.PutUint32(out[i*4:], uint32(v))
	}
	return out
}

// BuildMP4 muxes an ISO BMFF file. Offsets are resolved in a second pass so
// the chunk offset table is exact regardless of moov size.
func BuildMP4(spec MP4Spec) ([]byte, error) {
	if len(spec.Tracks) == 0 {
		return nil, fmt.Errorf("fixture: mp4 needs at least one track")
	}
	brand := spec.MajorBrand
	if brand == "" {
		brand = "isom"
	}
	ftyp := box("ftyp", []byte(brand), u32(0x200), []byte("isom"), []byte("iso2"), []byte("mjpg"))

	// Layout sample payloads back to back after ftyp+moov+mdat header.
	var essence []byte
	offsets := make([][]int64, len(spec.Tracks))
	// First pass assumes a chunk offset of 0; moov size is invariant to the
	// numeric value, so the second pass only replaces bytes.
	moov0, err := buildMoov(spec, nil, 0)
	if err != nil {
		return nil, err
	}
	base := int64(len(ftyp)+len(moov0)) + 8
	pos := base
	for ti, tr := range spec.Tracks {
		offsets[ti] = make([]int64, len(tr.Samples))
		for si, s := range tr.Samples {
			offsets[ti][si] = pos
			pos += int64(len(s.Payload))
			essence = append(essence, s.Payload...)
		}
	}
	moov, err := buildMoov(spec, offsets, base)
	if err != nil {
		return nil, err
	}
	if len(moov) != len(moov0) {
		return nil, fmt.Errorf("fixture: moov size changed between passes (%d vs %d)", len(moov0), len(moov))
	}
	mdat := box("mdat", essence)
	return cat(ftyp, moov, mdat), nil
}

func buildMoov(spec MP4Spec, offsets [][]int64, base int64) ([]byte, error) {
	var traks [][]byte
	maxMs := int64(0)
	for ti, tr := range spec.Tracks {
		trak, dur, err := buildTrak(tr, offsetsAt(offsets, ti), base)
		if err != nil {
			return nil, err
		}
		traks = append(traks, trak)
		if dur > maxMs {
			maxMs = dur
		}
	}
	mvhdDur := maxMs
	// Movie timescale is milliseconds; media durations are in track timescale.
	var mvhdParts [][]byte
	mvhdParts = append(mvhdParts, u32(0), u32(0), u32(1000), u32(uint32(clampU32(mvhdDur))),
		u32(0x00010000), u16(0x0100), u16(0), u32(0), u32(0),
		matrixBytes([9]int32{1 << 16, 0, 0, 0, 1 << 16, 0, 0, 0, 1 << 30}),
		make([]byte, 24), u32(uint32(len(spec.Tracks)+1)))
	mvhd := fullBox("mvhd", 0, 0, mvhdParts...)
	return box("moov", append([][]byte{mvhd}, traks...)...), nil
}

func offsetsAt(offsets [][]int64, ti int) []int64 {
	if offsets == nil {
		return nil
	}
	return offsets[ti]
}

func buildTrak(tr MP4TrackSpec, offs []int64, base int64) ([]byte, int64, error) {
	timescale := tr.Timescale
	if timescale == 0 {
		timescale = 1000
	}
	var mediaDur int64
	for _, s := range tr.Samples {
		mediaDur += s.Duration
	}
	// tkhd duration is expressed in the movie timescale (1000).
	msDur := float64(mediaDur) * 1000 / float64(timescale)
	if tr.Timescale == 0 {
		msDur = float64(mediaDur)
	}
	ms := int64(msDur)
	volume := uint16(0)
	if tr.Kind == model.Audio {
		volume = 0x0100
	}
	tkhdParts := [][]byte{
		u32(0), u32(0), u32(tr.ID), u32(0), u32(uint32(clampU32(ms))),
		u32(0), u32(0), u16(0), u16(0), u16(volume), u16(0),
		matrixBytes(rotationMatrix(tr.Rotation)),
		u32(uint32(tr.Width) << 16), u32(uint32(tr.Height) << 16),
	}
	tkhd := fullBox("tkhd", 0, 3, tkhdParts...)

	mdhdParts := [][]byte{
		u32(0), u32(0), u32(timescale), u32(uint32(clampU32(mediaDur))),
		u16(0x55c4), u16(0),
	}
	mdhd := fullBox("mdhd", 0, 0, mdhdParts...)

	handler := []byte("vide")
	if tr.Kind == model.Audio {
		handler = []byte("soun")
	} else if tr.Kind == model.Data {
		handler = []byte("meta")
	}
	hdlr := fullBox("hdlr", 0, 0, u32(0), handler, make([]byte, 12), []byte("RelayHandler\x00"))

	stbl, err := buildStbl(tr, offs)
	if err != nil {
		return nil, 0, err
	}
	var minf []byte
	if tr.Kind == model.Audio {
		minf = box("minf", fullBox("smhd", 0, 0, u16(0), u16(0)), box("dinf", drefBox()), stbl)
	} else {
		minf = box("minf", fullBox("vmhd", 0, 1, u16(0), u16(0), u16(0), u16(0)),
			box("dinf", drefBox()), stbl)
	}
	mdia := box("mdia", mdhd, hdlr, minf)
	parts := [][]byte{tkhd, mdia}
	if tr.ColrBox != nil {
		parts = append(parts, colrBox(*tr.ColrBox))
	}
	return box("trak", parts...), ms, nil
}

func drefBox() []byte {
	url := fullBox("url ", 0, 1)
	return fullBox("dref", 0, 0, u32(1), url)
}

func buildStbl(tr MP4TrackSpec, offs []int64) ([]byte, error) {
	stsd := fullBox("stsd", 0, 0, u32(1), sampleEntry(tr))
	// stts run-length encode durations.
	var sttsEnts bytesBuf
	count := 0
	prev := int64(-1)
	runs := 0
	var lastDelta int64
	flush := func() {
		if count > 0 {
			sttsEnts.u32(uint32(count))
			sttsEnts.u32(uint32(lastDelta))
			runs++
		}
	}
	for _, s := range tr.Samples {
		if s.Duration == prev {
			count++
			continue
		}
		flush()
		prev = s.Duration
		count = 1
		lastDelta = s.Duration
	}
	flush()
	stts := fullBox("stts", 0, 0, u32(uint32(runs)), sttsEnts.bytes())

	var sizes bytesBuf
	sizes.u32(0) // sample_size 0 means per-sample table
	sizes.u32(uint32(len(tr.Samples)))
	for _, s := range tr.Samples {
		sizes.u32(uint32(len(s.Payload)))
	}
	stsz := fullBox("stsz", 0, 0, sizes.bytes())

	var stco bytesBuf
	stco.u32(1)
	chunkOff := int64(0)
	if len(offs) > 0 {
		chunkOff = offs[0]
	}
	stco.u32(uint32(chunkOff))
	stcoBox := fullBox("stco", 0, 0, stco.bytes())

	stsc := fullBox("stsc", 0, 0, u32(1), u32(1), u32(uint32(len(tr.Samples))), u32(1))

	var parts [][]byte
	parts = append(parts, stsd, stts)
	// stss only when not every sample is a sync point.
	allSync := true
	for _, s := range tr.Samples {
		if !s.Sync {
			allSync = false
			break
		}
	}
	if !allSync {
		var stss bytesBuf
		n := 0
		for i, s := range tr.Samples {
			if s.Sync {
				n++
				stss.u32(uint32(i + 1))
			}
		}
		parts = append(parts, fullBox("stss", 0, 0, u32(uint32(n)), stss.bytes()))
	}
	parts = append(parts, stsz, stsc, stcoBox)
	return box("stbl", parts...), nil
}

func sampleEntry(tr MP4TrackSpec) []byte {
	fourcc := tr.FourCC
	if fourcc == "" {
		fourcc = "mjpg"
	}
	if tr.Kind == model.Audio {
		// AudioSampleEntry: 28 bytes fixed after header.
		body := cat(make([]byte, 6), u16(1), make([]byte, 8), u16(2), u16(16), u16(0), u16(0), u32(48000<<16))
		return box(fourcc, body)
	}
	compressor := make([]byte, 32)
	compressor[0] = 4
	copy(compressor[1:], "RLY\x00")
	entry := cat(
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), u32(0), u32(0), u32(0),
		u16(uint16(tr.Width)), u16(uint16(tr.Height)),
		u32(0x00480000), u32(0x00480000), u32(0),
		u16(1), compressor, u16(0x0018), u16(0xffff),
	)
	var children [][]byte
	if c := tr.ColrSampleEntry; c != nil {
		children = append(children, colrBox(*c))
	}
	if tr.ApertureW > 0 && tr.ApertureH > 0 {
		// clap aperture expressed as integers over denominator 1.
		children = append(children, box("clap",
			u32(uint32(tr.ApertureW)), u32(1),
			u32(uint32(tr.ApertureH)), u32(1),
			u32(0), u32(1), u32(0), u32(1)))
	}
	return box(fourcc, append([][]byte{entry}, children...)...)
}

func colrBox(c ColrSpec) []byte {
	full := uint8(0)
	if c.FullRange {
		full = 1
	}
	return box("colr", []byte("nclx"), u16(c.Primaries), u16(c.Transfer), u16(c.Matrix), u8(full), u8(0))
}

func clampU32(v int64) int64 {
	const max = int64(^uint32(0))
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// ColrContainerBox emits a track-level colr box (container description layer).
func ColrContainerBox(c ColrSpec) []byte { return colrBox(c) }

// bytesBuf accumulates big-endian integers.
type bytesBuf struct{ b []byte }

func (w *bytesBuf) u32(v uint32)  { w.b = append(w.b, u32(v)...) }
func (w *bytesBuf) bytes() []byte { return w.b }
