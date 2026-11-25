// Package recovery repairs and orders RTP packet streams: it expands 16-bit
// sequence wraparound, reorders out-of-order arrivals, drops duplicates, and
// recovers losses only from redundant copies carried in the fixture stream.
// Packets that cannot be recovered are counted as lost, never invented.
package recovery

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"mediad/internal/media"
)

// Packet is one delivered RTP packet in unwrapped sequence space.
type Packet struct {
	Seq       int64    `json:"seq"`  // unwrapped, monotonic
	Wire      uint16   `json:"wire"` // original 16-bit sequence
	Timestamp int64    `json:"timestamp"`
	Marker    bool     `json:"marker"`
	PT        uint8    `json:"pt"`
	SSRC      uint32   `json:"ssrc"`
	Payload   []byte   `json:"-"`
	Hash      [32]byte `json:"-"` // SHA-256 of Payload
	Recovered bool     `json:"recovered"`
}

// Duplicate records one discarded repeat of a sequence number.
type Duplicate struct {
	Seq  int64    `json:"seq"`
	Hash [32]byte `json:"-"`
}

// Result is the outcome of repairing one SSRC stream.
type Result struct {
	SSRC           uint32      `json:"ssrc"`
	Camera         string      `json:"camera"`
	Packets        []Packet    `json:"-"`
	Sent           []int64     `json:"sent"`
	Lost           []int64     `json:"lost"`
	Recovered      []int64     `json:"recovered"`
	Duplicates     []Duplicate `json:"duplicates"`
	ReceivedCount  int         `json:"received"`
	LostCount      int         `json:"lost_count"`
	RecoveredCount int         `json:"recovered_count"`
	DupCount       int         `json:"duplicate_count"`
	OutOfOrder     int         `json:"out_of_order"`
}

// RepairCopy is a redundant copy of one packet carried by another packet on
// the wire, as RTX or an FEC block would carry. Orig is the wire sequence
// number being repaired.
type RepairCopy struct {
	Orig      uint16
	Payload   []byte
	Timestamp uint32
	Marker    bool
}

// Redundancy maps a received carrier's wire sequence number to the repair copy
// it carries. Real streams derive this from RTX/FEC metadata; fixtures encode
// it explicitly so recovery can be scored.
type Redundancy map[uint16]RepairCopy

// Options bounds recovery work and supplies the sender's intended sequence
// window.
type Options struct {
	Limits media.Limits
	// SentSeqs is the sender's complete 16-bit sequence list. Sequence numbers
	// absent from it are never-sent, not lost.
	SentSeqs []uint16
}

