package mp4

// SeqGap records a closed range of missing mfhd sequence numbers.
type SeqGap struct {
	From, To uint32 // inclusive
}

// Len returns how many sequence numbers are missing in the gap.
func (g SeqGap) Len() int { return int(g.To-g.From) + 1 }

// SeqTracker observes mfhd sequence numbers in arrival order. It keeps the
// original numbers untouched and only annotates gaps, duplicates and
// out-of-order arrivals; it never invents samples for missing numbers.
type SeqTracker struct {
	seen       map[uint32]struct{}
	max        uint32
	started    bool
	Gaps       []SeqGap
	Duplicates []uint32
	OutOfOrder []uint32
}

// NewSeqTracker returns an empty tracker.
func NewSeqTracker() *SeqTracker {
	return &SeqTracker{seen: map[uint32]struct{}{}}
}

// Observe records one arrived sequence number.
func (t *SeqTracker) Observe(seq uint32) {
	if _, dup := t.seen[seq]; dup {
		t.Duplicates = append(t.Duplicates, seq)
		return
	}
	t.seen[seq] = struct{}{}
	if !t.started {
		t.max, t.started = seq, true
		return
	}
	switch {
	case seq > t.max+1:
		t.Gaps = append(t.Gaps, SeqGap{t.max + 1, seq - 1})
		t.max = seq
	case seq == t.max+1:
		t.max = seq
	default: // seq <= t.max: late or out-of-order arrival
		t.OutOfOrder = append(t.OutOfOrder, seq)
		t.fillGap(seq)
	}
}

// fillGap removes seq from a previously recorded gap.
func (t *SeqTracker) fillGap(seq uint32) {
	for i, g := range t.Gaps {
		if seq < g.From || seq > g.To {
			continue
		}
		switch {
		case g.From == g.To:
			t.Gaps = append(t.Gaps[:i], t.Gaps[i+1:]...)
		case seq == g.From:
			t.Gaps[i].From++
		case seq == g.To:
			t.Gaps[i].To--
		default:
			t.Gaps = append(t.Gaps[:i], append([]SeqGap{{g.From, seq - 1}, {seq + 1, g.To}}, t.Gaps[i+1:]...)...)
		}
		return
	}
}

// Missing returns the count of sequence numbers still unaccounted for.
func (t *SeqTracker) Missing() int {
	n := 0
	for _, g := range t.Gaps {
		n += g.Len()
	}
	return n
}
