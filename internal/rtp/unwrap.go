// Package rtp unwraps 16-bit RTP sequence numbers and 32-bit timestamps,
// detects wraps, and estimates per-device clock drift against a reference
// wall clock. Observations beyond half a wrap period are flagged undecidable.
package rtp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	prtp "github.com/pion/rtp"

	"mediaaudit/internal/model"
)

const (
	// tsMod is the 32-bit RTP timestamp modulus.
	tsMod int64 = 1 << 32
	// seqMod is the 16-bit RTP sequence number modulus.
	seqMod int64 = 1 << 16
	// halfWrap is half the timestamp modulus; larger raw deltas are ambiguous.
	halfWrap int64 = tsMod / 2
)

// Observation is one timestamp/wall-clock pair from an RTP stream. WallNanos
// is the capture/reference wall clock in nanoseconds.
type Observation struct {
	Seq           uint16
	RawTimestamp  uint32
	WallNanos     int64
	ExtendedSeq   int64
	ExtendedTS    int64
	Undecidable   bool
}

// Stream accumulates an ordered RTP stream and unwraps its counters.
type Stream struct {
	clockRate   uint32
	initialized bool
	lastSeq     uint16
	seqCycles   int64
	lastTS      uint32
	tsCycles    int64
	obs         []Observation
	faults      *model.FaultSet
	track       string
}

// NewStream starts an unwrapper for the given RTP clock rate.
func NewStream(track string, clockRate uint32) *Stream {
	if clockRate == 0 {
		clockRate = 1
	}
	return &Stream{clockRate: clockRate, faults: model.NewFaultSet(), track: track}
}

// Push ingests one packet's sequence/timestamp and wall clock, unwrapping both
// counters relative to the preceding packet.
func (s *Stream) Push(seq uint16, ts uint32, wallNanos int64) Observation {
	if !s.initialized {
		s.initialized = true
		s.lastSeq = seq
		s.lastTS = ts
		o := Observation{
			Seq: seq, RawTimestamp: ts, WallNanos: wallNanos,
			ExtendedSeq: int64(seq), ExtendedTS: int64(ts),
		}
		s.obs = append(s.obs, o)
		return o
	}
	// Sequence wrap (modulo 65536): positive shortest-step delta that crosses
	// the modulus boundary is a forward wrap.
	seqDelta := wrapDelta(int64(seq), int64(s.lastSeq), seqMod)
	if seqDelta > 0 && seq < s.lastSeq {
		s.seqCycles += seqMod
	}
	extSeq := s.seqCycles + int64(seq)

	// Timestamp wrap (modulo 2^32). A forward wrap increments tsCycles.
	rawDelta := wrapDelta(int64(ts), int64(s.lastTS), tsMod)
	undecidable := false
	if abs64(rawDelta) >= halfWrap {
		undecidable = true
		s.faults.Add(model.Fault{
			Kind:    model.KindClockUndecidable,
			Track:   s.track,
			Range:   model.FrameRange{First: model.Frame(extSeq), Last: model.Frame(extSeq + 1)},
			Message: fmt.Sprintf("timestamp jump %d exceeds half wrap period", rawDelta),
		})
	}
	if rawDelta > 0 && ts < s.lastTS {
		s.tsCycles += tsMod
	}
	extTS := s.tsCycles + int64(ts)
	s.lastSeq = seq
	s.lastTS = ts
	o := Observation{
		Seq: seq, RawTimestamp: ts, WallNanos: wallNanos,
		ExtendedSeq: extSeq, ExtendedTS: extTS, Undecidable: undecidable,
	}
	s.obs = append(s.obs, o)
	return o
}

// wrapDelta returns the signed shortest-step delta (a-b) under modulus m.
func wrapDelta(a, b, m int64) int64 {
	d := (a - b) % m
	if d < 0 {
		d += m
	}
	if d > m/2 {
		d -= m
	}
	return d
}

// Observation unwrapping helper.
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Observations returns all ingested observations in order.
func (s *Stream) Observations() []Observation { return s.obs }

// Faults returns clock-undecidable and related faults.
func (s *Stream) Faults() *model.FaultSet { return s.faults }

// DriftResult is a least-squares fit of RTP elapsed time vs wall-clock time.
type DriftResult struct {
	// PPM is parts-per-million clock drift (positive = device clock fast).
	PPM float64
	// InterceptNanos is the fitted wall-clock offset at t=0.
	InterceptNanos float64
	Samples        int
	Undecidable    bool
}

// EstimateDrift fits device_elapsed = (1+ppm*1e-6)*wall_elapsed over the
// decidable observations. A single observation cannot yield a slope.
func (s *Stream) EstimateDrift() (DriftResult, error) {
	type pt struct{ x, y float64 }
	var pts []pt
	// Reference: first observation's wall clock and extended timestamp.
	if len(s.obs) < 2 {
		return DriftResult{}, errors.New("rtp: need at least 2 observations for drift")
	}
	base := s.obs[0]
	for _, o := range s.obs {
		if o.Undecidable {
			continue
		}
		wallSec := float64(o.WallNanos-base.WallNanos) / 1e9
		devSec := float64(o.ExtendedTS-base.ExtendedTS) / float64(s.clockRate)
		pts = append(pts, pt{wallSec, devSec})
	}
	if len(pts) < 2 {
		return DriftResult{Samples: len(pts), Undecidable: true}, nil
	}
	var sumX, sumY, sumXX, sumXY float64
	for _, p := range pts {
		sumX += p.x
		sumY += p.y
		sumXX += p.x * p.x
		sumXY += p.x * p.y
	}
	n := float64(len(pts))
	den := n*sumXX - sumX*sumX
	if math.Abs(den) < 1e-12 {
		return DriftResult{Samples: len(pts), Undecidable: true}, nil
	}
	slope := (n*sumXY - sumX*sumY) / den
	intercept := (sumY - slope*sumX) / n
	return DriftResult{
		PPM:            (slope - 1) * 1e6,
		InterceptNanos: intercept * 1e9,
		Samples:        len(pts),
	}, nil
}

// ReadPackets parses a length-prefixed RTP capture: repeated [uint32 len][RTP].
// Wall clock is carried as a leading int64 before each packet, i.e.
// [int64 wallNanos][uint32 pktLen][RTP bytes]. This keeps the fixture fully
// deterministic and self-contained.
func ReadPackets(r io.Reader, handler func(p *prtp.Packet, wallNanos int64) error) error {
	var wall [8]byte
	var lenb [4]byte
	for {
		if _, err := io.ReadFull(r, wall[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		wallNanos := int64(binary.LittleEndian.Uint64(wall[:]))
		if _, err := io.ReadFull(r, lenb[:]); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(lenb[:])
		if n > (1<<24) { // 16 MiB per-packet cap
			return model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
				fmt.Sprintf("rtp packet length %d exceeds cap", n))
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		pkt := &prtp.Packet{}
		if err := pkt.Unmarshal(buf); err != nil {
			return model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
				"rtp unmarshal: "+err.Error())
		}
		if err := handler(pkt, wallNanos); err != nil {
			return err
		}
	}
}
