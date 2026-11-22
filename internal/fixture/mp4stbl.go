package fixture

import mp4 "github.com/abema/go-mp4"

// writeStbl writes the complete sample table: stsd, stts, stsc, stsz,
// stco/co64 and stss (video sync samples only).
func writeStbl(bw *boxWriter, t *builtTrack) error {
	s := t.spec
	if err := bw.start(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := writeStsd(bw, t); err != nil {
		return err
	}
	if err := bw.box(&mp4.Stts{EntryCount: uint32(len(t.deltas)), Entries: t.deltas}); err != nil {
		return err
	}
	if err := writeStsc(bw, t); err != nil {
		return err
	}
	if err := writeStsz(bw, t); err != nil {
		return err
	}
	if s.Kind == "video" && s.IncludeSyncTable {
		if err := bw.box(&mp4.Stss{
			EntryCount:   uint32(len(t.sync)),
			SampleNumber: t.sync,
		}); err != nil {
			return err
		}
	}
	if err := writeChunkOffsets(bw, t); err != nil {
		return err
	}
	return bw.end() // stbl
}

func writeStsd(bw *boxWriter, t *builtTrack) error {
	s := t.spec
	if err := bw.start(mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if err := bw.payload(&mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	switch s.Kind {
	case "video":
		if err := bw.start(mp4.StrToBoxType("avc1")); err != nil {
			return err
		}
		if err := bw.payload(visualEntry(s.Width, s.Height)); err != nil {
			return err
		}
		if s.IncludeColr {
			if err := bw.start(mp4.BoxTypeColr()); err != nil {
				return err
			}
			if err := bw.payload(colrBox(s.Primaries, s.Transfer, s.Matrix, s.FullRange)); err != nil {
				return err
			}
			if err := bw.end(); err != nil { // colr
				return err
			}
		}
		if err := bw.end(); err != nil { // avc1
			return err
		}
	case "audio":
		if err := bw.start(mp4.StrToBoxType("mp4a")); err != nil {
			return err
		}
		if err := bw.payload(audioEntry(s.Channels, s.SampleRate)); err != nil {
			return err
		}
		if err := bw.end(); err != nil { // mp4a
			return err
		}
	default:
		if err := bw.start(mp4.StrToBoxType("mett")); err != nil {
			return err
		}
		if err := bw.payload(newMarkerSampleEntry()); err != nil {
			return err
		}
		if err := bw.end(); err != nil { // mett
			return err
		}
	}
	return bw.end() // stsd
}

func writeStsc(bw *boxWriter, t *builtTrack) error {
	// Every chunk uses the same run size sequence; emit one entry per chunk
	// with 1-based first-chunk numbers, which any compliant reader accepts.
	entries := make([]mp4.StscEntry, len(t.chunkSize))
	for i, n := range t.chunkSize {
		entries[i] = mp4.StscEntry{
			FirstChunk:             uint32(i + 1),
			SamplesPerChunk:        n,
			SampleDescriptionIndex: 1,
		}
	}
	return bw.box(&mp4.Stsc{EntryCount: uint32(len(entries)), Entries: entries})
}

func writeStsz(bw *boxWriter, t *builtTrack) error {
	return bw.box(&mp4.Stsz{
		SampleSize:  0,
		SampleCount: uint32(len(t.sizes)),
		EntrySize:   t.sizes,
	})
}

// writeChunkOffsets emits one co64 entry per chunk; each chunk begins at the
// first sample's absolute file offset.
func writeChunkOffsets(bw *boxWriter, t *builtTrack) error {
	offsets := make([]uint64, 0, len(t.chunkSize))
	cursor := uint32(0)
	for _, n := range t.chunkSize {
		offsets = append(offsets, t.offsets[cursor])
		cursor += n
	}
	return bw.box(&mp4.Co64{EntryCount: uint32(len(offsets)), ChunkOffset: offsets})
}
