package clock

import (
	"fmt"
	"sort"
)

// minCalibSpan is the NTP span required before a drift estimate from two
// sender reports is considered trustworthy. NTP fractional seconds use 1/2^32
// units, so this is roughly 0.100 s.
const minCalibSpan uint64 = 429496730

// SenderReport is the subset of an RTCP SR (RFC 3550 §6.4.1) needed for clock
// calibration. NTPTime packs 32-bit seconds in the high word and the 32-bit
// fraction in the low word.
type SenderReport struct {
	SSRC         uint32
	NTPTime      uint64
	RTPTimestamp uint32
}

// StreamTime is the unified mapping of one RTP packet onto the media axis.
type StreamTime struct {
	SSRC         uint32
	Seq          uint16
	ExtTimestamp uint64 // unwrapped RTP clock units
	MediaTimeUs  int64  // microseconds since the stream's first packet
	Calibrated   bool   // a drift estimate backed by sufficient SR data
	Uncertain    bool   // mapping retained but not yet calibrated
	Version      int    // time-base version in force for this mapping
}

// UncertainInterval records a contiguous run of packets mapped before the
// calibration threshold was reached, so downstream stages can cite it.
type UncertainInterval struct {
	SSRC      uint32
	FirstTick int64
	LastTick  int64
}

type calibPoint struct {
	ntp uint64
	ext uint64
}

type streamState struct {
	clockRate  uint32
	unwrap     *Unwrapper
	srUnwrap   *Unwrapper // SR timestamps arrive on their own cadence
	anchor     uint64
	haveAnchor bool
	samples    []calibPoint
	ppm        int64
	calibrated bool
	uncertFrom int64 // first global tick still uncalibrated, -1 when closed
}

// Timeline owns one unwrapper and one calibration state per SSRC.
type Timeline struct {
	wrapPoint  uint64
	streams    map[uint32]*streamState
	version    int
	tick       int64
	uncertain  []UncertainInterval
	driftDirty bool
}

// NewTimeline builds an empty timeline. wrapPoint 0 selects the RTP modulus.
func NewTimeline(wrapPoint uint64) *Timeline {
	return &Timeline{wrapPoint: wrapPoint, streams: map[uint32]*streamState{}, version: 1}
}

// Version returns the current time-base version. It increments whenever the
// drift estimate becomes available or changes materially; checkpoints pin it.
func (t *Timeline) Version() int { return t.version }

func (t *Timeline) state(ssrc uint32, rate uint32) *streamState {
	s, ok := t.streams[ssrc]
	if !ok {
		s = &streamState{
			clockRate:  rate,
			unwrap:     NewUnwrapper(ssrc, rate, t.wrapPoint),
			srUnwrap:   NewUnwrapper(ssrc, rate, t.wrapPoint),
			uncertFrom: -1,
		}
		t.streams[ssrc] = s
	}
	return s
}

// ObserveSR folds an RTCP sender report into the drift estimate. Two reports
// spanning at least minCalibSpan are required; below that the stream stays
// uncalibrated and the interval remains explicitly uncertain.
func (t *Timeline) ObserveSR(sr SenderReport, rate uint32) error {
	s := t.state(sr.SSRC, rate)
	// SRs form their own ordered series; a synthetic sequence keeps the
	// unwrapper's order logic intact without touching packet state.
	ext, err := s.srUnwrap.Unwrap(uint16(len(s.samples)), sr.RTPTimestamp)
	if err != nil {
		return fmt.Errorf("clock: SR unwrap ssrc=%d: %w", sr.SSRC, err)
	}
	s.samples = append(s.samples, calibPoint{ntp: sr.NTPTime, ext: ext})
	if len(s.samples) < 2 {
		return nil // a single report carries no frequency information
	}
	a, b := s.samples[0], s.samples[len(s.samples)-1]
	spanNTP := b.ntp - a.ntp
	if spanNTP < minCalibSpan {
		return nil
	}
	secs := float64(spanNTP) / 65536.0 / 65536.0
	observed := float64(int64(b.ext - a.ext))
	expected := secs * float64(s.clockRate)
	if expected <= 0 || observed < 0 {
		return fmt.Errorf("%w: unusable SR span for ssrc=%d", ErrUncertain, sr.SSRC)
	}
	ppm := int64((observed-expected)/expected*1e6 + 0.5)
	if !s.calibrated || abs64(ppm-s.ppm) > 1 {
		t.version++
		t.driftDirty = true
	}
	s.ppm = ppm
	if !s.calibrated {
		s.calibrated = true
		if s.uncertFrom >= 0 {
			t.uncertain = append(t.uncertain, UncertainInterval{SSRC: sr.SSRC, FirstTick: s.uncertFrom, LastTick: t.tick - 1})
			s.uncertFrom = -1
		}
	}
	return nil
}

// Map extends and anchors one packet. The returned media time is always
// monotonic within an SSRC; Uncertain is set instead of guessing a correction.
func (t *Timeline) Map(ssrc uint32, seq uint16, raw uint32, rate uint32) (StreamTime, error) {
	s := t.state(ssrc, rate)
	ext, err := s.unwrap.Unwrap(seq, raw)
	if err != nil {
		return StreamTime{}, err
	}
	if !s.haveAnchor {
		s.anchor = ext
		s.haveAnchor = true
	}
	if !s.calibrated && s.uncertFrom < 0 {
		s.uncertFrom = t.tick
	}
	st := StreamTime{
		SSRC: ssrc, Seq: seq, ExtTimestamp: ext,
		MediaTimeUs: int64(float64(ext-s.anchor) / float64(s.clockRate) * 1e6),
		Calibrated:  s.calibrated, Uncertain: !s.calibrated, Version: t.version,
	}
	t.tick++
	return st, nil
}

// DriftPPM returns the latest sender-clock drift estimate. ok is false until
// the calibration threshold is met.
func (t *Timeline) DriftPPM(ssrc uint32) (ppm int64, ok bool) {
	s, ok := t.streams[ssrc]
	if !ok || !s.calibrated {
		return 0, false
	}
	return s.ppm, true
}

// UncertainIntervals returns the closed intervals mapped without calibration,
// sorted by first tick. Any still-open interval is included at read time.
func (t *Timeline) UncertainIntervals() []UncertainInterval {
	out := append([]UncertainInterval(nil), t.uncertain...)
	for ssrc, s := range t.streams {
		if s.uncertFrom >= 0 {
			out = append(out, UncertainInterval{SSRC: ssrc, FirstTick: s.uncertFrom, LastTick: t.tick - 1})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstTick < out[j].FirstTick })
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
