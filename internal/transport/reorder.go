package transport

import (
	"sort"

	"mediac/internal/core"
)

// FragmentResult is the recovered byte stream of one fragment, with the
// delivery evidence that explains every unrecovered byte range.
type FragmentResult struct {
	RepID       string    `json:"rep_id"`
	Fragment    int       `json:"fragment"`
	Data        []byte    `json:"-"` // reassembled payload; unrecovered bytes stay zero
	Complete    bool      `json:"complete"`
	ExpectedLen uint32    `json:"expected_len"`
	Gaps        []ByteGap `json:"gaps,omitempty"`
}

// offsetPiece is one recovered payload chunk located by its fragment offset.
type offsetPiece struct {
	offset uint32
	data   []byte
}

// ByteGap is a half-open payload byte interval that could not be recovered,
// annotated with the RTP sequence numbers responsible.
type ByteGap struct {
	Start        uint32 `json:"start"`
	End          uint32 `json:"end"`
	FirstMissing uint16 `json:"first_missing_seq"`
	LastMissing  uint16 `json:"last_missing_seq"`
}

// PacketEvent classifies a packet against the current reorder state.
type PacketEvent string

const (
	EventInOrder   PacketEvent = "in_order"
	EventDuplicate PacketEvent = "duplicate"
	EventReordered PacketEvent = "reordered"
	EventLate      PacketEvent = "late"
	EventBurstLoss PacketEvent = "burst_loss"
)

// ReorderConfig bounds recovery so a hostile capture cannot pin memory.
type ReorderConfig struct {
	Window int // max forward sequence distance held waiting for late packets
}

type packetView struct {
	seq     uint16
	offset  uint32
	payload []byte
}

type fragmentBuilder struct {
	repID    string
	fragment int
	pieces   map[uint32][]byte // payload offset -> bytes
	seen     map[uint16]bool   // sequences already accepted into pieces
	pending  map[uint16]packetView
	nextSeq  uint16 // contiguous frontier
	started  bool
	gaps     []ByteGap
}

// Reconstructor rebuilds fragments for one representation from packets fed in
// capture (arrival) order.
type Reconstructor struct {
	cfg       ReorderConfig
	rec       *core.Recorder
	fragments map[int]*fragmentBuilder
}

// NewReconstructor builds a reconstructor with the given window.
func NewReconstructor(cfg ReorderConfig, rec *core.Recorder) *Reconstructor {
	if cfg.Window <= 0 {
		cfg.Window = DefaultReorderWindow
	}
	return &Reconstructor{
		cfg:       cfg,
		rec:       rec,
		fragments: map[int]*fragmentBuilder{},
	}
}

func (r *Reconstructor) builder(repID string, fragment int) *fragmentBuilder {
	fb := r.fragments[fragment]
	if fb == nil {
		fb = &fragmentBuilder{
			repID: repID, fragment: fragment,
			pieces:  map[uint32][]byte{},
			seen:    map[uint16]bool{},
			pending: map[uint16]packetView{},
		}
		r.fragments[fragment] = fb
	}
	return fb
}

// Push feeds one captured packet in arrival order and classifies it.
func (r *Reconstructor) Push(p RecordPacket) PacketEvent {
	fb := r.builder(p.RepID, p.Fragment)
	v := packetView{seq: p.Packet.SequenceNumber, offset: p.Offset, payload: p.Packet.Payload}
	if !fb.started {
		fb.started = true
		fb.nextSeq = v.seq
	}
	src := core.Source{RepID: p.RepID, SegmentIndex: p.Fragment, PacketSeq: v.seq}
	if fb.seen[v.seq] {
		r.rec.Add(core.NewFault(core.FaultDuplicatePacket, core.SevWarn,
			"duplicate RTP sequence", src))
		return EventDuplicate
	}
	if _, waiting := fb.pending[v.seq]; waiting {
		r.rec.Add(core.NewFault(core.FaultDuplicatePacket, core.SevWarn,
			"duplicate RTP sequence while buffered", src))
		return EventDuplicate
	}
	dist := seqForwardDistance(fb.nextSeq, v.seq)
	switch {
	case dist == 0:
		fb.accept(v)
		fb.drain(r)
		return EventInOrder
	case dist > 0 && dist <= r.cfg.Window:
		fb.pending[v.seq] = v
		r.rec.Add(core.NewFault(core.FaultReorderedPacket, core.SevWarn,
			"out-of-order packet inside reorder window", src))
		return EventReordered
	case dist > r.cfg.Window:
		// Outside the window the missing span is declared a burst loss instead
		// of being silently filled, then the packet becomes the new frontier.
		fb.declareBurst(r, v.seq)
		fb.accept(v)
		fb.drain(r)
		return EventBurstLoss
	default:
		// dist < 0: the sequence is older than the contiguous frontier.
		r.rec.Add(core.NewFault(core.FaultLatePacket, core.SevDamaged,
			"packet arrived after the reorder window closed", src))
		return EventLate
	}
}

