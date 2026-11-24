package fixture

import "encoding/binary"

func (b *builder) buildMoov() []byte { return b.buildMoovAt(0) }

func (b *builder) buildMoovAt(mdatPayloadStart int64) []byte {
	var children [][]byte
	children = append(children, b.mvhd())
	for _, lay := range b.layout {
		children = append(children, b.trak(lay, mdatPayloadStart))
	}
	return containerBox("moov", children...)
}

func (b *builder) mvhd() []byte {
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 0) // creation
	p = binary.BigEndian.AppendUint32(p, 0) // modification
	p = binary.BigEndian.AppendUint32(p, uint32(TickNum))
	p = binary.BigEndian.AppendUint32(p, uint32(TotalTicks))
	p = binary.BigEndian.AppendUint32(p, 0x00010000) // rate
	p = binary.BigEndian.AppendUint16(p, 0x0100)     // volume
	p = append(p, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)      // reserved
	matrix := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for _, v := range matrix {
		p = binary.BigEndian.AppendUint32(p, v)
	}
	p = append(p, make([]byte, 24)...) // predefined
	p = binary.BigEndian.AppendUint32(p, uint32(len(b.layout)+1))
	return fullbox("mvhd", 0, 0, p)
}

func (b *builder) trak(lay trackLayout, mdatPayloadStart int64) []byte {
	tkhd := b.tkhd(lay)
	mdhd := b.mdhd(lay)
	hdlr := b.hdlr(lay)
	mediaHeader := ifBytes(lay.spec.Kind == "video", b.vmhd(), b.smhd())
	stbl := b.stbl(lay, mdatPayloadStart)
	minf := containerBox("minf", mediaHeader, b.dinf(), stbl)
	mdia := containerBox("mdia", mdhd, hdlr, minf)
	return containerBox("trak", tkhd, mdia)
}

func ifBytes(cond bool, a, b []byte) []byte {
	if cond {
		return a
	}
	return b
}

func (b *builder) tkhd(lay trackLayout) []byte {
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, lay.spec.ID)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, uint32(TotalTicks))
	p = append(p, make([]byte, 8)...)
	var layer, group int16
	volume := int16(0)
	if lay.spec.Kind == "audio" {
		volume = 0x0100
	}
	p = binary.BigEndian.AppendUint16(p, uint16(layer))
	p = binary.BigEndian.AppendUint16(p, uint16(group))
	p = binary.BigEndian.AppendUint16(p, uint16(volume))
	p = binary.BigEndian.AppendUint16(p, 0)
	matrix := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for _, v := range matrix {
		p = binary.BigEndian.AppendUint32(p, v)
	}
	w, h := uint32(0), uint32(0)
	if lay.spec.Kind == "video" {
		w, h = 160<<16, 90<<16
	}
	p = binary.BigEndian.AppendUint32(p, w)
	p = binary.BigEndian.AppendUint32(p, h)
	return fullbox("tkhd", 0, 7, p)
}

func (b *builder) mdhd(lay trackLayout) []byte {
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, lay.spec.Timescale)
	var dur uint64
	if lay.spec.Kind == "video" {
		dur = uint64(lay.spec.Count) * uint64(lay.spec.Delta)
	} else {
		dur = uint64(lay.spec.Count) * uint64(lay.spec.Delta)
	}
	if lay.spec.Timescale == 90000 {
		p = binary.BigEndian.AppendUint32(p, uint32(dur))
	} else {
		p = binary.BigEndian.AppendUint32(p, uint32(dur))
	}
	p = binary.BigEndian.AppendUint16(p, 0x55c4) // und
	p = binary.BigEndian.AppendUint16(p, 0)
	return fullbox("mdhd", 0, 0, p)
}

func (b *builder) hdlr(lay trackLayout) []byte {
	var p []byte
	p = binary.BigEndian.AppendUint32(p, 0)
	handler := "vide"
	if lay.spec.Kind == "audio" {
		handler = "soun"
	}
	p = append(p, []byte(handler)...)
	p = append(p, make([]byte, 12)...)
	p = append(p, 0) // empty name
	return fullbox("hdlr", 0, 0, p)
}

func (b *builder) vmhd() []byte {
	p := make([]byte, 8)
	p[4], p[5] = 0, 0
	return fullbox("vmhd", 0, 1, p)
}

func (b *builder) smhd() []byte {
	p := make([]byte, 4)
	return fullbox("smhd", 0, 0, p)
}

func (b *builder) dinf() []byte {
	url := fullbox("url ", 0, 1, nil)
	dref := fullbox("dref", 0, 0, binary.BigEndian.AppendUint32(nil, 1))
	dref = append(dref, url...)
	return containerBox("dinf", dref)
}

func (b *builder) stsd(lay trackLayout) []byte {
	var entry []byte
	if lay.spec.Kind == "video" {
		entry = box("avc1", videoSampleEntry())
	} else {
		entry = box("mp4a", audioSampleEntry())
	}
	p := binary.BigEndian.AppendUint32(nil, 1)
	return fullbox("stsd", 0, 0, append(p, entry...))
}

// videoSampleEntry is a 78-byte VisualSampleEntry payload (86 with header).
func videoSampleEntry() []byte {
	p := make([]byte, 0, 78)
	p = append(p, make([]byte, 6)...)       // reserved
	p = binary.BigEndian.AppendUint16(p, 1) // data ref index
	p = binary.BigEndian.AppendUint16(p, 0) // predefined
	p = binary.BigEndian.AppendUint16(p, 0) // reserved
	p = append(p, make([]byte, 12)...)      // predefined2
	p = binary.BigEndian.AppendUint16(p, 160)
	p = binary.BigEndian.AppendUint16(p, 90)
	p = binary.BigEndian.AppendUint32(p, 0x00480000)
	p = binary.BigEndian.AppendUint32(p, 0x00480000)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint16(p, 1) // frame count
	compressor := make([]byte, 32)
	compressor[0] = 4
	copy(compressor[1:], "meds")
	p = append(p, compressor...)
	p = binary.BigEndian.AppendUint16(p, 0x0018)
	p = binary.BigEndian.AppendUint16(p, 0xffff)
	return p // 78 bytes
}

// audioSampleEntry is a 28-byte AudioSampleEntry payload (36 with header).
func audioSampleEntry() []byte {
	p := make([]byte, 0, 28)
	p = append(p, make([]byte, 6)...)
	p = binary.BigEndian.AppendUint16(p, 1)
	p = append(p, make([]byte, 8)...) // version/revision/vendor
	p = binary.BigEndian.AppendUint16(p, 2)
	p = binary.BigEndian.AppendUint16(p, 16)
	p = binary.BigEndian.AppendUint16(p, 0)
	p = binary.BigEndian.AppendUint16(p, 0)
	p = binary.BigEndian.AppendUint32(p, 48000<<16)
	return p // 28 bytes
}
