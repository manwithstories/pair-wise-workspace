package ingest

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediarun/internal/mediaerr"
)

// parseStsd walks the sample-entry boxes nested in stsd (v1.4.1 only parses
// the stsd header itself) and returns the first entry's codec plus geometry.
func (s *mp4Streamer) parseStsd(stbl *boxHeader) (codec string, w, h uint32, ch uint16) {
	_ = findChild(s.file, stbl, mp4.BoxTypeStsd(), func(b *boxHeader) (bool, error) {
		// stsd payload begins with the FullBox version/flags(4) + entry_count(4).
		entriesStart := b.PayloadStart + 8
		if entriesStart >= b.End {
			return true, mediaerr.New(mediaerr.CodeUnknownStructure, "empty stsd").
				At("mp4", "", b.HeaderStart).OnPath(s.path)
		}
		ew := &seekingWalker{r: s.file, fileSize: b.End, pos: entriesStart}
		entry, err := ew.next()
		if err != nil {
			return true, err
		}
		val, err := parseBox(s.file, entry)
		if err != nil {
			return true, err
		}
		codec = entry.Type.String()
		switch t := val.(type) {
		case *mp4.VisualSampleEntry:
			w, h = uint32(t.Width), uint32(t.Height)
		case *mp4.AudioSampleEntry:
			ch = t.ChannelCount
		}
		return true, nil
	})
	return codec, w, h, ch
}

// loadTiming expands stts run-length entries into per-sample DTS/duration.
func (t *mp4Track) loadTiming(r io.ReadSeeker, b *boxHeader) error {
	v, err := parseBox(r, b)
	if err != nil {
		return err
	}
	stts := v.(*mp4.Stts)
	var dts int64
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			t.samples = append(t.samples, flatSample{dts: dts, pts: dts, duration: int64(e.SampleDelta)})
			dts += int64(e.SampleDelta)
		}
	}
	return nil
}

// applyCtts adds the composition offset to each sample's PTS.
func (t *mp4Track) applyCtts(r io.ReadSeeker, b *boxHeader) error {
	v, err := parseBox(r, b)
	if err != nil {
		return err
	}
	ctts := v.(*mp4.Ctts)
	idx := 0
	for ei, e := range ctts.Entries {
		off := ctts.GetSampleOffset(ei)
		for i := uint32(0); i < e.SampleCount; i++ {
			if idx >= len(t.samples) {
				return mediaerr.New(mediaerr.CodeUnknownStructure,
					"ctts has more samples than stts").At("mp4", t.desc.ID, b.HeaderStart)
			}
			t.samples[idx].pts = t.samples[idx].dts + off
			idx++
		}
	}
	return nil
}

// loadChunks resolves stsc/stsz/(stco|co64) into absolute byte ranges and
// enforces the 4 MiB per-sample ceiling and mdat containment before any
// payload is allocated.
func (t *mp4Track) loadChunks(r io.ReadSeeker, stbl *boxHeader, mdat []*boxHeader) error {
	var sizes []int64
	var fixedSize uint32
	if err := findChild(r, stbl, mp4.BoxTypeStsz(), func(b *boxHeader) (bool, error) {
		v, err := parseBox(r, b)
		if err != nil {
			return true, err
		}
		stsz := v.(*mp4.Stsz)
		fixedSize = stsz.SampleSize
		for _, z := range stsz.EntrySize {
			sizes = append(sizes, int64(z))
		}
		return true, nil
	}); err != nil {
		return err
	}
	if fixedSize > 0 {
		sizes = make([]int64, len(t.samples))
		for i := range sizes {
			sizes[i] = int64(fixedSize)
		}
	}
	if len(sizes) != len(t.samples) {
		return mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
			"stsz sample count %d != stts count %d", len(sizes), len(t.samples))).
			At("mp4", t.desc.ID, stbl.HeaderStart)
	}
	var offsets []int64
	var co64 bool
	_ = findChild(r, stbl, mp4.BoxTypeCo64(), func(b *boxHeader) (bool, error) {
		co64 = true
		v, err := parseBox(r, b)
		if err != nil {
			return true, err
		}
		for _, o := range v.(*mp4.Co64).ChunkOffset {
			offsets = append(offsets, int64(o))
		}
		return true, nil
	})
	if !co64 {
		if err := findChild(r, stbl, mp4.BoxTypeStco(), func(b *boxHeader) (bool, error) {
			v, err := parseBox(r, b)
			if err != nil {
				return true, err
			}
			for _, o := range v.(*mp4.Stco).ChunkOffset {
				offsets = append(offsets, int64(o))
			}
			return true, nil
		}); err != nil {
			return err
		}
	}
	var runs []mp4.StscEntry
	if err := findChild(r, stbl, mp4.BoxTypeStsc(), func(b *boxHeader) (bool, error) {
		v, err := parseBox(r, b)
		if err != nil {
			return true, err
		}
		runs = v.(*mp4.Stsc).Entries
		return true, nil
	}); err != nil {
		return err
	}
	if len(runs) == 0 || len(offsets) == 0 {
		return mediaerr.New(mediaerr.CodeUnknownStructure,
			"stsc/stco chunk tables are empty").At("mp4", t.desc.ID, stbl.HeaderStart)
	}
	sampleIdx := 0
	for chunk := 0; chunk < len(offsets); chunk++ {
		per := samplesPerChunk(runs, uint32(chunk+1))
		cursor := offsets[chunk]
		for j := uint32(0); j < per; j++ {
			if sampleIdx >= len(t.samples) {
				return mediaerr.New(mediaerr.CodeUnknownStructure,
					"chunk table references samples beyond stts count").
					At("mp4", t.desc.ID, cursor)
			}
			size := sizes[sampleIdx]
			if size > MaxBlockBytes {
				return mediaerr.New(mediaerr.CodeBlockTooLarge, fmt.Sprintf(
					"sample %d size %d exceeds 4 MiB ceiling", sampleIdx, size)).
					At("mp4", t.desc.ID, cursor)
			}
			if !insideAnyBox(cursor, size, mdat) {
				return mediaerr.New(mediaerr.CodeTruncatedBox, fmt.Sprintf(
					"sample %d range [%d,%d) is outside every mdat box",
					sampleIdx, cursor, cursor+size)).
					At("mp4", t.desc.ID, cursor)
			}
			t.samples[sampleIdx].offset = cursor
			t.samples[sampleIdx].size = size
			cursor += size
			sampleIdx++
		}
	}
	if sampleIdx != len(t.samples) {
		return mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
			"chunks resolved %d of %d samples", sampleIdx, len(t.samples))).
			At("mp4", t.desc.ID, stbl.HeaderStart)
	}
	return nil
}

// samplesPerChunk resolves the stsc first-chunk run table (1-based chunks).
func samplesPerChunk(runs []mp4.StscEntry, chunk1Based uint32) uint32 {
	per := runs[0].SamplesPerChunk
	for _, e := range runs {
		if chunk1Based >= e.FirstChunk {
			per = e.SamplesPerChunk
		} else {
			break
		}
	}
	return per
}

// insideAnyBox reports whether [off, off+size) is fully contained in one of
// the validated mdat boxes.
func insideAnyBox(off, size int64, boxes []*boxHeader) bool {
	for _, b := range boxes {
		if off >= b.PayloadStart && off+size <= b.End {
			return true
		}
	}
	return false
}
