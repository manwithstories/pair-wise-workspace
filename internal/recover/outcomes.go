package recover

import (
	"mediacheck/internal/model"
)

// Outcomes counts per-method fills and the slots that stayed missing.
type Outcomes struct {
	Slots   int
	FEC     int
	PLC     int
	Silence int
	Failed  int
	Bytes   int64
	Reasons map[string]int
	damage  model.DamageSet
	faults  []model.Fault
}

// RecoveryRate is the share of lost slots that were filled by any method.
// It is the metric required by the demo output.
func (o Outcomes) RecoveryRate() float64 {
	if o.Slots == 0 {
		return 100
	}
	return 100 * float64(o.Slots-o.Failed) / float64(o.Slots)
}

// Filled is the number of slots that received any placeholder.
func (o Outcomes) Filled() int { return o.Slots - o.Failed }

// Damage returns the merged damaged ranges produced by fills.
func (o Outcomes) Damage() []model.DamagedRange { return o.damage.Ranges() }

// Faults returns the per-slot fault records.
func (o Outcomes) Faults() []model.Fault { return o.faults }

// Tracker accumulates slot outcomes for one run and enforces the guarantee
// that the recovery rate never counts an unfilled slot as filled.
type Tracker struct {
	out Outcomes
}

// NewTracker returns a tracker with initialised reason counters.
func NewTracker() *Tracker {
	return &Tracker{out: Outcomes{Reasons: map[string]int{}}}
}

// Record adds one slot result to the totals and damaged ranges.
func (t *Tracker) Record(res SlotResult) {
	if res.Method == MethodGiveUp {
		t.out.Slots++
		t.out.Failed++
		t.out.damage.AddRange(res.Slot.Start, res.Slot.End, model.FaultLoss)
		t.out.faults = append(t.out.faults, model.Fault{
			Kind: model.FaultLoss, Track: model.TrackAudio, Slot: res.Slot,
			Note: "unfilled: " + res.Reason,
		})
		return
	}
	t.out.Slots++
	switch res.Method {
	case MethodFEC:
		t.out.FEC++
	case MethodPLC:
		t.out.PLC++
	case MethodSilence:
		t.out.Silence++
	}
	t.out.Bytes += int64(len(res.Data))
	if res.Reason != "" {
		t.out.Reasons[res.Reason]++
	}
	t.out.damage.AddRange(res.Slot.Start, res.Slot.End, res.Method.FaultKind())
	t.out.faults = append(t.out.faults, model.Fault{
		Kind: res.Method.FaultKind(), Track: model.TrackAudio, Slot: res.Slot, Note: res.Reason,
	})
}

// Outcomes returns a snapshot of what has been recorded.
func (t *Tracker) Outcomes() Outcomes {
	o := t.out
	o.Reasons = make(map[string]int, len(t.out.Reasons))
	for k, v := range t.out.Reasons {
		o.Reasons[k] = v
	}
	o.faults = append([]model.Fault(nil), t.out.faults...)
	return o
}

// Merge adds outcomes from a second tracker, used when audio and video
// recovery run separately.
func (o *Outcomes) Merge(other Outcomes) {
	o.Slots += other.Slots
	o.FEC += other.FEC
	o.PLC += other.PLC
	o.Silence += other.Silence
	o.Failed += other.Failed
	o.Bytes += other.Bytes
	o.faults = append(o.faults, other.faults...)
	for _, r := range other.damage.Ranges() {
		o.damage.AddRange(r.Start, r.End, r.Kind)
	}
}
