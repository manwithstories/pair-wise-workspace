package parse

import (
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"avscan/internal/model"
)

// tableState holds the raw sample-table boxes of one track until the walk
// finishes and they can be combined into sample events.
type tableState struct {
	hasStts, hasStsc, hasStsz, hasOffsets bool
	stts                                  []mp4.SttsEntry
	stsc                                  []mp4.StscEntry
	sizes                                 []uint32
	constantSize                          uint32
	sync                                  []uint32 // 1-based, nil if no stss
	hasStss                               bool
	co64                                  []uint64
	stco                                  []uint32
	tableFaults                           []model.Fault
}

func newTableState() *tableState { return &tableState{} }

func (tb *tableState) readTable(h *mp4.ReadHandle, source string) {
	t := h.Path[len(h.Path)-1]
	box, _, err := h.ReadPayload()
	if err != nil {
		tb.tableFaults = append(tb.tableFaults,
			byteFault(h, source, t.String()+" payload unreadable: "+err.Error()))
		return
	}
	switch b := box.(type) {
	case *mp4.Stts:
		tb.hasStts = true
		tb.stts = b.Entries
		if uint64(b.EntryCount) != uint64(len(b.Entries)) {
			tb.tableFaults = append(tb.tableFaults, byteFault(h, source,
				"stts entry_count disagrees with payload"))
		}
	case *mp4.Stsc:
		tb.hasStsc = true
		tb.stsc = b.Entries
	case *mp4.Stsz:
		tb.hasStsz = true
		tb.constantSize = b.SampleSize
		if b.SampleSize == 0 {
			tb.sizes = b.EntrySize
			if uint64(b.SampleCount) != uint64(len(b.EntrySize)) {
				tb.tableFaults = append(tb.tableFaults, model.NewByteFault(
					model.FaultTruncatedTable, source,
					int64(h.BoxInfo.Offset),
					fmt.Sprintf("stsz declares %d sizes, payload has %d",
						b.SampleCount, len(b.EntrySize))))
			}
		} else {
			tb.sizes = make([]uint32, b.SampleCount)
			for i := range tb.sizes {
				tb.sizes[i] = b.SampleSize
			}
		}
	case *mp4.Stss:
		tb.hasStss = true
		tb.sync = b.SampleNumber
	case *mp4.Co64:
		tb.hasOffsets = true
		tb.co64 = b.ChunkOffset
	case *mp4.Stco:
		tb.hasOffsets = true
		tb.stco = b.ChunkOffset
	}
}

// sampleCount derives the declared sample count from stts, cross-checking stsz.
func (tb *tableState) sampleCount() (uint32, bool) {
	var n uint64
	for _, e := range tb.stts {
		n += uint64(e.SampleCount)
	}
	return uint32(n), n > 0
}

// syncSamples returns 1-based key-frame numbers, or nil when stss is absent.
func (tb *tableState) syncSamples() []uint32 {
	if !tb.hasStss {
		return nil
	}
	return append([]uint32(nil), tb.sync...)
}

// chunkOffsets normalises stco and co64 onto one uint64 slice.
func (tb *tableState) chunkOffsets() []uint64 {
	if len(tb.co64) > 0 {
		return append([]uint64(nil), tb.co64...)
	}
	out := make([]uint64, len(tb.stco))
	for i, v := range tb.stco {
		out[i] = uint64(v)
	}
	return out
}

// expandChunks turns stsc run descriptions into samples-per-chunk aligned
// with the chunk offset table.
func (tb *tableState) expandChunks(chunkCount int) ([]uint32, []model.Fault) {
	perChunk := make([]uint32, 0, chunkCount)
	var faults []model.Fault
	run := 0
	for chunk := 1; chunk <= chunkCount; chunk++ {
		for run+1 < len(tb.stsc) && tb.stsc[run+1].FirstChunk <= uint32(chunk) {
			run++
		}
		if len(tb.stsc) == 0 {
			faults = append(faults, model.NewByteFault(
				model.FaultTruncatedTable, "", 0, "empty stsc run table"))
			return nil, faults
		}
		perChunk = append(perChunk, tb.stsc[run].SamplesPerChunk)
	}
	return perChunk, faults
}

