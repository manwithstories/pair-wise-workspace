package rtp

import "fmt"

// GapKind classifies what happened to one sequence gap.
type GapKind int

const (
	GapRecovered GapKind = iota
	GapUnrecoverable
)

// GapRecord is the audit trail entry for one gap.
type GapRecord struct {
	Gap      Gap
	Kind     GapKind
	ResumeAt string
}

// GapTracker accumulates gap outcomes and the unrecoverable intervals that
// the segmenter must exclude from playable output.
type GapTracker struct {
	Records []GapRecord
	First   *Gap
}

// Record stores a gap outcome; the first gap is retained for the report.
func (t *GapTracker) Record(g Gap, kind GapKind, resumeAt string) {
	if t.First == nil {
		cp := g
		t.First = &cp
	}
	t.Records = append(t.Records, GapRecord{Gap: g, Kind: kind, ResumeAt: resumeAt})
}

// Count returns totals by kind.
func (t *GapTracker) Count() (recovered, unrecoverable int) {
	for _, rec := range t.Records {
		if rec.Kind == GapRecovered {
			recovered++
		} else {
			unrecoverable++
		}
	}
	return
}

// String renders the first gap for terminal output.
func (t *GapTracker) String() string {
	rec, unrec := t.Count()
	s := fmt.Sprintf("gaps recovered=%d unrecoverable=%d", rec, unrec)
	if t.First != nil {
		s += fmt.Sprintf(" first=%d+%d", t.First.Start, t.First.Count)
	}
	return s
}

// ReceiverStats summarises one receive session.
type ReceiverStats struct {
	Emitted       int
	Duplicates    int
	Recovered     int
	Unrecoverable int
	Window        int
	Wait          string
	Pending       int
}

// Receiver composes the reorderer and XOR repair engine into the RTP front
// end. A one-packet gap is repaired from its XOR group; any wider gap is
// recorded as an unrecoverable interval and the flow resumes at the next
// contiguous packet. The TS layer later snaps resumption to a valid PSI
// table or IDR.
type Receiver struct {
	reorder  *Reorderer
	recovery *Recovery
	tracker  GapTracker
	emitted  int
	seenDecl int
	seenRep  int
}

// NewReceiver builds a receiver with default limits.
func NewReceiver() *Receiver {
	r := &Receiver{reorder: NewReorderer(), recovery: NewRecovery()}
	r.reorder.Repair = func(g Gap) ([]Packet, bool) {
		pkts, err := r.recovery.Recover(g)
		if err != nil {
			return nil, false
		}
		for _, p := range pkts {
			r.recovery.Observe(p)
		}
		for i := range pkts {
			pkts[i].PayloadType = 33
		}
		return pkts, true
	}
	return r
}

// AddFEC injects one XOR repair group.
func (r *Receiver) AddFEC(b FECBlock) error { return r.recovery.AddBlock(b) }

// SetBandwidth forwards the bitrate estimate to the jitter controller.
func (r *Receiver) SetBandwidth(bps int64) { r.reorder.SetBandwidth(bps) }

// Push ingests one packet and returns playable packets plus gap records.
func (r *Receiver) Push(p Packet) ([]Packet, []GapRecord) {
	r.recovery.Observe(p)
	out, _ := r.reorder.Push(p)
	recs := r.drainGapRecords()
	r.emitted += len(out)
	return out, recs
}

func (r *Receiver) drainGapRecords() []GapRecord {
	var recs []GapRecord
	for r.seenRep < len(r.reorder.Repaired) {
		g := r.reorder.Repaired[r.seenRep]
		r.seenRep++
		r.tracker.Record(g, GapRecovered, "xor")
		recs = append(recs, GapRecord{Gap: g, Kind: GapRecovered, ResumeAt: "xor"})
	}
	for r.seenDecl < len(r.reorder.Declared) {
		g := r.reorder.Declared[r.seenDecl]
		r.seenDecl++
		r.tracker.Record(g, GapUnrecoverable, "next-psi-or-idr")
		recs = append(recs, GapRecord{Gap: g, Kind: GapUnrecoverable, ResumeAt: "next-psi-or-idr"})
	}
	return recs
}

// Flush drains the reorder window at end of input.
func (r *Receiver) Flush() ([]Packet, []GapRecord) {
	out, _ := r.reorder.Flush()
	recs := r.drainGapRecords()
	r.emitted += len(out)
	return out, recs
}

// Stats snapshots counters for reports.
func (r *Receiver) Stats() ReceiverStats {
	rec, unrec := r.tracker.Count()
	return ReceiverStats{
		Emitted:       r.emitted,
		Duplicates:    r.reorder.Duplicates,
		Recovered:     rec,
		Unrecoverable: unrec,
		Window:        r.reorder.Window(),
		Wait:          r.reorder.Wait().String(),
		Pending:       r.reorder.Pending(),
	}
}

// Tracker exposes gap history.
func (r *Receiver) Tracker() *GapTracker { return &r.tracker }

// Reorderer exposes the reorderer for diagnostics.
func (r *Receiver) Reorderer() *Reorderer { return r.reorder }
