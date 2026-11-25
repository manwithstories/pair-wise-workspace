// Package isobmff analyzes MP4 box structure and sample tables: keyframe
// intervals, timestamp jumps, edit lists and truncated boxes. A truncated
// box only marks the affected range — remaining tracks are still analyzed.
package isobmff

import (
	"bytes"
	"fmt"
	"io"
	"math"

	mp4 "github.com/abema/go-mp4"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

// TrackInfo summarizes one parsed track.
type TrackInfo struct {
	Name        string
	Kind        string // "video" / "audio" / "other"
	Timescale   int64
	SampleCount uint64
	Duration    clock.Ratio // media duration from stts
	StartDTS    clock.Ratio // non-zero start from elst media_time
	Keyframes   int
	MaxKeyInt   clock.Ratio // largest keyframe interval
}

// Result is the outcome of analyzing one MP4 file.
type Result struct {
	Tracks []TrackInfo
	Faults *fault.Model
}

// Analyze parses data as one MP4 file named name.
func Analyze(name string, data []byte) (*Result, error) {
	a := &analyzer{name: name, size: int64(len(data)), faults: fault.NewModel()}
	if _, err := mp4.ReadBoxStructure(bytes.NewReader(data), a.handle); err != nil && err != io.EOF {
		a.faults.Add(fault.Span{Track: name, Code: fault.CodeTruncatedBox,
			Detail: fmt.Sprintf("box walk stopped: %v", err)})
	}
	res := &Result{Faults: a.faults}
	for i, tb := range a.tracks {
		res.Tracks = append(res.Tracks, a.finishTrack(tb, i))
	}
	return res, nil
}

type trackBuilder struct {
	id        uint32
	kind      string
	timescale int64
	stts      []mp4.SttsEntry
	stss      []uint32
	hasStss   bool
	stszCount uint64
	stszSize  uint32
	entrySize []uint32
	stco      []uint32
	stsc      []mp4.StscEntry
	elstMedia int64
	hasElst   bool
	truncated bool
}

type analyzer struct {
	name   string
	size   int64
	faults *fault.Model
	tracks []*trackBuilder
	cur    *trackBuilder
}

func (a *analyzer) trackName(tb *trackBuilder, idx int) string {
	if tb.id != 0 {
		return fmt.Sprintf("%s#%d", a.name, tb.id)
	}
	return fmt.Sprintf("%s#trak%d", a.name, idx)
}

func (a *analyzer) handle(h *mp4.ReadHandle) (interface{}, error) {
	switch h.BoxInfo.Type {
	case mp4.BoxTypeMoov(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
		mp4.BoxTypeStbl(), mp4.BoxTypeEdts():
		_, err := h.Expand()
		return nil, err
	case mp4.BoxTypeTrak():
		tb := &trackBuilder{kind: "other"}
		prev := a.cur
		a.cur = tb
		_, err := h.Expand()
		a.cur = prev
		a.tracks = append(a.tracks, tb)
		if err != nil {
			// Damage stays local to this trak; siblings keep parsing.
			tb.truncated = true
			a.faults.Add(fault.Span{Track: a.trackName(tb, len(a.tracks)-1),
				Code: fault.CodeTruncatedBox, Detail: fmt.Sprintf("truncated box in trak: %v", err)})
		}
		return nil, nil
	}
	if a.cur == nil {
		return nil, nil
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	tb := a.cur
	switch b := box.(type) {
	case *mp4.Tkhd:
		tb.id = b.TrackID
	case *mp4.Mdhd:
		tb.timescale = int64(b.Timescale)
	case *mp4.Hdlr:
		switch string(b.HandlerType[:]) {
		case "vide":
			tb.kind = "video"
		case "soun":
			tb.kind = "audio"
		}
	case *mp4.Stts:
		tb.stts = b.Entries
	case *mp4.Stss:
		tb.hasStss = true
		tb.stss = b.SampleNumber
	case *mp4.Stsz:
		tb.stszCount = uint64(b.SampleCount)
		tb.stszSize = b.SampleSize
		tb.entrySize = b.EntrySize
	case *mp4.Stco:
		tb.stco = b.ChunkOffset
	case *mp4.Stsc:
		tb.stsc = b.Entries
	case *mp4.Elst:
		if len(b.Entries) > 0 {
			tb.hasElst = true
			if b.GetVersion() == 1 {
				tb.elstMedia = b.Entries[0].MediaTimeV1
			} else {
				tb.elstMedia = int64(b.Entries[0].MediaTimeV0)
			}
		}
	}
	return nil, nil
}

// sampleRun maps a run of samples to their DTS start and delta.
type sampleRun struct{ firstSample, firstDTS, delta uint64 }

func (a *analyzer) finishTrack(tb *trackBuilder, idx int) TrackInfo {
	name := a.trackName(tb, idx)
	ti := TrackInfo{Name: name, Kind: tb.kind, Timescale: tb.timescale}
	if tb.timescale <= 0 {
		if !tb.truncated {
			a.faults.Add(fault.Span{Track: name, Code: fault.CodeTruncatedBox, Detail: "missing mdhd"})
		}
		return ti
	}
	var runs []sampleRun
	var dts, sample, prevDelta uint64
	for _, e := range tb.stts {
		delta, cnt := uint64(e.SampleDelta), uint64(e.SampleCount)
		if delta != 0 && cnt > math.MaxUint64/delta {
			a.faults.Add(fault.Span{Track: name, Code: fault.CodeOverflow, Detail: "stts entry overflows"})
			break
		}
		if cnt > 0 && prevDelta != 0 && delta != prevDelta && (delta > 2*prevDelta || prevDelta > 2*delta) {
			s, _ := clock.Ticks(int64(dts), tb.timescale)
			e2, _ := clock.Ticks(int64(dts+delta), tb.timescale)
			a.faults.Add(fault.Span{Track: name, Code: fault.CodeTimestampJump, Start: s, End: e2,
				Detail: fmt.Sprintf("sample delta %d -> %d", prevDelta, delta)})
		}
		if cnt > 0 {
			prevDelta = delta
		}
		runs = append(runs, sampleRun{sample, dts, delta})
		dts += delta * cnt
		sample += cnt
	}
	ti.SampleCount = sample
	ti.Duration, _ = clock.Ticks(int64(dts), tb.timescale)
	if tb.hasElst {
		ti.StartDTS, _ = clock.Ticks(tb.elstMedia, tb.timescale)
		if tb.elstMedia < 0 || uint64(tb.elstMedia) > dts {
			a.faults.Add(fault.Span{Track: name, Code: fault.CodeEditRange,
				Start: ti.StartDTS, End: ti.Duration,
				Detail: fmt.Sprintf("elst media_time %d outside [0,%d)", tb.elstMedia, dts)})
		}
	}
	sampleDTS := func(sn uint64) uint64 {
		lo, hi := 0, len(runs)
		for lo < hi {
			mid := (lo + hi) / 2
			if runs[mid].firstSample <= sn {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		r := runs[lo-1]
		return r.firstDTS + (sn-r.firstSample)*r.delta
	}
	if tb.hasStss && len(runs) > 0 {
		ti.Keyframes = len(tb.stss)
		var prevK uint64
		for i, kn := range tb.stss {
			kd := sampleDTS(uint64(kn) - 1)
			if i > 0 {
				if iv, err := clock.Ticks(int64(kd-prevK), tb.timescale); err == nil && iv.Cmp(ti.MaxKeyInt) > 0 {
					ti.MaxKeyInt = iv
				}
			}
			prevK = kd
		}
	}
	a.checkDataRanges(tb, name, sampleDTS, &ti, len(runs) > 0)
	return ti
}

// checkDataRanges flags the first chunk whose sample data extends past
// EOF (truncated mdat); the span covers only the affected range.
func (a *analyzer) checkDataRanges(tb *trackBuilder, name string, sampleDTS func(uint64) uint64, ti *TrackInfo, hasRuns bool) {
	if len(tb.stco) == 0 || len(tb.stsc) == 0 || tb.stszCount == 0 || !hasRuns {
		return
	}
	var sn uint64
	stscIdx := 0
	for ci, off := range tb.stco {
		for stscIdx+1 < len(tb.stsc) && uint64(tb.stsc[stscIdx+1].FirstChunk) <= uint64(ci)+1 {
			stscIdx++
		}
		spc := uint64(tb.stsc[stscIdx].SamplesPerChunk)
		var extent uint64
		if tb.stszSize != 0 {
			extent = spc * uint64(tb.stszSize)
		} else {
			for s := sn; s < sn+spc && s < tb.stszCount && s < uint64(len(tb.entrySize)); s++ {
				extent += uint64(tb.entrySize[s])
			}
		}
		if uint64(off)+extent > uint64(a.size) {
			start, _ := clock.Ticks(int64(sampleDTS(sn)), tb.timescale)
			a.faults.Add(fault.Span{Track: name, Code: fault.CodeTruncatedBox,
				Start: start, End: ti.Duration,
				Detail: fmt.Sprintf("sample data beyond EOF at chunk %d", ci)})
			return
		}
		sn += spc
		if sn >= tb.stszCount {
			return
		}
	}
}
