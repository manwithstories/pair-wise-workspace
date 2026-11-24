package ingest

import "math"

// SeqStatus records the finalized disposition of an extended RTP sequence.
type SeqStatus uint8

const (
	SeqPending SeqStatus = iota
	SeqDelivered
	SeqGap
	SeqLate
)

// OrderedPacket is a packet released in extended-sequence order.
type OrderedPacket struct {
	ExtSeq       int64
	ArrivalIndex int
	Record       *Record
}

// Gap identifies an RTP sequence declared missing after the bounded wait.
type Gap struct {
	ExtSeq int64
}

// FeedResult contains all dispositions caused by one incoming packet.
type FeedResult struct {
	Ordered    []OrderedPacket
	Gaps       []Gap
	Late       []OrderedPacket
	Duplicate  bool
	OutOfOrder bool
}

// Stats contains transport-level quality counters.
type Stats struct {
	RTPFrames         int
	RTCPFrames        int
	ParseErrors       int
	AcceptedPackets   int
	DuplicatePackets  int
	OutOfOrderPackets int
	GapPackets        int
	LatePackets       int
}

// ReorderBuffer restores order inside a fixed sequence span.
//
// A missing sequence is declared once highest-next >= Window. Packets that
// arrive after that declaration are returned in Late; the timeline decides
// whether they are still within replay retention.
type ReorderBuffer struct {
	Window     int
	StatusKeep int64
	next       int64
	maxExt     int64
	started    bool
	arrival    int
	pending    map[int64]*Record
	seen       map[int64]struct{}
	status     map[int64]SeqStatus
	Stats      Stats
}

// NewReorderBuffer creates a buffer with sensible bounded defaults.
func NewReorderBuffer(window int) *ReorderBuffer {
	if window <= 0 {
		window = 32
	}
	if window > 4096 {
		window = 4096
	}
	return &ReorderBuffer{
		Window:     window,
		StatusKeep: 4096,
		pending:    make(map[int64]*Record),
		seen:       make(map[int64]struct{}),
		status:     make(map[int64]SeqStatus),
	}
}

// ExtendSequence expands seq around prev across uint16 wraparound.
func ExtendSequence(prev int64, seq uint16) int64 {
	const wrap = 1 << 16
	u := int64(seq)
	candidate := u + wrap*int64(math.Round(float64(prev-u)/wrap))
	for candidate-prev > wrap/2 {
		candidate -= wrap
	}
	for prev-candidate > wrap/2 {
		candidate += wrap
	}
	return candidate
}

// Feed inserts one parsed RTP record.
func (b *ReorderBuffer) Feed(rec *Record) FeedResult {
	var result FeedResult
	if rec == nil || rec.Packet == nil {
		return result
	}
	b.arrival++
	b.Stats.RTPFrames++
	b.Stats.AcceptedPackets++

	seq := rec.Packet.Header.SequenceNumber
	if !b.started {
		ext := int64(seq)
		b.started = true
		b.next = ext
		b.maxExt = ext
		b.seen[ext] = struct{}{}
		b.status[ext] = SeqPending
		b.pending[ext] = rec
		b.advance(&result)
		return result
	}

	oldMax := b.maxExt
	ext := ExtendSequence(b.maxExt, seq)
	if ext > b.maxExt {
		b.maxExt = ext
	}

	if _, dup := b.seen[ext]; dup {
		b.Stats.DuplicatePackets++
		result.Duplicate = true
		return result
	}
	b.seen[ext] = struct{}{}

	op := OrderedPacket{ExtSeq: ext, ArrivalIndex: b.arrival, Record: rec}
	if ext < oldMax {
		b.Stats.OutOfOrderPackets++
		result.OutOfOrder = true
	}

	if ext < b.next {
		// The sequence was already declared as a gap or delivered. seen rules
		// out delivered duplicates, so this is a recovered late transmission.
		b.Stats.LatePackets++
		b.status[ext] = SeqLate
		result.Late = append(result.Late, op)
		b.prune()
		return result
	}

	b.pending[ext] = rec
	b.status[ext] = SeqPending
	b.advance(&result)
	b.prune()
	return result
}

func (b *ReorderBuffer) advance(result *FeedResult) {
	for b.started {
		if rec, ok := b.pending[b.next]; ok {
			result.Ordered = append(result.Ordered, OrderedPacket{
				ExtSeq:       b.next,
				ArrivalIndex: b.arrival,
				Record:       rec,
			})
			delete(b.pending, b.next)
			b.status[b.next] = SeqDelivered
			b.next++
			continue
		}

		if b.maxExt-b.next >= int64(b.Window) {
			result.Gaps = append(result.Gaps, Gap{ExtSeq: b.next})
			b.Stats.GapPackets++
			b.status[b.next] = SeqGap
			b.next++
			continue
		}
		break
	}
}

// Flush declares all remaining missing sequences and releases buffered data.
func (b *ReorderBuffer) Flush() FeedResult {
	var result FeedResult
	if !b.started {
		return result
	}
	for b.next <= b.maxExt {
		if rec, ok := b.pending[b.next]; ok {
			result.Ordered = append(result.Ordered, OrderedPacket{
				ExtSeq:       b.next,
				ArrivalIndex: b.arrival,
				Record:       rec,
			})
			delete(b.pending, b.next)
			b.status[b.next] = SeqDelivered
		} else {
			result.Gaps = append(result.Gaps, Gap{ExtSeq: b.next})
			b.Stats.GapPackets++
			b.status[b.next] = SeqGap
		}
		b.next++
	}
	b.prune()
	return result
}

// Next returns the next contiguous extended sequence position.
func (b *ReorderBuffer) Next() int64 { return b.next }

// Pending returns the number of packets held in the bounded window.
func (b *ReorderBuffer) Pending() int { return len(b.pending) }

func (b *ReorderBuffer) prune() {
	if b.next <= b.StatusKeep {
		return
	}
	cutoff := b.next - b.StatusKeep
	for k := range b.seen {
		if k < cutoff {
			delete(b.seen, k)
		}
	}
	for k := range b.status {
		if k < cutoff {
			delete(b.status, k)
		}
	}
}
