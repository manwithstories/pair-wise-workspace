package stream

import (
	"io"

	"github.com/abema/go-mp4"

	"mediacheck/internal/fault"
	"mediacheck/internal/timeline"
)

// Track describes one MP4 track's sample table on the shared ns base.
type Track struct {
	ID          uint32  `json:"id"`
	Timescale   uint32  `json:"timescale"`
	SampleCount uint32  `json:"sample_count"`
	DurationNs  int64   `json:"duration_ns"`
	Keyframes   []int64 `json:"keyframes_ns"` // sync-sample presentation times
	Chunks      []int64 `json:"chunks"`       // chunk file offsets
	IndexOK     bool    `json:"index_ok"`     // false when stts/stsz missing
}

// MP4Info is the result of a streaming box/sample-table scan.
type MP4Info struct {
	Tracks       []Track     `json:"tracks"`
	TrustedBytes int64       `json:"trusted_bytes"` // fully parsed prefix
	TotalBytes   int64       `json:"total_bytes"`
	Faults       *fault.List `json:"faults"`
}

// ScanMP4 walks the box tree of r (size = total length) collecting
// track sample tables. On tail truncation or a missing index it marks
// the trusted range, records a recoverable fault, and keeps whatever
// was read before the damage.
func ScanMP4(r io.ReadSeeker, size int64) *MP4Info {
	info := &MP4Info{TotalBytes: size, Faults: &fault.List{}}
	var raw []rawTrack
	cur := -1

	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		t := h.BoxInfo.Type
		end := int64(h.BoxInfo.Offset + h.BoxInfo.Size)
		// trust marks only fully verified bytes, never declared sizes
		trust := func() {
			if end <= size {
				info.TrustedBytes = max64(info.TrustedBytes, end)
			}
		}
		switch t {
		case mp4.BoxTypeMoov(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
			mp4.BoxTypeDinf(), mp4.BoxTypeStbl():
			return h.Expand()
		case mp4.BoxTypeTrak():
			raw = append(raw, rawTrack{})
			cur = len(raw) - 1
			return h.Expand()
		}
		if cur < 0 {
			trust()
			return nil, nil // box outside any trak (ftyp, mdat, ...)
		}
		rt := &raw[cur]
		box, _, err := h.ReadPayload()
		if err != nil {
			info.Faults.Addf(fault.CatStream, int64(h.BoxInfo.Offset), true,
				"unreadable %s payload: %v", t, err)
			return nil, nil // keep scanning siblings
		}
		trust()
		switch b := box.(type) {
		case *mp4.Tkhd:
			rt.id = b.TrackID
		case *mp4.Mdhd:
			rt.timescale = b.Timescale
			rt.duration = uint64(b.DurationV0) | uint64(b.DurationV1)
		case *mp4.Stts:
			rt.stts = b.Entries
		case *mp4.Stss:
			rt.stss = b.SampleNumber
			rt.hasStss = true
		case *mp4.Stsz:
			rt.sampleCount = b.SampleCount
		case *mp4.Stco:
			for _, o := range b.ChunkOffset {
				rt.chunks = append(rt.chunks, int64(o))
			}
		case *mp4.Co64:
			for _, o := range b.ChunkOffset {
				rt.chunks = append(rt.chunks, int64(o))
			}
		}
		return nil, nil
	}

	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		// Truncation or malformed box: keep the readable prefix.
		info.Faults.Addf(fault.CatStream, info.TrustedBytes, true,
			"box scan stopped at %d of %d bytes: %v",
			info.TrustedBytes, size, err)
	}
	if info.TrustedBytes < size {
		info.Faults.Addf(fault.CatStream, info.TrustedBytes, true,
			"trusted range is [0,%d) of %d bytes", info.TrustedBytes, size)
	}
	for i := range raw {
		info.Tracks = append(info.Tracks, raw[i].finalize(info.Faults))
	}
	return info
}

// rawTrack accumulates sample-table boxes before finalization.
type rawTrack struct {
	id          uint32
	timescale   uint32
	duration    uint64
	sampleCount uint32
	stts        []mp4.SttsEntry
	stss        []uint32
	hasStss     bool
	chunks      []int64
}

func (rt *rawTrack) finalize(fl *fault.List) Track {
	tr := Track{
		ID: rt.id, Timescale: rt.timescale,
		SampleCount: rt.sampleCount, Chunks: rt.chunks, IndexOK: true,
	}
	if rt.timescale == 0 {
		tr.IndexOK = false
		fl.Addf(fault.CatStream, -1, true, "track %d: missing mdhd timescale", rt.id)
		return tr
	}
	tr.DurationNs = int64(rt.duration) * timeline.NanosPerSecond / int64(rt.timescale)
	if len(rt.stts) == 0 || rt.sampleCount == 0 {
		tr.IndexOK = false
		fl.Addf(fault.CatStream, -1, true,
			"track %d: incomplete sample index (stts=%d stss=%v)",
			rt.id, len(rt.stts), rt.hasStss)
		return tr
	}
	syncs := rt.stss
	if !rt.hasStss {
		// No stss box: every sample is a sync sample per ISO 14496-12.
		syncs = make([]uint32, rt.sampleCount)
		for i := range syncs {
			syncs[i] = uint32(i + 1)
		}
	}
	tr.Keyframes = keyframeTimes(rt.stts, syncs, rt.timescale)
	return tr
}

// keyframeTimes maps 1-based sync sample numbers to presentation times
// in nanoseconds by walking the stts run-length table.
func keyframeTimes(stts []mp4.SttsEntry, syncs []uint32, timescale uint32) []int64 {
	out := make([]int64, 0, len(syncs))
	var sample, t uint64 // next sample index (0-based), its time in timescale units
	entry, runLeft := 0, uint32(0)
	for _, sn := range syncs {
		target := uint64(sn) - 1
		for sample < target && entry < len(stts) {
			if runLeft == 0 {
				runLeft = stts[entry].SampleCount
			}
			step := uint64(1)
			if remain := target - sample; uint64(runLeft) > remain {
				step = remain
			} else {
				step = uint64(runLeft)
			}
			t += step * uint64(stts[entry].SampleDelta)
			sample += step
			runLeft -= uint32(step)
			if runLeft == 0 {
				entry++
			}
		}
		if sample != target {
			break // sync sample beyond stts coverage: index damaged
		}
		out = append(out, int64(t)*timeline.NanosPerSecond/int64(timescale))
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
