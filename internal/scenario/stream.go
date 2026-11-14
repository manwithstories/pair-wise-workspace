package scenario

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/pion/rtp"
)

// SeqAt returns the 16-bit RTP sequence number of the packet at logical
// position pos, honoring the scenario start sequence and wrap-around.
func (sc *Scenario) SeqAt(pos int) uint16 {
	return uint16(sc.StartSeq + pos)
}

// TimestampAt returns the RTP timestamp of the packet at logical position pos.
// One frame spans PacketsPerFrame packets and advances the clock by one tick.
func (sc *Scenario) TimestampAt(pos int) uint32 {
	frame := pos / sc.PacketsPerFrame
	return uint32(frame) * (sc.ClockRate / 30)
}

// PayloadAt builds the deterministic payload of the packet at logical pos.
// The payload is self-describing so receivers can recompute identities:
//
//	[0:8]   sha256(seed || pos)[:8]
//	[8:12]  uint32 BE sequence number (logical position)
//	[12:]   repeating fill byte derived from pos
func PayloadAt(pos, size int) []byte {
	payload := make([]byte, size)
	var seed [8]byte
	binary.BigEndian.PutUint64(seed[:], 0x5254504341500000)
	sum := sha256.Sum256(append(seed[:], byte(pos), byte(pos>>8), byte(pos>>16), byte(pos>>24)))
	copy(payload, sum[:8])
	binary.BigEndian.PutUint32(payload[8:12], uint32(pos))
	fill := byte(0xA0 ^ (pos & 0x1f))
	for i := 12; i < size; i++ {
		payload[i] = fill
	}
	return payload
}

// PacketAt builds the marshaled RTP packet for logical position pos.
func (sc *Scenario) PacketAt(pos int) (*rtp.Packet, []byte) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: sc.SeqAt(pos),
			Timestamp:      sc.TimestampAt(pos),
			SSRC:           sc.SSRC,
			Marker:         (pos % sc.PacketsPerFrame) == sc.PacketsPerFrame-1,
		},
		Payload: PayloadAt(pos, sc.PayloadSize),
	}
	raw, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return pkt, raw
}

// SendSlot is one entry of the server send plan: a raw RTP packet tagged with
// its logical position. Duplicate positions appear more than once.
type SendSlot struct {
	Pos int
	Raw []byte
}

// BuildSendPlan applies drop / reorder / duplicate / late events and returns
// the ordered list of packets the server should send.
func (sc *Scenario) BuildSendPlan() []SendSlot {
	dropped := map[int]bool{}
	dupCount := map[int]int{}
	delay := map[int]int{}
	for _, ev := range sc.Events {
		switch ev.Type {
		case EventDrop:
			for k := 0; k < ev.Count; k++ {
				dropped[ev.Pos+k] = true
			}
		case EventDuplicate:
			dupCount[ev.Pos] = ev.Count
		case EventReorder, EventLate:
			delay[ev.Pos] = ev.Shift
		}
	}

	type item struct {
		pos    int
		sendAt int
		raw    []byte
	}
	var items []item
	for pos := 0; pos < sc.TotalPackets; pos++ {
		if dropped[pos] {
			continue
		}
		_, raw := sc.PacketAt(pos)
		shift := delay[pos]
		items = append(items, item{pos: pos, sendAt: pos + shift, raw: raw})
		for k := 0; k < dupCount[pos]; k++ {
			items = append(items, item{pos: pos, sendAt: pos + shift, raw: raw})
		}
	}

	// Stable sort by sendAt; equal sendAt keeps original logical order so a
	// delayed packet lands after the packets it was shifted past.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].sendAt > items[j].sendAt; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}

	plan := make([]SendSlot, len(items))
	for i, it := range items {
		plan[i] = SendSlot{Pos: it.pos, Raw: it.raw}
	}
	return plan
}
