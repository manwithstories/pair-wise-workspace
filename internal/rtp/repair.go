package rtp

import (
	"tsqc/internal/clock"
)

// FECBlock is one XOR repair group. Count consecutive data packets starting
// at Base are protected by Parity; each data packet i has Lengths[i] useful
// bytes and is zero-padded to len(Parity) before the XOR.
type FECBlock struct {
	Base    uint16
	Count   int
	Lengths []int
	Parity  []byte
}

// Recovery reconstructs single-packet losses from XOR groups. It keeps a
// bounded window of delivered payloads plus a bounded set of blocks.
type Recovery struct {
	maxWindow int
	data      map[uint16][]byte
	order     []uint16
	blocks    []FECBlock
	maxBlocks int

	Recovered int
	Failed    int
}

// NewRecovery builds a recovery engine with a 512-packet window.
func NewRecovery() *Recovery { return NewRecoveryWithLimits(MaxReorderWindow, 256) }

// NewRecoveryWithLimits bounds both retained packets and FEC blocks.
func NewRecoveryWithLimits(window, blocks int) *Recovery {
	if window <= 0 || window > MaxReorderWindow {
		window = MaxReorderWindow
	}
	if blocks <= 0 || blocks > 1024 {
		blocks = 256
	}
	return &Recovery{
		maxWindow: window,
		data:      make(map[uint16][]byte, window),
		maxBlocks: blocks,
	}
}

// AddBlock registers a parity group. Duplicate or malformed groups are
// rejected. Blocks with no parity or count outside [1,512] are unusable.
func (r *Recovery) AddBlock(b FECBlock) error {
	if b.Count < 1 || b.Count > MaxReorderWindow {
		return clock.RecoverableError(clock.CodeXORRecovery, "rtp.AddBlock", "invalid block count %d", b.Count)
	}
	if len(b.Lengths) != b.Count || len(b.Parity) == 0 {
		return clock.RecoverableError(clock.CodeXORRecovery, "rtp.AddBlock", "block shape mismatch count=%d lengths=%d parity=%d", b.Count, len(b.Lengths), len(b.Parity))
	}
	for _, n := range b.Lengths {
		if n < 0 || n > len(b.Parity) {
			return clock.RecoverableError(clock.CodeXORRecovery, "rtp.AddBlock", "length %d outside parity size %d", n, len(b.Parity))
		}
	}
	r.blocks = append(r.blocks, b)
	if len(r.blocks) > r.maxBlocks {
		r.blocks = r.blocks[len(r.blocks)-r.maxBlocks:]
	}
	return nil
}

// Observe records a delivered packet payload for later XOR reconstruction.
// Packets outside the window are ignored.
func (r *Recovery) Observe(p Packet) {
	if _, ok := r.data[p.Seq]; ok {
		return
	}
	r.data[p.Seq] = append([]byte(nil), p.Payload...)
	r.order = append(r.order, p.Seq)
	for len(r.order) > r.maxWindow {
		old := r.order[0]
		r.order = r.order[1:]
		delete(r.data, old)
	}
}

// Have reports whether a payload is available for XOR.
func (r *Recovery) Have(seq uint16) bool {
	_, ok := r.data[seq]
	return ok
}

// Recover attempts to restore the packets covered by gap. Only gaps of one
// or more packets whose members each belong to a block with exactly one
// missing member can be repaired. It returns the recovered packets in
// sequence order.
func (r *Recovery) Recover(g Gap) ([]Packet, error) {
	if g.Count < 1 {
		return nil, clock.RecoverableError(clock.CodeXORRecovery, "rtp.Recover", "empty gap")
	}
	if g.Count > MaxReorderWindow {
		return nil, clock.RecoverableError(clock.CodeXORRecovery, "rtp.Recover", "gap %d exceeds window", g.Count)
	}
	var out []Packet
	missing := make(map[uint16]bool, g.Count)
	for i := 0; i < g.Count; i++ {
		missing[g.Start+uint16(i)] = true
	}
	for seq := range missing {
		if r.Have(seq) {
			delete(missing, seq)
		}
	}
	for seq := range missing {
		p, ok := r.recoverOne(seq)
		if !ok {
			r.Failed++
			return nil, clock.RecoverableError(clock.CodeXORRecovery, "rtp.Recover", "no usable XOR group for seq %d", seq)
		}
		out = append(out, p)
		delete(missing, seq)
	}
	for i := 0; i < g.Count; i++ {
		seq := g.Start + uint16(i)
		found := false
		for _, p := range out {
			if p.Seq == seq {
				found = true
			}
		}
		if !found {
			r.Failed++
			return nil, clock.RecoverableError(clock.CodeXORRecovery, "rtp.Recover", "recovery incomplete at seq %d", seq)
		}
	}
	r.Recovered += len(out)
	return out, nil
}

func (r *Recovery) recoverOne(seq uint16) (Packet, bool) {
	for bi := len(r.blocks) - 1; bi >= 0; bi-- {
		b := r.blocks[bi]
		for i := 0; i < b.Count; i++ {
			if b.Base+uint16(i) != seq {
				continue
			}
			parity := append([]byte(nil), b.Parity...)
			ok := true
			for j := 0; j < b.Count; j++ {
				if j == i {
					continue
				}
				other := b.Base + uint16(j)
				data, have := r.data[other]
				if !have {
					ok = false
					break
				}
				xorInto(parity, data)
			}
			if !ok {
				continue
			}
			n := b.Lengths[i]
			if n > len(parity) {
				n = len(parity)
			}
			payload := parity[:n]
			return Packet{Seq: seq, Payload: payload}, true
		}
	}
	return Packet{}, false
}

func xorInto(dst, src []byte) {
	for i := range dst {
		if i < len(src) {
			dst[i] ^= src[i]
		}
	}
}

// XORParity builds the parity payload for a block from its data packets.
func XORParity(payloads [][]byte) ([]byte, int) {
	maxLen := 0
	for _, p := range payloads {
		if len(p) > maxLen {
			maxLen = len(p)
		}
	}
	parity := make([]byte, maxLen)
	for _, p := range payloads {
		xorInto(parity, p)
	}
	return parity, maxLen
}
