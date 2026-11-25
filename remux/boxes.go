package remux

import (
	"bytes"
	"encoding/binary"
)

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func ftypBox() []byte {
	var p bytes.Buffer
	p.WriteString("iso5")
	p.Write(u32(0x200))
	p.WriteString("iso5")
	p.WriteString("iso6")
	p.WriteString("dash")
	p.WriteString("msdh")
	return box("ftyp", p.Bytes())
}

func moovBox(tracks []trackInfo) []byte {
	var p bytes.Buffer
	p.Write(mvhdBox(uint32(len(tracks) + 1)))
	for _, tr := range tracks {
		p.Write(trakBox(tr))
	}
	// Movie extends declares fragment defaults.
	var mx bytes.Buffer
	mx.Write(box("mehd", fullPayload(1, 0, u64(0))))
	for _, tr := range tracks {
		var th bytes.Buffer
		th.Write(u32(tr.id))
		th.Write(u32(1)) // default_sample_description_index
		mx.Write(fullBox("trex", 0, 0, th.Bytes()))
	}
	p.Write(box("mvex", mx.Bytes()))
	return box("moov", p.Bytes())
}

func fullPayload(version byte, flags uint32, payload []byte) []byte {
	return append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, payload...)
}

func mvhdBox(nextTrackID uint32) []byte {
	var p bytes.Buffer
	p.Write(u32(0)) // creation
	p.Write(u32(0)) // modification
	p.Write(u32(90000))
	p.Write(u32(0)) // duration updated by readers if needed
	p.Write(u32(0x00010000))
	p.Write(u16(0x0100))
	p.Write(make([]byte, 10))
	matrix := []byte{
		0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0,
		0, 0, 0, 0, 0x00, 0x01, 0x00, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0,
		0x40, 0x00, 0x00, 0x00, 0, 0, 0, 0,
		0, 0, 0, 0,
	}
	p.Write(matrix)
	p.Write(make([]byte, 24)) // pre-defined
	p.Write(u32(nextTrackID))
	return fullBox("mvhd", 0, 0, p.Bytes())
}

type trackInfo struct {
	id        uint32
	kind      string
	codec     string
	timescale uint32
}

func trakBox(tr trackInfo) []byte {
	var tk bytes.Buffer
	tk.Write(u32(0))
	tk.Write(u32(0))
	tk.Write(u32(tr.id))
	tk.Write(u32(0))
	tk.Write(u32(0))
	tk.Write(make([]byte, 8))
	tk.Write(u16(0)) // layer
	tk.Write(u16(0)) // alt group
	tk.Write(u16(0x0100))
	tk.Write(u16(0))
	tk.Write(make([]byte, 36)) // matrix
	tk.Write(u32(640 << 16))   // width 16.16
	tk.Write(u32(360 << 16))
	tkhd := fullBox("tkhd", 0, 0x000007, tk.Bytes())

	var md bytes.Buffer
	md.Write(mdhdBox(tr.timescale))
	md.Write(hdlrBox(tr.kind))
	md.Write(minfBox(tr))
	mdia := box("mdia", md.Bytes())

	return box("trak", append(tkhd, mdia...))
}

func mdhdBox(scale uint32) []byte {
	var p bytes.Buffer
	p.Write(u32(0))
	p.Write(u32(0))
	p.Write(u32(scale))
	p.Write(u32(0))
	p.Write(u16(0x55c4)) // und language
	p.Write(u16(0))
	return fullBox("mdhd", 0, 0, p.Bytes())
}

func hdlrBox(kind string) []byte {
	var p bytes.Buffer
	p.Write(u32(0))
	if kind == "soun" {
		p.WriteString("soun")
	} else {
		p.WriteString("vide")
	}
	p.Write(make([]byte, 12))
	p.WriteString("FieldPost Handler\x00")
	return fullBox("hdlr", 0, 0, p.Bytes())
}

func minfBox(tr trackInfo) []byte {
	var p bytes.Buffer
	if tr.kind == "soun" {
		p.Write(fullBox("smhd", 0, 0, append(u16(0), u16(0)...)))
	} else {
		p.Write(fullBox("vmhd", 1, 0, u16(0)))
	}
	var dinf bytes.Buffer
	url := fullBox("url ", 0, 0x000001, nil)
	drefPayload := append(u32(1), url...)
	dinf.Write(fullBox("dref", 0, 0, drefPayload))
	p.Write(box("dinf", dinf.Bytes()))
	p.Write(stblBox(tr))
	return box("minf", p.Bytes())
}

func stblBox(tr trackInfo) []byte {
	var p bytes.Buffer
	// Minimal stsd with one synthetic sample entry.
	var stsd bytes.Buffer
	stsd.Write(u32(1))
	codec := "avc1"
	if tr.kind == "soun" {
		codec = "mp4a"
	}
	var entry bytes.Buffer
	entry.Write(make([]byte, 6))
	entry.Write(u16(1)) // data ref index
	entry.Write(make([]byte, 16))
	if tr.kind == "vide" {
		entry.Write(u16(640))
		entry.Write(u16(360))
		entry.Write(u32(0x00480000))
		entry.Write(u32(0x00480000))
		entry.Write(make([]byte, 4))
		entry.Write(u16(1))
		entry.Write(make([]byte, 32))
		entry.Write(u16(0x0018))
		entry.Write(u16(0xffff))
	} else {
		entry.Write(u16(2))
		entry.Write(u16(16))
		entry.Write(u16(0))
		entry.Write(u16(0))
		entry.Write(u32(48000 << 16))
	}
	stsd.Write(box(codec, entry.Bytes()))
	p.Write(fullBox("stsd", 0, 0, stsd.Bytes()))
	p.Write(box("stts", fullPayload(0, 0, u32(0))))
	p.Write(box("stsc", fullPayload(0, 0, u32(0))))
	p.Write(box("stsz", fullPayload(0, 0, append(u32(0), u32(0)...))))
	p.Write(box("stco", fullPayload(0, 0, u32(0))))
	return box("stbl", p.Bytes())
}
