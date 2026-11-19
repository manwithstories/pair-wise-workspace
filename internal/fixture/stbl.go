package fixture

import (
	mp4 "github.com/abema/go-mp4"
)

// buildStbl assembles the sample table of one track.
func buildStbl(tr trackSpec, chunkOffset uint64, omitStss bool) []byte {
	n := uint32(len(tr.samples))
	var stsdEntry []byte
	if tr.kind == "video" {
		stsdEntry = buildAvc1Entry(tr)
	} else {
		stsdEntry = buildMp4aEntry(tr)
	}
	stsd := boxOf("stsd", concat(leaf(&mp4.Stsd{EntryCount: 1}), stsdEntry))
	// stts: run-length compressed deltas.
	var sttsEntries []mp4.SttsEntry
	for _, s := range tr.samples {
		if len(sttsEntries) > 0 && sttsEntries[len(sttsEntries)-1].SampleDelta == s.delta {
			sttsEntries[len(sttsEntries)-1].SampleCount++
		} else {
			sttsEntries = append(sttsEntries, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.delta})
		}
	}
	stts := leafBox(&mp4.Stts{EntryCount: uint32(len(sttsEntries)), Entries: sttsEntries})
	// ctts v1: only when any composition offset is non-zero.
	var ctts []byte
	hasCTO := false
	for _, s := range tr.samples {
		if s.cto != 0 {
			hasCTO = true
			break
		}
	}
	if hasCTO {
		var entries []mp4.CttsEntry
		for _, s := range tr.samples {
			if len(entries) > 0 && entries[len(entries)-1].SampleOffsetV1 == s.cto {
				entries[len(entries)-1].SampleCount++
			} else {
				entries = append(entries, mp4.CttsEntry{SampleCount: 1, SampleOffsetV1: s.cto})
			}
		}
		ctts = leafBox(&mp4.Ctts{
			FullBox:    mp4.FullBox{Version: 1},
			EntryCount: uint32(len(entries)),
			Entries:    entries,
		})
	}
	stsc := leafBox(&mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: n, SampleDescriptionIndex: 1},
	}})
	sizes := make([]uint32, n)
	for i, s := range tr.samples {
		sizes[i] = s.size
	}
	stsz := leafBox(&mp4.Stsz{SampleCount: n, EntrySize: sizes})
	var stss []byte
	if tr.kind == "video" && !omitStss {
		var kfs []uint32
		for i, s := range tr.samples {
			if s.keyframe {
				kfs = append(kfs, uint32(i+1))
			}
		}
		stss = leafBox(&mp4.Stss{EntryCount: uint32(len(kfs)), SampleNumber: kfs})
	}
	var stco []byte
	if n > 0 {
		stco = leafBox(&mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(chunkOffset)}})
	} else {
		stco = leafBox(&mp4.Stco{EntryCount: 0})
	}
	return boxOf("stbl", concat(stsd, stts, ctts, stsc, stsz, stss, stco))
}

// buildAvc1Entry builds a minimal avc1 sample entry with an avcC child.
func buildAvc1Entry(tr trackSpec) []byte {
	vse := &mp4.VisualSampleEntry{
		Width:           uint16(tr.width),
		Height:          uint16(tr.height),
		Horizresolution: 0x480000,
		Vertresolution:  0x480000,
		FrameCount:      1,
		Depth:           0x18,
		PreDefined3:     -1,
	}
	vse.DataReferenceIndex = 1
	vse.SetType(mp4.BoxTypeAvc1())
	sps := []byte{0x67, 0x64, 0x00, 0x1e}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	avcc := &mp4.AVCDecoderConfiguration{
		ConfigurationVersion:       1,
		Profile:                    100,
		ProfileCompatibility:       0,
		Level:                      30,
		LengthSizeMinusOne:         3,
		NumOfSequenceParameterSets: 1,
		SequenceParameterSets: []mp4.AVCParameterSet{
			{Length: uint16(len(sps)), NALUnit: sps},
		},
		NumOfPictureParameterSets: 1,
		PictureParameterSets: []mp4.AVCParameterSet{
			{Length: uint16(len(pps)), NALUnit: pps},
		},
	}
	avcc.SetType(mp4.BoxTypeAvcC())
	return boxOf("avc1", concat(leaf(vse), leafBox(avcc)))
}

// buildMp4aEntry builds a minimal mp4a sample entry.
func buildMp4aEntry(tr trackSpec) []byte {
	ase := &mp4.AudioSampleEntry{
		ChannelCount: 2,
		SampleSize:   16,
		SampleRate:   tr.timescale << 16,
	}
	ase.DataReferenceIndex = 1
	ase.SetType(mp4.BoxTypeMp4a())
	return boxOf("mp4a", leaf(ase))
}
