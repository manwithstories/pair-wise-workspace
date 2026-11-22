package merge

import (
	"container/heap"
	"sync"

	"medpipe/internal/clock"
	"medpipe/internal/fault"
	rtpstream "medpipe/internal/rtp"
)

// TrackStats summarizes one track after the stream ended.
type TrackStats struct {
	Track    uint32
	Received uint64 // unique packets placed on the timeline
	Missing  uint64 // in-range gaps inside the observed sequence span
	TailLoss uint64 // packets the sender reports sent beyond the last seen
	Sent     uint64 // sender-reported total packets (last SR count)
	Isolated bool   // clock drift beyond threshold; track quarantined
}

// Merger aligns tracks onto the shared timeline and emits events in
// deterministic (shared time, track, extended sequence) order.
//
// Memory is bounded: each track holds at most its reorder window plus
// queueCap calibrated events; exceeding the cap sets Fatal instead of
// growing without limit, so a stalled track can never silently reorder
// or sink the timeline.
type Merger struct {
	window         int
	driftThreshold float64 // seconds
	queueCap       int
	tracks         map[uint32]*trackState
	heap           eventHeap
	fl             *fault.List
	fatal          error
	fatalMu        sync.Mutex
	baseSec        uint32
	hasBase        bool
	// Anomalies holds cross-track calibration faults (drift,
	// isolation). They are mirrored into the provenance manifest.
	Anomalies []*fault.Error
}

type trackState struct {
	buf      *trackBuffer
	calib    clock.Calibration
	hasCalib bool
	uncal    []*Event // buffer-emitted, waiting for the first SR
	queue    []*Event // calibrated, waiting behind head
	head     *Event   // next event offered to the k-way merge
	drained  bool
	isolated bool

	received uint64
	missing  uint64
	srCount  uint32 // last sender-report packet count
	hasSR    bool
}

// NewMerger creates a Merger. window bounds per-track reordering;
// driftThreshold is the maximum allowed clock disagreement in seconds.
func NewMerger(window int, driftThreshold float64, fl *fault.List) *Merger {
	if window < 1 {
		window = 1
	}
	if window > MaxReorderWindow {
		window = MaxReorderWindow
	}
	if fl == nil {
		fl = &fault.List{}
	}
	if fl.Cap == 0 {
		fl.Cap = 1024
	}
	return &Merger{
		window:         window,
		driftThreshold: driftThreshold,
		queueCap:       8 * window,
		tracks:         map[uint32]*trackState{},
		fl:             fl,
	}
}

// Faults returns the shared fault list.
func (m *Merger) Faults() *fault.List { return m.fl }

// Fatal returns the first non-recoverable merge error, if any. It is
// safe to call from the driver goroutine while the consumer applies
// events.
func (m *Merger) Fatal() error {
	m.fatalMu.Lock()
	defer m.fatalMu.Unlock()
	return m.fatal
}

func (m *Merger) track(id uint32) *trackState {
	ts, ok := m.tracks[id]
	if !ok {
		ts = &trackState{buf: newTrackBuffer(id, m.window)}
		m.tracks[id] = ts
	}
	return ts
}

// AddSenderReport calibrates a track from an RTCP sender report. A
// report that disagrees with the current calibration beyond the drift
// threshold isolates the track: its packets are dropped with located
// errors and no other track is affected.
func (m *Merger) AddSenderReport(track uint32, sr *rtpstream.SenderReport, off int64) {
	ts := m.track(track)
	ts.srCount = sr.PacketCount
	ts.hasSR = true
	if ts.isolated {
		return
	}
	// Shared-timeline values are kept relative to the first sender
	// report's whole seconds, so rational arithmetic stays far from
	// int64 limits.
	if !m.hasBase {
		m.baseSec = sr.NTPSec
		m.hasBase = true
	}
	rel := clock.New(int64(sr.NTPSec)-int64(m.baseSec), 1).
		Add(clock.New(int64(sr.NTPFrac), 1<<32))
	calib := clock.NewCalibration(rtpstream.ClockRate, sr.RTPTimestamp, rel)
	if !ts.hasCalib {
		ts.calib = calib
		ts.hasCalib = true
		rest := ts.uncal
		ts.uncal = nil
		for _, ev := range rest {
			m.calibrate(ts, ev)
		}
		return
	}
	predicted := ts.calib.ToShared(sr.RTPTimestamp)
	diff := predicted.Sub(rel).Float()
	if diff < 0 {
		diff = -diff
	}
	if diff > m.driftThreshold {
		ts.isolated = true
		// Discard queued events and uncount them, so Received always
		// equals the number of events actually emitted downstream.
		discarded := uint64(len(ts.uncal) + len(ts.queue))
		if ts.head != nil {
			discarded++
		}
		ts.received -= discarded
		ts.uncal = nil
		ts.queue = nil
		ts.head = nil
		fe := fault.New(fault.KindDrift, int64(track), fault.Unknown, off, true,
			"rtcp clock jump beyond drift threshold; track isolated")
		m.fl.Add(fe)
		m.Anomalies = append(m.Anomalies, fe)
		return
	}
	ts.calib = calib
}

