package mp4

import (
	"math/big"
	"sort"

	"mediad/internal/media"
)

// RepairReport records deterministic table repairs.
type RepairReport struct {
	Tracks         map[uint32]int `json:"samples_repaired"`
	TimestampFixes int            `json:"timestamp_fixes"`
	OffsetFixes    int            `json:"offset_fixes"`
	EditFixes      int            `json:"edit_fixes"`
	Faults         []*media.Fault `json:"faults"`
}

// RepairTimetables fills missing durations, removes PTS regressions, normalizes
// edit lists, and marks sample offsets for deterministic rebuilding.
func RepairTimetables(p *media.Project) RepairReport {
	rep := RepairReport{Tracks: map[uint32]int{}}
	for _, t := range p.Tracks {
		if t == nil {
			continue
		}
		defaultDelta := defaultDuration(t)
		for i := range t.Samples {
			s := &t.Samples[i]
			if s.Duration == 0 {
				s.Duration = defaultDelta
				rep.TimestampFixes++
				rep.Tracks[t.ID]++
			}
		}

		// Rebuild the monotonic decode clock from repaired stts durations.
		var dts int64
		for i := range t.Samples {
			t.Samples[i].DTS = dts
			dts += int64(t.Samples[i].Duration)
		}

		if ptsRegression(t) {
			for i := range t.Samples {
				t.Samples[i].PTS = t.Samples[i].DTS // low-latency profile: no negative ctts
			}
			f := media.NewFault(media.CodeTimestampRegression, "normalized regressing composition timeline to DTS").
				WithTrack(t.ID).WithBox("ctts").WithBasis("low-latency monotonic-PTS profile")
			t.Faults = append(t.Faults, f)
			p.Faults = append(p.Faults, f)
			rep.TimestampFixes++
		} else {
			for i := range t.Samples {
				// Keep ctts offsets bounded and non-negative after repair.
				if t.Samples[i].PTS < t.Samples[i].DTS {
					t.Samples[i].PTS = t.Samples[i].DTS
					rep.TimestampFixes++
				}
			}
		}

		edits, editFaults := RepairEdits(t.ID, t.Edits, dts)
		if len(editFaults) > 0 {
			t.Edits = edits
			rep.EditFixes += len(editFaults)
			t.Faults = append(t.Faults, editFaults...)
			p.Faults = append(p.Faults, editFaults...)
		}

		// Offsets are reassigned deterministically by Build. Count parsed
		// offset faults first, then invalidate every stale stco value.
		for _, f := range t.Faults {
			if f != nil && f.Code == media.CodeBadSampleOffset {
				rep.OffsetFixes++
			}
		}
		for i := range t.Samples {
			t.Samples[i].Offset = -1
		}
		rep.Faults = append(rep.Faults, t.Faults...)
	}
	return rep
}

func ptsRegression(t *media.Track) bool {
	for i := 1; i < len(t.Samples); i++ {
		if t.Samples[i].PTS < t.Samples[i-1].PTS {
			return true
		}
	}
	return false
}

func defaultDuration(t *media.Track) uint32 {
	counts := map[uint32]int{}
	for _, s := range t.Samples {
		if s.Duration > 0 {
			counts[s.Duration]++
		}
	}
	type candidate struct {
		d     uint32
		count int
	}
	var cs []candidate
	for d, n := range counts {
		cs = append(cs, candidate{d, n})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].count != cs[j].count {
			return cs[i].count > cs[j].count
		}
		return cs[i].d < cs[j].d
	})
	if len(cs) > 0 {
		return cs[0].d
	}
	if t.Timescale > 0 {
		if t.Kind == media.KindAudio {
			return t.Timescale / 100 // 10 ms fallback
		}
		return t.Timescale / 30 // 30 fps fallback
	}
	return 1
}

