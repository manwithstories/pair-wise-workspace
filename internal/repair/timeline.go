// Package repair reconstructs trustworthy timelines and recovers RTP
// losses. It shares clock for exact rational conversion and fault for
// localized damage records.
package repair

import (
	"sort"

	"archivectl/internal/clock"
	"archivectl/internal/fault"
	"archivectl/internal/scan"
)

// Segment is one manifest-ordered fragment input for a track.
type Segment struct {
	ID            string
	Index         int
	TrackID       string
	Timescale     uint32
	Fragment      *scan.Fragment
	Discontinuity bool
}

// Breakpoint describes a timeline boundary encountered during repair.
type Breakpoint struct {
	SegmentIndex int    `json:"segment_index"`
	Kind         string `json:"kind"`
	Detail       string `json:"detail,omitempty"`
}

// NormalizedSample is a sample laid onto clock.CommonTimescale.
type NormalizedSample struct {
	SegmentIndex  int    `json:"segment_index"`
	SampleIndex   int    `json:"sample_index"`
	DTS           int64  `json:"dts_common"`
	PTS           int64  `json:"pts_common"`
	Duration      int64  `json:"duration_common"`
	Size          uint32 `json:"size"`
	Sync          bool   `json:"sync"`
	PayloadOffset int64  `json:"payload_offset"`
}

// SegmentRange records the normalized span and the corrected native
// tfdt value the executor must write.
type SegmentRange struct {
	SegmentIndex        int    `json:"segment_index"`
	ID                  string `json:"id"`
	StartCommon         int64  `json:"start_common"`
	EndCommon           int64  `json:"end_common"`
	SampleCount         int    `json:"sample_count"`
	SyncSampleIndexes   []int  `json:"sync_sample_indexes,omitempty"`
	CorrectedNativeBase int64  `json:"corrected_native_base"`
	Isolated            bool   `json:"isolated,omitempty"`
	NativeTimescale     uint32 `json:"native_timescale"`
}

// TrackTimeline is the repaired, monotonically ordered timeline.
type TrackTimeline struct {
	TrackID        string             `json:"track_id"`
	Timescale      uint32             `json:"timescale"`
	Samples        []NormalizedSample `json:"samples"`
	Ranges         []SegmentRange     `json:"ranges"`
	Breakpoints    []Breakpoint       `json:"breakpoints,omitempty"`
	IsolatedRanges []SegmentRange     `json:"isolated_ranges,omitempty"`
	Record         *fault.Record      `json:"-"`
}

