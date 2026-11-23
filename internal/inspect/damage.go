package inspect

import (
	"fmt"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

// DamageRange is one damaged media window on the shared clock, carrying both
// the unified position and the original container offset.
type DamageRange struct {
	StartUs       int64           `json:"start_us"`
	EndUs         int64           `json:"end_us"`
	TrackID       uint32          `json:"track_id"`
	Kind          clock.ErrorKind `json:"kind"`
	Segment       int             `json:"segment"`
	Offset        uint64          `json:"offset"`
	PositionKnown bool            `json:"position_known"`
	Detail        string          `json:"detail,omitempty"`
}

// DurationMS returns the damaged window length in milliseconds.
func (d DamageRange) DurationMS() float64 {
	if d.EndUs <= d.StartUs {
		return 0
	}
	return float64(d.EndUs-d.StartUs) / float64(clock.UsPerMS)
}

// DamageLog accumulates damaged ranges across segments of one representation.
// Ranges use the unified clock; when the start is unknown the range is kept
// with PositionKnown=false and excluded from media-time arithmetic.
type DamageLog struct {
	trackID    uint32
	nominalUs  int64
	ranges     []DamageRange
	Errors     *clock.ErrorModel
	trailEndUs int64
	haveTrail  bool
}

// NewDamageLog returns a damage log. nominalFrameUs closes ranges whose
// following sample is unavailable.
func NewDamageLog(trackID uint32, nominalFrameUs int64, model *clock.ErrorModel) *DamageLog {
	if nominalFrameUs <= 0 {
		nominalFrameUs = 40_000
	}
	return &DamageLog{trackID: trackID, nominalUs: nominalFrameUs, Errors: model}
}

// Add records a damaged range [startUs, endUs) pinned to a container offset.
func (l *DamageLog) Add(startUs, endUs int64, kind clock.ErrorKind, seg int, offset uint64, known bool, detail string) {
	if !known {
		l.ranges = append(l.ranges, DamageRange{
			Kind: kind, Segment: seg, Offset: offset, Detail: detail, TrackID: l.trackID,
		})
		if l.Errors != nil {
			l.Errors.Record(clock.LocatedError{
				Kind: kind, AtUs: 0, PositionKnown: false, TrackID: l.trackID,
				Offset: offset, Segment: seg, Detail: detail,
			})
		}
		return
	}
	if endUs <= startUs {
		endUs = startUs + l.nominalUs
	}
	l.ranges = append(l.ranges, DamageRange{
		StartUs: startUs, EndUs: endUs, TrackID: l.trackID, Kind: kind,
		Segment: seg, Offset: offset, PositionKnown: true, Detail: detail,
	})
	if l.Errors != nil {
		l.Errors.Record(clock.LocatedError{
			Kind: kind, AtUs: startUs, PositionKnown: true, TrackID: l.trackID,
			Offset: offset, Segment: seg, Detail: detail,
		})
	}
	l.noteTrail(endUs)
}

func (l *DamageLog) noteTrail(endUs int64) {
	if !l.haveTrail || endUs > l.trailEndUs {
		l.trailEndUs = endUs
		l.haveTrail = true
	}
}

// ObserveSample extends the known-good frontier so a later unmappable error
// can be anchored to the last trustworthy media time.
func (l *DamageLog) ObserveSample(startUs, endUs int64, known bool) {
	if !known {
		return
	}
	if !l.haveTrail || startUs > l.trailEndUs {
		l.trailEndUs = startUs
		l.haveTrail = true
	}
	l.noteTrail(endUs)
}

// Ranges returns recorded damage ranges in encounter (segment) order.
func (l *DamageLog) Ranges() []DamageRange { return l.ranges }

// Coverage returns only the mappable ranges, merged, for cross-checks.
func (l *DamageLog) Coverage() clock.Coverage {
	var ivs []clock.Interval
	for _, r := range l.ranges {
		if r.PositionKnown && r.EndUs > r.StartUs {
			ivs = append(ivs, clock.Interval{StartUs: r.StartUs, EndUs: r.EndUs})
		}
	}
	return clock.Merge(ivs)
}

// FragmentState tracks mfhd/tfdt continuity across a representation. It is
// the single source of truth for gap/duplicate/regression classification, and
// is reused by the repair stage so summaries cannot disagree.
type FragmentState struct {
	trackID     uint32
	timeline    *clock.Timeline
	lastSeq     uint32
	haveSeq     bool
	lastBaseUs  int64
	lastBase    uint64
	haveBase    bool
	lastEndUs   int64
	duplicates  int
	gaps        int
	regressions int
}

// NewFragmentState returns continuity state bound to the shared timeline.
func NewFragmentState(trackID uint32, tl *clock.Timeline) *FragmentState {
	return &FragmentState{trackID: trackID, timeline: tl}
}

// Counters exposes continuity damage counts.
func (f *FragmentState) Counters() (gaps, duplicates, regressions int) {
	return f.gaps, f.duplicates, f.regressions
}

// ObserveFragment checks mfhd sequence continuity, tfdt monotonicity, and
// sample truncation, writing each damage window to log.
func (f *FragmentState) ObserveFragment(frag cmaf.Fragment, seg int, log *DamageLog) {
	start, known := f.fragmentStart(frag)
	if frag.HasMfhd {
		switch {
		case f.haveSeq && frag.SequenceNumber == f.lastSeq:
			f.duplicates++
			log.Add(start, start, clock.ErrMfhdDuplicate, seg, frag.MoofOffset, known,
				fmt.Sprintf("mfhd sequence %d repeated", frag.SequenceNumber))
		case f.haveSeq && frag.SequenceNumber < f.lastSeq:
			f.gaps++
			log.Add(start, start, clock.ErrMfhdGap, seg, frag.MoofOffset, known,
				fmt.Sprintf("mfhd sequence regressed %d -> %d", f.lastSeq, frag.SequenceNumber))
		case f.haveSeq && frag.SequenceNumber > f.lastSeq+1:
			f.gaps++
			log.Add(start, start, clock.ErrMfhdGap, seg, frag.MoofOffset, known,
				fmt.Sprintf("mfhd sequence jumped %d -> %d (%d missing)",
					f.lastSeq, frag.SequenceNumber, frag.SequenceNumber-f.lastSeq-1))
		}
		f.lastSeq = frag.SequenceNumber
		f.haveSeq = true
	}

	if base, ok := frag.TfdtTicks[f.trackID]; ok {
		baseUs, err := f.timeline.ToUs(f.trackID, base)
		if err == nil && f.haveBase && baseUs < f.lastBaseUs {
			f.regressions++
			log.Add(baseUs, f.lastBaseUs, clock.ErrTfdtRegression, seg, frag.MoofOffset, true,
				fmt.Sprintf("tfdt %d regressed below previous %d ticks", base, f.lastBase))
		}
		if err == nil {
			f.lastBaseUs = baseUs
			f.lastBase = base
			f.haveBase = true
		}
	}

	prevEnd := f.lastEndUs
	for _, sm := range frag.Samples {
		sStart, sKnown := f.sampleStart(sm)
		if sm.Truncated {
			end := sStart
			if sm.TimeKnown {
				end = sStart + int64(sm.DurationTicks)*1_000_000/int64(maxTimescale(f.timeline, sm.TrackID))
			}
			log.Add(sStart, end, clock.ErrSampleOutOfBounds, seg, sm.Offset, sKnown,
				fmt.Sprintf("sample %d of %d bytes crosses the segment end", sm.Index, sm.Size))
		}
		if sm.TimeKnown {
			// A gap between this fragment's tfdt and the previous known end
			// means media vanished between segments.
			if prevEnd > 0 && sm.Index == 0 && sStart > prevEnd+int64(clock.UsPerMS) {
				log.Add(prevEnd, sStart, clock.ErrSegmentTruncated, seg, frag.MoofOffset, true,
					fmt.Sprintf("media gap of %d ms before fragment", (sStart-prevEnd)/1000))
			}
		}
		prevEnd = sStart + l2us(sm.DurationTicks, f.timeline, sm.TrackID)
		f.lastEndUs = prevEnd
	}
}

func (f *FragmentState) fragmentStart(frag cmaf.Fragment) (int64, bool) {
	if base, ok := frag.TfdtTicks[f.trackID]; ok {
		if us, err := f.timeline.ToUs(f.trackID, base); err == nil {
			return us, true
		}
	}
	if f.haveBase {
		return f.lastBaseUs, true
	}
	return 0, false
}

func (f *FragmentState) sampleStart(sm cmaf.Sample) (int64, bool) {
	if !sm.TimeKnown {
		return 0, false
	}
	us, err := f.timeline.ToUs(sm.TrackID, sm.DecodeTimeTicks)
	if err != nil {
		return 0, false
	}
	return us, true
}

func maxTimescale(tl *clock.Timeline, trackID uint32) uint32 {
	if tc, ok := tl.Track(trackID); ok && tc.Timescale > 0 {
		return tc.Timescale
	}
	return 1
}

func l2us(ticks uint32, tl *clock.Timeline, trackID uint32) int64 {
	ts := maxTimescale(tl, trackID)
	return int64(ticks) * 1_000_000 / int64(ts)
}