// RepairEdits removes impossible media times and zero-length edits. It never
// silently invents an offset: media_time zero is the documented start basis.
func RepairEdits(trackID uint32, edits []media.EditEntry, trackDuration int64) ([]media.EditEntry, []*media.Fault) {
	if len(edits) == 0 {
		return nil, nil
	}
	var out []media.EditEntry
	var faults []*media.Fault
	for _, e := range edits {
		if e.MediaTime < -1 || (e.MediaTime == -1 && e.SegmentDuration == 0) {
			faults = append(faults, media.NewFault(media.CodeEditListConflict, "invalid empty edit media_time=%d duration=%d", e.MediaTime, e.SegmentDuration).
				WithTrack(trackID).WithBox("elst").WithOriginal("media_time< -1 or zero empty duration").
				WithBasis("dropped invalid edit"))
			continue
		}
		if e.MediaTime < 0 {
			// Valid MP4 empty edit; preserve it.
			out = append(out, e)
			continue
		}
		if trackDuration > 0 && e.MediaTime > trackDuration {
			faults = append(faults, media.NewFault(media.CodeEditListConflict, "edit media_time %d exceeds track duration %d", e.MediaTime, trackDuration).
				WithTrack(trackID).WithBox("elst").
				WithOriginal("media_time_above_duration").
				WithBasis("clamped media_time to zero"))
			e.MediaTime = 0
		}
		if e.SegmentDuration == 0 {
			e.SegmentDuration = uint64(trackDuration)
			faults = append(faults, media.NewFault(media.CodeEditListConflict, "zero edit duration").
				WithTrack(trackID).WithBox("elst").WithBasis("used track duration"))
		}
		if e.MediaRate == 0 {
			e.MediaRate = 1
		}
		out = append(out, e)
	}
	return out, faults
}

// NormalizeTimestamps unwraps modulo-window values and repairs missing or
// regressing timestamps. A zero window disables wrap arithmetic. Missing values
// are represented by nil. The result is strictly non-decreasing and uses step
// when evidence is absent.
func NormalizeTimestamps(values []*int64, window uint64, step int64) ([]int64, []media.Fault) {
	if step <= 0 {
		step = 1
	}
	out := make([]int64, len(values))
	var faults []media.Fault
	prev := int64(0)
	havePrev := false
	for i, v := range values {
		switch {
		case v == nil:
			cur := prev + step
			out[i] = cur
			prev = cur
			faults = append(faults, *media.NewFault(media.CodeTimestampMissing, "filled timestamp %d with step %d", i, step))
			havePrev = true
		case !havePrev:
			out[i] = *v
			prev = *v
			havePrev = true
		default:
			cur := *v
			if window > 0 {
				w := new(big.Int).SetUint64(window)
				curBig := big.NewInt(cur)
				prevBig := big.NewInt(prev)
				k := new(big.Int).Sub(prevBig, curBig)
				k.Quo(k, w)
				k.Add(k, big.NewInt(1))
				candidate := new(big.Int).Add(curBig, new(big.Int).Mul(k, w))
				// candidate is first wrapped value > prev. If the forward gap
				// is at most half the wrap window, treat it as a wrap.
				gap := new(big.Int).Sub(candidate, prevBig)
				half := new(big.Int).Rsh(w, 1)
				if gap.Sign() >= 0 && gap.Cmp(half) <= 0 && candidate.IsInt64() {
					cur = candidate.Int64()
					if cur != *v {
						faults = append(faults, *media.NewFault(media.CodeTimestampWrap, "unwrapped timestamp %d from %d to %d", i, *v, cur).
							WithOriginal("modulo window").WithBasis("nearest monotonic wrap candidate"))
					}
				}
			}
			if cur < prev {
				cur = prev + step
				faults = append(faults, *media.NewFault(media.CodeTimestampRegression, "repaired timestamp %d to %d", i, cur).
					WithOriginal("backward value").WithBasis("previous timestamp plus median step"))
			}
			out[i] = cur
			prev = cur
		}
	}
	return out, faults
}
