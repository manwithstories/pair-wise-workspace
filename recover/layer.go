// Package recover implements interleaved parity recovery independently for
// each 1x/2x/4x representation.
package recover

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"medcheck/fault"
)

// Block is one received or reconstructed media block.
type Block struct {
	Position int    `json:"position"`
	Seq      uint64 `json:"seq"`
	Data     []byte `json:"-"`
	Parity   bool   `json:"parity"`
}

// SegmentRepair is an interleaved XOR codeword for one segment.
//
// Positions are striped across Depth rows (position % Depth). A burst shorter
// than Depth therefore hits each row at most once and is fully recoverable
// from that row's XOR parity block.
type SegmentRepair struct {
	Segment int
	Depth   int
	Blocks  [][]byte
	Parity  [][]byte

	received map[int][]byte
	corrupt  map[int]bool
}

// NewSegmentRepair builds original blocks and their row parity blocks.
func NewSegmentRepair(segment, depth int, original [][]byte) *SegmentRepair {
	if depth < 1 {
		depth = 1
	}
	s := &SegmentRepair{
		Segment:  segment,
		Depth:    depth,
		Blocks:   original,
		Parity:   make([][]byte, depth),
		received: make(map[int][]byte),
		corrupt:  make(map[int]bool),
	}
	for row := 0; row < depth; row++ {
		var parity []byte
		for pos, block := range original {
			if pos%depth != row {
				continue
			}
			parity = xorInto(parity, block)
		}
		s.Parity[row] = parity
	}
	return s
}

// Receive supplies a block. A failed CRC is treated as missing/corrupt and is
// never used in XOR reconstruction.
func (s *SegmentRepair) Receive(pos int, data []byte, crcValid bool) {
	if pos < 0 || pos >= len(s.Blocks) {
		return
	}
	if !crcValid {
		s.corrupt[pos] = true
		delete(s.received, pos)
		return
	}
	s.corrupt[pos] = false
	cp := append([]byte(nil), data...)
	s.received[pos] = cp
}

func xorInto(dst, src []byte) []byte {
	if len(dst) < len(src) {
		dst = append(dst, make([]byte, len(src)-len(dst))...)
	}
	for i := range src {
		dst[i] ^= src[i]
	}
	return dst
}

// Recover reconstructs every row with exactly one missing data block. Rows
// with two or more missing/corrupt blocks remain precise unrecoverable ranges.
func (s *SegmentRepair) Recover() (recovered []int, unrecoverable []int) {
	for row := 0; row < s.Depth; row++ {
		var rowPositions []int
		missing := make([]int, 0, 1)
		for pos := range s.Blocks {
			if pos%s.Depth == row {
				rowPositions = append(rowPositions, pos)
				if _, ok := s.received[pos]; !ok {
					missing = append(missing, pos)
				}
			}
		}
		if len(missing) == 0 {
			continue
		}
		if len(missing) != 1 || s.Parity[row] == nil {
			unrecoverable = append(unrecoverable, missing...)
			continue
		}
		target := missing[0]
		out := append([]byte(nil), s.Parity[row]...)
		for _, pos := range rowPositions {
			if pos == target {
				continue
			}
			out = xorInto(out, s.received[pos])
		}
		s.received[target] = out
		recovered = append(recovered, target)
	}
	return recovered, unrecoverable
}

// Data returns the current block or nil if still unavailable.
func (s *SegmentRepair) Data(pos int) []byte { return s.received[pos] }

// Complete reports whether all positions are available.
func (s *SegmentRepair) Complete() bool {
	for pos := range s.Blocks {
		if _, ok := s.received[pos]; !ok {
			return false
		}
	}
	return true
}

// SHA256 hashes the canonical concatenation of available blocks. It is stable
// across reruns for the same generated/recovered content.
func (s *SegmentRepair) SHA256() string {
	h := sha256.New()
	for pos := range s.Blocks {
		if b, ok := s.received[pos]; ok {
			fmt.Fprintf(h, "block=%d len=%d\n", pos, len(b))
			h.Write(b)
			h.Write([]byte("\n"))
		} else {
			fmt.Fprintf(h, "block=%d UNAVAILABLE\n", pos)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Layer is an independent representation recovery state.
type Layer struct {
	Name            string
	Segments        map[int]*SegmentRepair
	RecoveredBlocks int
	BadSegments     []int
	SegmentHashes   map[int]string
	faults          *fault.Model
}

// NewLayer creates a named recovery layer.
func NewLayer(name string, faults *fault.Model) *Layer {
	return &Layer{Name: name, Segments: make(map[int]*SegmentRepair), SegmentHashes: make(map[int]string), faults: faults}
}

// AddSegment registers a codeword.
func (l *Layer) AddSegment(s *SegmentRepair) { l.Segments[s.Segment] = s }

// RecoverAll runs every segment independently and records precise bad segments.
func (l *Layer) RecoverAll() {
	l.SegmentHashes = make(map[int]string, len(l.Segments))
	for id, seg := range l.Segments {
		recovered, badPositions := seg.Recover()
		l.RecoveredBlocks += len(recovered)
		if len(badPositions) > 0 {
			l.BadSegments = append(l.BadSegments, id)
			l.faults.Add(fault.Fault{
				Kind:    fault.BadSegment,
				Segment: id,
				Message: fmt.Sprintf("layer %s has unrecoverable blocks %v", l.Name, badPositions),
			})
		}
		l.SegmentHashes[id] = seg.SHA256()
	}
	sort.Ints(l.BadSegments)
}
