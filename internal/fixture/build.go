package fixture

import (
	"hash/crc32"

	"archivectl/internal/clock"
	"archivectl/internal/core"
	"archivectl/internal/verify"
)

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[s] = true
	}
	return m
}

func variableBlockLengths(frames int, vfr bool) []int {
	if !vfr {
		var out []int
		for left := frames; left > 0; {
			n := 4096
			if n > left {
				n = left
			}
			out = append(out, n)
			left -= n
		}
		return out
	}
	var out []int
	for i, left := 0, frames; left > 0; i++ {
		n := 3072
		if i%2 == 1 {
			n = 5120
		}
		if n > left {
			n = left
		}
		out = append(out, n)
		left -= n
	}
	return out
}

func starts(lengths []int) []int64 {
	out := make([]int64, len(lengths))
	var total int64
	for i, n := range lengths {
		out[i] = total
		total += int64(n)
	}
	return out
}

func keyframeCues(frames, interval int) []Cue {
	var cues []Cue
	id := uint32(1)
	for f := 0; f < frames; f += interval {
		cues = append(cues, Cue{ID: id, Frame: int64(f)})
		id++
	}
	return cues
}

func buildClock(cfg Config, inj map[string]bool, lengths []int, blockStarts []int64, cues []Cue) ClockFile {
	rate := core.MustRat(1, 1)
	if inj["drift"] {
		rate = core.MustRat(1003, 1000)
	}
	start := uint32(1_000)
	if inj["wrap"] {
		start = 0xFFFFFC00 // Wraps after roughly 1024 frames.
	}
	tickAt := func(frame int64) uint32 {
		return uint32(int64(start) + core.ScaleFloor(frame, rate))
	}
	blocks := make([]clock.BlockRecord, len(lengths))
	for i := range lengths {
		blocks[i] = clock.BlockRecord{Index: i, Tick: tickAt(blockStarts[i]), FrameCount: lengths[i]}
	}
	anchors := make([]clock.Anchor, len(cues))
	for i, c := range cues {
		anchors[i] = clock.Anchor{CueID: c.ID, Tick: tickAt(c.Frame), Frame: c.Frame}
	}
	return ClockFile{
		StartTick: start, TicksPerFrame: rate, WrapInjected: inj["wrap"],
		DriftInjected: inj["drift"], Blocks: blocks, Anchors: anchors,
	}
}

func buildChecks(pcm []byte, lengths []int, blockStarts []int64, frameBytes int) ChecksFile {
	blocks := make([]verify.BlockCheck, len(lengths))
	for i, n := range lengths {
		off := int(blockStarts[i]) * frameBytes
		end := off + n*frameBytes
		blocks[i] = verify.BlockCheck{
			Index: i, StartFrame: blockStarts[i], FrameCount: n,
			CRC32: crc32.ChecksumIEEE(pcm[off:end]),
		}
	}
	return ChecksFile{SegmentFrames: verify.SegmentFrames, Blocks: blocks}
}
