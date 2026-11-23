package fixtures

import "fmt"

// BuildMXF generates a two-track MXF stream: a header partition with a primer
// pack and structural metadata, a body partition, one essence KLV packet per
// sample, and a footer partition.
func BuildMXF(o BuildMxfOptions) (*MxfBundle, error) {
	o, err := o.validated()
	if err != nil {
		return nil, err
	}
	b := &MxfBundle{Name: "field.mxf"}

	vid := MxfTrackInfo{TrackID: o.VideoTrackID, Kind: "video",
		EditRateNum: o.VideoEditRateNum, EditRateDen: o.VideoEditRateDen}
	aud := MxfTrackInfo{TrackID: o.AudioTrackID, Kind: "audio",
		EditRateNum: int64(o.AudioEditRate), EditRateDen: 1}

	for i := 0; i < o.VideoSamples; i++ {
		size := sampleSize(o.Seed, o.VideoTrackID+100, i, 700, 500)
		vid.Sizes = append(vid.Sizes, size)
		vid.Payloads = append(vid.Payloads, Payload(o.Seed, o.VideoTrackID+100, i, size))
	}
	for i := 0; i < o.AudioSamples; i++ {
		size := sampleSize(o.Seed, o.AudioTrackID+100, i, 160, 40)
		aud.Sizes = append(aud.Sizes, size)
		aud.Payloads = append(aud.Payloads, Payload(o.Seed, o.AudioTrackID+100, i, size))
	}
	if vid.Hash, err = HashSamples(vid.Payloads); err != nil {
		return nil, err
	}
	if aud.Hash, err = HashSamples(aud.Payloads); err != nil {
		return nil, err
	}
	vid.FirstPacket = 0
	aud.FirstPacket = len(vid.Payloads)

	// The metadata length is independent of partition offsets, so one build
	// with zero offsets yields the length needed to compute real offsets.
	primer := buildPrimer()
	meta := buildMetadata(o, vid, aud)
	headerPack := buildPartitionPack(ulHeaderPartition, 0, uint64(len(meta)), uint64(len(primer)), 0, false)

	headerLen := len(headerPack) + len(primer) + len(meta)
	bodyOffset := uint64(headerLen)

	var essence []byte
	for _, p := range vid.Payloads {
		essence = append(essence, klv(ulEssence, p)...)
	}
	for _, p := range aud.Payloads {
		essence = append(essence, klv(ulEssence, p)...)
	}
	if headerLen+len(essence) > maxFixtureBytes {
		return nil, fmt.Errorf("fixtures: MXF stream exceeds %d bytes", maxFixtureBytes)
	}
	bodyPack := buildPartitionPack(ulBodyPartition, bodyOffset, 0, 0, uint64(len(essence)), false)

	out := make([]byte, 0, headerLen+len(bodyPack)+len(essence)+64)
	out = append(out, headerPack...)
	out = append(out, primer...)
	out = append(out, meta...)
	out = append(out, bodyPack...)
	out = append(out, essence...)
	out = append(out, buildPartitionPack(ulFooterPartition, uint64(len(out)), 0, 0, 0, true)...)
	b.Data = out
	b.Stats = []MxfTrackInfo{vid, aud}
	return b, nil
}

