package mp4

import (
	"fmt"

	gomp4 "github.com/abema/go-mp4"

	"mediabridge/internal/clock"
	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// trakData accumulates the boxes needed to build one track.
type trakData struct {
	tkhd    *gomp4.Tkhd
	mdhd    *gomp4.Mdhd
	hdlr    *gomp4.Hdlr
	elst    *gomp4.Elst
	elstOff int64
	stts    *gomp4.Stts
	ctts    *gomp4.Ctts
	stsc    *gomp4.Stsc
	stsz    *gomp4.Stsz
	stss    *gomp4.Stss
	stco    *gomp4.Stco
	co64    *gomp4.Co64
	stsd    *gomp4.Stsd
	sdi     sampleDescInfo
}

// sampleDescInfo captures the first sample description entry.
type sampleDescInfo struct {
	kind       media.TrackKind
	typeCode   media.MediaType
	config     []byte
	width      uint32
	height     uint32
	channels   uint16
	sampleRate uint32
	offset     int64
	found      bool
}

// parseTrak extracts one trak into a media.Track and stashes its sample table
// for the packet-build phase.
func (p *parser) parseTrak(bi *gomp4.BoxInfo, index int, mdatOffset, mdatSize, fileSize int64) (*media.Track, error) {
	td := &trakData{}
	trakOff := int64(bi.Offset)
	if err := p.collectTrakBoxes(bi, td); err != nil {
		return nil, err
	}
	if td.tkhd == nil || td.mdhd == nil {
		return nil, p.errTrack(fault.CodeCorruptContainer, trakOff, 0,
			"trak missing tkhd/mdhd")
	}
	trackID := td.tkhd.TrackID
	if trackID == 0 {
		return nil, p.errTrack(fault.CodeCorruptContainer, trakOff, 0, "track id 0")
	}

	scale, err := clock.NewScale(td.mdhd.Timescale)
	if err != nil {
		return nil, p.errTrack(fault.CodeTimescale, trakOff, trackID,
			"timescale 0 for track %d", trackID)
	}

	kind := media.KindAux
	if td.hdlr != nil {
		switch string(td.hdlr.HandlerType[:]) {
		case "vide":
			kind = media.KindVideo
		case "soun":
			kind = media.KindAudio
		}
	}

	if td.stts == nil || td.stsz == nil || td.stsc == nil || (td.stco == nil && td.co64 == nil) {
		return nil, p.errTrack(fault.CodeCorruptContainer, trakOff, trackID,
			"track %d missing required sample table boxes", trackID)
	}

	table, err := p.expandSampleTable(td, trackID, scale, mdatOffset, mdatSize, fileSize)
	if err != nil {
		return nil, err
	}
	if table.count == 0 {
		p.warn("track %d has no samples", trackID)
	}

	// Non-zero media start / edit list normalisation.
	originNS, editWarn, err := p.computeOrigin(td, scale, trackID)
	if err != nil {
		return nil, err
	}
	if editWarn != "" {
		p.warn(editWarn)
	}

	// Convert every timing array from media units to nanoseconds and shift by
	// the origin. Doing the conversion here (rather than per packet later)
	// keeps rounding consistent across DTS/PTS/duration.
	var lastEnd int64
	for i := int64(0); i < table.count; i++ {
		dts, err := scale.Nanos(table.dts[i])
		if err != nil {
			return nil, p.errTrack(fault.CodeTimestampWrap, table.offsets[i], trackID,
				"sample %d DTS overflows: %v", i, err)
		}
		pts, err := scale.Nanos(table.pts[i])
		if err != nil {
			return nil, p.errTrack(fault.CodeTimestampWrap, table.offsets[i], trackID,
				"sample %d PTS overflows: %v", i, err)
		}
		dur, err := scale.Nanos(table.dur[i])
		if err != nil {
			return nil, p.errTrack(fault.CodeTimestampWrap, table.offsets[i], trackID,
				"sample %d duration overflows: %v", i, err)
		}
		table.dts[i] = dts - originNS
		table.pts[i] = pts - originNS
		table.dur[i] = dur
	}

	// Trim packets whose presentation end precedes the timeline origin. This
	// is the media_time>0 case: the edit list declares the first N units are
	// not presented, so they must not appear as negative timestamps.
	firstKept := int64(0)
	for firstKept < table.count {
		idx := firstKept
		if table.pts[idx]+table.dur[idx] > 0 {
			break
		}
		firstKept++
	}
	if firstKept > 0 {
		table.dropLeading(firstKept)
		p.warn("track %d trimmed %d samples before timeline origin", trackID, firstKept)
	}

	for i := int64(0); i < table.count; i++ {
		if end := table.pts[i] + table.dur[i]; end > lastEnd {
			lastEnd = end
		}
	}
	if lastEnd < 0 {
		lastEnd = 0
	}

	codec := media.Codec{
		SampleEntry: td.sdi.typeCode,
		Config:      td.sdi.config,
		Width:       td.sdi.width,
		Height:      td.sdi.height,
		Channels:    td.sdi.channels,
		SampleRate:  td.sdi.sampleRate,
	}
	codec.Seal()

	trk := &media.Track{
		Index:             index,
		ID:                trackID,
		Kind:              kind,
		Timescale:         scale.Value(),
		TimescaleName:     "mp4.mdhd",
		OriginNS:          originNS,
		DurationNS:        lastEnd,
		Codec:             codec,
		SampleEntryOffset: td.sdi.offset,
		PacketCount:       table.count,
	}
	p.tables = append(p.tables, tableEntry{track: trk, table: table})
	return trk, nil
}

// tableEntry ties a parsed track to its expanded sample table.
type tableEntry struct {
	track *media.Track
	table *sampleTable
}

// expandSampleTable materialises DTS/PTS/duration/size/offset/sync arrays.
func (p *parser) expandSampleTable(td *trakData, trackID uint32, scale clock.Scale, mdatOffset, mdatSize, fileSize int64) (*sampleTable, error) {
	count := int64(td.stsz.SampleCount)
	if count < 0 {
		return nil, p.errTrack(fault.CodeCorruptContainer, int64(td.stsdOffset()), trackID, "negative sample count")
	}
	if err := clock.ValidatePacketCount(count); err != nil {
		return nil, p.errTrack(fault.CodeDurationRange, 0, trackID,
			"track %d has %d samples exceeding limit", trackID, count)
	}

	tbl := &sampleTable{
		count:   count,
		dts:     make([]int64, count),
		pts:     make([]int64, count),
		dur:     make([]int64, count),
		sizes:   make([]uint32, count),
		offsets: make([]int64, count),
		sync:    make([]bool, count),
	}

	// Sample sizes.
	for i := int64(0); i < count; i++ {
		if td.stsz.SampleSize != 0 {
			tbl.sizes[i] = td.stsz.SampleSize
		} else {
			if i >= int64(len(td.stsz.EntrySize)) {
				return nil, p.errTrack(fault.CodeBadIndex, 0, trackID,
					"stsz entry %d missing (count %d)", i, count)
			}
			tbl.sizes[i] = td.stsz.EntrySize[i]
		}
		if int64(tbl.sizes[i]) > media.MaxPacketBytes {
			return nil, p.errTrack(fault.CodeBadIndex, 0, trackID,
				"sample %d size %d exceeds limit", i, tbl.sizes[i])
		}
	}

	// Decode times from stts.
	var dts int64
	sample := int64(0)
	for _, e := range td.stts.Entries {
		for k := uint32(0); k < e.SampleCount; k++ {
			if sample >= count {
				break
			}
			tbl.dts[sample] = dts
			tbl.dur[sample] = int64(e.SampleDelta)
			dts += int64(e.SampleDelta)
			sample++
		}
	}
	if sample != count {
		// A short stts is a bad index: the timing table disagrees with stsz.
		return nil, p.errTrack(fault.CodeBadIndex, 0, trackID,
			"stts covers %d of %d samples", sample, count)
	}

	// Presentation times from ctts.
	if td.ctts != nil {
		sample = 0
		for _, e := range td.ctts.Entries {
			for k := uint32(0); k < e.SampleCount; k++ {
				if sample >= count {
					break
				}
				var off int64
				if td.ctts.GetVersion() == 1 {
					off = int64(e.SampleOffsetV1)
				} else {
					off = int64(e.SampleOffsetV0)
				}
				tbl.pts[sample] = tbl.dts[sample] + off
				sample++
			}
		}
		if sample != count {
			return nil, p.errTrack(fault.CodeBadIndex, 0, trackID,
				"ctts covers %d of %d samples", sample, count)
		}
	} else {
		copy(tbl.pts, tbl.dts)
	}

	// Sync samples: if stss is absent every sample is sync (audio), else the
	// listed sample numbers (1-based) are sync.
	if td.stss != nil && len(td.stss.SampleNumber) > 0 {
		for _, sn := range td.stss.SampleNumber {
			if sn == 0 || int64(sn) > count {
				return nil, p.errTrack(fault.CodeBadIndex, 0, trackID,
					"stss sample number %d out of range (count %d)", sn, count)
			}
			tbl.sync[sn-1] = true
		}
	} else {
		for i := range tbl.sync {
			tbl.sync[i] = true
		}
	}

	// Chunk offsets → per-sample byte offsets via stsc.
	chunkOffsets, err := chunkOffsets(td)
	if err != nil {
		return nil, p.errTrack(fault.CodeBadIndex, 0, trackID, "%v", err)
	}
	if int64(len(chunkOffsets)) == 0 && count > 0 {
		return nil, p.errTrack(fault.CodeBadIndex, 0, trackID, "no chunk offsets")
	}
	if badOff, err := assignOffsets(td.stsc, chunkOffsets, tbl, fileSize); err != nil {
		// Report the offending chunk offset so the operator can point at the
		// exact byte the index claims, not just the track.
		return nil, p.errTrack(fault.CodeBadIndex, badOff, trackID, "%v", err)
	}

	// Duration cap per track.
	totalUnits := dts
	durNS, err := scale.Nanos(totalUnits)
	if err != nil || durNS > p.opts.MaxAssetDurationNS {
		return nil, p.errTrack(fault.CodeDurationRange, 0, trackID,
			"track %d duration exceeds 24h limit", trackID)
	}
	return tbl, nil
}

// stsdOffset returns the sample-entry byte offset (best effort).
func (td *trakData) stsdOffset() int64 { return td.sdi.offset }

func chunkOffsets(td *trakData) ([]int64, error) {
	if td.stco != nil {
		out := make([]int64, len(td.stco.ChunkOffset))
		for i, v := range td.stco.ChunkOffset {
			out[i] = int64(v)
		}
		return out, nil
	}
	if td.co64 != nil {
		out := make([]int64, len(td.co64.ChunkOffset))
		for i, v := range td.co64.ChunkOffset {
			if v > 1<<62 {
				return nil, fmt.Errorf("co64 offset %d out of range", v)
			}
			out[i] = int64(v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("no stco/co64")
}

// assignOffsets walks stsc runs to map samples to source byte offsets.
func assignOffsets(stsc *gomp4.Stsc, chunks []int64, tbl *sampleTable, fileSize int64) (int64, error) {
	if len(stsc.Entries) == 0 && tbl.count > 0 {
		return 0, fmt.Errorf("empty stsc")
	}
	sample := int64(0)
	for run := 0; run < len(stsc.Entries); run++ {
		first := int64(stsc.Entries[run].FirstChunk)
		if first == 0 {
			return 0, fmt.Errorf("stsc first_chunk 0")
		}
		chunkStart := first - 1
		var chunkEnd int64
		if run+1 < len(stsc.Entries) {
			chunkEnd = int64(stsc.Entries[run+1].FirstChunk) - 1
		} else {
			chunkEnd = int64(len(chunks))
		}
		if chunkStart < 0 || chunkStart > int64(len(chunks)) {
			return 0, fmt.Errorf("stsc chunk run %d out of range", first)
		}
		if chunkEnd > int64(len(chunks)) {
			chunkEnd = int64(len(chunks))
		}
		spc := int64(stsc.Entries[run].SamplesPerChunk)
		if spc < 0 {
			return 0, fmt.Errorf("stsc samples_per_chunk negative")
		}
		for c := chunkStart; c < chunkEnd && sample < tbl.count; c++ {
			off := chunks[c]
			for k := int64(0); k < spc && sample < tbl.count; k++ {
				if off < 0 || off+int64(tbl.sizes[sample]) > fileSize {
					return off, fmt.Errorf("sample %d byte range [%d,%d) exceeds file size %d",
						sample, off, off+int64(tbl.sizes[sample]), fileSize)
				}
				tbl.offsets[sample] = off
				off += int64(tbl.sizes[sample])
				sample++
			}
		}
	}
	if sample != tbl.count {
		return 0, fmt.Errorf("stsc placed %d of %d samples", sample, tbl.count)
	}
	return 0, nil
}

// computeOrigin derives the nanosecond offset to subtract so the asset
// timeline starts at zero. It handles:
//   - no edit list: origin is the media DTS origin (0 by convention);
//   - empty edit list entries: rejected as a bad edit list;
//   - media_time > 0 (non-zero start): subtract that media time;
//   - media_time < 0 (negative edit): rejected as CodeNegativeEdit.
func (p *parser) computeOrigin(td *trakData, scale clock.Scale, trackID uint32) (int64, string, error) {
	if td.elst == nil || len(td.elst.Entries) == 0 {
		return 0, "", nil
	}
	// Sum of leading empty edits (media_time == -1) delays the media start.
	var emptyDelay int64
	entries := td.elst.Entries
	i := 0
	for ; i < len(entries); i++ {
		mt := editMediaTime(td.elst, entries[i])
		if mt < -1 {
			return 0, "", p.errTrack(fault.CodeNegativeEdit, td.elstOff, trackID,
				"edit list entry %d media_time %d is invalid", i, mt)
		}
		if mt == -1 {
			dur := editDuration(td.elst, entries[i])
			durNS, err := scale.Nanos(int64(dur))
			if err != nil {
				return 0, "", p.errTrack(fault.CodeEditList, td.elstOff, trackID,
					"empty edit %d duration overflows", i)
			}
			emptyDelay += durNS
			continue
		}
		break
	}
	if i >= len(entries) {
		// Entire edit list is empty: nothing is presented.
		p.warn("track %d edit list has only empty edits", trackID)
		return 0, "empty edit list on track %d: media suppressed", nil
	}
	mediaTime := editMediaTime(td.elst, entries[i])
	if mediaTime < 0 {
		return 0, "", p.errTrack(fault.CodeNegativeEdit, td.elstOff, trackID,
			"negative media_time %d on track %d", mediaTime, trackID)
	}
	originNS, err := scale.Nanos(mediaTime)
	if err != nil {
		return 0, "", p.errTrack(fault.CodeEditList, td.elstOff, trackID,
			"edit media_time overflows for track %d", trackID)
	}
	originNS -= emptyDelay
	if len(entries) > i+1 {
		p.warn("track %d has %d trailing edit entries; only the first media edit is applied",
			trackID, len(entries)-i-1)
	}
	return originNS, "", nil
}

func editMediaTime(elst *gomp4.Elst, e gomp4.ElstEntry) int64 {
	if elst.GetVersion() == 1 {
		return e.MediaTimeV1
	}
	return int64(e.MediaTimeV0)
}

func editDuration(elst *gomp4.Elst, e gomp4.ElstEntry) uint64 {
	if elst.GetVersion() == 1 {
		return e.SegmentDurationV1
	}
	return uint64(e.SegmentDurationV0)
}

// dropLeading removes the first n entries from every parallel array and
// renumbers the retained packets from zero, so emitted packet indices are
// contiguous on the unified timeline.
func (t *sampleTable) dropLeading(n int64) {
	if n <= 0 || n > t.count {
		return
	}
	t.dts = append([]int64(nil), t.dts[n:]...)
	t.pts = append([]int64(nil), t.pts[n:]...)
	t.dur = append([]int64(nil), t.dur[n:]...)
	t.sizes = append([]uint32(nil), t.sizes[n:]...)
	t.offsets = append([]int64(nil), t.offsets[n:]...)
	t.sync = append([]bool(nil), t.sync[n:]...)
	t.count -= n
}
