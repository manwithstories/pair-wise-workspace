package repair

import (
	"github.com/pion/rtcp"

	"mediaroute/internal/timeline"
)

// TSError is a locatable timestamp anomaly: the first affected
// extended sequence number plus the deviation magnitude.
type TSError struct {
	Kind        string // "wrap", "backward", "drift"
	SSRC        uint32
	ExtSeq      uint64
	DeviationNs int64
	Detail      string
}

// TSRepair owns one timeline.Clock per track and converts clock
// anomalies into the pipeline error model.
type TSRepair struct {
	clocks   map[uint32]*timeline.Clock
	rates    map[uint32]uint32
	driftPPM uint32
	// lastExt tracks the current extended sequence per SSRC so SR
	// events can be located.
	lastExt map[uint32]uint64
}

func NewTSRepair(driftPPM uint32) *TSRepair {
	return &TSRepair{
		clocks:   map[uint32]*timeline.Clock{},
		rates:    map[uint32]uint32{},
		driftPPM: driftPPM,
		lastExt:  map[uint32]uint64{},
	}
}

// RegisterTrack sets the nominal clock rate for an SSRC.
func (r *TSRepair) RegisterTrack(ssrc, rate uint32) {
	r.rates[ssrc] = rate
}

func (r *TSRepair) clock(ssrc uint32) *timeline.Clock {
	c, ok := r.clocks[ssrc]
	if !ok {
		c = timeline.NewClock(r.rates[ssrc], r.driftPPM)
		r.clocks[ssrc] = c
	}
	return c
}

// Map converts one packet's RTP timestamp to unified nanoseconds and
// returns the corrected (monotonic) extended timestamp in RTP units,
// emitting at most one error.
func (r *TSRepair) Map(ssrc uint32, extSeq uint64, rtpTS uint32) (int64, int64, *TSError) {
	c := r.clock(ssrc)
	ns, ext, ev := c.Map(rtpTS)
	r.lastExt[ssrc] = extSeq
	if ev == nil {
		return ns, ext, nil
	}
	kind := map[timeline.EventKind]string{
		timeline.EvWrap:     "wrap",
		timeline.EvBackward: "backward",
		timeline.EvDrift:    "drift",
	}[ev.Kind]
	return ns, ext, &TSError{Kind: kind, SSRC: ssrc, ExtSeq: extSeq,
		DeviationNs: ev.DeviationNs, Detail: ev.Detail}
}

// AddSenderReport anchors a track clock; drift beyond the configured
// threshold produces a locatable error.
func (r *TSRepair) AddSenderReport(sr *rtcp.SenderReport) *TSError {
	c := r.clock(sr.SSRC)
	sec := sr.NTPTime >> 32
	frac := sr.NTPTime & 0xFFFFFFFF
	ntpNs := int64(sec)*1_000_000_000 + int64(frac)*1_000_000_000/0x1_0000_0000
	curExt := r.lastExt[sr.SSRC]
	ev := c.AddSenderReport(sr.RTPTime, ntpNs, curExt)
	if ev == nil {
		return nil
	}
	return &TSError{Kind: "drift", SSRC: sr.SSRC, ExtSeq: curExt,
		DeviationNs: ev.DeviationNs, Detail: ev.Detail}
}

// Estimated reports whether a track still lacks a Sender Report anchor.
func (r *TSRepair) Estimated(ssrc uint32) bool {
	c, ok := r.clocks[ssrc]
	return !ok || c.Estimated
}

// State exports clock state for checkpointing.
func (r *TSRepair) State() map[uint32]ClockState {
	out := map[uint32]ClockState{}
	for ssrc, c := range r.clocks {
		init, lastExt, adjust, aRTP, aNs, est := c.Export()
		out[ssrc] = ClockState{
			Init: init, LastExt: lastExt, Adjust: adjust,
			AnchorRTP: aRTP, AnchorNs: aNs, Estimated: est,
		}
	}
	return out
}

// Restore loads clock state from a checkpoint.
func (r *TSRepair) Restore(states map[uint32]ClockState) {
	for ssrc, st := range states {
		c := timeline.NewClock(r.rates[ssrc], r.driftPPM)
		c.Import(st.Init, st.LastExt, st.Adjust, st.AnchorRTP, st.AnchorNs, st.Estimated)
		r.clocks[ssrc] = c
	}
}

// ClockState is the serializable form of a timeline.Clock.
type ClockState struct {
	Init      bool
	LastExt   uint64
	Adjust    int64
	AnchorRTP int64
	AnchorNs  int64
	Estimated bool
}
