package rtpx

import "sync"

// Analyzer groups per-SSRC stream state and is safe for concurrent use.
type Analyzer struct {
	mu     sync.Mutex
	cfg    Config
	stream map[uint32]*streamState
}

// NewAnalyzer builds an analyser. A non-positive clock rate falls back to
// 90000 and a non-positive window falls back to the default, so a bad config
// can never produce divide-by-zero jitter.
func NewAnalyzer(cfg Config) *Analyzer {
	if cfg.ClockRate <= 0 {
		cfg.ClockRate = 90000
	}
	if cfg.ReorderWindow <= 0 {
		cfg.ReorderWindow = DefaultReorderWindow
	}
	return &Analyzer{cfg: cfg, stream: make(map[uint32]*streamState)}
}

// Observe folds one received packet into the per-SSRC state.
func (a *Analyzer) Observe(p Packet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.stream[p.SSRC]
	if st == nil {
		st = &streamState{cfg: a.cfg, seen: make(map[uint64]struct{})}
		a.stream[p.SSRC] = st
	}
	st.observe(p)
}

// SSRCs returns the SSRCs observed so far, in no particular order.
func (a *Analyzer) SSRCs() []uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]uint32, 0, len(a.stream))
	for ssrc := range a.stream {
		out = append(out, ssrc)
	}
	return out
}

// observe classifies one packet and updates every derived statistic.
//
// Order of the cases matters: a sequence above the high-water mark extends the
// stream, a sequence already recorded is a duplicate, and everything else is a
// late arrival that is either inside the reorder window (counted as reordered)
// or outside it (counted as late, requirement 6).
func (st *streamState) observe(p Packet) {
	st.received++
	st.payload += uint64(p.Payload)
	if p.Marker {
		st.markers++
	}
	if p.Arrival.IsZero() {
		p.Arrival = nowMonotonic()
	}

	if !st.started {
		st.started = true
		st.firstSeq = uint64(p.Sequence)
		st.highest = uint64(p.Sequence)
		st.unique = 1
		st.seen[st.highest] = struct{}{}
		st.lastArr, st.lastTS, st.haveTrans = p.Arrival, p.Timestamp, true
		return
	}

	ext, wrapped := st.extend(p.Sequence)
	if wrapped {
		st.wraps++
	}
	switch {
	case ext > st.highest:
		st.unique++
		st.seen[ext] = struct{}{}
		st.highest = ext
		st.recordJitter(p, true)
	case st.seenHas(ext):
		st.duplicates++
	default:
		st.unique++
		st.seen[ext] = struct{}{}
		if st.highest-ext > uint64(st.cfg.ReorderWindow) {
			// Too late to attribute to reordering: report it separately
			// instead of pretending the sender lost it.
			st.late++
			return
		}
		st.reordered++
		st.recordJitter(p, false)
	}
	st.prune()
}
