package rtp

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/pion/rtp"

	"mediaqc/internal/media"
)

const (
	// MaxPacketBytes bounds one ingress datagram and its parsed payload.
	MaxPacketBytes = 1500
	// MaxSeenPackets bounds the duplicate/conflict retention cache.
	MaxSeenPackets = 4096
	// MaxSSRCState bounds per-SSRC sequence/timestamp state.
	MaxSSRCState = 64
)

// Verdict explains how the ingestor treated one datagram.
type Verdict string

const (
	VerdictAccepted  Verdict = "accepted"
	VerdictDuplicate Verdict = "duplicate"
	VerdictConflict  Verdict = "conflict"
	VerdictMalformed Verdict = "malformed"
)

// Packet is a parsed RTP packet with wrap-extended identifiers.
type Packet struct {
	SSRC          uint32
	Sequence      uint16
	ExtSequence   uint64
	Timestamp     uint32
	ExtTimestamp  uint64
	Marker        bool
	PayloadType   uint8
	Payload       []byte
	ArrivalMS     int64
	PayloadSHA256 [32]byte
}

// Result is returned for every bounded ingress datagram.
type Result struct {
	Verdict Verdict
	Packet  *Packet
	Err     error
}

// Stats contains ingest counters.
type Stats struct {
	Received   int `json:"received"`
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
	Conflicts  int `json:"conflicts"`
	Malformed  int `json:"malformed"`
	Oversized  int `json:"oversized"`
}

type dedupKey struct {
	ssrc uint32
	seq  uint16
}

type seenEntry struct {
	hash [32]byte
	elem *list.Element
}

// SeqExtender unwraps 16-bit RTP sequence numbers.
type SeqExtender struct {
	highest uint64
	started bool
}

// Extend returns the nearest monotonic sequence and tracks the highest seen.
func (x *SeqExtender) Extend(seq uint16) uint64 {
	if !x.started {
		x.started = true
		x.highest = uint64(seq)
		return x.highest
	}
	base := x.highest &^ 0xffff
	candidates := []uint64{base + uint64(seq)}
	if base >= 1<<16 {
		candidates = append(candidates, base-(1<<16)+uint64(seq))
	}
	candidates = append(candidates, base+(1<<16)+uint64(seq))

	best := candidates[0]
	for _, cand := range candidates[1:] {
		if dist(cand, x.highest) < dist(best, x.highest) {
			best = cand
		}
	}
	if best > x.highest {
		x.highest = best
	}
	return best
}

func dist(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// Ingestor parses RTP and enforces same-SSRC/sequence/payload identity.
type Ingestor struct {
	maxBytes int
	maxSeen  int
	seq      map[uint32]*SeqExtender
	ts       map[uint32]*media.TimestampExtender
	seen     map[dedupKey]*seenEntry
	order    *list.List
	Stats    Stats
}

// NewIngestor creates an ingestor with bounded default caches.
func NewIngestor() *Ingestor {
	return &Ingestor{
		maxBytes: MaxPacketBytes,
		maxSeen:  MaxSeenPackets,
		seq:      make(map[uint32]*SeqExtender),
		ts:       make(map[uint32]*media.TimestampExtender),
		seen:     make(map[dedupKey]*seenEntry),
		order:    list.New(),
	}
}

// HandleDatagram parses and deduplicates one raw RTP datagram.
func (in *Ingestor) HandleDatagram(raw []byte, arrival time.Duration) Result {
	in.Stats.Received++
	if len(raw) > in.maxBytes {
		in.Stats.Oversized++
		err := media.New(media.KindMalformed, "rtp.ingest", fmt.Sprintf("packet too large: %d>%d", len(raw), in.maxBytes))
		in.Stats.Malformed++
		return Result{Verdict: VerdictMalformed, Err: err}
	}

	var rp rtp.Packet
	if err := rp.Unmarshal(raw); err != nil {
		werr := media.Wrap(media.KindMalformed, "rtp.ingest", "RTP unmarshal failed", err)
		in.Stats.Malformed++
		return Result{Verdict: VerdictMalformed, Err: werr}
	}
	if len(in.seq) >= MaxSSRCState {
		if _, ok := in.seq[rp.Header.SSRC]; !ok {
			err := media.New(media.KindInternal, "rtp.ingest", "too many SSRC states")
			return Result{Verdict: VerdictMalformed, Err: err}
		}
	}

	se, ok := in.seq[rp.Header.SSRC]
	if !ok {
		se = &SeqExtender{}
		in.seq[rp.Header.SSRC] = se
		in.ts[rp.Header.SSRC] = &media.TimestampExtender{}
	}
	extSeq := se.Extend(rp.Header.SequenceNumber)
	extTS := in.ts[rp.Header.SSRC].Extend(rp.Header.Timestamp)
	sum := sha256.Sum256(rp.Payload)
	pkt := &Packet{
		SSRC: rp.Header.SSRC, Sequence: rp.Header.SequenceNumber, ExtSequence: extSeq,
		Timestamp: rp.Header.Timestamp, ExtTimestamp: extTS, Marker: rp.Header.Marker,
		PayloadType: rp.Header.PayloadType, Payload: append([]byte(nil), rp.Payload...),
		ArrivalMS: int64(arrival / time.Millisecond), PayloadSHA256: sum,
	}

	key := dedupKey{ssrc: pkt.SSRC, seq: pkt.Sequence}
	if old, ok := in.seen[key]; ok {
		if old.hash == sum {
			in.Stats.Duplicates++
			return Result{Verdict: VerdictDuplicate, Packet: pkt}
		}
		in.Stats.Conflicts++
		err := media.New(media.KindConflict, "rtp.ingest",
			fmt.Sprintf("ssrc=%08x seq=%d existing=%x incoming=%x", pkt.SSRC, pkt.Sequence, old.hash[:8], sum[:8]))
		return Result{Verdict: VerdictConflict, Packet: pkt, Err: err}
	}

	in.remember(key, sum)
	in.Stats.Accepted++
	return Result{Verdict: VerdictAccepted, Packet: pkt}
}

func (in *Ingestor) remember(key dedupKey, hash [32]byte) {
	if len(in.seen) >= in.maxSeen {
		oldest := in.order.Front()
		if oldest != nil {
			oldKey := oldest.Value.(dedupKey)
			delete(in.seen, oldKey)
			in.order.Remove(oldest)
		}
	}
	elem := in.order.PushBack(key)
	in.seen[key] = &seenEntry{hash: hash, elem: elem}
}

// MakeDatagram is a deterministic fixture helper for tests and sample data.
func MakeDatagram(ssrc uint32, seq uint16, ts uint32, pt uint8, marker bool, payload []byte) ([]byte, error) {
	p := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts,
			SSRC: ssrc, Marker: marker,
		},
		Payload: payload,
	}
	return p.Marshal()
}
