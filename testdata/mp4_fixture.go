package testdata

import "encoding/binary"

// buildMP4 constructs a valid non-fragmented MP4 for the documented subset:
// ftyp + moov(mvhd, trak[video], trak[audio]) + mdat, one chunk per track
// with stts/stsc/stsz/stco tables describing every sample.
func buildMP4(spec FixtureSpec) ([]byte, error) {
	var videoSamples, audioSamples [][]byte
	if spec.TargetBitrate > 0 {
		// Total encoded bytes = bitrate * duration / 8, with duration derived
		// exactly from the video timeline ticks and timescale.
		videoTicks := int64(spec.VideoSamples) * spec.FrameDur
		totalBytes := spec.TargetBitrate * videoTicks / spec.VideoScale / 8
		videoBytes := totalBytes * 90 / 100
		audioBytes := totalBytes - videoBytes
		videoSamples = SizedPayloads(0, spec.VideoSamples, videoBytes)
		audioSamples = SizedPayloads(1, spec.AudioSamples, audioBytes)
	} else {
		videoSamples = make([][]byte, spec.VideoSamples)
		audioSamples = make([][]byte, spec.AudioSamples)
		for i := range videoSamples {
			videoSamples[i] = videoPayload(int64(i), spec.Width, spec.Height)
		}
		for i := range audioSamples {
			audioSamples[i] = audioPayload(int64(i))
		}
	}

	var mdatPayload []byte
	var videoSizes []uint32
	for _, p := range videoSamples {
		videoSizes = append(videoSizes, uint32(len(p)))
		mdatPayload = append(mdatPayload, p...)
	}
	var audioSizes []uint32
	for _, p := range audioSamples {
		audioSizes = append(audioSizes, uint32(len(p)))
		mdatPayload = append(mdatPayload, p...)
	}

	// ftyp: major_brand('isom') + minor_version(0) + compatible brand 'isom'.
	ftyp := box("ftyp", cat([]byte("isom"), u32(0), []byte("isom")))

	// Measure the real moov (offsets do not change moov size: stco is always
	// one uint32), then mdat payload begins right after ftyp + moov + the
	// 8-byte mdat header.
	moov := box("moov", cat(buildMvhd(spec),
		buildVideoTrakAt(spec, videoSizes, 0),
		buildAudioTrakAt(spec, audioSizes, 0)))
	mdatHeaderLen := 8
	mdatDataStart := len(ftyp) + len(moov) + mdatHeaderLen

	// Real chunk offsets: video chunk starts at mdat data, audio follows it.
	videoChunkOffset := uint32(mdatDataStart)
	audioChunkOffset := videoChunkOffset
	for _, z := range videoSizes {
		audioChunkOffset += z
	}
	moov = box("moov", cat(buildMvhd(spec),
		buildVideoTrakAt(spec, videoSizes, videoChunkOffset),
		buildAudioTrakAt(spec, audioSizes, audioChunkOffset)))

	// mdat header must declare the full payload length in its size field.
	mdat := make([]byte, 8+len(mdatPayload))
	binary.BigEndian.PutUint32(mdat[:4], uint32(8+len(mdatPayload)))
	copy(mdat[4:8], "mdat")
	copy(mdat[8:], mdatPayload)
	return cat(ftyp, moov, mdat), nil
}

func buildMvhd(spec FixtureSpec) []byte {
	body := fullbox(0, 0)
	body = append(body, make([]byte, 8)...)
	body = append(body, u32(uint32(spec.VideoScale))...)
	body = append(body, u32(uint32(spec.VideoScale*2))...)
	body = append(body, make([]byte, 80)...)
	body = append(body, u32(2)...)
	return box("mvhd", body)
}

func buildVideoTrakAt(spec FixtureSpec, sizes []uint32, chunkOffset uint32) []byte {
	n := len(sizes)
	mdhd := box("mdhd", mdhdBody(spec.VideoScale, int64(n)*spec.FrameDur))
	hdlr := box("hdlr", hdlrBody("vide", "VideoHandler"))
	// Visual sample entry layout after the 8-byte box header:
	// reserved(6)+dri(2) | predefined(2)+reserved(2)+predefined2(12) |
	// width(2)+height(2)+hres(4)+vres(4)+reserved(4)+framecount(2)+
	// compressorname(32)+depth(2)+predefined(2) = 70 bytes.
	entry := make([]byte, 8)
	copy(entry[4:8], "avc1")
	entry = append(entry, make([]byte, 6)...) // reserved
	entry = append(entry, u16(1)...)          // data_reference_index
	entry = append(entry, make([]byte, 16)...) // predefined/reserved/predefined2
	entry = append(entry, u16(uint16(spec.Width))...)
	entry = append(entry, u16(uint16(spec.Height))...)
	entry = append(entry, u32(0x00480000)...) // horiz resolution 72dpi
	entry = append(entry, u32(0x00480000)...) // vert resolution
	entry = append(entry, u32(0)...)          // reserved
	entry = append(entry, u16(1)...)          // frame count
	entry = append(entry, make([]byte, 32)...) // compressor name
	entry = append(entry, u16(0x0018)...)     // depth
	entry = append(entry, u16(0xFFFF)...)     // predefined -1
	stsd := box("stsd", cat(fullbox(0, 0), u32(1), entry))
	stbl := buildStbl(spec.FrameDur, 0, sizes, chunkOffset, stsd)
	dref := box("dref", cat(fullbox(0, 0), u32(0)))
	minf := box("minf", cat(box("dinf", dref), stbl))
	mdia := box("mdia", cat(mdhd, hdlr, minf))
	tkhd := box("tkhd", append(fullbox(0, 0x7), make([]byte, 84)...))
	return box("trak", cat(tkhd, mdia))
}

