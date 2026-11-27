// Package align maps every camera onto one shared wall-clock timeline using
// each container's creation_time, then plans common segment cut points that
// snap to IDR keyframes. All arithmetic stays on the rational media clock so
// variable frame rates are handled exactly, never via float frame counting.
package align

import (
	"fmt"
	"sort"
	"time"

	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
)

// Camera is one input angle bound to its parsed video track and the wall-clock
// instant at which its media time origin begins (mvhd creation_time).
type Camera struct {
	Name     string
	Path     string
	Creation time.Time
	Track    *mp4.TrackIndex

	offset clock.Rat // shared-time seconds between Epoch and media time zero
}

// Frame is one presentation-ordered sample located on the shared timeline.
type Frame struct {
	Sample int
	At     clock.Rat // shared time in seconds since Timeline.Epoch
	PTS    clock.Rat // track media time
	Dur    clock.Rat
	Sync   bool
}

// Timeline is the merged, intersecting view of every camera.
type Timeline struct {
	Epoch   time.Time
	Cameras []*Camera
	Frames  map[string][]Frame
	Start   clock.Rat // common coverage start (latest camera origin)
	End     clock.Rat // common coverage end (earliest camera tail)

	// FrameTol is one frame on the fastest camera: the largest cross-camera
	// cut deviation the publisher is allowed to accept.
	FrameTol clock.Rat
}

// Build maps all cameras onto one timeline and intersects their coverage.
func Build(cams []*Camera) (*Timeline, error) {
	if len(cams) == 0 {
		return nil, media.New(media.PhaseAlign, media.KindBounds, "", -1,
			"cannot align zero cameras")
	}
	epoch := cams[0].Creation
	for _, c := range cams[1:] {
		if c.Creation.Before(epoch) {
			epoch = c.Creation
		}
	}
	tl := &Timeline{Epoch: epoch, Cameras: cams, Frames: map[string][]Frame{}}
	for _, c := range cams {
		off, err := ratFromDuration(c.Creation.Sub(epoch))
		if err != nil {
			return nil, media.New(media.PhaseAlign, media.KindOverflow, c.Path, -1,
				"creation_time offset overflow").Wrap(err)
		}
		c.offset = off
		if c.Track == nil || len(c.Track.Samples) == 0 {
			return nil, media.New(media.PhaseAlign, media.KindTruncated, c.Path, -1,
				"camera has no indexed video samples")
		}
		frames := make([]Frame, 0, len(c.Track.Samples))
		for _, idx := range presentationOrder(c.Track.Samples) {
			s := c.Track.Samples[idx]
			at, err := off.Add(s.PTS)
			if err != nil {
				return nil, media.New(media.PhaseAlign, media.KindOverflow, c.Path, -1,
					"shared timestamp overflow").Wrap(err)
			}
			frames = append(frames, Frame{
				Sample: idx, At: at, PTS: s.PTS, Dur: s.Duration, Sync: s.Sync,
			})
		}
		tl.Frames[c.Name] = frames
	}
	// Intersect coverage: latest first frame to earliest tail.
	start := tl.Frames[cams[0].Name][0].At
	end, err := tail(tl.Frames[cams[0].Name])
	if err != nil {
		return nil, err
	}
	for _, c := range cams[1:] {
		fs := tl.Frames[c.Name]
		if fs[0].At.Cmp(start) > 0 {
			start = fs[0].At
		}
		tailEnd, err := tail(fs)
		if err != nil {
			return nil, err
		}
		if tailEnd.Cmp(end) < 0 {
			end = tailEnd
		}
	}
	if end.Cmp(start) <= 0 {
		return nil, media.New(media.PhaseAlign, media.KindBounds, "", -1,
			"cameras share no overlapping time range")
	}
	tl.Start, tl.End = start, end
	tol, err := tl.crossFrameTolerance()
	if err != nil {
		return nil, err
	}
	tl.FrameTol = tol
	return tl, nil
}

// SampleRange identifies a half-open run of samples on one camera.
type SampleRange struct {
	First     int // decode index, inclusive
	Last      int // decode index, inclusive
	Keyframe  int // decode index of the IDR at the cut
	KfAt      clock.Rat
}

// Cut is one common boundary across all cameras.
type Cut struct {
	Index  int
	At     clock.Rat // shared time of the cut start
	Till   clock.Rat // shared time of the next cut (or timeline end)
	Ranges map[string]SampleRange
}