// buildPrimer maps every local tag the file uses to the property UL it stands
// for. A reader that trusts unknown tags can at least see their identity.
func buildPrimer() []byte {
	pairs := [][2][]byte{
		{uint16Value(tagInstanceUID), ulIdentification},
		{uint16Value(tagGenerationUID), ulIdentification},
		{uint16Value(tagPackageUID), ulMaterialPackage},
		{uint16Value(tagTrackCount), ulMaterialPackage},
		{uint16Value(tagTrackSets), ulMaterialPackage},
		{uint16Value(tagTrackID), ulTrack},
		{uint16Value(tagTrackNumber), ulTrack},
		{uint16Value(tagSequenceSet), ulTrack},
		{uint16Value(tagDuration), ulSequence},
		{uint16Value(tagEditRate), ulTrack},
		{uint16Value(tagOrigin), ulTrack},
		{uint16Value(tagEventCount), ulSequence},
		{uint16Value(tagEventEditRate), ulSequence},
		{uint16Value(tagEssenceDescSets), ulSourcePackage},
		{uint16Value(tagFrameLayout), ulCDCI},
		{uint16Value(tagStoredWidth), ulCDCI},
		{uint16Value(tagStoredHeight), ulCDCI},
		{uint16Value(tagDisplayWidth), ulCDCI},
		{uint16Value(tagDisplayHeight), ulCDCI},
		{uint16Value(tagComponentDepth), ulCDCI},
		{uint16Value(tagHorizSubsampling), ulCDCI},
		{uint16Value(tagVertSubsampling), ulCDCI},
		{uint16Value(tagAudioSamplingRate), ulWave},
		{uint16Value(tagChannelCount), ulWave},
		{uint16Value(tagQuantizationBits), ulWave},
		{uint16Value(tagMapEntries), ulEssenceMap},
		{uint16Value(tagMapTrackID), ulEssenceMap},
		{uint16Value(tagMapKind), ulEssenceMap},
		{uint16Value(tagMapFirstPacket), ulEssenceMap},
		{uint16Value(tagMapPacketCount), ulEssenceMap},
		{uint16Value(tagMapEditRate), ulEssenceMap},
	}
	v := uint32Value(uint32(len(pairs)))
	for _, p := range pairs {
		v = append(v, p[0]...)
		v = append(v, p[1]...)
	}
	return klv(ulPrimer, v)
}

func uint16Value(v uint16) []byte {
	b := make([]byte, 2)
	b[0] = byte(v >> 8)
	b[1] = byte(v)
	return b
}

func buildMetadata(o BuildMxfOptions, vid, aud MxfTrackInfo) []byte {
	var meta []byte
	ident := append(prop(tagInstanceUID, uuidValue(o.Seed, 0x11)),
		prop(tagGenerationUID, uuidValue(o.Seed, 0x22))...)
	meta = append(meta, klv(ulIdentification, ident)...)

	mat := []byte{}
	mat = append(mat, prop(tagPackageUID, uuidValue(o.Seed, 0x44))...)
	mat = append(mat, prop(tagTrackCount, uint32Value(2))...)
	mat = append(mat, klv(ulTrack, buildTrackSet(o.Seed, 0x50, vid, 0x01))...)
	mat = append(mat, klv(ulTrack, buildTrackSet(o.Seed, 0x51, aud, 0x02))...)
	meta = append(meta, klv(ulMaterialPackage, mat)...)

	src := []byte{}
	src = append(src, prop(tagPackageUID, uuidValue(o.Seed, 0x55))...)
	src = append(src, prop(tagTrackCount, uint32Value(2))...)
	src = append(src, prop(tagEssenceDescSets, uint32Value(2))...)
	src = append(src, klv(ulCDCI, buildCDCISet(o))...)
	src = append(src, klv(ulWave, buildWaveSet(o))...)
	meta = append(meta, klv(ulSourcePackage, src)...)

	meta = append(meta, klv(ulEssenceMap, buildEssenceMap(vid, aud))...)
	return meta
}

