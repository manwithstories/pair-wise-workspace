package fixture

// BuildMP4 serializes tracks into a structurally valid, non-fragmented MP4.
// It is exported so demux tests can inject targeted mutations.
func BuildMP4(tracks []TrackSpec) []byte { return buildMP4(tracks) }

// StudioTracks and CameraTracks expose the canonical fixture track layout.
func StudioTracks(seed int64) []TrackSpec { return studioTracks(seed) }

// buildMP4 serializes tracks into a structurally valid, non-fragmented MP4.
// Samples are stored verbatim as one chunk each so that byte hashes survive
// the round trip exactly.
func buildMP4(tracks []TrackSpec) []byte {
	ftyp := box("ftyp", []byte("isom"), be32(0x200), []byte("isom"), []byte("iso2"), []byte("avc1"), []byte("mp41"))

	// mdat payload is the concatenation of every sample, track by track.
	var mdatPayload []byte
	trackOffsets := make([][]int64, len(tracks))
	for ti, t := range tracks {
		samples := samplesFor(t)
		trackOffsets[ti] = make([]int64, len(samples))
		for si, s := range samples {
			trackOffsets[ti][si] = int64(len(mdatPayload))
			mdatPayload = append(mdatPayload, s.Data...)
		}
	}

	// Build moov with placeholder offsets to learn its size, then rebuild.
	moov := buildMoov(tracks, trackOffsets, 0)
	headerLen := int64(len(ftyp) + len(moov) + 8)
	// Rebuild with correct absolute chunk offsets.
	moov = buildMoov(tracks, trackOffsets, headerLen)
	if int64(len(ftyp)+len(moov)+8) != headerLen {
		panic("moov size changed between passes")
	}
	mdat := box("mdat", mdatPayload)
	return concat(ftyp, moov, mdat)
}

// buildMoov lays out mvhd plus one trak per track. mdatPayloadBase is the
// absolute file offset of the first mdat payload byte.
func buildMoov(tracks []TrackSpec, trackOffsets [][]int64, mdatPayloadBase int64) []byte {
	var maxDur int64
	traks := make([][]byte, 0, len(tracks))
	for ti, t := range tracks {
		var dur int64
		for _, s := range samplesFor(t) {
			if s.PTS+s.Dur > dur {
				dur = s.PTS + s.Dur
			}
		}
		// mvhd duration is expressed in the movie timescale (we use 1000).
		ms := dur * 1000 / t.Timescale
		if ms > maxDur {
			maxDur = ms
		}
		traks = append(traks, buildTrak(t, trackOffsets[ti], mdatPayloadBase, ms))
	}
	// Movie timescale 1000.
	mvhd := fullbox("mvhd", 0, 0,
		be32(0), be32(0), be32(1000), be32(uint32(maxDur)),
		be32(0x00010000), be16(0x0100), zeros(2), zeros(8),
		join(matrixParts(matrixForRotation(0))...), zeros(24), be32(uint32(len(tracks)+1)))
	return box("moov", append([][]byte{mvhd}, traks...)...)
}

func buildTrak(t TrackSpec, relOffsets []int64, mdatPayloadBase int64, trailerDur int64) []byte {
	offsets := make([]int64, len(relOffsets))
	for i, o := range relOffsets {
		offsets[i] = mdatPayloadBase + o
	}
	// tkhd duration is in movie timescale.
	tkhdDur := uint32(trailerDur)
	volume := uint16(0)
	if t.Kind == "audio" {
		volume = 0x0100
	}
	width, height := uint32(t.Width)<<16, uint32(t.Height)<<16
	if t.Rotation == 90 || t.Rotation == 270 {
		width, height = height, width
	}
	tkhd := fullbox("tkhd", 0, 0x000007,
		be32(0), be32(0), be32(t.ID), be32(0), be32(tkhdDur),
		zeros(8), be16s(0), be16s(0), be16s(int16(volume)), be16(0),
		join(matrixParts(matrixForRotation(t.Rotation))...),
		be32(width), be32(height))
	mdhd := fullbox("mdhd", 0, 0,
		be32(0), be32(0), be32(uint32(t.Timescale)), be32(uint32(trackDur(t))),
		be16(0x55c4), be16(0))
	hdlrType := "vide"
	if t.Kind == "audio" {
		hdlrType = "soun"
	}
	hdlr := fullbox("hdlr", 0, 0, be32(0), []byte(hdlrType), zeros(12), []byte("MediaMeld\x00"))

	var minf []byte
	if t.Kind == "audio" {
		minf = box("minf", fullbox("smhd", 0, 0, be16(0), be16(0)), dinfBox(),
			sampleTables(t, offsets))
	} else {
		minf = box("minf", fullbox("vmhd", 0, 1, be16(0), be16(0), be16(0), be16(0)), dinfBox(),
			sampleTables(t, offsets))
	}
	mdia := box("mdia", mdhd, hdlr, minf)
	return box("trak", tkhd, mdia)
}