// buildEvents joins stts/stsc/stsz/co64 into SampleEvents on the nanosecond
// axis and validates every offset and length against the file boundaries.
func (tb *tableState) buildEvents(tr *model.Track, source string, fileSize int64) ([]model.SampleEvent, []model.Fault) {
	var faults []model.Fault
	faults = append(faults, tb.tableFaults...)
	count, ok := tb.sampleCount()
	if !ok || !tb.hasStsz || !tb.hasOffsets || !tb.hasStsc {
		faults = append(faults, model.Fault{
			Kind: model.FaultTruncatedTable, Source: source,
			TrackID: tr.ID, SampleIndex: -1,
			Message: "incomplete sample table (stts/stsc/stsz/offsets)",
		})
		return nil, faults
	}
	if len(tb.sizes) != int(count) {
		faults = append(faults, model.Fault{
			Kind: model.FaultTruncatedTable, Source: source,
			TrackID: tr.ID, SampleIndex: -1,
			Message: fmt.Sprintf("size count %d != stts count %d",
				len(tb.sizes), count),
		})
	}
	offsets := tb.chunkOffsets()
	perChunk, cf := tb.expandChunks(len(offsets))
	faults = append(faults, cf...)
	if len(perChunk) == 0 {
		return nil, faults
	}
	// Build a 0-based lookup of sync samples so events carry the key flag.
	syncSet := make(map[uint32]bool, len(tb.sync))
	for _, oneBased := range tb.sync {
		if oneBased >= 1 {
			syncSet[oneBased-1] = true
		}
	}

	events := make([]model.SampleEvent, 0, count)
	var mediaTick uint64 // running presentation time in track timescale
	var sampleIdx uint32
	for chunk, chunkOff := range offsets {
		var n uint32
		if chunk < len(perChunk) {
			n = perChunk[chunk]
		}
		cursor := chunkOff
		for k := uint32(0); k < n && sampleIdx < count; k++ {
			size := tb.sizes[sampleIdx]
			ev := model.SampleEvent{
				TrackID: tr.ID, Index: sampleIdx,
				Offset: cursor, Size: size,
				IsSync: syncSet[sampleIdx],
			}
			if !guardOffset(cursor, uint64(size), fileSize) {
				faults = append(faults, model.NewSampleFault(
					model.FaultOffsetOutOfRange, source, tr.ID,
					sampleIdx, &ev,
					fmt.Sprintf("offset %d + size %d exceeds file %d",
						cursor, size, fileSize)))
			} else if uint64(size) > uint64(fileSize) {
				faults = append(faults, model.NewSampleFault(
					model.FaultInvalidLength, source, tr.ID,
					sampleIdx, &ev,
					fmt.Sprintf("sample length %d larger than file", size)))
			}
			startNS, err := model.TicksToNS(mediaTick, tr.Timescale)
			if err == nil {
				ev.StartNS = startNS
			}
			delta := tb.deltaAt(sampleIdx)
			durNS, err := model.TicksToNS(uint64(delta), tr.Timescale)
			if err == nil {
				ev.DurationNS = durNS
			}
			events = append(events, ev)
			mediaTick += uint64(delta)
			// Guard against uint64 wrap when a corrupt size would otherwise
			// push the next cursor back to a low, seemingly-valid offset.
			if cursor > ^uint64(0)-uint64(size) {
				faults = append(faults, model.NewSampleFault(
					model.FaultInvalidLength, source, tr.ID, sampleIdx, &ev,
					"offset addition overflows uint64"))
				break
			}
			cursor += uint64(size)
			sampleIdx++
		}
	}
	return events, faults
}

// deltaAt returns the stts delta governing one 0-based sample.
func (tb *tableState) deltaAt(idx uint32) uint32 {
	var seen uint32
	for _, e := range tb.stts {
		if idx < seen+e.SampleCount {
			return e.SampleDelta
		}
		seen += e.SampleCount
	}
	if len(tb.stts) > 0 {
		return tb.stts[len(tb.stts)-1].SampleDelta
	}
	return 0
}
