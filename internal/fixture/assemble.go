package fixture

import (
	"bytes"
	"os"

	mp4 "github.com/abema/go-mp4"
)

func stsdBox(handler string) []byte {
	var b builder
	b.fullbox(0, 0)
	b.u32(1) // entry count
	if handler == "vide" {
		b.raw(videoSampleEntry())
	} else {
		b.raw(audioSampleEntry())
	}
	return box("stsd", b.Bytes())
}

// minimal but structurally valid sample entry; codec config is irrelevant to
// the replay pipeline which treats sample bytes opaquely.
func videoSampleEntry() []byte {
	var b builder
	b.fourcc("avc1")
	b.raw(make([]byte, 6))  // reserved
	b.u16(1)                // data_reference_index
	b.u16(0)                // pre_defined
	b.u16(0)                // reserved
	b.raw(make([]byte, 12)) // pre_defined[3]
	b.u16(1920)             // width
	b.u16(1080)             // height
	b.u32(0x00480000)       // horiz/vert resolution 72dpi
	b.u32(0x00480000)
	b.u32(0) // reserved
	b.u16(1) // frame count
	var name [32]byte
	b.raw(name[:])
	b.u16(0x0018) // depth
	b.i16(-1)     // pre_defined
	return box("avc1", b.Bytes())
}

func audioSampleEntry() []byte {
	var b builder
	b.fourcc("mp4a")
	b.raw(make([]byte, 6))
	b.u16(1) // data_reference_index
	b.raw(make([]byte, 8))
	b.u16(2)  // channels
	b.u16(16) // sample size
	b.u16(0)  // pre_defined
	b.u16(0)  // reserved
	b.u32(48000 << 16)
	return box("mp4a", b.Bytes())
}

func genericMediaHeader(handler string) []byte {
	if handler == "vide" {
		return box("vmhd", []byte{0, 0, 0, 1, 0, 0, 0, 0})
	}
	return box("smhd", []byte{0, 0, 0, 0, 0, 0, 0, 0})
}

func dinfBox() []byte {
	url := box("url ", []byte{0, 0, 0, 1}) // self-contained flag
	dref := box("dref", append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, url...))
	return container("dinf", dref)
}

// elstBox writes an empty edit (media_time -1) plus a media_time offset edit.
// Segment durations are in the movie timescale passed in.
func elstBox(movieTS, mediaTS uint32, mediaDur uint64) ([]byte, error) {
	emptyDur := uint32(movieTS / 2)       // 500 ms empty edit
	offset := int64(mediaTS)              // 1 second media_time offset
	mediaSeg := mediaDur - uint64(offset) // media-timescale units
	e := &mp4.Elst{
		EntryCount: 2,
		Entries: []mp4.ElstEntry{
			{SegmentDurationV1: uint64(emptyDur), MediaTimeV1: -1,
				MediaRateInteger: 1},
			{SegmentDurationV1: scaleToMovie(mediaSeg, mediaTS, movieTS),
				MediaTimeV1:      offset,
				MediaRateInteger: 1},
		},
	}
	e.SetVersion(1)
	return leaf("elst", e)
}

func scaleToMovie(v uint64, fromTS, toTS uint32) uint64 {
	// rescale a value expressed in fromTS units into toTS units
	return v * uint64(toTS) / uint64(fromTS)
}

// buildTrak serializes one full trak box given computed chunk offsets.
func buildTrak(sp *trackSpec, useCo64, cttsV0, withElst bool,
	movieTS uint32, mediaDur uint64) ([]byte, error) {
	stts, err := sttsRunBox(sp.deltas)
	if err != nil {
		return nil, err
	}
	stblKids := [][]byte{
		stsdBox(sp.handler), stts,
	}
	if sp.handler == "vide" && len(sp.ctts) > 0 {
		var c []byte
		var err error
		if cttsV0 {
			c, err = cttsBoxV0(sp.ctts)
		} else {
			c, err = cttsBoxV1(sp.ctts)
		}
		if err != nil {
			return nil, err
		}
		stblKids = append(stblKids, c)
	}
	if sp.handler == "vide" {
		s, err := stssBox(sp.sync)
		if err != nil {
			return nil, err
		}
		stblKids = append(stblKids, s)
	}
	stsc, err := stscBox(sp.perChunk)
	if err != nil {
		return nil, err
	}
	stsz, err := stszBox(sp.sizes)
	if err != nil {
		return nil, err
	}
	var offBox []byte
	if useCo64 {
		offBox, err = co64Box(sp.offsets)
	} else {
		offBox, err = stcoBox(sp.offsets)
	}
	if err != nil {
		return nil, err
	}
	stblKids = append(stblKids, stsc, stsz, offBox)
	stbl := container("stbl", stblKids...)
	minf := container("minf", genericMediaHeader(sp.handler), dinfBox(), stbl)
	mdhd, err := mdhdBox(sp.ts, mediaDur)
	if err != nil {
		return nil, err
	}
	hdlr, err := hdlrBox(sp.handler)
	if err != nil {
		return nil, err
	}
	mdia := container("mdia", mdhd, hdlr, minf)
	tkhd, err := tkhdBox(sp.trackID, scaleToMovie(mediaDur, sp.ts, movieTS),
		sp.handler == "vide")
	if err != nil {
		return nil, err
	}
	kids := [][]byte{tkhd, mdia}
	if withElst && sp.handler == "vide" {
		el, err := elstBox(movieTS, sp.ts, mediaDur)
		if err != nil {
			return nil, err
		}
		kids = append([][]byte{tkhd, container("edts", el)}, mdia)
	}
	return container("trak", kids...), nil
}

// writeFile assembles ftyp, optional moov, mdat and optional trailing moov.
func writeFile(path string, ftyp, moov, mdat []byte, moovAtEnd bool) error {
	var f bytes.Buffer
	f.Write(ftyp)
	if !moovAtEnd {
		f.Write(moov)
	}
	f.Write(mdat)
	if moovAtEnd {
		f.Write(moov)
	}
	return os.WriteFile(path, f.Bytes(), 0o644)
}
