package rtp

import (
	"sort"

	"archive/internal/model"
)

// JitterBuffer reorders RTP packets by unwrapped sequence number, removes
// duplicates and drops arrivals that fall outside its retention window.
// Sequence wrap-around is handled entirely by the unwrapped ExtSeq values,
// so the buffer itself only works with monotonic int64 keys.
type JitterBuffer struct {
	window    int
	log       *model.FaultLog
	packets   map[int64]*MediaPacket
	latest    int64 // highest sequence ever observed
	floor     int64 // explicit retention floor set by AdvanceFloor
	haveFloor bool
	primed    bool
	peakBytes int64 // deterministic high-water mark over retained packets
}

// NewJitterBuffer builds a buffer retaining at most window sequence numbers
// behind the current head. window is clamped to model.MaxRepairWindow.
func NewJitterBuffer(window int, log *model.FaultLog) *JitterBuffer {
	if window <= 0 || window > model.MaxRepairWindow {
		window = model.MaxRepairWindow
	}
	return &JitterBuffer{window: window, log: log, packets: map[int64]*MediaPacket{}}
}

// lateBoundary is the lowest sequence still accepted: the larger of the
// explicit floor and the window-derived boundary behind the furthest packet.
func (j *JitterBuffer) lateBoundary() int64 {
	boundary := j.latest - int64(j.window)
	if j.haveFloor && j.floor > boundary {
		boundary = j.floor
	}
	return boundary
}

// Push inserts a packet. Returns false when the packet is a duplicate or a
// late arrival outside the retention window; the reason is logged in both
// cases so no packet disappears without a locatable record.
func (j *JitterBuffer) Push(p *MediaPacket) bool {
	seq := p.ExtSeq
	if !j.primed {
		j.primed = true
		j.latest = seq
	} else if seq > j.latest {
		j.latest = seq
	}
	if _, exists := j.packets[seq]; exists {
		j.log.Add(model.Fault{Phase: model.PhaseRTP, Kind: model.FaultDuplicatePacket,
			SeqLo: seq, SeqHi: seq, Detail: "duplicate sequence dropped"})
		return false
	}
	// Anything below the retention boundary arrived too late: its position
	// has already been emitted or declared a gap.
	if seq < j.lateBoundary() {
		j.log.Add(model.Fault{Phase: model.PhaseRTP, Kind: model.FaultLatePacket,
			SeqLo: seq, SeqHi: seq, Detail: "packet outside retention window"})
		return false
	}
	j.packets[seq] = p
	j.observePeak()
	return true
}

// observePeak recomputes the high-water mark from the packets currently
// retained. Because retention is a pure function of the input sequence and
// the fixed window, this peak is deterministic across runs and goroutine
// scheduling, unlike a queue's instantaneous occupancy.
func (j *JitterBuffer) observePeak() {
	var total int64
	for _, p := range j.packets {
		total += int64(p.Size)
	}
	if total > j.peakBytes {
		j.peakBytes = total
	}
}

// Drain returns retained packets in sequence order up to and including the
// current head, along with the missing sequences among them. The baseline
// is the lowest retained sequence, so stream-start reordering is preserved
// while holes inside the observed range are reported.
func (j *JitterBuffer) Drain() (ordered []*MediaPacket, missing []int64) {
	if len(j.packets) == 0 {
		return nil, nil
	}
	seqs := make([]int64, 0, len(j.packets))
	for s := range j.packets {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })

	head := j.latest
	want := seqs[0]
	for _, s := range seqs {
		for want < s {
			missing = append(missing, want)
			want++
		}
		ordered = append(ordered, j.packets[s])
		want = s + 1
	}
	for want <= head {
		missing = append(missing, want)
		want++
	}
	return ordered, missing
}

// Pop removes and returns one retained packet by sequence number.
func (j *JitterBuffer) Pop(seq int64) (*MediaPacket, bool) {
	p, ok := j.packets[seq]
	if ok {
		delete(j.packets, seq)
	}
	return p, ok
}

// Put stores an externally produced packet (retransmission or parity
// rebuild) under its sequence number.
func (j *JitterBuffer) Put(p *MediaPacket) {
	j.packets[p.ExtSeq] = p
	if !j.primed || p.ExtSeq > j.latest {
		j.primed = true
		j.latest = p.ExtSeq
	}
	j.observePeak()
}

// PeakBytes returns the deterministic high-water mark of retained payload.
func (j *JitterBuffer) PeakBytes() int64 { return j.peakBytes }

// AdvanceFloor moves the retention floor to floor, evicting older entries.
func (j *JitterBuffer) AdvanceFloor(floor int64) {
	if j.haveFloor && floor <= j.floor {
		return
	}
	j.haveFloor = true
	j.floor = floor
	for s := range j.packets {
		if s < floor {
			delete(j.packets, s)
		}
	}
}

// Floor returns the current effective retention floor.
func (j *JitterBuffer) Floor() int64 { return j.lateBoundary() }

// Latest returns the highest sequence observed so far.
func (j *JitterBuffer) Latest() int64 { return j.latest }

// Len reports how many packets are currently retained.
func (j *JitterBuffer) Len() int { return len(j.packets) }
