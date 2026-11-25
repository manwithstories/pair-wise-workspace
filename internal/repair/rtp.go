package repair

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pion/rtp"
)

// SeqMod is the 16-bit RTP sequence modulus.
const SeqMod int64 = 1 << 16

// FECGroup is one side-band XOR redundancy record: Parity equals the
// XOR of every protected media payload. With exactly one missing
// member the missing payload is reconstructed.
type FECGroup struct {
	Index     int      `json:"index"`
	Sequences []uint16 `json:"sequences"`
	Parity    []byte   `json:"parity"`
}

// Gap is one contiguous run of sequence numbers that could not be
// recovered.
type Gap struct {
	StartExtended int64  `json:"start_extended"`
	EndExtended   int64  `json:"end_extended"`
	StartSeq      uint16 `json:"start_seq"`
	EndSeq        uint16 `json:"end_seq"`
	PacketCount   int    `json:"packet_count"`
}

// RecoveredPacket is a reconstructed missing RTP packet.
type RecoveredPacket struct {
	Sequence uint16 `json:"sequence"`
	Extended int64  `json:"extended"`
	Payload  []byte `json:"-"`
}

// RTPReport summarizes order, duplication and loss recovery.
type RTPReport struct {
	PacketCount       int                `json:"packet_count"`
	Duplicates        int                `json:"duplicates"`
	Reordered         int                `json:"reordered"`
	FirstExtended     int64              `json:"first_extended"`
	LastExtended      int64              `json:"last_extended"`
	TotalLost         int                `json:"total_lost"`
	Recovered         int                `json:"recovered"`
	LossRecovery      float64            `json:"loss_recovery"`
	UnrecoveredGaps   []Gap              `json:"unrecovered_gaps"`
	RecoveredPackets  []RecoveredPacket  `json:"-"`
}

// Unwrap16 extends a 16-bit sequence relative to prevExtended using
// nearest-continuation wrap handling. It correctly resolves reordering
// and a single 65535->0 cycle as long as displacement is < 32768.
func Unwrap16(prevExtended *int64, started *bool, seq uint16) int64 {
	s := int64(seq)
	if !*started {
		*started = true
		*prevExtended = s
		return s
	}
	prev := *prevExtended
	delta := s - (prev % SeqMod)
	if delta > SeqMod/2 {
		delta -= SeqMod
	} else if delta < -SeqMod/2 {
		delta += SeqMod
	}
	ext := prev + delta
	*prevExtended = ext
	return ext
}

