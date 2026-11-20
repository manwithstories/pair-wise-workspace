// Package rtp orders captured frame descriptors by their RTP sequence
// numbers before any later stage touches them. It is deliberately payload-free:
// callers pass a small descriptor per frame, so the reorder buffer's memory is
// bounded by the window capacity rather than by frame sizes.
//
// The stage distinguishes the defects retransmission actually produces:
//
//   - duplicates (same extended sequence seen twice),
//   - reordering inside the window (held and emitted in order),
//   - late frames whose slot has already been passed (never inserted at a
//     guessed position; recorded as late arrivals and left as gaps),
//   - 16-bit sequence rollover (expanded transparently),
//   - batch resets, where capture numbering restarts at zero,
//   - burst loss (a gap larger than one frame).
package rtp

import (
	"fmt"
	"sort"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/pion/rtp"
)

// SeqMod is the 16-bit RTP sequence modulus.
const SeqMod uint64 = 1 << 16

// WrapDistance is half the sequence space.
const WrapDistance = SeqMod / 2

// DefaultWindow is the reorder depth used when none is configured.
const DefaultWindow = 64

// ResetThreshold is the smallest backwards jump to sequence zero that is
// interpreted as a new capture batch rather than reordering.
const ResetThreshold = 1024

// maxHistory bounds the duplicate-detection history of already-delivered
// sequence numbers so memory stays flat on long streams.
const maxHistory = 1 << 14

// Frame is one frame descriptor entering the reorder stage.
type Frame struct {
	Camera    string
	FrameSeq  uint64 // source frame record sequence (batch-relative)
	MasterTS  uint64 // unified 90 kHz timestamp, when known
	Offset    int64  // byte offset of the source record
	RTPSeq    uint16
	Timestamp uint32 // RTP 32-bit timestamp
	Keyframe  bool
	Size      int // retained payload size, used for the byte budget
	// Hash and Expected carry the payload digest through reordering so the
	// verifier can compare them without retaining payload bytes.
	Hash     string
	Expected string
}

// Gap is a run of missing extended sequence numbers, inclusive.
type Gap struct {
	From     uint64 `json:"from"`
	To       uint64 `json:"to"`
	Missing  uint64 `json:"missing"`
	AfterSeq uint64 `json:"after_seq"`
	MasterTS uint64 `json:"master_ts"`
}

// Late is a frame that arrived after its slot had already been passed.
type Late struct {
	FrameSeq uint64 `json:"frame_seq"`
	ExtSeq   uint64 `json:"ext_seq"`
	Offset   int64  `json:"offset"`
	Distance uint64 `json:"distance"`
}

// Config controls one reorderer.
type Config struct {
	Camera string
	// Window is the maximum number of frames held while waiting for a hole.
	Window int
	// MaxBytes bounds the described payload bytes held in the window.
	MaxBytes int
}

// Result is the ordering outcome for one camera.
type Result struct {
	Camera       string
	Ordered      []Frame
	Duplicates   int
	Late         []Late
	Gaps         []Gap
	Wraps        int
	Resets       int
	PeakBuffered int
	Faults       fault.List
}

// Reorderer is a stateful single-camera reorder buffer.
type Reorderer struct {
	cfg Config

	haveSeq   bool
	prevRaw   uint16 // raw sequence of the most recent arrival
	epochBase uint64 // extended value assigned to raw sequence 0 of this epoch
	lastExt   uint64 // extended sequence of the most recent arrival

	next     uint64 // next extended sequence to deliver
	haveNext bool

	buf      map[uint64]Frame
	bufBytes int

	emitted map[uint64]bool
	lateSet map[uint64]bool

	drained       int // number of entries in res.Ordered already drained
	drainedFaults int // number of entries in res.Faults already drained

	res Result
}

