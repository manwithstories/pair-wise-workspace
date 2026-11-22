package ingest

import (
	"errors"
	"fmt"
	"time"

	"github.com/pion/rtp"

	"mediacq/internal/clock"
)

// ErrLatePacket reports an arrival that cannot move the committed timeline.
var ErrLatePacket = errors.New("ingest: late or out-of-order RTP packet")

// RTPConfig parameterizes the packet stream consumer.
type RTPConfig struct {
	ClockRate       int64 // Hz, e.g. 90000 for video
	DriftThreshold  float64
	DriftWindow     int64 // nanoseconds around an over-threshold reading
	MaxDriftWindows int
}

// RTPPoint is one accepted packet positioned on the unified timeline.
type RTPPoint struct {
	Sequence  uint16
	Time      clock.Time
	Linear    int64
	Wrapped   bool
	Late      bool
	DriftPPM  float64 // populated once enough points exist
	DriftFlag bool
}

// RTPStream consumes marshalled RTP packets in arrival order. It unwraps the
// 32-bit timestamp space, estimates sender drift, and refuses to rewrite the
// committed timeline when a late packet shows up.
type RTPStream struct {
	cfg      RTPConfig
	rate     clock.Rate
	unwrap   *clock.Unwrapper32
	drift    *clock.DriftEstimator
	windows  []clock.DriftWindow
	count    int
	lastPPM  float64
}

// NewRTPStream validates the clock before any packet is accepted.
func NewRTPStream(cfg RTPConfig) (*RTPStream, error) {
	rate, err := clock.NewRate(cfg.ClockRate, 1)
	if err != nil {
		return nil, err
	}
	u, err := clock.NewUnwrapper32(rate)
	if err != nil {
		return nil, err
	}
	d, err := clock.NewDriftEstimator(rate)
	if err != nil {
		return nil, err
	}
	return &RTPStream{cfg: cfg, rate: rate, unwrap: u, drift: d}, nil
}

// Push accepts one marshalled RFC 3550 packet.
func (s *RTPStream) Push(raw []byte, refTime clock.Time) (RTPPoint, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return RTPPoint{}, fmt.Errorf("ingest: rtp unmarshal: %w", err)
	}
	res, lerr := s.unwrap.Push(clock.RawPoint{
		Raw32: pkt.Timestamp, RefTime: refTime, Offset: int64(s.count),
	})
	if lerr != nil {
		return RTPPoint{}, lerr
	}
	out := RTPPoint{
		Sequence: pkt.SequenceNumber, Time: res.Time,
		Linear: res.Linear, Wrapped: res.Wrapped, Late: res.Late,
	}
	if res.Late {
		// Record but never feed late data into the drift fit.
		s.count++
		return out, ErrLatePacket
	}
	if err := s.drift.Add(clock.DriftPoint{Linear: res.Linear, RefTime: refTime}); err != nil {
		return RTPPoint{}, err
	}
	s.count++
	if s.count >= 2 {
		ppm, err := s.drift.SlopePPM()
		if err == nil {
			s.lastPPM = ppm
			out.DriftPPM = ppm
			if clock.ExceedsThreshold(ppm, s.cfg.DriftThreshold) {
				out.DriftFlag = true
				if s.cfg.DriftWindow > 0 && len(s.windows) < s.cfg.MaxDriftWindows {
					w, err := clock.WindowFromPoint(refTime,
						time.Duration(s.cfg.DriftWindow), ppm)
					if err == nil {
						s.windows = append(s.windows, w)
					}
				}
			}
		}
	}
	return out, nil
}

// DriftWindows returns the program ranges affected by over-threshold drift.
func (s *RTPStream) DriftWindows() []clock.DriftWindow { return s.windows }

// Frontier exposes the immutable committed tick frontier.
func (s *RTPStream) Frontier() int64 { return s.unwrap.Frontier() }

// LastPPM is the most recent drift estimate.
func (s *RTPStream) LastPPM() float64 { return s.lastPPM }