func buildAudioTrakAt(spec FixtureSpec, sizes []uint32, chunkOffset uint32) []byte {
	// Align the audio timeline with the video timeline: the audio track
	// spans the same wall-clock duration, expressed in its own clock.
	videoTicks := int64(spec.VideoSamples) * spec.FrameDur
	audioTotal := videoTicks * spec.AudioRate / spec.VideoScale
	audioDelta := audioTotal / int64(len(sizes))
	if audioDelta < 1 {
		audioDelta = 1
	}
	mdhd := box("mdhd", mdhdBody(spec.AudioRate, audioDelta*int64(len(sizes))))
	hdlr := box("hdlr", hdlrBody("soun", "AudioHandler"))
	// Audio sample entry after the 8-byte box header:
	// reserved(6)+dri(2) | entryversion(2)+reserved(6) |
	// channels(2)+samplesize(2)+predefined(2)+reserved(2)+samplerate(4).
	entry := make([]byte, 8)
	copy(entry[4:8], "mp4a")
	entry = append(entry, make([]byte, 6)...) // reserved
	entry = append(entry, u16(1)...)          // data_reference_index
	entry = append(entry, u16(0)...)          // entry version
	entry = append(entry, make([]byte, 6)...) // reserved [3]uint16
	entry = append(entry, u16(2)...)          // channels
	entry = append(entry, u16(16)...)         // sample size
	entry = append(entry, u16(0)...)          // predefined
	entry = append(entry, u16(0)...)          // reserved2
	entry = append(entry, u32(uint32(spec.AudioRate)<<16)...)
	stsd := box("stsd", cat(fullbox(0, 0), u32(1), entry))
	stbl := buildStbl(audioDelta, audioTotal, sizes, chunkOffset, stsd)
	dref := box("dref", cat(fullbox(0, 0), u32(0)))
	minf := box("minf", cat(box("dinf", dref), stbl))
	mdia := box("mdia", cat(mdhd, hdlr, minf))
	tkhd := box("tkhd", append(fullbox(0, 0x7), make([]byte, 84)...))
	return box("trak", cat(tkhd, mdia))
}

func mdhdBody(scale, duration int64) []byte {
	body := fullbox(0, 0)
	body = append(body, make([]byte, 8)...)
	body = append(body, u32(uint32(scale))...)
	body = append(body, u32(uint32(duration))...)
	body = append(body, make([]byte, 4)...)
	return body
}

func hdlrBody(handler, name string) []byte {
	body := fullbox(0, 0)
	body = append(body, u32(0)...)
	body = append(body, []byte(handler)...)
	body = append(body, make([]byte, 12)...)
	return append(body, []byte(name)...)
}

// buildStbl writes stts, stsc (all samples in one chunk), stsz (per-sample
// sizes) and stco (single chunk offset). When totalTicks is > 0 the stts run
// table distributes that exact total across the samples (the remainder lands
// on the final sample); otherwise every sample uses uniformDur.
func buildStbl(uniformDur, totalTicks int64, sizes []uint32, chunkOffset uint32, stsd []byte) []byte {
	n := uint32(len(sizes))
	var stts []byte
	if totalTicks > 0 && n > 1 {
		base := totalTicks / int64(n)
		rem := totalTicks - base*int64(n)
		first := int64(n) - 1
		sttsBody := cat(fullbox(0, 0), u32(2),
			u32(uint32(first)), u32(uint32(base)),
			u32(1), u32(uint32(base+rem)))
		stts = box("stts", sttsBody)
	} else {
		dur := uniformDur
		if totalTicks > 0 {
			dur = totalTicks / int64(n)
		}
		stts = box("stts", cat(fullbox(0, 0), u32(1), u32(n), u32(uint32(dur))))
	}

	stscBody := cat(fullbox(0, 0), u32(1), u32(1), u32(n), u32(1))
	stsc := box("stsc", stscBody)

	stszBody := append(fullbox(0, 0), u32(0)...)
	stszBody = append(stszBody, u32(n)...)
	for _, z := range sizes {
		stszBody = append(stszBody, u32(z)...)
	}
	stsz := box("stsz", stszBody)

	stco := box("stco", cat(fullbox(0, 0), u32(1), u32(chunkOffset)))
	return box("stbl", cat(stsd, stts, stsc, stsz, stco))
}