// Recover processes received packets for one SSRC and returns a repaired,
// ordered result. A lost packet is restored only when some received packet
// redundantly carries the identical payload; otherwise it is counted lost.
func Recover(ssrc uint32, camera string, in []*rtp.Packet, red Redundancy, opts Options) (*Result, error) {
	lim := opts.Limits
	if lim == (media.Limits{}) {
		lim = media.DefaultLimits()
	}
	if len(in) > lim.MaxPackets {
		return nil, media.New(media.CodeLimit, "rtp-recover", media.SourcePos{},
			"packet count %d exceeds limit %d", len(in), lim.MaxPackets)
	}

	// Expand the sender's sequence list into unwrapped space and index the wire
	// value back to its unwrapped position.
	sent := unwrapAll(opts.SentSeqs)
	wireToSeq := make(map[uint16]int64, len(sent))
	for i, w := range opts.SentSeqs {
		if _, ok := wireToSeq[w]; !ok {
			wireToSeq[w] = sent[i]
		}
	}
	// Map a wire value to the latest unwrapped position at or before it; this
	// handles streams longer than 65536 packets.
	wireLast := make(map[uint16]int64, len(sent))
	for i := len(sent) - 1; i >= 0; i-- {
		w := opts.SentSeqs[i]
		if _, ok := wireLast[w]; !ok {
			wireLast[w] = sent[i]
		}
	}
	seqForWire := func(w uint16) int64 {
		if s, ok := wireLast[w]; ok {
			return s
		}
		return int64(w)
	}

	res := &Result{SSRC: ssrc, Camera: camera, Sent: sent}
	if len(sent) == 0 {
		return nil, media.New(media.CodeSegmentParse, "rtp-recover", media.SourcePos{}, "no packets received")
	}

	type received struct {
		pkt Packet
	}
	bySeq := make(map[int64]*received, len(in))
	wireSeen := make(map[uint16][32]byte, len(in))
	outOfOrder := 0
	maxSeq := int64(-1)
	for _, raw := range in {
		if raw == nil {
			continue
		}
		if lim.MaxPacketBytes > 0 && len(raw.Payload) > lim.MaxPacketBytes {
			return nil, media.New(media.CodeLimit, "rtp-recover", media.SourcePos{},
				"packet %d payload %d exceeds limit %d", raw.SequenceNumber, len(raw.Payload), lim.MaxPacketBytes)
		}
		seq := seqForWire(raw.SequenceNumber)
		h := sha256.Sum256(raw.Payload)
		if prev, ok := wireSeen[raw.SequenceNumber]; ok {
			if prev != h {
				return nil, media.New(media.CodeSegmentParse, "rtp-recover", media.SourcePos{},
					"sequence %d arrived with conflicting payloads", raw.SequenceNumber)
			}
			res.DupCount++
			res.Duplicates = append(res.Duplicates, Duplicate{Seq: seq, Hash: h})
			continue
		}
		wireSeen[raw.SequenceNumber] = h
		if seq < maxSeq {
			outOfOrder++
		} else {
			maxSeq = seq
		}
		bySeq[seq] = &received{pkt: Packet{
			Seq:       seq,
			Wire:      raw.SequenceNumber,
			Timestamp: media.Unwrap32Timestamp(0, raw.Timestamp),
			Marker:    raw.Marker,
			PT:        raw.PayloadType,
			SSRC:      raw.SSRC,
			Payload:   append([]byte(nil), raw.Payload...),
			Hash:      h,
		}}
	}
	res.OutOfOrder = outOfOrder
	res.ReceivedCount = len(bySeq)

	// Index repair copies by the unwrapped sequence they restore. A repair is
	// usable only if the carrier that holds it actually arrived.
	repair := make(map[int64]Packet)
	for wire, rc := range red {
		if _, ok := bySeq[seqForWire(wire)]; !ok {
			continue // carrier lost
		}
		target := seqForWire(rc.Orig)
		if _, already := bySeq[target]; already {
			continue
		}
		if _, dup := repair[target]; dup {
			continue
		}
		payload := append([]byte(nil), rc.Payload...)
		repair[target] = Packet{
			Seq:       target,
			Wire:      rc.Orig,
			Timestamp: media.Unwrap32Timestamp(0, rc.Timestamp),
			Marker:    rc.Marker,
			PT:        96,
			SSRC:      ssrc,
			Payload:   payload,
			Hash:      sha256.Sum256(payload),
			Recovered: true,
		}
	}

	sentSet := make(map[int64]bool, len(sent))
	for _, s := range sent {
		sentSet[s] = true
	}
	for _, s := range sent {
		if rec, ok := bySeq[s]; ok {
			res.Packets = append(res.Packets, rec.pkt)
			continue
		}
		if p, ok := repair[s]; ok {
			res.Packets = append(res.Packets, p)
			res.Recovered = append(res.Recovered, s)
			res.RecoveredCount++
			continue
		}
		res.Lost = append(res.Lost, s)
		res.LostCount++
	}
	sort.Slice(res.Packets, func(i, j int) bool { return res.Packets[i].Seq < res.Packets[j].Seq })
	return res, nil
}

// unwrapAll expands a 16-bit sequence list into monotonic space using each
// successive value's distance from the previous one.
func unwrapAll(seqs []uint16) []int64 {
	out := make([]int64, len(seqs))
	var base int64
	var prev int64 = -1
	for i, w := range seqs {
		if prev < 0 {
			base = int64(w)
			out[i] = base
			prev = int64(w)
			continue
		}
		diff := int64(int(w) - int(prev))
		if diff < -32768 {
			diff += 65536
		} else if diff > 32767 {
			diff -= 65536
		}
		base += diff
		out[i] = base
		prev = int64(w)
	}
	return out
}

// DeliveryPct reports delivered packets (received + recovered) over sent.
func (r *Result) DeliveryPct() float64 {
	if len(r.Sent) == 0 {
		return 100
	}
	return 100 * float64(len(r.Packets)) / float64(len(r.Sent))
}

// RecoveryPct reports recovered packets over packets that were lost or
// recovered. Unrecoverable losses are excluded from the denominator so the
// figure reflects repair effectiveness, not loss rate.
func (r *Result) RecoveryPct() float64 {
	lostBeforeRepair := len(r.Packets) - r.ReceivedCount
	if lostBeforeRepair == 0 {
		return 100
	}
	return 100 * float64(r.RecoveredCount) / float64(lostBeforeRepair)
}

// Summary renders the recovery counters for CLI output.
func (r *Result) Summary() string {
	return fmt.Sprintf("ssrc=%08x received=%d lost=%d recovered=%d duplicates=%d out_of_order=%d delivery=%.2f%%",
		r.SSRC, r.ReceivedCount, r.LostCount, r.RecoveredCount, r.DupCount, r.OutOfOrder, r.DeliveryPct())
}