// RepairTrack orders segments by manifest index, resolves 32-bit tfdt
// wrap, rebases the non-zero start to the common origin and lays
// samples by their intact durations. DTS is therefore strictly
// monotonic while every PTS-DTS composition offset is preserved
// exactly. Unexplained backward jumps isolate the segment instead of
// guessing timestamps.
func RepairTrack(trackID string, timescale uint32, segments []Segment) *TrackTimeline {
	tt := &TrackTimeline{TrackID: trackID, Timescale: timescale, Record: fault.NewRecord()}
	ordered := append([]Segment(nil), segments...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	scale := clock.CommonTimescale
	toCommon := func(native int64) int64 {
		v, ok := clock.RescaleExact(native, timescale, scale)
		if !ok {
			// All fixture timescales divide CommonTimescale; record if not.
			tt.Record.Add(fault.New("timescale_inexact", trackID, native, fault.LevelAmbiguous,
				"timestamp does not rescale exactly onto common clock"))
			return clock.RescaleRound(native, timescale, scale)
		}
		return v
	}

	var cursorCommon int64
	var prevRawEndExtended int64
	havePrev := false
	wrapCycles := int64(0)

	for _, seg := range ordered {
		frag := seg.Fragment
		if frag == nil || frag.Truncated || len(frag.Samples) == 0 {
			tt.Record.Add(fault.New("segment_unusable", trackID, int64(seg.Index), fault.LevelFatal,
				"fragment missing, truncated or has no samples; quarantined"))
			tt.IsolatedRanges = append(tt.IsolatedRanges, SegmentRange{
				SegmentIndex: seg.Index, ID: seg.ID, Isolated: true, NativeTimescale: timescale,
			})
			continue
		}

		rawBase := int64(frag.TFDT.Base)
		width := int64(frag.TFDT.ValueWidth)
		rawDurations := int64(0)
		for _, s := range frag.Samples {
			rawDurations += int64(s.Duration)
		}

		extBase := rawBase
		switch {
		case seg.Discontinuity && havePrev:
			// Declared reset begins a fresh raw epoch; presentation output
			// still continues by intact sample durations.
			tt.Breakpoints = append(tt.Breakpoints, Breakpoint{
				SegmentIndex: seg.Index, Kind: "declared_discontinuity",
				Detail: "manifest-declared reset; presentation continues by durations",
			})
			wrapCycles = 0
			extBase = rawBase

		case seg.Index == 0 || !havePrev:
			tt.Breakpoints = append(tt.Breakpoints, Breakpoint{
				SegmentIndex: seg.Index, Kind: "nonzero_start",
				Detail: "rebased to common origin",
			})
			if width == 4 {
				wrapCycles = 0
			}

		default:
			// Lift a 32-bit tfdt into the correct wrap cycle.
			before := wrapCycles
			if width == 4 {
				tol := int64(timescale) / 10 // 100 ms jitter tolerance
				for rawBase+wrapCycles*seqMod32() < prevRawEndExtended-tol {
					wrapCycles++
				}
				extBase = rawBase + wrapCycles*seqMod32()
				if wrapCycles > before {
					tt.Breakpoints = append(tt.Breakpoints, Breakpoint{
						SegmentIndex: seg.Index, Kind: "wrap",
						Detail: "tfdt 32-bit cycle applied",
					})
				}
			}
			jump := extBase - prevRawEndExtended
			if jump < -int64(timescale)/10 || jump > int64(timescale)/10 {
				// Unexplained large discontinuity: isolate, do not guess.
				f := fault.New("ambiguous_timeline_jump", trackID, int64(seg.Index), fault.LevelAmbiguous,
					"undeclared non-monotonic tfdt jump; range isolated")
				tt.Record.Add(f)
				tt.Breakpoints = append(tt.Breakpoints, Breakpoint{
					SegmentIndex: seg.Index, Kind: "ambiguous_jump", Detail: f.Detail,
				})
				tt.IsolatedRanges = append(tt.IsolatedRanges, SegmentRange{
					SegmentIndex: seg.Index, ID: seg.ID, Isolated: true, NativeTimescale: timescale,
				})
				continue
			}
		}

		rawEnd := extBase + rawDurations
		startCommon := cursorCommon
		nativeBase, exact := clock.RescaleExact(startCommon, scale, timescale)
		if !exact {
			nativeBase = clock.RescaleRound(startCommon, scale, timescale)
		}
		rng := SegmentRange{
			SegmentIndex: seg.Index, ID: seg.ID, StartCommon: startCommon,
			CorrectedNativeBase: nativeBase, NativeTimescale: timescale,
			SyncSampleIndexes: append([]int(nil), frag.SyncSampleIndexes...),
		}

		var cum int64
		for _, s := range frag.Samples {
			durC := toCommon(int64(s.Duration))
			dts := startCommon + toCommon(cum)
			ctoC := toCommon(s.CompositionOffset)
			tt.Samples = append(tt.Samples, NormalizedSample{
				SegmentIndex: seg.Index, SampleIndex: s.Index,
				DTS: dts, PTS: dts + ctoC, Duration: durC, Size: s.Size,
				Sync: s.Sync, PayloadOffset: s.PayloadOffset,
			})
			cum += int64(s.Duration)
		}
		cursorCommon += toCommon(rawDurations)
		rng.EndCommon = cursorCommon
		rng.SampleCount = len(frag.Samples)
		tt.Ranges = append(tt.Ranges, rng)

		prevRawEndExtended = rawEnd
		havePrev = true
	}
	return tt
}

func seqMod32() int64 { return 1 << 32 }

// MaxAVSyncMillis returns the maximum absolute mismatch between two
// tracks at shared segment-boundary indices. Exact rational rescale
// makes aligned fixture boundaries differ by 0.
func MaxAVSyncMillis(a, b *TrackTimeline) float64 {
	starts := map[int]int64{}
	for _, r := range a.Ranges {
		starts[r.SegmentIndex] = r.StartCommon
	}
	worst := 0.0
	for _, r := range b.Ranges {
		if sa, ok := starts[r.SegmentIndex]; ok {
			if d := clock.MillisDiff(
				clock.New(sa, int64(clock.CommonTimescale)),
				clock.New(r.StartCommon, int64(clock.CommonTimescale)),
			); d > worst {
				worst = d
			}
		}
	}
	return worst
}
