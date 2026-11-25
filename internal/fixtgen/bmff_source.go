package fixtgen

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
)

// codecUnsupported mirrors demux.CodecVC01; kept local so fixtgen has no
// runtime dependency on the demux package's constants.
const codecUnsupported = "vc01"

// box is a small local builder mirroring the on-disk ISO-BMFF layout expected
// by the demux parser. It is deliberately independent of remux.boxBuilder so a
// defect in one cannot silently corrupt the other's tests.
type box struct {
	typ      string
	header   []byte
	payload  []byte
	children []box
}

func b(typ string) *box { return &box{typ: typ} }
func (x *box) raw(p []byte) *box { x.payload = p; return x }
func (x *box) hdr(h []byte) *box { x.header = h; return x }
func (x *box) add(c box) *box { x.children = append(x.children, c); return x }

func (x box) len() int {
	n := 8 + len(x.header) + len(x.payload)
	for _, c := range x.children {
		n += c.len()
	}
	return n
}

func (x box) write(buf *bytes.Buffer) {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(x.len()))
	copy(hdr[4:], x.typ)
	buf.Write(hdr)
	buf.Write(x.header)
	buf.Write(x.payload)
	for _, c := range x.children {
		c.write(buf)
	}
}

func u32b(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u16b(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func full(v byte, flags uint32) []byte {
	return []byte{v, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

// WriteBMFFSource renders one segment as a plain (non-fragmented) MP4 with
// three tracks and the requested fault injected. It returns the file path.
func WriteBMFFSource(dir string, seg Segment) (string, error) {
	buf := &bytes.Buffer{}
	writeFtyp(buf)
	var trackBoxes []box
	var mdatOffsets [][]int64
	var mdatSizes [][]uint32
	mdat := bytes.NewBuffer(nil)

	moovLen := probeMoovLen()
	mdatStart := 20 + moovLen // ftyp(20) + moov + mdat-header(8)
	cursor := int64(mdatStart + 8)

	for tr, spec := range Tracks() {
		codec := spec.Codec
		if seg.Fault == FaultUnsupported && tr == 0 {
			codec = codecUnsupported
		}
		var offs []int64
		var sizes []uint32
		var keySamples []uint32
		for s := 0; s < SamplesPerTrack; s++ {
			data := makeSample(seg.Index, tr, s, codec)
			// AVCC length prefix for video; raw frame for others.
			frame := data
			if codec == "avc1" || codec == codecUnsupported {
				frame = data // already length-prefixed by makeSample
			}
			offs = append(offs, cursor)
			sizes = append(sizes, uint32(len(frame)))
			cursor += int64(len(frame))
			mdat.Write(frame)
			if spec.Handler == "vide" && s%KeyEvery == 0 {
				keySamples = append(keySamples, uint32(s+1))
			}
		}
		mdatOffsets = append(mdatOffsets, offs)
		mdatSizes = append(mdatSizes, sizes)
		trackBoxes = append(trackBoxes, buildTrak(spec, codec, offs, sizes, keySamples, seg.Fault))
	}

	moov := b("moov")
	moov.add(buildMvhd())
	for _, t := range trackBoxes {
		moov.add(t)
	}
	moov.write(buf)

	// mdat
	mh := make([]byte, 8)
	binary.BigEndian.PutUint32(mh, uint32(8+mdat.Len()))
	copy(mh[4:], "mdat")
	buf.Write(mh)
	mdatBytes := mdat.Bytes()
	buf.Write(mdatBytes)

	out := buf.Bytes()
	out = applyFault(out, seg, int64(mdatStart))

	path := filepath.Join(dir, seg.ID+".mp4")
	if seg.Fault == FaultRTPTruncated || seg.Fault == FaultIncompleteFrag {
		path = filepath.Join(dir, seg.ID+".src")
	}
	return path, os.WriteFile(path, out, 0o644)
}

func writeFtyp(buf *bytes.Buffer) {
	payload := append([]byte("isom"), u32b(0x200)...)
	payload = append(payload, []byte("isom")...)
	payload = append(payload, []byte("iso2")...)
	payload = append(payload, []byte("avc1")...)
	b("ftyp").raw(payload).write(buf)
}

func probeMoovLen() int {
	moov := b("moov").add(buildMvhd())
	for _, spec := range Tracks() {
		moov.add(buildTrak(spec, spec.Codec,
			make([]int64, SamplesPerTrack), make([]uint32, SamplesPerTrack),
			[]uint32{1, 4, 7, 10}, FaultNone))
	}
	var probe bytes.Buffer
	moov.write(&probe)
	return probe.Len()
}

func buildMvhd() box {
	body := make([]byte, 96)
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[12:16], 1000)
	body[15] = 0x10
	binary.BigEndian.PutUint16(body[32:34], 1)
	binary.BigEndian.PutUint32(body[36:40], 0x10000)
	binary.BigEndian.PutUint16(body[52:54], 1)
	binary.BigEndian.PutUint32(body[56:60], 0x10000)
	binary.BigEndian.PutUint16(body[72:74], 0x4000)
	binary.BigEndian.PutUint32(body[76:80], 0x40000000)
	binary.BigEndian.PutUint32(body[88:92], TrackCount)
	return *b("mvhd").raw(body)
}

func buildTrak(spec TrackSpec, codec string, offs []int64, sizes []uint32, keys []uint32, fault Fault) box {
	timescale := uint32(90000)
	if spec.Handler == "soun" {
		timescale = 48000
	}
	mdhd := make([]byte, 24)
	copy(mdhd[0:4], full(0, 0))
	binary.BigEndian.PutUint32(mdhd[12:16], timescale)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(SamplesPerTrack)*1000)

	hdlr := make([]byte, 29)
	copy(hdlr[8:12], spec.Handler)

	stbl := b("stbl").
		add(buildStsd(codec, spec.Handler)).
		add(buildStts()).
		add(buildStsc()).
		add(buildCo64(offs)).
		add(buildStsz(sizes))
	if spec.Handler == "vide" {
		switch fault {
		case FaultMissingIndex:
			// no stss at all
		case FaultCorruptIndex:
			// stss claims sample 2 (a P-frame) is a sync sample
			stbl.add(*u32Leaf("stss", []uint32{2, 4, 7, 10}))
		default:
			stbl.add(*u32Leaf("stss", keys))
		}
	}
	minf := b("minf").add(buildMediaHeader(spec.Handler)).add(buildDinf()).add(*stbl)
	mdia := b("mdia").
		add(*b("mdhd").raw(mdhd)).
		add(*b("hdlr").raw(hdlr)).
		add(*minf)
	tkhd := buildTkhd(spec)
	return *b("trak").add(*tkhd).add(*mdia)
}

func buildTkhd(spec TrackSpec) *box {
	body := make([]byte, 84)
	copy(body[0:4], full(0, 7))
	binary.BigEndian.PutUint32(body[20:24], 1000)
	body[31] = 0x10
	if spec.Handler == "soun" {
		body[39] = 1
	}
	binary.BigEndian.PutUint16(body[48:50], 1)
	binary.BigEndian.PutUint32(body[52:56], 0x10000)
	binary.BigEndian.PutUint16(body[68:70], 1)
	binary.BigEndian.PutUint32(body[72:76], 0x10000)
	binary.BigEndian.PutUint16(body[88:90], 0x4000)
	if spec.Handler == "vide" {
		binary.BigEndian.PutUint32(body[80:84], 0x40000000)
		binary.BigEndian.PutUint32(body[84:88], 320<<16)
	}
	return b("tkhd").raw(body)
}

func buildMediaHeader(handler string) box {
	switch handler {
	case "vide":
		return *b("vmhd").raw(append(full(0, 1), make([]byte, 8)...))
	case "soun":
		return *b("smhd").raw(make([]byte, 8))
	default:
		return *b("nmhd").raw(make([]byte, 4))
	}
}

func buildDinf() box {
	url := make([]byte, 12)
	copy(url[4:8], "url ")
	binary.BigEndian.PutUint32(url[8:12], 1)
	dref := b("dref").hdr(full(0, 0)).raw(append(u32b(1), url...))
	return *b("dinf").add(*dref)
}

func buildStsd(codec, handler string) box {
	var entry []byte
	switch handler {
	case "vide":
		entry = make([]byte, 78)
		binary.BigEndian.PutUint16(entry[6:8], 1)
		binary.BigEndian.PutUint16(entry[24:26], 320)
		binary.BigEndian.PutUint16(entry[26:28], 240)
		binary.BigEndian.PutUint32(entry[28:32], 0x00480000)
		binary.BigEndian.PutUint32(entry[32:36], 0x00480000)
		binary.BigEndian.PutUint32(entry[40:44], 0x00010000)
		binary.BigEndian.PutUint16(entry[44:46], 24)
		entry[47] = 0xFF
		binary.BigEndian.PutUint16(entry[48:50], 0x0018)
	case "soun":
		entry = make([]byte, 28)
		binary.BigEndian.PutUint16(entry[4:6], 1)
		binary.BigEndian.PutUint32(entry[12:16], 48000<<16)
		binary.BigEndian.PutUint16(entry[20:22], 2)
	default:
		entry = make([]byte, 37)
		binary.BigEndian.PutUint16(entry[4:6], 1)
	}
	stsd := b("stsd").hdr(full(0, 0)).raw(u32b(1)).add(*b(codec).raw(entry))
	return *stsd
}

func buildStts() box {
	body := make([]byte, 16)
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], 1)
	binary.BigEndian.PutUint32(body[8:12], SamplesPerTrack)
	binary.BigEndian.PutUint32(body[12:16], 1000)
	return *b("stts").raw(body)
}

func buildStsc() box {
	body := make([]byte, 20)
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], 1)
	binary.BigEndian.PutUint32(body[8:12], 1)
	binary.BigEndian.PutUint32(body[12:16], SamplesPerTrack)
	binary.BigEndian.PutUint32(body[16:20], 1)
	return *b("stsc").raw(body)
}

func buildCo64(offs []int64) box {
	body := make([]byte, 8+8*len(offs))
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], uint32(len(offs)))
	for i, o := range offs {
		binary.BigEndian.PutUint64(body[8+8*i:], uint64(o))
	}
	return *b("co64").raw(body)
}

func buildStsz(sizes []uint32) box {
	body := make([]byte, 12+4*len(sizes))
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[8:12], uint32(len(sizes)))
	for i, s := range sizes {
		binary.BigEndian.PutUint32(body[12+4*i:], s)
	}
	return *b("stsz").raw(body)
}

func u32Leaf(typ string, vals []uint32) *box {
	body := make([]byte, 8+4*len(vals))
	copy(body[0:4], full(0, 0))
	binary.BigEndian.PutUint32(body[4:8], uint32(len(vals)))
	for i, v := range vals {
		binary.BigEndian.PutUint32(body[8+4*i:], v)
	}
	return b(typ).raw(body)
}
