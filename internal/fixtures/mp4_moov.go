package fixtures

func makeMvhd(o Options, videoTS, audioTS uint32) []byte {
	videoMs := int64(o.VideoFrames) * 1000 * int64(o.FPSDen) / int64(o.FPSNum)
	audioMs := int64(o.AudioSamples) * audioFrameTicks * 1000 / int64(o.SampleRate)
	dur := videoMs
	if audioMs > dur {
		dur = audioMs
	}
	matrix := identityMatrix()
	payload := []byte{}
	payload = append(payload, be32(0)...)                               // creation
	payload = append(payload, be32(0)...)                               // modification
	payload = append(payload, be32(1000)...)                            // timescale
	payload = append(payload, be32(uint32(dur))...)                     // duration
	payload = append(payload, be32(0x00010000)...)                      // rate
	payload = append(payload, be16(0x0100)...)                          // volume
	payload = append(payload, make([]byte, 10)...)                      // reserved
	payload = append(payload, matrix...)                                // matrix
	payload = append(payload, make([]byte, 24)...)                      // pre_defined
	payload = append(payload, be32(o.VideoTrackID+o.AudioTrackID+1)...) // next track id
	return fullBox(nil, "mvhd", 0, 0, payload)
}

func makeTkhd(trackID uint32, duration uint64, matrix []byte, width, height int, volume uint16) []byte {
	p := []byte{}
	p = append(p, be32(0)...)
	p = append(p, be32(0)...)
	p = append(p, be32(trackID)...)
	p = append(p, be32(0)...)
	p = append(p, be32(uint32(duration))...)
	p = append(p, be32(0)...)
	p = append(p, be32(0)...)
	p = append(p, be16(0)...) // layer
	p = append(p, be16(0)...) // alternate group
	p = append(p, be16(volume)...)
	p = append(p, be16(0)...) // reserved
	p = append(p, matrix...)
	p = append(p, be32(uint32(width)<<16)...)
	p = append(p, be32(uint32(height)<<16)...)
	return fullBox(nil, "tkhd", 0, 0x3, p)
}

func makeTrak(tkhd, mdia []byte) []byte { return box(nil, "trak", tkhd, mdia) }

func makeMdhd(timescale uint32, duration uint64) []byte {
	p := []byte{}
	p = append(p, be32(0)...)
	p = append(p, be32(0)...)
	p = append(p, be32(timescale)...)
	p = append(p, be32(uint32(duration))...)
	p = append(p, be16(0x55c4)...) // language "und"
	p = append(p, be16(0)...)
	return fullBox(nil, "mdhd", 0, 0, p)
}

func makeHdlr(typ string, name string) []byte {
	p := append([]byte{}, be32(0)...)
	p = append(p, typ...)
	p = append(p, make([]byte, 12)...)
	p = append(p, []byte(name)...)
	p = append(p, 0)
	return fullBox(nil, "hdlr", 0, 0, p)
}

func makeDinf() []byte {
	url := fullBox(nil, "url ", 0, 1)
	dref := fullBox(nil, "dref", 0, 0, be32(1), url)
	return box(nil, "dinf", dref)
}
