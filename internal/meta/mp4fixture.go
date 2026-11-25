package meta

import (
	"encoding/binary"
)

// MP4Cases is the catalog of deterministic MP4 fixture variants.
var MP4Cases = []string{
	"ok", "illegal-rotation", "bad-crop", "unknown-color",
	"color-conflict", "truncated-mp4", "bad-length", "oversized-sample",
}

func boxBytes(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fullBox(typ string, version byte, payload []byte) []byte {
	return boxBytes(typ, append([]byte{version, 0, 0, 0}, payload...))
}

func ftypBox() []byte {
	p := make([]byte, 16)
	copy(p[0:4], "isom")
	binary.BigEndian.PutUint32(p[4:8], 0x200)
	copy(p[8:12], "isom")
	copy(p[12:16], "mp42")
	return boxBytes("ftyp", p)
}

var matrixIdentity = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
var matrixRotate90 = [9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}
var matrixRotate45 = [9]int32{46341, 46341, 0, -46341, 46341, 0, 0, 0, 0x40000000}

func tkhdBox(trackID, duration uint32, w, h uint32, matrix [9]int32) []byte {
	p := make([]byte, 80)
	binary.BigEndian.PutUint32(p[8:12], trackID)   // track_ID
	binary.BigEndian.PutUint32(p[16:20], duration) // duration
	// reserved 8 bytes at 20:28 already zero; layer/alt/volume/reserved 28:36 zero
	for i, v := range matrix {
		binary.BigEndian.PutUint32(p[36+4*i:40+4*i], uint32(v))
	}
	binary.BigEndian.PutUint32(p[72:76], w<<16)
	binary.BigEndian.PutUint32(p[76:80], h<<16)
	return fullBox("tkhd", 0, p)
}

func mdhdBox(timescale, duration uint32) []byte {
	p := make([]byte, 20)
	binary.BigEndian.PutUint32(p[8:12], timescale)
	binary.BigEndian.PutUint32(p[12:16], duration)
	// language + pre_defined remain zero
	return fullBox("mdhd", 0, p)
}

func colrBoxNCLX(prim, trc, mat uint16, fullRange bool) []byte {
	p := make([]byte, 11)
	copy(p[0:4], "nclx")
	binary.BigEndian.PutUint16(p[4:6], prim)
	binary.BigEndian.PutUint16(p[6:8], trc)
	binary.BigEndian.PutUint16(p[8:10], mat)
	if fullRange {
		p[10] = 0x80
	}
	return boxBytes("colr", p)
}

func clapBox(wN, wD, hN, hD uint32) []byte {
	p := make([]byte, 32)
	put := func(at int, n, d uint32) {
		binary.BigEndian.PutUint32(p[at:at+4], n)
		binary.BigEndian.PutUint32(p[at+4:at+8], d)
	}
	put(0, wN, wD)
	put(8, hN, hD)
	put(16, 0, 1)
	put(24, 0, 1)
	return boxBytes("clap", p)
}

func stszOversizedBox() []byte {
	// After FullBox header: sample_size=0, sample_count=1, one huge entry.
	rest := make([]byte, 12)
	binary.BigEndian.PutUint32(rest[0:4], 0)
	binary.BigEndian.PutUint32(rest[4:8], 1)
	binary.BigEndian.PutUint32(rest[8:12], uint32(MaxTrackBuffer)+1)
	return fullBox("stsz", 0, rest)
}

type mp4Spec struct {
	trackID, timescale, duration uint32
	w, h                         uint32
	matrix                       [9]int32
	trackColor                   []byte // raw box
	containerColor               []byte // raw moov-level box before trak
	clap                         []byte
	stsz                         []byte
}

func buildMP4(s mp4Spec) []byte {
	if s.trackID == 0 {
		s.trackID = 1
	}
	if s.timescale == 0 {
		s.timescale = 48000
	}
	if s.duration == 0 {
		s.duration = 192000 // 4 seconds
	}
	if s.w == 0 {
		s.w, s.h = 1920, 1080
	}
	if s.matrix == [9]int32{} {
		s.matrix = matrixIdentity
	}

	stblPayload := []byte{}
	if s.clap != nil {
		stblPayload = append(stblPayload, s.clap...)
	}
	if s.trackColor != nil {
		stblPayload = append(stblPayload, s.trackColor...)
	}
	if s.stsz != nil {
		stblPayload = append(stblPayload, s.stsz...)
	}
	stbl := boxBytes("stbl", stblPayload)
	minf := boxBytes("minf", stbl)
	mdia := boxBytes("mdia", append(mdhdBox(s.timescale, s.duration), minf...))
	trakPayload := append(tkhdBox(s.trackID, s.duration, s.w, s.h, s.matrix), mdia...)
	trak := boxBytes("trak", trakPayload)

	moovPayload := []byte{}
	if s.containerColor != nil {
		moovPayload = append(moovPayload, s.containerColor...)
	}
	moovPayload = append(moovPayload, trak...)
	moov := boxBytes("moov", moovPayload)

	out := append(ftypBox(), moov...)
	return out
}

// BuildMP4Case returns a deterministic MP4 byte slice for the named case.
func BuildMP4Case(name string) []byte {
	bt709 := colrBoxNCLX(1, 1, 1, false)
	bt2020 := colrBoxNCLX(9, 16, 9, true)

	switch name {
	case "illegal-rotation":
		return buildMP4(mp4Spec{matrix: matrixRotate45, trackColor: bt709})
	case "bad-crop":
		return buildMP4(mp4Spec{
			matrix: matrixRotate90, trackColor: bt709,
			clap: clapBox(5000, 1, 1080, 1), // wider than the 1920 frame
		})
	case "unknown-color":
		return buildMP4(mp4Spec{trackColor: colrBoxNCLX(60000, 60001, 60002, false)})
	case "color-conflict":
		return buildMP4(mp4Spec{trackColor: bt2020, containerColor: bt709})
	case "oversized-sample":
		return buildMP4(mp4Spec{trackColor: bt709, stsz: stszOversizedBox()})
	case "bad-length":
		// ftyp then a box whose declared size (4) is below its 8-byte header.
		bad := []byte{0, 0, 0, 4, 'f', 'r', 'e', 'e'}
		return append(ftypBox(), bad...)
	case "truncated-mp4":
		good := buildMP4(mp4Spec{trackColor: bt709})
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr[0:4], 1<<20) // claims 1 MiB
		copy(hdr[4:8], "mdat")
		return append(append(good, hdr...), 0, 1, 2, 3) // only 4 payload bytes
	default: // "ok"
		return buildMP4(mp4Spec{
			matrix: matrixRotate90, trackColor: bt2020, containerColor: bt709,
			clap: clapBox(1920, 1, 1080, 1),
		})
	}
}