// New validates cfg and returns a reorderer.
func New(cfg Config) (*Reorderer, error) {
	if cfg.Camera == "" {
		return nil, fmt.Errorf("rtp: empty camera id")
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 4 << 20
	}
	return &Reorderer{
		cfg:     cfg,
		buf:     map[uint64]Frame{},
		emitted: map[uint64]bool{},
		lateSet: map[uint64]bool{},
		res:     Result{Camera: cfg.Camera},
	}, nil
}

// ParsePacket extracts sequence number, timestamp and marker from an RTP
// packet. It is the one place the pion/rtp dependency is used.
func ParsePacket(b []byte) (seq uint16, ts uint32, marker bool, err error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(b); err != nil {
		return 0, 0, false, err
	}
	return pkt.SequenceNumber, pkt.Timestamp, pkt.Marker, nil
}

// Delta returns the signed 16-bit distance from prev to raw. Values above
// half the sequence space are negative, which is exactly the RTP rule for
// distinguishing a small backwards reorder from a forward rollover.
func Delta(prev, raw uint16) int {
	return int(int16(raw - prev))
}

// Extend expands raw against the previous extended value using the signed
// 16-bit distance. The bool reports a forward rollover.
func Extend(prevExt uint64, prevRaw, raw uint16) (uint64, bool) {
	d := Delta(prevRaw, raw)
	return prevExt + uint64(int64(d)), d > 0 && raw < prevRaw
}

// Push feeds one frame into the buffer. Frames become available in order
// immediately; Finish returns everything including the flushed tail.
func (r *Reorderer) Push(f Frame) {
	raw := f.RTPSeq
	var ext uint64
	reset := false

	switch {
	case !r.haveSeq:
		ext = uint64(raw)
	default:
		prevRaw := r.prevRaw
		d := Delta(prevRaw, raw)
		switch {
		case raw == 0 && prevRaw >= ResetThreshold && -d >= ResetThreshold:
			// Capture numbering restarted at zero: open a new epoch after
			// the last sequence seen so gaps stay well defined.
			reset = true
			r.epochBase = r.lastExt + 1
			ext = r.epochBase + uint64(raw)
		case d > 0 && raw < prevRaw:
			// Forward rollover past 65535. Extend the epoch base so the
			// extended sequence keeps increasing.
			r.epochBase += SeqMod
			ext = r.epochBase + uint64(raw)
			r.res.Wraps++
			r.res.Faults = append(r.res.Faults, fault.Fault{
				Kind: fault.KindSequenceWrap, Stage: fault.StageReorder,
				Camera: r.cfg.Camera, Seq: f.FrameSeq, Offset: f.Offset,
				Message: fmt.Sprintf("16-bit RTP sequence wrapped: %d -> %d", prevRaw, raw),
			})
		case d < 0 && uint64(prevRaw) < SeqMod-WrapDistance && uint64(raw) > SeqMod-WrapDistance && r.epochBase >= SeqMod:
			// A late frame from the previous epoch (e.g. 65535 arriving
			// after 3). Map it back so it is reported as late, not future.
			ext = r.epochBase - SeqMod + uint64(raw)
		default:
			ext = r.epochBase + uint64(raw)
		}
	}
	r.apply(f, ext, reset)
}

