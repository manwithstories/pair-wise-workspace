package fixture

import (
	"encoding/binary"
)

func buildMvhd(tracks []MP4Track) []byte {
	next := uint32(1)
	for _, t := range tracks {
		if t.ID >= next {
			next = t.ID + 1
		}
	}
	var b []byte
	b = append(b, u32(0)...)                           // creation
	b = append(b, u32(0)...)                           // modification
	b = append(b, u32(1000)...)                        // timescale
	b = append(b, u32(0)...)                           // duration
	b = append(b, u32(0x00010000)...)                  // rate
	b = append(b, u16(0x0100)...)                      // volume
	b = append(b, u16(0)...)                           // reserved
	b = append(b, concat([][]byte{u32(0), u32(0)})...) // reserved
	b = append(b, matrix()...)                         // matrix
	b = append(b, make([]byte, 24)...)                 // pre_defined
	b = append(b, u32(next)...)                        // next_track_ID
	return fullBox("mvhd", 0, 0, b)
}

func buildMvex(tracks []MP4Track) []byte {
	var trex [][]byte
	for _, t := range tracks {
		var b []byte
		b = append(b, u32(t.ID)...)
		b = append(b, u32(1)...) // default_sample_description_index
		b = append(b, u32(0)...) // default_sample_duration
		b = append(b, u32(0)...) // default_sample_size
		b = append(b, u32(0)...) // default_sample_flags
		trex = append(trex, fullBox("trex", 0, 0, b))
	}
	return box("mvex", concat(trex))
}

func buildTrak(t MP4Track) []byte {
	var tkhd []byte
	tkhd = append(tkhd, concat([][]byte{u32(0), u32(0)})...) // creation, modification
	tkhd = append(tkhd, u32(t.ID)...)
	tkhd = append(tkhd, u32(0)...) // reserved
	tkhd = append(tkhd, u32(0)...) // duration
	tkhd = append(tkhd, concat([][]byte{u32(0), u32(0)})...)
	tkhd = append(tkhd, u16(0)...) // layer
	tkhd = append(tkhd, u16(0)...) // alternate_group
	vol := uint16(0)
	if t.Handler == "soun" {
		vol = 0x0100
	}
	tkhd = append(tkhd, u16(vol)...)
	tkhd = append(tkhd, u16(0)...)
	tkhd = append(tkhd, matrix()...)
	tkhd = append(tkhd, u32(uint32(t.Width)<<16)...)
	tkhd = append(tkhd, u32(uint32(t.Height)<<16)...)

	var mdhd []byte
	mdhd = append(mdhd, concat([][]byte{u32(0), u32(0)})...)
	mdhd = append(mdhd, u32(t.Timescale)...)
	mdhd = append(mdhd, u32(0)...)      // duration
	mdhd = append(mdhd, u16(0x55C4)...) // language 'und'
	mdhd = append(mdhd, u16(0)...)

	var hdlr []byte
	hdlr = append(hdlr, u32(0)...)
	hdlr = append(hdlr, []byte(t.Handler)...)
	hdlr = append(hdlr, make([]byte, 12)...)
	name := "Media"
	if t.Handler == "vide" {
		name = "VideoHandler"
	} else {
		name = "SoundHandler"
	}
	hdlr = append(hdlr, []byte(name)...)
	hdlr = append(hdlr, 0)

	var media []byte
	var header []byte
	if t.Handler == "vide" {
		header = fullBox("vmhd", 0, 1, concat([][]byte{u16(0), u16(0), u16(0), u16(0)}))
	} else {
		header = fullBox("smhd", 0, 0, concat([][]byte{u16(0), u16(0)}))
	}
	media = append(media, header...)
	media = append(media, box("dinf", fullBox("dref", 0, 0,
		concat([][]byte{u32(1), fullBox("url ", 0, 1, nil)})))...)
	media = append(media, box("stbl", fullBox("stsd", 0, 0, concat([][]byte{u32(1), sampleEntry(t)})))...)
	minf := box("minf", media)

	mdia := box("mdia", concat([][]byte{fullBox("mdhd", 0, 0, mdhd), fullBox("hdlr", 0, 0, hdlr), minf}))
	return box("trak", concat([][]byte{fullBox("tkhd", 0, 7, tkhd), mdia}))
}

func sampleEntry(t MP4Track) []byte {
	entry := make([]byte, 6)
	entry = append(entry, u16(1)...) // data_reference_index
	if t.Handler == "vide" {
		entry = append(entry, concat([][]byte{u16(0), u16(0)})...) // pre_defined, reserved
		entry = append(entry, make([]byte, 12)...)                 // pre_defined2
		entry = append(entry, u16(t.Width)...)
		entry = append(entry, u16(t.Height)...)
		entry = append(entry, concat([][]byte{u32(0x00480000), u32(0x00480000)})...)
		entry = append(entry, u32(0)...)
		entry = append(entry, u16(1)...) // frame_count
		compressor := make([]byte, 32)
		compressor[0] = 4
		copy(compressor[1:], "Test")
		entry = append(entry, compressor...)
		entry = append(entry, u16(0x0018)...)
		entry = append(entry, u16(0xFFFF)...)
		children := avcC()
		if t.Primaries != 0 {
			children = append(children, colr(t.Primaries, t.Transfer, t.Matrix, t.FullRange)...)
		}
		return box("avc1", concat([][]byte{entry, children}))
	}
	entry = append(entry, concat([][]byte{u16(0), u16(0)})...) // version, revision
	entry = append(entry, u32(0)...)                           // vendor
	entry = append(entry, u16(t.Channels)...)
	entry = append(entry, u16(16)...)
	entry = append(entry, concat([][]byte{u16(0), u16(0)})...)
	entry = append(entry, u32(t.SampleRate<<16)...)
	return box("mp4a", concat([][]byte{entry, esds(t.ObjectType, t.ChanConfig, t.SampleRate)}))
}

