package clock

import (
	"math"
)

// MasterTimescaleMS is the canonical master clock: units per second.
const MasterTimescaleMS uint32 = 1000

// TrackConfig registers one media track on the shared clock.
type TrackConfig struct {
	TrackID   uint32
	Timescale uint32
	Kind      string // "video" | "audio", informational
}

type trackState struct {
	cfg        TrackConfig
	unwrap     *Unwrap64
	drift      *DriftEstimator
	lastTicks  uint64 // last accepted unwrapped tfdt baseline
	have       bool
	rejected   bool
	presentEnd uint64 // highest sample end already presented (track ticks)
	haveEnd    bool
}

// MediaClock normalizes every track onto millisecond presentation time.
// A single instance is shared by transport recovery, A/V sync and the DAG
// so all of them see identical numbers, confidence and errors.
type MediaClock struct {
	tracks map[uint32]*trackState
	errs   []*TimelineError
}

// New returns an empty clock on the millisecond master timescale.
func New() *MediaClock {
	return &MediaClock{tracks: map[uint32]*trackState{}}
}

// AddTrack registers a track; a zero/duplicate timescale is a usage error.
func (m *MediaClock) AddTrack(cfg TrackConfig) error {
	if cfg.Timescale == 0 {
		return &TimelineError{TrackID: cfg.TrackID, Kind: ErrTimescaleInvalid,
			Detail: "timescale must be non-zero"}
	}
	if _, ok := m.tracks[cfg.TrackID]; ok {
		return &TimelineError{TrackID: cfg.TrackID, Kind: ErrTimescaleInvalid,
			Detail: "duplicate track registration"}
	}
	m.tracks[cfg.TrackID] = &trackState{
		cfg:    cfg,
		unwrap: NewUnwrap64(33),
		drift:  NewDriftEstimator(cfg.Timescale, MasterTimescaleMS),
	}
	return nil
}

// Observation is the normalized result of feeding one tfdt value.
type Observation struct {
	TrackID      uint32
	RawTfdt      uint64
	ExtendedTfdt uint64
	MasterMS     int64
	Confidence   Confidence
	Wraps        int
}

// ObserveTfdt feeds a raw (possibly 32-bit wrapped) tfdt. Wrap extension is
// applied automatically; a true rollback or excessive drift is returned as
// a Rejected observation plus a TimelineError and marks the track blocked.
func (m *MediaClock) ObserveTfdt(trackID uint32, raw uint64) (Observation, error) {
	ts, ok := m.tracks[trackID]
	if !ok {
		return Observation{}, &TimelineError{TrackID: trackID, Kind: ErrTrackUnknown}
	}
	extended, wraps := ts.unwrap.Extend(raw)
	if ts.have && extended <= ts.lastTicks {
		ts.rejected = true
		err := &TimelineError{TrackID: trackID, Kind: ErrTfdtRollback,
			At: ticksToMS(extended, ts.cfg.Timescale),
			Detail: "tfdt moved backwards after unwrap"}
		m.errs = append(m.errs, err)
		return Observation{TrackID: trackID, RawTfdt: raw, ExtendedTfdt: extended,
			Confidence: Rejected, Wraps: wraps}, err
	}
	masterMS := ticksToMS(extended, ts.cfg.Timescale)
	conf, derr := ts.drift.Observe(masterMS, extended)
	if derr != nil {
		ts.rejected = true
		if te, ok := derr.(*TimelineError); ok {
			te.TrackID = trackID
			m.errs = append(m.errs, te)
		}
		return Observation{TrackID: trackID, RawTfdt: raw, ExtendedTfdt: extended,
			MasterMS: masterMS, Confidence: Rejected, Wraps: wraps}, derr
	}
	ts.lastTicks, ts.have = extended, true
	conf = m.grade(ts, conf, wraps)
	return Observation{TrackID: trackID, RawTfdt: raw, ExtendedTfdt: extended,
		MasterMS: masterMS, Confidence: conf, Wraps: wraps}, nil
}

// grade lowers confidence to Uncertain when a wrap was repaired, keeps
// Confirmed only for an untouched monotonic stream.
func (m *MediaClock) grade(ts *trackState, base Confidence, wraps int) Confidence {
	if wraps > 0 || ts.drift.Samples > 1 {
		if base == Confirmed {
			return Uncertain
		}
	}
	return base
}

// SamplePoint is one VFR sample's presentation position on the master clock.
type SamplePoint struct {
	Index      int
	StartMS    int64
	DurationMS int64
}

// ProjectSamples converts per-sample tfdt-relative durations into master
// millisecond points, enforcing strict monotonic presentation times.
func (m *MediaClock) ProjectSamples(trackID uint32, baseTfdt uint64, durations []uint32) ([]SamplePoint, error) {
	ts, ok := m.tracks[trackID]
	if !ok {
		return nil, &TimelineError{TrackID: trackID, Kind: ErrTrackUnknown}
	}
	out := make([]SamplePoint, 0, len(durations))
	cursor := baseTfdt
	var prevEnd int64 = math.MinInt64
	if ts.haveEnd && baseTfdt < ts.presentEnd {
		err := &TimelineError{TrackID: trackID, Kind: ErrNonMonotonicSamples,
			At: ticksToMS(baseTfdt, ts.cfg.Timescale),
			Detail: "fragment base precedes already-presented sample end"}
		m.errs = append(m.errs, err)
		ts.rejected = true
		return nil, err
	}
	for i, dur := range durations {
		startMS := ticksToMS(cursor, ts.cfg.Timescale)
		durMS := ticksToMS(uint64(dur), ts.cfg.Timescale)
		if startMS < prevEnd {
			err := &TimelineError{TrackID: trackID, Kind: ErrNonMonotonicSamples,
				At: startMS, Detail: "VFR sample overlaps previous presentation time"}
			m.errs = append(m.errs, err)
			return nil, err
		}
		out = append(out, SamplePoint{Index: i, StartMS: startMS, DurationMS: durMS})
		prevEnd = startMS + durMS
		cursor += uint64(dur)
	}
	ts.presentEnd, ts.haveEnd = cursor, true
	return out, nil
}

// IsBlocked reports whether a track carries a rejected timeline observation.
func (m *MediaClock) IsBlocked(trackID uint32) bool {
	ts, ok := m.tracks[trackID]
	return ok && ts.rejected
}

// Errors returns a copy of every timeline error recorded so far.
func (m *MediaClock) Errors() []*TimelineError {
	cp := make([]*TimelineError, len(m.errs))
	copy(cp, m.errs)
	return cp
}

// ticksToMS converts track ticks to rounded master milliseconds.
func ticksToMS(ticks uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	// Rounded half-up division; safe well past realistic media durations.
	return int64((ticks*uint64(MasterTimescaleMS) + uint64(timescale)/2) / uint64(timescale))
}

// MSToTicks is the inverse used when building synthetic fixtures.
func MSToTicks(ms int64, timescale uint32) uint64 {
	return (uint64(ms) * uint64(timescale) + uint64(MasterTimescaleMS)/2) / uint64(MasterTimescaleMS)
}