// RepairRTP deduplicates, orders and unwraps captured packets, then
// applies side-band XOR FEC to recover isolated losses. Consecutive
// unrecoverable losses are merged into explicit gaps.
func RepairRTP(packets []rtp.Packet, groups []FECGroup) *RTPReport {
	rep := &RTPReport{}
	received := map[int64]rtp.Packet{}
	var prev int64
	started := false
	maxSeen := int64(-1)
	haveMax := false

	for _, p := range packets {
		ext := Unwrap16(&prev, &started, p.SequenceNumber)
		if _, dup := received[ext]; dup {
			rep.Duplicates++
			continue
		}
		received[ext] = p
		if haveMax && ext < maxSeen {
			rep.Reordered++
		}
		if !haveMax || ext > maxSeen {
			maxSeen = ext
			haveMax = true
		}
	}
	if len(received) == 0 {
		rep.LossRecovery = 1.0
		return rep
	}

	lo := minKey(received)
	hi := maxKey(received)

	// FEC-protected sequence numbers define the expected span too, so a
	// loss at the very edge (before the first received packet) is counted.
	groupMembers := make([][]int64, len(groups))
	for gi, g := range groups {
		for _, s16 := range g.Sequences {
			ext := resolveWithin(s16, lo, hi)
			groupMembers[gi] = append(groupMembers[gi], ext)
			if ext < lo {
				lo = ext
			}
			if ext > hi {
				hi = ext
			}
		}
	}
	rep.FirstExtended, rep.LastExtended = lo, hi
	rep.PacketCount = len(received)

	present := map[int64]bool{}
	for e := range received {
		present[e] = true
	}

	// Apply XOR redundancy.
	recovered := map[int64][]byte{}
	for gi, g := range groups {
		members := groupMembers[gi]
		var missing []int64
		for _, ext := range members {
			if !present[ext] {
				missing = append(missing, ext)
			}
		}
		if len(missing) != 1 {
			continue // zero loss or >1 loss: XOR cannot uniquely recover
		}
		target := missing[0]
		out := append([]byte(nil), g.Parity...)
		for _, ext := range members {
			if ext == target {
				continue
			}
			p, ok := received[ext]
			if !ok {
				continue
			}
			out = xorBytes(out, p.Payload)
		}
		recovered[target] = out
	}

	for ext, payload := range recovered {
		rep.Recovered++
		rep.RecoveredPackets = append(rep.RecoveredPackets, RecoveredPacket{
			Sequence: uint16(ext & 0xFFFF), Extended: ext, Payload: payload,
		})
	}

	// Enumerate missing extended sequences and merge into gaps.
	var gap *Gap
	for ext := lo; ext <= hi; ext++ {
		if present[ext] {
			if gap != nil {
				rep.UnrecoveredGaps = append(rep.UnrecoveredGaps, *gap)
				gap = nil
			}
			continue
		}
		rep.TotalLost++
		if _, isRec := recovered[ext]; isRec {
			continue
		}
		if gap == nil {
			gap = &Gap{StartExtended: ext, StartSeq: uint16(ext & 0xFFFF)}
		}
		gap.EndExtended = ext
		gap.EndSeq = uint16(ext & 0xFFFF)
		gap.PacketCount++
	}
	if gap != nil {
		rep.UnrecoveredGaps = append(rep.UnrecoveredGaps, *gap)
	}

	if rep.TotalLost > 0 {
		rep.LossRecovery = float64(rep.Recovered) / float64(rep.TotalLost)
	} else {
		rep.LossRecovery = 1.0
	}
	return rep
}

// ParseCapture reads length-prefixed (uint16) pion/rtp packets.
func ParseCapture(r io.Reader) ([]rtp.Packet, error) {
	var out []rtp.Packet
	var lenBuf [2]byte
	for {
		if _, err := io.ReadFull(r, lenBuf[:]); err == io.EOF {
			return out, nil
		} else if err != nil {
			return out, err
		}
		n := binary.BigEndian.Uint16(lenBuf[:])
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return out, err
		}
		var p rtp.Packet
		if err := p.Unmarshal(buf); err != nil {
			return out, fmt.Errorf("repair: rtp unmarshal: %w", err)
		}
		out = append(out, p)
	}
}

// MarshalCapture encodes packets as uint16 length-prefixed frames.
func MarshalCapture(packets []rtp.Packet) ([]byte, error) {
	var out []byte
	for _, p := range packets {
		b, err := p.Marshal()
		if err != nil {
			return nil, err
		}
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(b)))
		out = append(out, l[:]...)
		out = append(out, b...)
	}
	return out, nil
}

// XORPayload computes the parity of equal-length payloads.
func XORPayload(payloads [][]byte) []byte {
	if len(payloads) == 0 {
		return nil
	}
	out := make([]byte, len(payloads[0]))
	for _, p := range payloads {
		out = xorBytes(out, p)
	}
	return out
}

func xorBytes(a, b []byte) []byte {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		a[i] ^= b[i]
	}
	return a
}

func resolveWithin(raw uint16, lo, hi int64) int64 {
	s := int64(raw)
	kLo := lo/SeqMod - 1
	kHi := hi/SeqMod + 1
	best := s
	bestDist := int64(1 << 62)
	for k := kLo; k <= kHi; k++ {
		cand := s + k*SeqMod
		if cand >= lo && cand <= hi {
			return cand
		}
		d := cand - hi
		if cand < lo {
			d = lo - cand
		}
		if d < bestDist {
			bestDist = d
			best = cand
		}
	}
	return best
}

func minKey(m map[int64]rtp.Packet) int64 {
	first := true
	var v int64
	for k := range m {
		if first || k < v {
			v, first = k, false
		}
	}
	return v
}

func maxKey(m map[int64]rtp.Packet) int64 {
	first := true
	var v int64
	for k := range m {
		if first || k > v {
			v, first = k, false
		}
	}
	return v
}
