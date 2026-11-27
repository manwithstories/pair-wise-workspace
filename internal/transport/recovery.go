package transport

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// ErrUnrecoverable marks a burst loss that local FEC cannot rebuild. The
// missing sequence numbers are preserved on the error so callers keep an
// explicit loss record instead of silently dropping data.
var ErrUnrecoverable = errors.New("transport: unrecoverable packet loss")

// LossError carries every sequence number that could not be rebuilt.
type LossError struct {
	Missing []uint16
}

func (e *LossError) Error() string {
	return fmt.Sprintf("%v: %d packets %v", ErrUnrecoverable, len(e.Missing), e.Missing)
}

func (e *LossError) Unwrap() error { return ErrUnrecoverable }

// FECBlock is one locally generated XOR redundancy block. It covers an
// inclusive sequence range [FirstSeq, LastSeq]; the parity payload is the
// byte-wise XOR of every covered packet payload (shorter payloads are
// zero-extended).
type FECBlock struct {
	FirstSeq uint16
	LastSeq  uint16
	SSRC     uint32
	Parity   []byte
}

// EncodeFEC builds one XOR parity block over packets, which must be one
// contiguous run of sequence numbers. An empty group yields an error.
func EncodeFEC(packets []*rtp.Packet) (FECBlock, error) {
	if len(packets) == 0 {
		return FECBlock{}, errors.New("transport: cannot encode FEC over empty group")
	}
	maxLen := 0
	for _, p := range packets {
		if len(p.Payload) > maxLen {
			maxLen = len(p.Payload)
		}
	}
	parity := make([]byte, maxLen)
	for _, p := range packets {
		for i, b := range p.Payload {
			parity[i] ^= b
		}
	}
	return FECBlock{
		FirstSeq: packets[0].SequenceNumber,
		LastSeq:  packets[len(packets)-1].SequenceNumber,
		SSRC:     packets[0].SSRC,
		Parity:   parity,
	}, nil
}

// covers reports whether seq belongs to the block's range.
func (b FECBlock) covers(seq uint16) bool {
	return seqDistance(b.FirstSeq, seq) >= 0 && seqDistance(seq, b.LastSeq) >= 0
}

// Recover attempts to rebuild every missing packet using the FEC blocks.
// present maps a sequence number to its received packet; missing is the set
// the jitter buffer could not deliver. A block can repair exactly one hole;
// two or more holes inside one block make the burst unrecoverable.
func Recover(present map[uint16]*rtp.Packet, missing []uint16, blocks []FECBlock) (recovered map[uint16]*rtp.Packet, unrecoverable []uint16, err error) {
	recovered = map[uint16]*rtp.Packet{}
	stillMissing := map[uint16]bool{}
	for _, s := range missing {
		stillMissing[s] = true
	}

	// Iterate because repairing one packet can unlock a block for another.
	for progress := true; progress; {
		progress = false
		for _, blk := range blocks {
			holes := blkHoles(blk, stillMissing)
			if len(holes) == 0 {
				continue
			}
			if len(holes) > 1 {
				continue // burst inside this block: not XOR-repairable
			}
			seq := holes[0]
			pkt, ok := rebuild(blk, seq, present, recovered)
			if !ok {
				continue
			}
			recovered[seq] = pkt
			delete(stillMissing, seq)
			progress = true
		}
	}

	for s := range stillMissing {
		unrecoverable = append(unrecoverable, s)
	}
	if len(unrecoverable) > 0 {
		sortSeqs(unrecoverable)
		return recovered, unrecoverable, &LossError{Missing: unrecoverable}
	}
	return recovered, nil, nil
}

// blkHoles returns missing sequence numbers still inside the block.
func blkHoles(blk FECBlock, missing map[uint16]bool) []uint16 {
	var holes []uint16
	for seq := range missing {
		if blk.covers(seq) {
			holes = append(holes, seq)
		}
	}
	sortSeqs(holes)
	return holes
}

// rebuild reconstructs the single missing packet seq from parity XOR.
func rebuild(blk FECBlock, seq uint16, present, recovered map[uint16]*rtp.Packet) (*rtp.Packet, bool) {
	template := blockTemplate(blk, present, recovered)
	if template == nil {
		return nil, false
	}
	payload := append([]byte(nil), blk.Parity...)
	allThere := true
	for s := blk.FirstSeq; ; s++ {
		if s == seq {
			if s == blk.LastSeq {
				break
			}
			continue
		}
		p := lookup(s, present, recovered)
		if p == nil {
			allThere = false
			break
		}
		for i, b := range p.Payload {
			if i < len(payload) {
				payload[i] ^= b
			}
		}
		if s == blk.LastSeq {
			break
		}
	}
	if !allThere {
		return nil, false
	}
	out := template.Clone()
	out.SequenceNumber = seq
	out.Payload = payload
	return out, true
}

func blockTemplate(blk FECBlock, present, recovered map[uint16]*rtp.Packet) *rtp.Packet {
	for s := blk.FirstSeq; ; s++ {
		if p := lookup(s, present, recovered); p != nil {
			return p
		}
		if s == blk.LastSeq {
			return nil
		}
	}
}

func lookup(seq uint16, present, recovered map[uint16]*rtp.Packet) *rtp.Packet {
	if p, ok := present[seq]; ok {
		return p
	}
	return recovered[seq]
}

// sortSeqs orders sequence numbers by ascending ring distance from the
// smallest element, so a set crossing the 16-bit wrap stays ordered.
func sortSeqs(seqs []uint16) {
	if len(seqs) < 2 {
		return
	}
	anchor := seqs[0]
	for _, s := range seqs[1:] {
		if seqDistance(s, anchor) < 0 {
			anchor = s
		}
	}
	for i := 1; i < len(seqs); i++ {
		for j := i; j > 0; j-- {
			if seqDistance(anchor, seqs[j-1]) <= seqDistance(anchor, seqs[j]) {
				break
			}
			seqs[j-1], seqs[j] = seqs[j], seqs[j-1]
		}
	}
}
