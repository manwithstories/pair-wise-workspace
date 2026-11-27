package pipeline

import (
	"time"

	"mediacheck/internal/inspect"
	"mediacheck/internal/model"
	"mediacheck/internal/recover"
)

// Options configures a run.
type Options struct {
	// QueueItems and QueueBytes are the hard in-flight bounds.
	QueueItems int
	QueueBytes int64
	// SlowConsumer delays every consumer step, which is how the demo proves
	// backpressure reaches the read stage.  Zero disables the delay.
	SlowConsumer time.Duration
	// Recovery enables Opus FEC/PLC/silence filling for audio holes.
	Recovery bool
	// LatencyWindow bounds how many consumer latencies are kept for the P95
	// calculation.  Older samples are discarded, not averaged.
	LatencyWindow int
	// Emit, when set, receives every packet that leaves the consumer after
	// recovery, in stream order.  It is how remux writes exactly the
	// packets the pipeline accepted, including recovered placeholders.  A
	// returned error aborts the run.
	Emit func(model.Packet) error
}

// Result is everything the report needs after a run.
type Result struct {
	Packets      int
	Bytes        int64
	Faults       []model.Fault
	Damage       model.DamageSet
	Recovery     recover.Outcomes
	Capabilities recover.Capabilities
	Inspection   inspect.Result
	Queue        QueueStats
	Latency      LatencyStats
	Canceled     bool
	Elapsed      time.Duration
	FirstSlot    int64
	LastSlot     int64
	TrackSpan    map[model.Track]int64
}

// SpanNanos returns the media span covered by the run.
func (r *Result) SpanNanos() int64 {
	if r.FirstSlot == 0 && r.LastSlot == 0 {
		return 0
	}
	return r.LastSlot - r.FirstSlot
}

// P95Millis exposes the latency metric in the unit the demo prints.
func (r *Result) P95Millis() float64 { return float64(r.Latency.P95) / float64(time.Millisecond) }

// SyncErrorMillis is the difference between the audio and video timelines at
// the end of the run, in milliseconds.  It is computed from the exact
// nanosecond slots the inspector recorded, not from packet counts, so a
// lost audio tail or a video frame that never arrived shows up as drift.
func (r *Result) SyncErrorMillis() float64 {
	a, aok := r.TrackSpan[model.TrackAudio]
	v, vok := r.TrackSpan[model.TrackVideo]
	if !aok || !vok {
		return 0
	}
	diff := a - v
	if diff < 0 {
		diff = -diff
	}
	return float64(diff) / 1e6
}
