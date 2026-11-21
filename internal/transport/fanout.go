package transport

import (
	"fmt"

	"github.com/pion/rtp"

	"mediasync/internal/model"
)

// SimPacket is one packet at the transport input before representation routing.
type SimPacket struct {
	RepIndex  int
	Seq       uint16
	RTPTS     uint32
	Marker    bool
	Payload   []byte
	SendTicks int64 // shared simulation clock
	RecvTicks int64
}

// SwitchEvent marks one representation change at a shared-clock boundary.
type SwitchEvent struct {
	AtTicks int64
	FromRep string
	ToRep   string
}

// Fanout owns one reorder buffer per representation, the bandwidth estimator and
// the hysteresis selector. Frames fan back in through a single shared clock and
// no frame timestamp may be contributed by two representations.
type Fanout struct {
	track      string
	clock      model.Clock
	reps       []Representation
	buffers    []*ReorderBuffer
	estimator  *Estimator
	selector   *Selector
	seenTS     map[int64]string // extended timestamp -> contributing rep
	switches   []SwitchEvent
	suppressed int
	external   bool // when true, Push never runs the internal selector
	diag       model.Bag
}

// FanoutConfig wires the three transport pieces together.
type FanoutConfig struct {
	Track             string
	Clock             model.Clock
	Reps              []Representation
	EstimatorAlpha    float64
	TickRate          int64
	HoldTicks         int64
	Reorder           ReorderConfig
	ExternalSelection bool // adaptation driven via NoteSwitch, not the estimator
}

// NewFanout validates and constructs the per-track fanout pipeline.
func NewFanout(cfg FanoutConfig) (*Fanout, error) {
	sel, err := NewSelector(cfg.Reps, cfg.HoldTicks)
	if err != nil {
		return nil, err
	}
	f := &Fanout{
		track:     cfg.Track,
		clock:     cfg.Clock,
		reps:      cfg.Reps,
		estimator: NewEstimator(cfg.EstimatorAlpha, cfg.TickRate),
		selector:  sel,
		seenTS:    map[int64]string{},
		external:  cfg.ExternalSelection,
	}
	for i, r := range cfg.Reps {
		f.buffers = append(f.buffers, NewReorderBuffer(cfg.Track, r.Name, cfg.Clock, cfg.Reorder))
		_ = i
	}
	return f, nil
}

// NoteSwitch records an externally driven ABR decision at a shared-clock
// boundary. This is used when adaptation policy lives outside the fanout (the
// synthetic fixture drives selection); the estimator/selector path remains
// available for internally driven streams.
func (f *Fanout) NoteSwitch(repIndex int, atTicks int64) error {
	if repIndex < 0 || repIndex >= len(f.reps) {
		return fmt.Errorf("switch rep index %d out of range", repIndex)
	}
	current := f.selector.Current()
	if repIndex == current {
		return nil
	}
	f.switches = append(f.switches, SwitchEvent{
		AtTicks: atTicks, FromRep: f.reps[current].Name, ToRep: f.reps[repIndex].Name,
	})
	f.selector.current = repIndex
	return nil
}

// Push feeds one received packet to its representation buffer, updates the
// bandwidth estimate and returns the frames that became releasable. Selection
// decisions at shared-timestamp boundaries are recorded as switch events.
func (f *Fanout) Push(p SimPacket) ([]model.Frame, error) {
	if p.RepIndex < 0 || p.RepIndex >= len(f.buffers) {
		return nil, fmt.Errorf("rep index %d out of range 0..%d", p.RepIndex, len(f.buffers)-1)
	}
	f.estimator.Observe(p.RecvTicks, len(p.Payload))
	if !f.external {
		prev := f.selector.Current()
		if idx, changed := f.selector.Choose(f.estimator.BitsPerSecond(), p.RecvTicks); changed {
			f.switches = append(f.switches, SwitchEvent{
				AtTicks: p.RecvTicks, FromRep: f.reps[prev].Name, ToRep: f.reps[idx].Name,
			})
		}
	}
	pkt := makeRTPPacket(p)
	frames := f.buffers[p.RepIndex].Push(pkt, p.SendTicks, p.RecvTicks)
	return f.dedup(frames), nil
}

// Flush releases every remaining buffered packet and closes open gaps.
func (f *Fanout) Flush() []model.Frame {
	var out []model.Frame
	for _, b := range f.buffers {
		out = append(out, f.dedup(b.Flush())...)
	}
	return out
}

// dedup enforces the single-contributor rule: each extended timestamp is
// contributed by exactly one representation; later copies are suppressed with
// a diagnostic instead of becoming a second frame.
func (f *Fanout) dedup(frames []model.Frame) []model.Frame {
	out := make([]model.Frame, 0, len(frames))
	for _, fr := range frames {
		// Gap placeholders are diagnostics, not contributed content: they do not
		// claim a timestamp and must not suppress one another.
		if fr.Incomplete {
			out = append(out, fr)
			continue
		}
		if owner, ok := f.seenTS[fr.Timestamp]; ok {
			f.suppressed++
			f.diag.Add(model.Diagnostic{Kind: model.KindFrameSuppressed, Severity: model.SeverityWarning,
				Track: f.track, AtTicks: fr.Timestamp, Recoverable: true,
				Message: fmt.Sprintf("timestamp %d already contributed by rep=%s; rep=%s copy suppressed",
					fr.Timestamp, owner, fr.Rep)})
			continue
		}
		f.seenTS[fr.Timestamp] = fr.Rep
		out = append(out, fr)
	}
	return out
}

// Switches returns the representation switch history in order.
func (f *Fanout) Switches() []SwitchEvent {
	out := make([]SwitchEvent, len(f.switches))
	copy(out, f.switches)
	return out
}

// RepStats returns per-representation recovery counters.
func (f *Fanout) RepStats() []Stats {
	out := make([]Stats, 0, len(f.buffers))
	for _, b := range f.buffers {
		out = append(out, b.Stats())
	}
	return out
}

// EstimatedBPS exposes the current smoothed bandwidth estimate.
func (f *Fanout) EstimatedBPS() int64 { return f.estimator.BitsPerSecond() }

// Suppressed returns the count of cross-representation duplicate frames dropped.
func (f *Fanout) Suppressed() int { return f.suppressed }

// Diagnostics aggregates fanout-level and per-buffer diagnostics.
func (f *Fanout) Diagnostics() []model.Diagnostic {
	out := f.diag.All()
	for _, b := range f.buffers {
		out = append(out, b.Diagnostics()...)
	}
	return out
}

// PendingTotal returns the total buffered packets across representations; the
// memory-bound test asserts this tracks the reorder window, not clip length.
func (f *Fanout) PendingTotal() int {
	n := 0
	for _, b := range f.buffers {
		n += b.Pending()
	}
	return n
}

func makeRTPPacket(p SimPacket) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: p.Seq,
			Timestamp:      p.RTPTS,
			Marker:         p.Marker,
		},
		Payload: append([]byte(nil), p.Payload...),
	}
}
