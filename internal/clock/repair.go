package clock

import (
	"math"

	"bcast/internal/boxes"
	"bcast/internal/media"
)

// Repair converts every unique track to the shared time base and repairs
// regressed tfdt values using cumulative trun durations and RTP anchors.
func Repair(report *boxes.BatchReport, anchors map[uint32]map[uint32]*Anchor, opts RepairOptions) (*Result, error) {
	if report == nil {
		return nil, media.Corrupt("clock", "nil_scan_report", "", 0, -1, "scan report is nil")
	}
	res := &Result{SharedBase: media.Timebase(), Tracks: map[uint32]*TrackTimeline{}}
	byTrack := map[uint32][]*boxes.Fragment{}
	for _, f := range report.Unique {
		if f == nil || f.Isolated {
			continue
		}
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}

	for trackID, fs := range byTrack {
		tt, err := repairTrack(trackID, fs, anchors[trackID], res.SharedBase, opts)
		if err != nil {
			return nil, err
		}
		res.Tracks[trackID] = tt
		res.Fragments = append(res.Fragments, tt.Fragments...)
	}
	if len(res.Tracks) == 0 {
		return nil, media.Corrupt("clock", "no_repaired_tracks", report.Dir, 0, -1, "no unique fragments could be repaired")
	}
	return res, nil
}

func repairTrack(trackID uint32, fs []*boxes.Fragment, anchors map[uint32]*Anchor, shared media.Rational, opts RepairOptions) (*TrackTimeline, error) {
	if len(fs) == 0 {
		return nil, media.Corrupt("clock", "empty_track", "", trackID, -1, "track has no fragments")
	}
	ts := fs[0].Timescale
	if ts == 0 {
		return nil, media.Corrupt("clock", "zero_timescale", fs[0].Path, fs[0].Sequence, fs[0].MoofOffset, "track timescale is zero")
	}
	tb := media.TrackTimebase(ts)
	tt := &TrackTimeline{TrackID: trackID, Timescale: ts}
	seenSeq := map[uint32]string{}
	durations := make([]int64, len(fs))
	contStart := make([]int64, len(fs))
	rtpX := make([]int64, 0, len(fs))
	rtpY := make([]int64, 0, len(fs))
	unwrapped := map[uint32]int64{}
	var cumulative int64

	// Pass 1: lossless trun durations and RTP unwrapping/drift estimation.
	for i, f := range fs {
		if existing, ok := seenSeq[f.Sequence]; ok && existing != f.ContentSHA256 {
			return nil, media.Corrupt("clock", "conflicting_mfhd", f.Path, f.Sequence, f.MoofOffset,
				"mfhd %d reused by different content digests", f.Sequence)
		}
		seenSeq[f.Sequence] = f.ContentSHA256

		contStart[i] = int64(f.TFDT) // replaced below for i>0 by cumulative from first base
		if i == 0 {
			if f.TFDT > math.MaxInt64 {
				return nil, overflowErr(f)
			}
			cumulative = 0
			contStart[0] = int64(f.TFDT)
		} else {
			contStart[i] = contStart[0] + cumulative
		}
		var fragDur int64
		for _, s := range f.Samples {
			d, err := media.MulChecked(int64(s.Duration), 1)
			if err != nil {
				return nil, media.Corrupt("clock", "duration_overflow", f.Path, f.Sequence, f.MoofOffset, "%v", err)
			}
			if fragDur > math.MaxInt64-d {
				return nil, media.Corrupt("clock", "duration_sum_overflow", f.Path, f.Sequence, f.MoofOffset, "fragment duration overflow")
			}
			fragDur += d
		}
		durations[i] = fragDur
		if cumulative > math.MaxInt64-fragDur {
			return nil, media.Corrupt("clock", "timeline_overflow", f.Path, f.Sequence, f.MoofOffset, "cumulative duration overflow")
		}
		cumulative += fragDur

		if a := anchors[f.Sequence]; a != nil {
			clockTB := media.Rational{Num: 1, Den: int64(a.ClockRate)}
			x, err := media.ConvertTicks(contStart[i], tb, clockTB)
			if err != nil {
				return nil, media.Corrupt("clock", "anchor_conversion_overflow", f.Path, f.Sequence, f.MoofOffset, "%v", err)
			}
			expected := int64(a.RTP32)
			if prev, ok := unwrapped[prevSequence(fs, i)]; ok && i > 0 {
				delta, err := media.ConvertTicks(durations[i-1], tb, clockTB)
				if err == nil {
					expected = prev + delta
				}
			}
			u := UnwrapRTP(0, a.RTP32, expected)
			if i > 0 {
				if prev, ok := unwrapped[fs[i-1].Sequence]; ok {
					u = UnwrapRTP(prev, a.RTP32, expected)
				}
			}
			unwrapped[f.Sequence] = u
			rtpX = append(rtpX, x)
			rtpY = append(rtpY, u)
		}
	}
	tt.DriftPPM = estimateDriftPPM(rtpX, rtpY)
	scale := 1.0 + tt.DriftPPM/1_000_000.0
	if math.Abs(scale) < 1e-12 {
		scale = 1
	}

	// Pass 2: choose monotonic fragment bases and expand every sample.
	var prevEnd int64
	for i, f := range fs {
		start, actions, err := chooseStart(f, i, contStart[i], prevEnd, anchors[f.Sequence], unwrapped, rtpX, rtpY, scale, tb, opts)
		if err != nil {
			return nil, err
		}
		ft := &FragmentTimeline{
			OriginalFragment: uint32(f.No),
			FileOrdinal:      f.No,
			TrackID:          trackID,
			Sequence:         f.Sequence,
			Path:             f.Path,
			MoofOffset:       f.MoofOffset,
			ContentSHA256:    f.ContentSHA256,
			Timescale:        ts,
			StartDTS:         start,
			Actions:          actions,
		}
		cursor := start
		var sync []int
		for _, s := range f.Samples {
			if cursor > math.MaxInt64-int64(s.Duration) {
				return nil, media.Corrupt("clock", "dts_overflow", f.Path, f.Sequence, f.MoofOffset+s.PayloadOffset, "DTS overflow")
			}
			pts, err := media.MulChecked(cursor, 1)
			if err != nil {
				return nil, overflowErr(f)
			}
			if s.CompositionOffset > 0 && pts > math.MaxInt64-s.CompositionOffset {
				return nil, media.Corrupt("clock", "pts_overflow", f.Path, f.Sequence, f.MoofOffset+s.PayloadOffset, "PTS overflow")
			}
			pts += s.CompositionOffset
			sd, err := media.ConvertTicks(cursor, tb, shared)
			if err != nil {
				return nil, media.Corrupt("clock", "shared_dts_overflow", f.Path, f.Sequence, f.MoofOffset, "%v", err)
			}
			sp, err := media.ConvertTicks(pts, tb, shared)
			if err != nil {
				return nil, media.Corrupt("clock", "shared_pts_overflow", f.Path, f.Sequence, f.MoofOffset, "%v", err)
			}
			ft.Samples = append(ft.Samples, RepairedSample{
				SampleTiming: s,
				DTS:          cursor,
				PTS:          pts,
				SharedDTS:    sd,
				SharedPTS:    sp,
			})
			if s.Sync {
				sync = append(sync, s.Index)
			}
			cursor += int64(s.Duration)
		}
		ft.EndDTS = cursor
		ft.SyncIndexes = sync
		ss, err := media.ConvertTicks(ft.StartDTS, tb, shared)
		if err != nil {
			return nil, overflowErr(f)
		}
		se, err := media.ConvertTicks(ft.EndDTS, tb, shared)
		if err != nil {
			return nil, overflowErr(f)
		}
		ft.StartShared, ft.EndShared = ss, se
		if len(tt.Fragments) == 0 {
			tt.StartShared = ss
		}
		tt.EndShared = se
		tt.Fragments = append(tt.Fragments, ft)
		prevEnd = cursor
	}
	return tt, nil
}