func samplesFor(t TrackSpec) []SampleSpec { return t.Samples }

func trackDur(t TrackSpec) int64 {
	var d int64
	for _, s := range samplesFor(t) {
		if s.PTS+s.Dur > d {
			d = s.PTS + s.Dur
		}
	}
	return d
}

func dinfBox() []byte {
	dref := fullbox("dref", 0, 0, be32(1), fullbox("url ", 0, 1))
	return box("dinf", dref)
}

func sampleTables(t TrackSpec, offsets []int64) []byte {
	t.Samples = samplesFor(t)
	stsd := stsdBox(t)
	// stts: coalesce consecutive equal deltas.
	samples := samplesFor(t)
	var sttsEntries [][]byte
	count := 0
	for i := 0; i < len(samples); {
		j := i + 1
		for j < len(samples) && samples[j].Dur == samples[i].Dur {
			j++
		}
		sttsEntries = append(sttsEntries, be32(uint32(j-i)), be32(uint32(samples[i].Dur)))
		count++
		i = j
	}
	stts := fullbox("stts", 0, 0, be32(uint32(count)), join(sttsEntries...))

	// stsc: one sample per chunk.
	stsc := fullbox("stsc", 0, 0, be32(1), be32(1), be32(1), be32(1))
	var sizes [][]byte
	for _, s := range samplesFor(t) {
		sizes = append(sizes, be32(uint32(len(s.Data))))
	}
	stsz := fullbox("stsz", 0, 0, be32(0), be32(uint32(len(samplesFor(t)))), join(sizes...))
	var chunkOff [][]byte
	for _, o := range offsets {
		chunkOff = append(chunkOff, be32(uint32(o)))
	}
	stco := fullbox("stco", 0, 0, be32(uint32(len(offsets))), join(chunkOff...))

	parts := [][]byte{stsd, stts, stsc, stsz, stco}
	if hasCTTS(t) {
		var entries [][]byte
		for _, s := range samplesFor(t) {
			entries = append(entries, be32(1), be32s(int32(s.PTS-s.DTS)))
		}
		ctts := fullbox("ctts", 0, 0, be32(uint32(len(samplesFor(t)))), join(entries...))
		parts = append(parts, ctts)
	}
	return box("stbl", parts...)
}

func hasCTTS(t TrackSpec) bool {
	for _, s := range samplesFor(t) {
		if s.PTS != s.DTS {
			return true
		}
	}
	return false
}

func stsdBox(t TrackSpec) []byte {
	var entry []byte
	if t.Kind == "audio" {
		entry = box("mp4a", zeros(6), be16(1), zeros(8),
			be16(2), be16(16), be16(0), be16(0), be32(48000<<16))
	} else {
		var children [][]byte
		children = append(children, box("avcC", avcCBlob()))
		if t.Color != nil {
			children = append(children, box("colr", []byte("nclx"),
				be16(t.Color.Primaries), be16(t.Color.Transfer), be16(t.Color.Matrix),
				[]byte{boolByte(t.Color.FullRange), 0}))
		} else {
			// Unrecognized colour token must survive as "unknown".
			children = append(children, box("colr", []byte("zzzz"), []byte{0xde, 0xad, 0xbe, 0xef}))
		}
		if t.Crop != nil {
			children = append(children, clapBox(*t.Crop, t.Width, t.Height))
		}
		entry = box("avc1", concat(
			zeros(6), be16(1), zeros(16),
			be16(uint16(t.Width)), be16(uint16(t.Height)),
			be32(72<<16), be32(72<<16), be32(0), be16(1),
			zeros(32), be16(0x18), be16s(-1)), concat(children...))
	}
	return fullbox("stsd", 0, 0, be32(1), entry)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func clapBox(c CropSpec, frameW, frameH int) []byte {
	// cleanApertureWidthN/D, HeightN/D, horizOffN/D, vertOffN/D. A top-left
	// crop is a centered aperture shifted by the crop centre.
	cw, ch := c.W, c.H
	horizOff := c.X + cw/2 - frameW/2
	vertOff := c.Y + ch/2 - frameH/2
	return box("clap",
		be32(uint32(cw)), be32(1), be32(uint32(ch)), be32(1),
		be32(uint32(int32(horizOff))), be32(1), be32(uint32(int32(vertOff))), be32(1))
}
