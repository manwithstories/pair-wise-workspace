// Package sync aligns audio and video presentation timelines on the shared
// clock.MediaClock. It never owns a private clock or error model: the same
// MediaClock instance feeds transport recovery, this aligner and the DAG,
// so a rejected timeline blocks packaging with one consistent verdict.
package sync

import (
	"errors"
	"fmt"

	"relayctl/internal/clock"
)

// DefaultThresholdMS is the lip-sync tolerance (ITU-R BT.1359 region):
// audio leading or lagging beyond this magnitude is not deliverable.
const DefaultThresholdMS int64 = 125

// Confidence mirrors clock.Confidence so callers can stay within the sync
// package while still switching on the shared semantics.
type Confidence = clock.Confidence

const (
	Confirmed = clock.Confirmed
	Uncertain = clock.Uncertain
	Rejected  = clock.Rejected
)

// TrackTimeline is one track's projected points on the master clock.
type TrackTimeline struct {
	TrackID uint32
	Kind    string
	Points  []clock.SamplePoint
	// ClockConfidence is the worst confidence observed on this track.
	ClockConfidence Confidence
}

// Offset is the measured audio-vs-video presentation offset at a point.
type Offset struct {
	AtMS          int64
	AudioMS       int64
	VideoMS       int64
	DeltaMS       int64 // audio - video on the master clock
	AbsDeltaMS    int64
	Confidence    Confidence
}

// Result is the alignment verdict for one fragment group.
type Result struct {
	Offsets      []Offset
	WorstAbsMS   int64
	Confidence   Confidence
	Blocked      bool
	BlockingKind clock.ErrorKind
}

// Aligner binds one audio and one video timeline to a shared clock.
type Aligner struct {
	mc        *clock.MediaClock
	threshold int64
}

// New builds an aligner over the shared clock with the default threshold.
func New(mc *clock.MediaClock) *Aligner {
	return &Aligner{mc: mc, threshold: DefaultThresholdMS}
}

// WithThreshold overrides the lip-sync tolerance (milliseconds).
func (a *Aligner) WithThreshold(ms int64) *Aligner {
	if ms > 0 {
		a.threshold = ms
	}
	return a
}

// ErrTimelineBlocked signals that a rejected clock timeline prevents
// packaging; callers must surface it rather than emit a fragment.
var ErrTimelineBlocked = errors.New("sync: timeline rejected, packaging blocked")

// Align compares the two track timelines sample-by-sample. If either track
// is already blocked on the shared clock, it returns ErrTimelineBlocked
// carrying the first clock error's kind. An offset beyond the threshold
// rejects alignment even when the clock itself stayed monotonic.
func (a *Aligner) Align(audio, video TrackTimeline) (Result, error) {
	res := Result{Confidence: Confirmed}
	if blocked, kind := a.clockBlocked(audio, video); blocked {
		res.Blocked = true
		res.BlockingKind = kind
		res.Confidence = Rejected
		return res, fmt.Errorf("%w: %s", ErrTimelineBlocked, kind)
	}

	n := len(audio.Points)
	if len(video.Points) < n {
		n = len(video.Points)
	}
	worst := clock.Confidence(0)
	for i := 0; i < n; i++ {
		ap, vp := audio.Points[i], video.Points[i]
		delta := ap.StartMS - vp.StartMS
		abs := delta
		if abs < 0 {
			abs = -abs
		}
		conf := pairConfidence(audio.ClockConfidence, video.ClockConfidence)
		if abs > a.threshold {
			conf = Rejected
		} else if abs > 0 {
			conf = lift(conf)
		}
		if conf > worst {
			worst = conf
		}
		if abs > res.WorstAbsMS {
			res.WorstAbsMS = abs
		}
		res.Offsets = append(res.Offsets, Offset{
			AtMS: vp.StartMS, AudioMS: ap.StartMS, VideoMS: vp.StartMS,
			DeltaMS: delta, AbsDeltaMS: abs, Confidence: conf,
		})
	}
	res.Confidence = worst
	res.Blocked = worst == Rejected
	if res.Blocked {
		res.BlockingKind = clock.ErrNonMonotonicSamples
		return res, fmt.Errorf("%w: offset beyond %dms", ErrTimelineBlocked, a.threshold)
	}
	return res, nil
}

// clockBlocked inspects the shared clock's own rejection state for either
// track and returns the first offending error kind.
func (a *Aligner) clockBlocked(audio, video TrackTimeline) (bool, clock.ErrorKind) {
	for _, tl := range []TrackTimeline{audio, video} {
		if a.mc.IsBlocked(tl.TrackID) {
			for _, e := range a.mc.Errors() {
				if e.TrackID == tl.TrackID {
					return true, e.Kind
				}
			}
			return true, clock.ErrTfdtRollback
		}
		if tl.ClockConfidence == Rejected {
			return true, clock.ErrTfdtRollback
		}
	}
	return false, ""
}

// pairConfidence takes the weaker (higher enum) of two confidences.
func pairConfidence(a, b Confidence) Confidence {
	if a > b {
		return a
	}
	return b
}

// lift promotes Confirmed to Uncertain; Rejected stays Rejected.
func lift(c Confidence) Confidence {
	if c == Confirmed {
		return Uncertain
	}
	return c
}