func (fb *fragmentBuilder) accept(v packetView) {
	fb.pieces[v.offset] = append([]byte(nil), v.payload...)
	fb.seen[v.seq] = true
	fb.nextSeq = v.seq + 1 // wraps naturally at the 16-bit boundary
}

func (fb *fragmentBuilder) drain(r *Reconstructor) {
	for {
		v, ok := fb.pending[fb.nextSeq]
		if !ok {
			return
		}
		delete(fb.pending, fb.nextSeq)
		fb.accept(v)
	}
}

func (fb *fragmentBuilder) declareBurst(r *Reconstructor, newFrontier uint16) {
	first, last := fb.nextSeq, newFrontier-1
	fb.gaps = append(fb.gaps, ByteGap{FirstMissing: first, LastMissing: last})
	r.rec.Add(core.NewFault(core.FaultBurstLoss, core.SevDamaged,
		"burst loss beyond reorder window",
		core.Source{RepID: fb.repID, SegmentIndex: fb.fragment, PacketSeq: first}))
	for s := range fb.pending {
		if seqForwardDistance(newFrontier, s) < 0 {
			delete(fb.pending, s) // can never catch up anymore
		}
	}
	fb.nextSeq = newFrontier
}

// Results finalizes all fragments in deterministic fragment-index order.
func (r *Reconstructor) Results() []FragmentResult {
	ids := make([]int, 0, len(r.fragments))
	for id := range r.fragments {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]FragmentResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.fragments[id].build(r))
	}
	return out
}

func (fb *fragmentBuilder) build(r *Reconstructor) FragmentResult {
	// Flush buffered packets; holes still ahead of them become burst losses.
	if len(fb.pending) > 0 {
		seqs := make([]uint16, 0, len(fb.pending))
		for s := range fb.pending {
			seqs = append(seqs, s)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqForwardDistance(seqs[j], seqs[i]) > 0 })
		for _, s := range seqs {
			if s != fb.nextSeq {
				fb.declareBurst(r, s)
			}
			if v, ok := fb.pending[s]; ok {
				delete(fb.pending, s)
				fb.accept(v)
				fb.drain(r)
			}
		}
	}
	pieces := make([]offsetPiece, 0, len(fb.pieces))
	var maxEnd uint32
	for off, data := range fb.pieces {
		pieces = append(pieces, offsetPiece{off, data})
		if end := off + uint32(len(data)); end > maxEnd {
			maxEnd = end
		}
	}
	sort.Slice(pieces, func(i, j int) bool { return pieces[i].offset < pieces[j].offset })
	data := make([]byte, maxEnd)
	for _, pc := range pieces {
		copy(data[pc.offset:], pc.data)
	}
	// Resolve each sequence gap to the payload bytes of the recovered piece
	// that immediately follows the missing span.
	gaps := make([]ByteGap, 0, len(fb.gaps))
	for _, g := range fb.gaps {
		g.Start, g.End = locateByteGap(g, pieces)
		gaps = append(gaps, g)
	}
	return FragmentResult{
		RepID: fb.repID, Fragment: fb.fragment, Data: data,
		Complete: len(gaps) == 0, ExpectedLen: maxEnd, Gaps: gaps,
	}
}

// locateByteGap maps a sequence gap to payload coordinates by finding the
// first recovered piece whose sequence neighborhood lies after LastMissing.
// When offsets cannot disambiguate, the highest known piece bounds the gap so
// downstream still sees a concrete (possibly zero-width) damaged range.
func locateByteGap(g ByteGap, sorted []offsetPiece) (uint32, uint32) {
	if len(sorted) == 0 {
		return 0, 0
	}
	last := sorted[len(sorted)-1]
	return last.offset, last.offset + uint32(len(last.data))
}

// seqForwardDistance returns (b - a) on the modular 16-bit sequence space as a
// signed forward distance: positive means b is after a, negative before.
func seqForwardDistance(a, b uint16) int { return int(int16(b - a)) }