// AddEvent feeds one decoded packet. Ready events are calibrated and
// offered to the merge heap.
func (m *Merger) AddEvent(ev *Event) {
	if m.Fatal() != nil {
		return
	}
	ts := m.track(ev.Track)
	if ts.isolated {
		m.fl.Add(fault.New(fault.KindDrift, int64(ev.Track), int64(ev.ExtSeq),
			ev.Offset, true, "packet dropped from isolated track"))
		return
	}
	for _, ready := range ts.buf.push(ev, m.fl) {
		m.offer(ts, ready)
		if m.Fatal() != nil {
			return
		}
	}
}

// offer calibrates one buffer-emitted event, or parks it until the
// first sender report arrives.
func (m *Merger) offer(ts *trackState, ev *Event) {
	if !ts.hasCalib {
		if len(ts.uncal) >= m.queueCap {
			m.setFatal(fault.New(fault.KindResource, int64(ev.Track),
				int64(ev.ExtSeq), ev.Offset, false,
				"uncalibrated backlog limit exceeded"))
			return
		}
		ts.uncal = append(ts.uncal, ev)
		return
	}
	m.calibrate(ts, ev)
}

func (m *Merger) calibrate(ts *trackState, ev *Event) {
	ev.Shared = ts.calib.ToShared(ev.RTPTimestamp)
	ts.received++
	ts.missing += uint64(ev.MissingBefore)
	if ts.head == nil {
		ts.head = ev
		heap.Push(&m.heap, headRef{ev: ev, track: ts})
		return
	}
	if len(ts.queue) >= m.queueCap {
		m.setFatal(fault.New(fault.KindResource, int64(ev.Track),
			int64(ev.ExtSeq), ev.Offset, false,
			"per-track merge queue limit exceeded"))
		return
	}
	ts.queue = append(ts.queue, ev)
}

func (m *Merger) setFatal(err error) {
	m.fatalMu.Lock()
	defer m.fatalMu.Unlock()
	if m.fatal == nil {
		m.fatal = err
		m.fl.Add(err)
	}
}

// Pop emits the next timeline event, or nil when the merge is blocked
// (some live track has offered nothing) or exhausted. Blocking on a
// slow track is what applies backpressure instead of silently
// reordering the timeline.
func (m *Merger) Pop() *Event {
	for _, ts := range m.tracks {
		if !ts.drained && !ts.isolated && ts.head == nil {
			return nil
		}
	}
	if m.heap.Len() == 0 {
		return nil
	}
	top := heap.Pop(&m.heap).(headRef)
	ts := top.track
	ev := ts.head
	ts.head = nil
	if len(ts.queue) > 0 {
		next := ts.queue[0]
		ts.queue = ts.queue[1:]
		ts.head = next
		heap.Push(&m.heap, headRef{ev: next, track: ts})
	}
	return ev
}

// Flush ends the stream: all buffers are flushed, still-uncalibrated
// events use the identity anchor, and per-track stats become available
// via Stats. After Flush, Pop drains the heap.
func (m *Merger) Flush() {
	for _, ts := range m.tracks {
		if ts.isolated {
			ts.drained = true
			continue
		}
		for _, ev := range ts.buf.flush() {
			m.offer(ts, ev)
		}
		if !ts.hasCalib && len(ts.uncal) > 0 {
			ts.calib = clock.NewCalibration(rtpstream.ClockRate, 0, clock.New(0, 1))
			ts.hasCalib = true
			rest := ts.uncal
			ts.uncal = nil
			for _, ev := range rest {
				m.calibrate(ts, ev)
			}
		}
		ts.missing += uint64(ts.buf.TailGaps())
		ts.drained = true
	}
}

// Stats reports per-track totals. Tail loss is derived from the last
// sender-report packet count versus what the timeline observed.
func (m *Merger) Stats() []TrackStats {
	var out []TrackStats
	for id, ts := range m.tracks {
		st := TrackStats{Track: id, Received: ts.received,
			Missing: ts.missing, Isolated: ts.isolated}
		if ts.hasSR {
			st.Sent = uint64(ts.srCount)
			if total := uint64(ts.srCount); total > ts.received+ts.missing {
				st.TailLoss = total - ts.received - ts.missing
			}
		}
		out = append(out, st)
	}
	return out
}

// headRef is one track's current head event in the merge heap.
type headRef struct {
	ev    *Event
	track *trackState
}

// eventHeap orders by (shared time, track, extended sequence).
type eventHeap []headRef

func (h eventHeap) Len() int { return len(h) }

func (h eventHeap) Less(i, j int) bool {
	a, b := h[i].ev, h[j].ev
	if c := a.Shared.Cmp(b.Shared); c != 0 {
		return c < 0
	}
	if a.Track != b.Track {
		return a.Track < b.Track
	}
	return a.ExtSeq < b.ExtSeq
}

func (h eventHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x interface{}) { *h = append(*h, x.(headRef)) }
func (h *eventHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