func prevSequence(fs []*boxes.Fragment, i int) uint32 {
	if i == 0 {
		return 0
	}
	return fs[i-1].Sequence
}

func chooseStart(
	f *boxes.Fragment,
	i int,
	continuous int64,
	prevEnd int64,
	a *Anchor,
	unwrapped map[uint32]int64,
	rtpX, rtpY []int64,
	scale float64,
	tb media.Rational,
	opts RepairOptions,
) (int64, []string, error) {
	actions := []string{}
	if f.TFDT > math.MaxInt64 {
		return 0, nil, overflowErr(f)
	}
	tfdt := int64(f.TFDT)
	if i == 0 {
		actions = append(actions, "base_from_tfdt")
		return tfdt, actions, nil
	}
	if tfdt >= prevEnd {
		if tfdt > prevEnd {
			actions = append(actions, "preserved_gap_from_tfdt")
		} else {
			actions = append(actions, "monotonic_tfdt")
		}
		return tfdt, actions, nil
	}

	// tfdt moved backwards.  Prefer the independent RTP estimate.
	if a != nil && len(rtpX) >= 2 && math.Abs(scale) > 1e-12 {
		// Find this anchor's regression point by fragment value.  The slices
		// are tiny per track and remain ordered.
		for j := range rtpY {
			if j < len(rtpY) && rtpY[j] == unwrapped[f.Sequence] {
				clockTB := media.Rational{Num: 1, Den: int64(a.ClockRate)}
				xTrackUnits := rtpX[0] + int64(math.Round(float64(rtpX[j]-rtpX[0])/scale))
				start, err := media.ConvertTicks(xTrackUnits, clockTB, tb)
				if err == nil {
					actions = append(actions, "tfdt_regression_repaired_from_rtp")
					if start < prevEnd {
						start = prevEnd
						actions = append(actions, "rtp_start_clamped_to_monotonic_end")
					}
					return start, actions, nil
				}
			}
		}
	}
	if opts.AllowAnchorlessClamp {
		actions = append(actions, "tfdt_regression_clamped_to_previous_end")
		return prevEnd, actions, nil
	}
	return 0, nil, media.Corrupt("clock", "unrepairable_tfdt_regression", f.Path, f.Sequence, f.MoofOffset,
		"tfdt=%d precedes previous end=%d and no usable RTP anchor exists", tfdt, prevEnd)
}

func overflowErr(f *boxes.Fragment) error {
	return media.Corrupt("clock", "integer_overflow", f.Path, f.Sequence, f.MoofOffset, "timestamp conversion overflow")
}