// Plan lays out common cuts of segmentDur, snapping each camera to its IDR and
// rejecting the whole publish when any camera deviates by more than one frame.
func (t *Timeline) Plan(segmentDur clock.Rat) ([]Cut, error) {
	if segmentDur.Num <= 0 {
		return nil, media.New(media.PhaseAlign, media.KindBadTimebase, "", -1,
			"segment duration must be positive")
	}
	span, err := t.End.Sub(t.Start)
	if err != nil {
		return nil, err
	}
	count := span.Num * segmentDur.Den / (segmentDur.Num * span.Den)
	if count < 1 {
		return nil, media.New(media.PhaseAlign, media.KindBounds, "", -1,
			"common timeline shorter than one segment")
	}
	cuts := make([]Cut, 0, count)
	for k := int64(0); k < count; k++ {
		step, err := segmentDur.Mul(k, 1)
		if err != nil {
			return nil, err
		}
		at, err := t.Start.Add(step)
		if err != nil {
			return nil, err
		}
		nextStep, err := segmentDur.Mul(k+1, 1)
		if err != nil {
			return nil, err
		}
		till, err := t.Start.Add(nextStep)
		if err != nil {
			return nil, err
		}
		if till.Cmp(t.End) > 0 {
			till = t.End
		}
		cut := Cut{Index: int(k), At: at, Till: till, Ranges: map[string]SampleRange{}}
		minAt, maxAt := at, at
		first := true
		for _, c := range t.Cameras {
			r, err := t.cameraRange(c, at, till)
			if err != nil {
				return nil, err
			}
			cut.Ranges[c.Name] = r
			if first || r.KfAt.Cmp(minAt) < 0 {
				minAt = r.KfAt
			}
			if first || r.KfAt.Cmp(maxAt) > 0 {
				maxAt = r.KfAt
			}
			first = false
		}
		spread, err := maxAt.Sub(minAt)
		if err != nil {
			return nil, err
		}
		if spread.Cmp(t.FrameTol) > 0 {
			return nil, media.New(media.PhaseAlign, media.KindBounds, "", -1,
				fmt.Sprintf("cut %d: cross-camera keyframe spread %s exceeds one frame %s",
					int(k), spread.String(), t.FrameTol.String())).AtTime(at)
		}
		cuts = append(cuts, cut)
	}
	return cuts, nil
}

// cameraRange snaps the cut to the nearest IDR (before or after at) and
// returns the half-open sample run covering [at, till).
func (t *Timeline) cameraRange(c *Camera, at, till clock.Rat) (SampleRange, error) {
	frames := t.Frames[c.Name]
	kf := nearestKeyframe(frames, at)
	if kf < 0 {
		return SampleRange{}, media.New(media.PhaseAlign, media.KindBounds, c.Path, -1,
			"no IDR keyframe available near cut point").AtTime(at)
	}
	dev, err := absDiff(at, frames[kf].At)
	if err != nil {
		return SampleRange{}, err
	}
	if dev.Cmp(t.FrameTol) > 0 {
		return SampleRange{}, media.New(media.PhaseAlign, media.KindBounds, c.Path, -1,
			fmt.Sprintf("nearest keyframe %s deviates from cut by %s, beyond one frame %s",
				frames[kf].At.String(), dev.String(), t.FrameTol.String())).AtTime(at)
	}
	first, last := -1, -1
	for i, f := range frames {
		if f.At.Cmp(at) >= 0 && f.At.Cmp(till) < 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		first, last = kf, kf
	}
	return SampleRange{
		First: frames[first].Sample, Last: frames[last].Sample,
		Keyframe: frames[kf].Sample, KfAt: frames[kf].At,
	}, nil
}

// nearestKeyframe returns the presentation index of the sync sample closest to
// at, or -1 when the track has none.
func nearestKeyframe(frames []Frame, at clock.Rat) int {
	best := -1
	for i, f := range frames {
		if !f.Sync {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		dCur, _ := absDiff(at, f.At)
		dBest, _ := absDiff(at, frames[best].At)
		if dCur.Cmp(dBest) < 0 {
			best = i
		}
	}
	return best
}

func absDiff(a, b clock.Rat) (clock.Rat, error) {
	d, err := a.Sub(b)
	if err != nil {
		return clock.Rat{}, err
	}
	if d.Num < 0 {
		d.Num = -d.Num
	}
	return d, nil
}

func (t *Timeline) crossFrameTolerance() (clock.Rat, error) {
	tol, err := clock.New(1, 1)
	if err != nil {
		return clock.Rat{}, err
	}
	for _, c := range t.Cameras {
		maxDur, err := maxSampleDuration(c.Track)
		if err != nil {
			return clock.Rat{}, err
		}
		if maxDur.Cmp(tol) < 0 {
			tol = maxDur
		}
	}
	return tol, nil
}

func maxSampleDuration(tr *mp4.TrackIndex) (clock.Rat, error) {
	var max clock.Rat
	for i := range tr.Samples {
		d := tr.Samples[i].Duration
		if i == 0 || d.Cmp(max) > 0 {
			max = d
		}
	}
	if max.Den == 0 {
		return clock.Rat{}, media.New(media.PhaseAlign, media.KindBadTimebase, "", -1,
			"track without usable sample durations")
	}
	return max, nil
}

func tail(frames []Frame) (clock.Rat, error) {
	last := frames[len(frames)-1]
	return last.At.Add(last.Dur)
}

func ratFromDuration(d time.Duration) (clock.Rat, error) {
	return clock.New(int64(d), int64(time.Second))
}

func presentationOrder(samples []mp4.Sample) []int {
	order := make([]int, len(samples))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return samples[order[a]].PTS.Cmp(samples[order[b]].PTS) < 0
	})
	return order
}
