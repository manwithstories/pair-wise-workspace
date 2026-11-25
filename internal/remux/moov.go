package remux

import (
	"encoding/binary"

	"archivecheck/internal/demux"
)

// atom helpers for fixed header blocks.
func u16(v uint16) []byte { b := make([]byte, 2); putU16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); putU32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// buildMoov assembles mvhd plus one trak per input track. Offsets may be nil
// during the size-probe pass; the values only affect leaf payload length.
func buildMoov(tracks []TrackInput, offs [][]int64, sizes [][]uint32, mdatPayloadStart int64) boxBuilder {
	moov := newBox("moov")
	moov.add(buildMvhd(tracks))
	for ti, tr := range tracks {
		var trackOffs []int64
		if offs != nil {
			trackOffs = offs[ti]
		}
		var trackSizes []uint32
		if sizes != nil {
			trackSizes = sizes[ti]
		}
		moov.add(buildTrak(tr, ti+1, trackOffs, trackSizes))
	}
	return *moov
}

func buildMvhd(tracks []TrackInput) boxBuilder {
	body := make([]byte, 96)
	putU32(body[0:4], 0)          // version + flags
	putU32(body[12:16], 1000)     // timescale: milliseconds
	body[15] = 0x10               // rate = 1.0 fixed point
	body[23] = 0x01               // volume = 1.0 fixed point
	putU16At(body[32:34], 0x0001) // identity matrix
	putU32At(body[36:40], 0x10000)
	putU16At(body[52:54], 0x0001)
	putU32At(body[56:60], 0x10000)
	putU16At(body[72:74], 0x4000)
	putU32At(body[76:80], 0x40000000)
	putU32At(body[88:92], uint32(len(tracks)))
	return *newBox("mvhd").setRaw(body)
}

func putU16At(b []byte, v uint16) { putU16(b, v) }
func putU32At(b []byte, v uint32) { putU32(b, v) }

func buildTrak(tr TrackInput, trakIndex int, offs []int64, sizes []uint32) boxBuilder {
	tb := tr.Meta.TimeBase.Num
	tkhd := buildTkhd(tr.Meta, trakIndex)
	mdia := newBox("mdia").
		add(buildMdhd(tb, tr.Samples)).
		add(buildHdlr(tr.Meta.Handler)).
		add(buildMinf(tr, offs, sizes))
	return *newBox("trak").add(tkhd).add(*mdia)
}

func buildTkhd(meta demux.TrackMeta, index int) boxBuilder {
	body := make([]byte, 84)
	putU32(body[0:4], 0x000007) // flags: track_enabled+in_movie+in_preview
	putU32(body[12:16], uint32(index))
	putU32(body[20:24], 1000)
	body[31] = 0x10 // rate
	body[39] = 0x01 // volume for sound
	putU16At(body[48:50], 0x0001)
	putU32At(body[52:56], 0x10000)
	putU16At(body[68:70], 0x0001)
	putU32At(body[72:76], 0x10000)
	putU16At(body[88:90], 0x4000)
	if meta.Handler == "vide" {
		putU32At(body[80:84], 0x40000000)
		putU32At(body[84:88], 320<<16)
		putU32At(body[88:92], 240<<16)
	}
	return *newBox("tkhd").setRaw(body)
}

func buildMdhd(timescale int64, samples []demux.Sample) boxBuilder {
	body := make([]byte, 24)
	putU32(body[0:4], 0)
	putU32(body[12:16], uint32(timescale))
	var dur uint64
	for _, s := range samples {
		dur += uint64(s.Duration)
	}
	putU32(body[16:20], uint32(dur))
	return *newBox("mdhd").setRaw(body)
}

func buildHdlr(handler string) boxBuilder {
	body := make([]byte, 29)
	putU32(body[0:4], 0)
	copy(body[8:12], pad4(handler))
	body[28] = 0 // null-terminated name
	return *newBox("hdlr").setRaw(body)
}

func pad4(s string) []byte {
	b := []byte(s)
	for len(b) < 4 {
		b = append(b, 0)
	}
	return b[:4]
}

func buildMinf(tr TrackInput, offs []int64, sizes []uint32) boxBuilder {
	var content boxBuilder
	switch tr.Meta.Handler {
	case "vide":
		content = *newBox("minf").add(buildVMHD()).add(buildDinf()).add(buildStbl(tr, offs, sizes))
	case "soun":
		content = *newBox("minf").add(buildSMHD()).add(buildDinf()).add(buildStbl(tr, offs, sizes))
	default:
		content = *newBox("minf").add(buildNMHD()).add(buildDinf()).add(buildStbl(tr, offs, sizes))
	}
	return content
}

