// Package recover implements XOR parity repair for lost or corrupt PCM
// blocks. Each group holds k data blocks plus one parity block whose bytes
// are the XOR of its group members. A group with exactly one bad member is
// repaired exactly; groups with two or more bad members keep their gaps
// and are reported as unrecovered — the pipeline never fabricates audio.
package recover

import (
	"hash/crc32"

	"audiorestore/internal/model"
	"audiorestore/internal/wav"
)

// Group is one parity protection domain: k data blocks followed by their
// parity block, all sharing the same byte width.
type Group struct {
	ID      int
	Members []int // block indices; last member is the parity block
}

// Outcome summarizes one recovery run.
type Outcome struct {
	Recovered   []int // block indices repaired from parity
	Unrecovered []int // block indices still missing/corrupt afterwards
	Groups      int
}

// Recoverer repairs a block stream against its group plan.
type Recoverer struct {
	Groups      []Group
	Log         *model.ErrorLog
	Track       string
}

func bad(b wav.FrameBlock) bool { return b.Missing || b.Corrupt || b.Truncated }

// Repair runs XOR recovery in place. Restored blocks are CRC-checked
// against the manifest before being accepted; a failed check is itself
// reported and the block stays unrecovered.
func (r *Recoverer) Repair(blocks []wav.FrameBlock) Outcome {
	out := Outcome{Groups: len(r.Groups)}
	for _, g := range r.Groups {
		badIdx := r.badMembers(blocks, g)
		if len(badIdx) == 0 {
			continue
		}
		if len(badIdx) > 1 {
			for _, idx := range badIdx {
				out.Unrecovered = append(out.Unrecovered, idx)
				r.Log.Addf(model.ErrUnrecovered, r.Track, idx,
					blocks[idx].StartFrame,
					"%d losses in group %d exceed parity capacity", len(badIdx), g.ID)
			}
			continue
		}
		idx := badIdx[0]
		repaired, ok := r.xorGroup(blocks, g, idx)
		if !ok {
			out.Unrecovered = append(out.Unrecovered, idx)
			continue
		}
		want := blocks[idx].WantCRC
		if want != 0 && crc32.ChecksumIEEE(repaired) != want {
			r.Log.Addf(model.ErrUnrecovered, r.Track, idx,
				blocks[idx].StartFrame, "repaired bytes failed CRC verification")
			out.Unrecovered = append(out.Unrecovered, idx)
			continue
		}
		b := &blocks[idx]
		b.Data = repaired
		b.CRC = crc32.ChecksumIEEE(repaired)
		b.Missing, b.Corrupt, b.Truncated = false, false, false
		out.Recovered = append(out.Recovered, idx)
	}
	return out
}

func (r *Recoverer) badMembers(blocks []wav.FrameBlock, g Group) []int {
	var badIdx []int
	for _, idx := range g.Members {
		if idx < 0 || idx >= len(blocks) || bad(blocks[idx]) {
			badIdx = append(badIdx, idx)
		}
	}
	return badIdx
}

// xorGroup folds every intact member (including parity) into the missing
// member's slot. All blocks in a group share width; the shortest intact
// member bounds the repair, and a width mismatch rejects the repair.
func (r *Recoverer) xorGroup(blocks []wav.FrameBlock, g Group, target int) ([]byte, bool) {
	width := 0
	for _, idx := range g.Members {
		if idx == target || bad(blocks[idx]) {
			continue
		}
		if width == 0 {
			width = len(blocks[idx].Data)
			continue
		}
		if len(blocks[idx].Data) != width {
			r.Log.Addf(model.ErrUnrecovered, r.Track, target,
				blocks[target].StartFrame, "group %d has mixed block widths", g.ID)
			return nil, false
		}
	}
	if width == 0 {
		r.Log.Addf(model.ErrUnrecovered, r.Track, target,
			blocks[target].StartFrame, "group %d has no intact member", g.ID)
		return nil, false
	}
	out := make([]byte, width)
	for _, idx := range g.Members {
		if idx == target || bad(blocks[idx]) {
			continue
		}
		for i := range out {
			out[i] ^= blocks[idx].Data[i]
		}
	}
	return out, true
}

// BuildGroups lays out consecutive blocks into groups of groupSize
// members (k data + 1 parity). The final short group, if any, is included
// only when it still contains a parity block.
func BuildGroups(nBlocks, dataPerGroup int) []Group {
	if dataPerGroup <= 0 {
		return nil
	}
	stride := dataPerGroup + 1
	var groups []Group
	for start := 0; start+stride <= nBlocks; start += stride {
		members := make([]int, stride)
		for i := range members {
			members[i] = start + i
		}
		groups = append(groups, Group{ID: len(groups), Members: members})
	}
	return groups
}