// apply buffers or rejects a frame once its extended sequence is known.
func (r *Reorderer) apply(f Frame, ext uint64, reset bool) {
	if reset {
		r.res.Resets++
		r.res.Faults = append(r.res.Faults, fault.Fault{
			Kind: fault.KindSequenceWrap, Stage: fault.StageReorder,
			Camera: r.cfg.Camera, Seq: f.FrameSeq, Offset: f.Offset,
			Message: fmt.Sprintf("RTP sequence reset %d -> %d starts a new batch", r.prevRaw, f.RTPSeq),
		})
		r.flushAll()
		r.next = ext
		r.haveNext = true
		r.emitted = map[uint64]bool{}
		r.lateSet = map[uint64]bool{}
	}
	r.haveSeq = true
	r.prevRaw = f.RTPSeq
	r.lastExt = ext
	if !r.haveNext {
		r.next = ext
		r.haveNext = true
	}

	switch {
	case r.emitted[ext]:
		r.res.Duplicates++
		r.res.Faults = append(r.res.Faults, fault.Fault{
			Kind: fault.KindDuplicateFrame, Stage: fault.StageReorder,
			Camera: r.cfg.Camera, Seq: f.FrameSeq, Offset: f.Offset,
			Message:  fmt.Sprintf("extended RTP sequence %d already delivered", ext),
			Expected: fmt.Sprintf(">= %d", r.next), Actual: fmt.Sprintf("%d", ext),
		})
		return
	case ext < r.next:
		// The slot has already been passed. Never insert at a guessed
		// position; record the late arrival so callers can account for it.
		if r.lateSet[ext] {
			r.res.Duplicates++
			r.res.Faults = append(r.res.Faults, fault.Fault{
				Kind: fault.KindDuplicateFrame, Stage: fault.StageReorder,
				Camera: r.cfg.Camera, Seq: f.FrameSeq, Offset: f.Offset,
				Message:  fmt.Sprintf("extended RTP sequence %d already recorded late", ext),
				Expected: fmt.Sprintf(">= %d", r.next), Actual: fmt.Sprintf("%d", ext),
			})
			return
		}
		r.lateSet[ext] = true
		r.res.Late = append(r.res.Late, Late{
			FrameSeq: f.FrameSeq, ExtSeq: ext, Offset: f.Offset, Distance: r.next - ext,
		})
		r.res.Faults = append(r.res.Faults, fault.Fault{
			Kind: fault.KindLateFrame, Stage: fault.StageReorder,
			Camera: r.cfg.Camera, Seq: f.FrameSeq, Offset: f.Offset,
			Message:  fmt.Sprintf("frame arrived %d slots late; kept as a gap, not inserted", r.next-ext),
			Expected: fmt.Sprintf(">= %d", r.next), Actual: fmt.Sprintf("%d", ext),
		})
		return
	}
	if _, dup := r.buf[ext]; dup {
		r.res.Duplicates++
		r.res.Faults = append(r.res.Faults, fault.Fault{
			Kind: fault.KindDuplicateFrame, Stage: fault.StageReorder,
			Camera: r.cfg.Camera, Seq: f.FrameSeq, Offset: f.Offset,
			Message:  fmt.Sprintf("extended RTP sequence %d buffered twice", ext),
			Expected: fmt.Sprintf("> %d", r.next-1), Actual: fmt.Sprintf("%d", ext),
		})
		return
	}

	r.buf[ext] = f
	r.bufBytes += f.Size
	r.drain()
	for len(r.buf) > r.cfg.Window || r.bufBytes > r.cfg.MaxBytes {
		r.forceLowest()
	}
	// Peak counts frames actually retained after enforcement, so it never
	// exceeds the configured window.
	if len(r.buf) > r.res.PeakBuffered {
		r.res.PeakBuffered = len(r.buf)
	}
}

// drain delivers in-order frames starting at next.
func (r *Reorderer) drain() {
	for r.haveNext {
		f, ok := r.buf[r.next]
		if !ok {
			return
		}
		r.deliver(r.next, f)
		r.next++
	}
}

// forceLowest delivers the lowest buffered frame ahead of missing
// predecessors, recording the skipped range as a gap.
func (r *Reorderer) forceLowest() {
	if len(r.buf) == 0 {
		return
	}
	low := ^uint64(0)
	for ext := range r.buf {
		if ext < low {
			low = ext
		}
	}
	if low > r.next {
		r.recordGap(r.next, low-1)
	}
	f := r.buf[low]
	r.deliver(low, f)
	if r.next <= low {
		r.next = low + 1
	}
	r.drain()
}