func buildVMHD() boxBuilder {
	return *newBox("vmhd").setRaw(append(fullHeader(0, 1), make([]byte, 8)...))
}
func buildSMHD() boxBuilder { return *newBox("smhd").setRaw(make([]byte, 8)) }
func buildNMHD() boxBuilder { return *newBox("nmhd").setRaw(make([]byte, 4)) }

func buildDinf() boxBuilder {
	dref := newBox("dref").
		setHeader(fullHeader(0, 0)).
		setRaw(append(u32(1), buildUrlEntry()...))
	return *newBox("dinf").add(*dref)
}

func buildUrlEntry() []byte {
	b := make([]byte, 12)
	copy(b[4:8], "url ")
	putU32(b[8:12], 1) // self-contained flag
	return b
}

func buildStbl(tr TrackInput, offs []int64, sizes []uint32) boxBuilder {
	stbl := newBox("stbl").
		add(buildStsd(tr.Meta)).
		add(buildStts(tr.Samples)).
		add(buildStsc(len(sizes))).
		add(u64Box("co64", offs)).
		add(buildStsz(sizes))
	if tr.Meta.Handler == "vide" {
		stbl.add(buildStss(tr.Samples))
	}
	return *stbl
}

func buildStsd(meta demux.TrackMeta) boxBuilder {
	entry := buildSampleEntry(meta)
	body := append(fullHeader(0, 0), u32(1)...)
	stsd := newBox("stsd").setHeader(body).add(entry)
	return *stsd
}

// buildSampleEntry writes the minimal codec-specific sample entry. Payloads
// are descriptive only; the coded samples themselves are never re-encoded.
func buildSampleEntry(meta demux.TrackMeta) boxBuilder {
	var body []byte
	switch meta.Codec {
	case demux.CodecAVC1:
		body = make([]byte, 78)
		putU16(body[0:2], 0)
		putU16(body[2:4], 0)
		putU16(body[4:6], 0)
		putU16(body[6:8], 1) // reserved data refs
		putU16(body[24:26], 320)
		putU16(body[26:28], 240)
		putU32(body[28:32], 0x00480000)
		putU32(body[32:36], 0x00480000)
		putU32(body[40:44], 0x00010000)
		putU16(body[44:46], 24)
		body[47] = 0xFF
		putU16(body[48:50], 0x0018)
	case demux.CodecMP4A:
		body = make([]byte, 28)
		putU16(body[0:2], 0)
		putU16(body[2:4], 0)
		putU16(body[4:6], 1)
		putU32(body[12:16], 48000<<16)
		putU16(body[20:22], 2)
	default: // text
		body = make([]byte, 37)
		putU16(body[4:6], 1)
	}
	return *newBox(meta.Codec).setRaw(body)
}

func buildStts(samples []demux.Sample) boxBuilder {
	// One run per distinct (count,delta) keeps the table compact and exact.
	type run struct{ count, delta uint32 }
	var runs []run
	for _, s := range samples {
		if n := len(runs); n > 0 && runs[n-1].delta == uint32(s.Duration) {
			runs[n-1].count++
		} else {
			runs = append(runs, run{1, uint32(s.Duration)})
		}
	}
	body := make([]byte, 8+8*len(runs))
	putU32(body[4:8], uint32(len(runs)))
	for i, r := range runs {
		putU32(body[8+8*i:], r.count)
		putU32(body[12+8*i:], r.delta)
	}
	return *newBox("stts").setHeader(body[:4]).setRaw(body[4:])
}

func buildStsc(n int) boxBuilder {
	body := make([]byte, 20)
	putU32(body[4:8], 1)
	putU32(body[8:12], 1) // first chunk
	putU32(body[12:16], uint32(n))
	putU32(body[16:20], 1) // sample description index
	return *newBox("stsc").setHeader(body[:4]).setRaw(body[4:])
}

func buildStsz(sizes []uint32) boxBuilder {
	body := make([]byte, 12+4*len(sizes))
	putU32(body[4:8], 0) // sample_size 0 => table present
	putU32(body[8:12], uint32(len(sizes)))
	for i, s := range sizes {
		putU32(body[12+4*i:], s)
	}
	return *newBox("stsz").setHeader(body[:4]).setRaw(body[4:])
}

func buildStss(samples []demux.Sample) boxBuilder {
	var sync []uint32
	for i, s := range samples {
		if s.Keyframe {
			sync = append(sync, uint32(i+1)) // 1-based
		}
	}
	return u32Box("stss", sync)
}