func avcC() []byte {
	b := []byte{1, 0x64, 0x00, 0x1f, 0xFC | 3, 0xE0 | 1}
	b = append(b, u16(3)...)
	b = append(b, 0x67, 0x64, 0x00) // fake SPS
	b = append(b, 1)                // num PPS
	b = append(b, u16(2)...)
	b = append(b, 0x68, 0xEE) // fake PPS
	return box("avcC", b)
}

func colr(primaries, transfer, matrix uint16, full bool) []byte {
	b := []byte("nclx")
	b = append(b, u16(primaries)...)
	b = append(b, u16(transfer)...)
	b = append(b, u16(matrix)...)
	if full {
		b = append(b, 0x80)
	} else {
		b = append(b, 0x00)
	}
	return box("colr", b)
}

func esds(objectType, chanConfig uint8, rate uint32) []byte {
	asc := audioSpecificConfig(objectType, chanConfig, rate)
	dsi := descriptor(0x05, asc)
	dc := []byte{objectType, 0x15, 0, 0, 0} // OTI, streamType audio
	dc = append(dc, u32(0)...)              // bufferSizeDB
	dc = append(dc, u32(128000)...)         // maxBitrate
	dc = append(dc, u32(96000)...)          // avgBitrate
	dc = append(dc, dsi...)
	dcd := descriptor(0x04, dc)
	es := []byte{0x00, 0x01, 0x00} // ES_ID, flags
	es = append(es, dcd...)
	esd := descriptor(0x03, es)
	return fullBox("esds", 0, 0, esd)
}

func descriptor(tag byte, payload []byte) []byte {
	out := []byte{tag}
	// Single-byte length is sufficient for the generated definitions.
	out = append(out, byte(len(payload)))
	return append(out, payload...)
}

func audioSpecificConfig(objectType, chanConfig uint8, rate uint32) []byte {
	sfi := byte(0x0F)
	for i, r := range []uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000} {
		if r == rate {
			sfi = byte(i)
			break
		}
	}
	// AOT(5) SFI(4) ChanCfg(4), padded with three zero bits.
	v := uint32(objectType&0x1F)<<11 | uint32(sfi&0x0F)<<7 | uint32(chanConfig&0x0F)<<3
	return []byte{byte(v >> 8), byte(v)}
}

func buildMoofWithOffset(f MP4Fragment, seq uint32, dataOffset int32) ([]byte, int32) {
	var trunEntries []byte
	var trunFlags uint32 = 0x000001 | 0x000100 | 0x000200 | 0x000400
	var count uint32
	for _, s := range f.Samples {
		flags := uint32(0)
		if !s.Sync {
			flags = 0x00010000
		}
		trunEntries = append(trunEntries, u32(s.Duration)...)
		trunEntries = append(trunEntries, u32(uint32(len(s.Data)))...)
		trunEntries = append(trunEntries, u32(flags)...)
		count++
	}
	var trun []byte
	trun = append(trun, u32(count)...)
	trun = append(trun, u32(uint32(dataOffset))...)
	trun = append(trun, trunEntries...)

	var tfhd []byte
	tfhd = append(tfhd, u32(f.TrackID)...)

	var tfdt []byte
	tfdt = append(tfdt, u64(f.BaseDecodeTime)...)

	traf := box("traf", concat([][]byte{
		fullBox("tfhd", 0, 0x020000, tfhd),
		fullBox("tfdt", 1, 0, tfdt),
		fullBox("trun", 0, trunFlags, trun),
	}))
	moof := box("moof", concat([][]byte{fullBox("mfhd", 0, 0, u32(seq)), traf}))
	return moof, dataOffset
}

func concatSamples(samples []MP4Sample) []byte {
	var out []byte
	for _, s := range samples {
		out = append(out, s.Data...)
	}
	return out
}

func matrix() []byte {
	var b []byte
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		b = append(b, u32(v)...)
	}
	return b
}

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:], uint32(8+len(payload)))
	copy(out[4:8], typ)
	return append(out, payload...)
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	out[0] = version
	out[1] = byte(flags >> 16)
	out[2] = byte(flags >> 8)
	out[3] = byte(flags)
	return box(typ, append(out, payload...))
}

func concat(parts [][]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// SampleData returns deterministic pseudo-random bytes for a sample.
func SampleData(track uint32, seq, index int) []byte {
	n := 64 + int((track*37+uint32(seq*11)+uint32(index*7))%97)
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((int(track)*31 + seq*17 + index*13 + i*7) & 0xff)
	}
	return out
}