// flushAll delivers every buffered frame in order, recording holes as gaps.
func (r *Reorderer) flushAll() {
	if len(r.buf) == 0 {
		return
	}
	exts := make([]uint64, 0, len(r.buf))
	for ext := range r.buf {
		exts = append(exts, ext)
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i] < exts[j] })
	for _, ext := range exts {
		if r.haveNext && ext > r.next {
			r.recordGap(r.next, ext-1)
		}
		f, ok := r.buf[ext]
		if !ok {
			continue
		}
		r.deliver(ext, f)
		if r.haveNext && r.next <= ext {
			r.next = ext + 1
		}
	}
}

func (r *Reorderer) deliver(ext uint64, f Frame) {
	delete(r.buf, ext)
	r.bufBytes -= f.Size
	if r.bufBytes < 0 {
		r.bufBytes = 0
	}
	r.emitted[ext] = true
	r.res.Ordered = append(r.res.Ordered, f)
	if len(r.emitted) > maxHistory && r.next > maxHistory {
		cutoff := r.next - maxHistory
		for e := range r.emitted {
			if e < cutoff {
				delete(r.emitted, e)
			}
		}
		for e := range r.lateSet {
			if e < cutoff {
				delete(r.lateSet, e)
			}
		}
	}
}

// recordGap notes an inclusive missing range.
func (r *Reorderer) recordGap(from, to uint64) {
	if to < from {
		return
	}
	g := Gap{From: from, To: to, Missing: to - from + 1}
	if n := len(r.res.Ordered); n > 0 {
		g.AfterSeq = r.res.Ordered[n-1].FrameSeq
		g.MasterTS = r.res.Ordered[n-1].MasterTS
	}
	r.res.Gaps = append(r.res.Gaps, g)
	severity := "gap"
	if g.Missing > 1 {
		severity = "burst gap"
	}
	r.res.Faults = append(r.res.Faults, fault.Fault{
		Kind: fault.KindMissingFrame, Stage: fault.StageReorder,
		Camera: r.cfg.Camera, Seq: g.AfterSeq, Offset: -1,
		Message:  fmt.Sprintf("%s: %d frame(s) missing (ext seq %d..%d)", severity, g.Missing, from, to),
		Expected: fmt.Sprintf("ext seq %d", from), Actual: fmt.Sprintf("next %d", to+1),
	})
}

// Drain returns frames delivered since the previous call, in order. Callers
// using Push incrementally (rather than Finish) drain after every push.
func (r *Reorderer) Drain() []Frame {
	out := r.res.Ordered[r.drained:]
	r.drained = len(r.res.Ordered)
	return out
}

// DrainFaults returns faults recorded since the previous call.
func (r *Reorderer) DrainFaults() fault.List {
	out := r.res.Faults[r.drainedFaults:]
	r.drainedFaults = len(r.res.Faults)
	return out
}

// Buffered reports how many frames are currently held for reordering.
func (r *Reorderer) Buffered() int { return len(r.buf) }

// Flush flushes all buffered frames and returns every not-yet-drained frame
// plus the faults recorded while flushing. It is the incremental analogue of
// Finish for stream consumers.
func (r *Reorderer) Flush() ([]Frame, fault.List) {
	r.flushAll()
	frames := r.Drain()
	return frames, r.DrainFaults()
}

// Finish flushes the buffer and returns the final result.
func (r *Reorderer) Finish() Result {
	r.flushAll()
	res := r.res
	res.Ordered = append([]Frame(nil), res.Ordered...)
	res.Late = append([]Late(nil), res.Late...)
	res.Gaps = append([]Gap(nil), res.Gaps...)
	res.Faults = append(fault.List(nil), res.Faults...)
	return res
}

// MissingCount sums the missing frames across recorded gaps.
func (r Result) MissingCount() uint64 {
	var n uint64
	for _, g := range r.Gaps {
		n += g.Missing
	}
	return n
}

// Timestamps returns the master timestamps of the ordered frames.
func (r Result) Timestamps() []uint64 {
	out := make([]uint64, len(r.Ordered))
	for i := range r.Ordered {
		out[i] = r.Ordered[i].MasterTS
	}
	return out
}
