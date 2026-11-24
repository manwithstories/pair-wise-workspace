// Package reorder removes duplicates, restores RTP order across sequence
// wrap-around, and represents missing packets as time-base-aligned gaps.
package reorder

import (
	"fmt"

	"github.com/pion/rtp"

	"mediad/internal/clock"
)

// Block is one ordered audio packet or one concealed gap.
type Block struct {
	ExtSeq    int64
	Sequence  uint16
	ExtTS     int64
	Timestamp uint32
	ArrivalNS int64
	Payload   []byte
	Missing   bool
	Samples   int
}

// Stats describes packet health for terminal output and tests.
type Stats struct {
	Received  int `json:"received"`
	Unique    int `json:"unique"`
	Duplicate int `json:"duplicate"`
	Reordered int `json:"reordered"`
	Lost      int `json:"lost"`
	Blocks    int `json:"blocks"`
}

type heldPacket struct {
	pkt       rtp.Packet
	extSeq    int64
	extTS     int64
	arrivalNS int64
}

// Buffer is a finite-window-safe whole-stream reorder buffer.
//
// The demo input is small enough to retain unique compressed/L16 packets.
// Sequence and timestamp expansion are deterministic and independent of map
// iteration order.
type Buffer struct {
	Clock         *clock.Base
	Channels      int
	SamplesPerPkt int
	MaxGap        int64
	stats         Stats
	packets       map[int64]heldPacket
	maxExtSeq     int64
	haveSeqAnchor bool
	maxTimestamp  int64
}

func NewBuffer(clk *clock.Base, channels, samplesPerPacket int) *Buffer {
	if channels <= 0 {
		channels = 1
	}
	if samplesPerPacket <= 0 {
		samplesPerPacket = 1
	}
	return &Buffer{
		Clock: clk, Channels: channels, SamplesPerPkt: samplesPerPacket,
		MaxGap: 4096, packets: make(map[int64]heldPacket),
	}
}

func (b *Buffer) Stats() Stats { return b.stats }

// Push consumes one valid RTP packet. CRC-invalid records should not be pushed;
// their absent sequence becomes a gap if no valid duplicate exists.
func (b *Buffer) Push(pkt *rtp.Packet, arrivalNS int64) error {
	if pkt == nil {
		return fmt.Errorf("nil RTP packet")
	}
	b.stats.Received++

	extSeq := b.extendSequence(pkt.SequenceNumber)
	if _, exists := b.packets[extSeq]; exists {
		b.stats.Duplicate++
		return nil
	}
	if b.haveSeqAnchor && extSeq < b.maxExtSeq {
		b.stats.Reordered++
	}
	extTS := b.Clock.Unwrap(pkt.Timestamp)
	b.packets[extSeq] = heldPacket{
		pkt: *pkt, extSeq: extSeq, extTS: extTS, arrivalNS: arrivalNS,
	}
	b.stats.Unique++
	if extSeq > b.maxExtSeq {
		b.maxExtSeq = extSeq
	}
	if extTS > b.maxTimestamp {
		b.maxTimestamp = extTS
	}
	return nil
}

func (b *Buffer) extendSequence(seq uint16) int64 {
	if !b.haveSeqAnchor {
		b.haveSeqAnchor = true
		b.maxExtSeq = int64(seq)
		return int64(seq)
	}
	delta := int32(seq - uint16(b.maxExtSeq))
	return b.maxExtSeq + int64(delta)
}

// Flush returns blocks in strict extended-sequence order. Missing sequence
// numbers become Missing blocks. Their timestamps are derived from the shared
// clock and packet duration, so emitted media time never retreats.
func (b *Buffer) Flush() ([]Block, Stats, error) {
	if len(b.packets) == 0 {
		return nil, b.stats, nil
	}
	minSeq := b.maxExtSeq
	for seq := range b.packets {
		if seq < minSeq {
			minSeq = seq
		}
	}
	if b.maxExtSeq-minSeq > b.MaxGap {
		return nil, b.stats, fmt.Errorf("reorder span %d exceeds safety gap %d", b.maxExtSeq-minSeq, b.MaxGap)
	}

	first, ok := b.packets[minSeq]
	if !ok {
		return nil, b.stats, fmt.Errorf("internal error: minimum sequence absent")
	}
	firstTS := first.extTS
	blocks := make([]Block, 0, b.maxExtSeq-minSeq+1)
	var expectedTS int64 = firstTS

	for extSeq := minSeq; extSeq <= b.maxExtSeq; extSeq++ {
		seq16 := uint16(extSeq)
		wantTS := firstTS + (extSeq-minSeq)*int64(b.SamplesPerPkt)
		if wantTS < expectedTS {
			return nil, b.stats, fmt.Errorf("timestamp regression at seq %d", seq16)
		}
		expectedTS = wantTS

		hp, ok := b.packets[extSeq]
		if !ok {
			b.stats.Lost++
			blocks = append(blocks, Block{
				ExtSeq: extSeq, Sequence: seq16, ExtTS: wantTS,
				Timestamp: uint32(wantTS), Missing: true, Samples: b.SamplesPerPkt,
			})
			continue
		}

		samples := len(hp.pkt.Payload) / (2 * b.Channels)
		if samples <= 0 {
			samples = b.SamplesPerPkt
		}
		// Prefer the RTP timestamp when a real packet has it; it must agree
		// with the sequence-derived baseline for this fixed packetization.
		ts := hp.extTS
		if ts < wantTS {
			ts = wantTS
		}
		blocks = append(blocks, Block{
			ExtSeq: extSeq, Sequence: hp.pkt.SequenceNumber, ExtTS: ts,
			Timestamp: hp.pkt.Timestamp, ArrivalNS: hp.arrivalNS,
			Payload: hp.pkt.Payload, Samples: samples,
		})
	}
	b.stats.Blocks = len(blocks)
	return blocks, b.stats, nil
}

// SilenceL16 returns big-endian signed-zero L16 for the requested geometry.
func SilenceL16(samples, channels int) []byte {
	if samples < 0 || channels <= 0 {
		return nil
	}
	return make([]byte, samples*channels*2)
}
