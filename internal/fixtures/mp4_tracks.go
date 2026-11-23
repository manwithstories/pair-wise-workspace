package fixtures

// audioFrameTicks is one AAC access unit at 48 kHz.
const audioFrameTicks = 1024

func makeVideoTrak(o Options, timescale, chunkOffset uint32) []byte {
	duration := uint64(o.VideoFrames) * uint64(o.FPSDen)
	tkhd := makeTkhd(o.VideoTrackID, duration*1000/uint64(timescale), rotationMatrix(o.Rotation), o.Width, o.Height, 0)
	mdhd := makeMdhd(timescale, duration)
	hdlr := makeHdlr("vide", "VideoHandler")
	vmhd := fullBox(nil, "vmhd", 0, 1, concat(be16(0), be16(0), be16(0), be16(0)))
	stbl := makeVideoStbl(o, timescale, chunkOffset)
	minf := box(nil, "minf", vmhd, makeDinf(), stbl)
	mdia := box(nil, "mdia", mdhd, hdlr, minf)
	return makeTrak(tkhd, mdia)
}

func makeVideoStbl(o Options, timescale, chunkOffset uint32) []byte {
	avc1 := makeAvc1(o)
	stsd := fullBox(nil, "stsd", 0, 0, be32(1), avc1)
	stts := fullBox(nil, "stts", 0, 0, be32(1), be32(uint32(o.VideoFrames)), be32(uint32(o.FPSDen)))
	stsc := fullBox(nil, "stsc", 0, 0, be32(1), be32(1), be32(uint32(o.VideoFrames)), be32(1))
	stsz := fullBox(nil, "stsz", 0, 0, concat(be32(0), be32(uint32(o.VideoFrames)), sizeList(o.Seed, o.VideoTrackID, o.VideoFrames, 6400, 600)))
	stco := fullBox(nil, "stco", 0, 0, be32(1), be32(chunkOffset))
	stss := fullBox(nil, "stss", 0, 0, be32(1), be32(1))
	return box(nil, "stbl", stsd, stts, stsc, stsz, stco, stss)
}

func makeAvc1(o Options) []byte {
	payload := []byte{}
	payload = append(payload, make([]byte, 6)...)  // reserved
	payload = append(payload, be16(1)...)          // data reference index
	payload = append(payload, be16(0)...)          // pre_defined
	payload = append(payload, be16(0)...)          // reserved
	payload = append(payload, make([]byte, 12)...) // pre_defined2
	payload = append(payload, be16(uint16(o.Width))...)
	payload = append(payload, be16(uint16(o.Height))...)
	payload = append(payload, be32(0x00480000)...) // 72 dpi
	payload = append(payload, be32(0x00480000)...)
	payload = append(payload, be32(0)...)
	payload = append(payload, be16(1)...) // frame count
	payload = append(payload, make([]byte, 32)...)
	payload = append(payload, be16(0x0018)...)
	payload = append(payload, be16(0xffff)...)

	children := [][]byte{makeAvcC(), makeColr(o), makeClap(o)}
	all := append([][]byte{payload}, children...)
	return box(nil, "avc1", all...)
}

func makeColr(o Options) []byte {
	if o.ColourPrimaries == 0 && o.ColourTransfer == 0 && o.ColourMatrix == 0 && !o.UnknownColour {
		return nil
	}
	p := []byte("nclx")
	if o.UnknownColour {
		// Codes well outside ISO/IEC 23001-8; must be preserved and flagged.
		p = append(p, be16(0xffff)...)
		p = append(p, be16(0xfffe)...)
		p = append(p, be16(0xfffd)...)
	} else {
		p = append(p, be16(o.ColourPrimaries)...)
		p = append(p, be16(o.ColourTransfer)...)
		p = append(p, be16(o.ColourMatrix)...)
	}
	p = append(p, 0x80) // full range
	return box(nil, "colr", p)
}

func makeClap(o Options) []byte {
	if o.CropPixels == 0 && o.ClapCleanW == 0 && o.ClapCleanH == 0 {
		return nil
	}
	cleanW := o.Width - 2*o.CropPixels
	cleanH := o.Height - 2*o.CropPixels
	if o.ClapCleanW != 0 {
		cleanW = o.ClapCleanW
	}
	if o.ClapCleanH != 0 {
		cleanH = o.ClapCleanH
	}
	vals := []int32{
		int32(cleanW) << 16, 1 << 16,
		int32(cleanH) << 16, 1 << 16,
		0, 1 << 16,
		0, 1 << 16,
	}
	out := make([]byte, 0, 32)
	for _, v := range vals {
		out = append(out, be32(uint32(v))...)
	}
	return box(nil, "clap", out)
}

func makeAudioTrak(o Options, timescale, chunkOffset uint32) []byte {
	duration := uint64(o.AudioSamples) * audioFrameTicks
	tkhd := makeTkhd(o.AudioTrackID, duration*1000/uint64(timescale), identityMatrix(), 0, 0, 0x0100)
	mdhd := makeMdhd(timescale, duration)
	hdlr := makeHdlr("soun", "SoundHandler")
	smhd := fullBox(nil, "smhd", 0, 0, concat(be16(0), be16(0)))
	mp4a := makeMp4a(o)
	stsd := fullBox(nil, "stsd", 0, 0, be32(1), mp4a)
	stts := fullBox(nil, "stts", 0, 0, be32(1), be32(uint32(o.AudioSamples)), be32(audioFrameTicks))
	stsc := fullBox(nil, "stsc", 0, 0, be32(1), be32(1), be32(uint32(o.AudioSamples)), be32(1))
	stsz := fullBox(nil, "stsz", 0, 0, concat(be32(0), be32(uint32(o.AudioSamples)), sizeList(o.Seed, o.AudioTrackID, o.AudioSamples, 980, 64)))
	stco := fullBox(nil, "stco", 0, 0, be32(1), be32(chunkOffset))
	stbl := box(nil, "stbl", stsd, stts, stsc, stsz, stco)
	minf := box(nil, "minf", smhd, makeDinf(), stbl)
	mdia := box(nil, "mdia", mdhd, hdlr, minf)
	return makeTrak(tkhd, mdia)
}

func makeMp4a(o Options) []byte {
	p := []byte{}
	p = append(p, make([]byte, 6)...)
	p = append(p, be16(1)...)  // data reference index
	p = append(p, be16(0)...)  // version
	p = append(p, be16(0)...)  // revision
	p = append(p, be32(0)...)  // vendor
	p = append(p, be16(2)...)  // channels
	p = append(p, be16(16)...) // sample size
	p = append(p, be16(0)...)  // compression id
	p = append(p, be16(0)...)  // packet size
	p = append(p, be32(uint32(o.SampleRate)<<16)...)
	// esds describing AAC-LC at the fixture sample rate.
	dsi := []byte{0x12, 0x10}
	decSpecific := append([]byte{0x05, byte(len(dsi))}, dsi...)
	decConfig := append([]byte{0x04, 0x11, 0x40, 0x15, 0x00, 0x00, 0x00}, be32(uint32(o.SampleRate))...)
	decConfig = append(decConfig, be32(128000)...)
	decConfig = append(decConfig, decSpecific...)
	es := append([]byte{0x03, byte(3 + len(decConfig) + 2), 0x00, 0x01, 0x00}, decConfig...)
	es = append(es, 0x06, 0x01, 0x02)
	esds := fullBox(nil, "esds", 0, 0, es)
	return box(nil, "mp4a", p, esds)
}