func buildTrackSet(seed int64, kind byte, info MxfTrackInfo, trackNum uint32) []byte {
	var tr []byte
	tr = append(tr, prop(tagInstanceUID, uuidValue(seed, kind))...)
	tr = append(tr, prop(tagTrackID, uint32Value(info.TrackID))...)
	tr = append(tr, prop(tagTrackNumber, uint32Value(trackNum))...)
	tr = append(tr, prop(tagEditRate, rationalValue(info.EditRateNum, info.EditRateDen))...)
	tr = append(tr, prop(tagDuration, uint32Value(uint32(len(info.Sizes))))...)

	var seq []byte
	seq = append(seq, prop(tagInstanceUID, uuidValue(seed, kind+1))...)
	seq = append(seq, prop(tagEventCount, uint32Value(uint32(len(info.Sizes))))...)
	seq = append(seq, prop(tagEventEditRate, rationalValue(info.EditRateNum, info.EditRateDen))...)
	seq = append(seq, prop(tagOrigin, uint32Value(uint32(info.TrackID)))...)
	seq = append(seq, prop(tagDuration, uint32Value(uint32(len(info.Sizes))))...)
	tr = append(tr, prop(tagSequenceSet, seq)...)
	return tr
}

func buildCDCISet(o BuildMxfOptions) []byte {
	var v []byte
	v = append(v, prop(tagFrameLayout, []byte{o.FrameLayout})...)
	v = append(v, prop(tagStoredWidth, uint32Value(uint32(o.StoredWidth)))...)
	v = append(v, prop(tagStoredHeight, uint32Value(uint32(o.StoredHeight)))...)
	v = append(v, prop(tagDisplayWidth, uint32Value(uint32(o.StoredWidth)))...)
	v = append(v, prop(tagDisplayHeight, uint32Value(uint32(o.StoredHeight)))...)
	v = append(v, prop(tagComponentDepth, uint32Value(10))...)
	v = append(v, prop(tagHorizSubsampling, uint32Value(2))...)
	v = append(v, prop(tagVertSubsampling, uint32Value(2))...)
	return v
}

func buildWaveSet(o BuildMxfOptions) []byte {
	var v []byte
	v = append(v, prop(tagAudioSamplingRate, rationalValue(int64(o.AudioEditRate), 1))...)
	v = append(v, prop(tagChannelCount, uint32Value(2))...)
	v = append(v, prop(tagQuantizationBits, uint32Value(24))...)
	return v
}

// buildEssenceMap lists the essence packet runs in file order. Each entry names
// the track, its kind, the first packet ordinal and the packet count.
func buildEssenceMap(vid, aud MxfTrackInfo) []byte {
	var v []byte
	v = append(v, prop(tagMapEntries, uint32Value(2))...)
	v = append(v, prop(tagMapTrackID, uint32Value(vid.TrackID))...)
	v = append(v, prop(tagMapKind, []byte(vid.Kind))...)
	v = append(v, prop(tagMapFirstPacket, uint32Value(uint32(vid.FirstPacket)))...)
	v = append(v, prop(tagMapPacketCount, uint32Value(uint32(len(vid.Sizes))))...)
	v = append(v, prop(tagMapEditRate, rationalValue(vid.EditRateNum, vid.EditRateDen))...)
	v = append(v, prop(tagMapTrackID, uint32Value(aud.TrackID))...)
	v = append(v, prop(tagMapKind, []byte(aud.Kind))...)
	v = append(v, prop(tagMapFirstPacket, uint32Value(uint32(aud.FirstPacket)))...)
	v = append(v, prop(tagMapPacketCount, uint32Value(uint32(len(aud.Sizes))))...)
	v = append(v, prop(tagMapEditRate, rationalValue(aud.EditRateNum, aud.EditRateDen))...)
	return v
}

// buildPartitionPack writes the 16-byte UL, BER length and the fixed partition
// pack fields: version pair, KAG size, partition offset and section lengths.
func buildPartitionPack(ul []byte, offset, headerLen, indexLen, bodyLen uint64, footer bool) []byte {
	val := []byte{0x00, 0x03, 0x00, 0x04}
	val = append(val, uint32Value(1)...) // KAG size in bytes
	val = append(val, uint64Value(offset)...)
	val = append(val, uint64Value(headerLen)...)
	val = append(val, uint64Value(indexLen)...)
	val = append(val, uint64Value(bodyLen)...)
	if footer {
		val = append(val, 0x00)
	}
	return klv(ul, val)
}
