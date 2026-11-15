// Package slicer plans and writes version-stable TS material fragments.
package slicer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"tsprobe/internal/demux"
	"tsprobe/internal/psi"
)

// Options controls slice planning.
type Options struct {
	SegmentMS int
	OutDir    string
}

// PlayableRange is a packet range inside a corrupt slice that remains playable.
type PlayableRange struct {
	StartPacket uint64 `json:"start_packet"`
	EndPacket   uint64 `json:"end_packet"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
}

// Slice is one material fragment and its source mapping.
type Slice struct {
	Index          int             `json:"index"`
	File           string          `json:"file"`
	StartPacket    uint64          `json:"start_packet"`
	EndPacket      uint64          `json:"end_packet"`
	StartOffset    int64           `json:"start_offset"`
	EndOffset      int64           `json:"end_offset"`
	StartMS        int64           `json:"start_ms"`
	EndMS          int64           `json:"end_ms"`
	DurationMS     int64           `json:"duration_ms"`
	Programs       []uint16        `json:"programs"`
	PIDs           []uint16        `json:"pids"`
	Corrupt        bool            `json:"corrupt"`
	PlayableRanges []PlayableRange `json:"playable_ranges,omitempty"`
	WrittenPackets uint64          `json:"written_packets"`
}

// Summary is the top-level slices object in index.json.
type Summary struct {
	Total   int     `json:"total"`
	Corrupt int     `json:"corrupt"`
	Items   []Slice `json:"items"`
}

// Index is the deterministic slicing output document.
type Index struct {
	File      string  `json:"file"`
	SegmentMS int     `json:"segment_ms"`
	Slices    Summary `json:"slices"`
}

// Build plans slices from the same demux result used by scan.
func Build(result *demux.Result, opt Options) (Index, error) {
	if opt.SegmentMS <= 0 {
		return Index{}, fmt.Errorf("segment-ms must be positive")
	}
	idx := Index{File: result.Path, SegmentMS: opt.SegmentMS, Slices: Summary{Items: []Slice{}}}
	if len(result.Keyframes) == 0 || result.PacketsTotal == 0 {
		return idx, nil
	}
	keyframes := chooseAnchor(result.Keyframes)
	boundaries := versionBoundaries(result.Store.Events(), result.PacketsTotal)
	basePTS := keyframes[0].PTSMillis
	for _, k := range keyframes {
		if k.PTSMillis < basePTS {
			basePTS = k.PTSMillis
		}
	}
	var plan []Slice
	intervalStart := uint64(1)
	for _, boundary := range boundaries {
		plan = append(plan, planInterval(result, keyframes, intervalStart, boundary-1, basePTS, opt.SegmentMS, opt.OutDir)...)
		intervalStart = boundary
	}
	plan = append(plan, planInterval(result, keyframes, intervalStart, result.PacketsTotal, basePTS, opt.SegmentMS, opt.OutDir)...)
	for i := range plan {
		plan[i].Index = i + 1
		plan[i].File = filepath.Join(opt.OutDir, fmt.Sprintf("slice_%06d.ts", i+1))
		plan[i].Corrupt, plan[i].PlayableRanges = playableRanges(plan[i], result.CCGaps)
		for j := range plan[i].PlayableRanges {
			pr := &plan[i].PlayableRanges[j]
			pr.StartMS = estimateMS(result, pr.StartPacket)
			pr.EndMS = estimateMS(result, pr.EndPacket+1)
		}
		if plan[i].Corrupt {
			idx.Slices.Corrupt++
		}
	}
	idx.Slices.Items = plan
	idx.Slices.Total = len(plan)
	return idx, nil
}

func chooseAnchor(all []demux.Keyframe) []demux.Keyframe {
	counts := make(map[uint16]int)
	for _, k := range all {
		counts[k.PID]++
	}
	var anchor uint16
	best := -1
	for pid, n := range counts {
		if n > best || (n == best && pid < anchor) {
			anchor, best = pid, n
		}
	}
	out := make([]demux.Keyframe, 0, counts[anchor])
	for _, k := range all {
		if k.PID == anchor {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PacketIndex < out[j].PacketIndex })
	return out
}

func versionBoundaries(events []psi.VersionEvent, total uint64) []uint64 {
	seen := make(map[uint64]bool)
	var out []uint64
	for _, e := range events {
		if e.PacketIndex <= 1 || e.PacketIndex > total || seen[e.PacketIndex] {
			continue
		}
		seen[e.PacketIndex] = true
		out = append(out, e.PacketIndex)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func planInterval(result *demux.Result, kfs []demux.Keyframe, start, end uint64, basePTS int64, segmentMS int, outDir string) []Slice {
	_ = outDir
	first := sort.Search(len(kfs), func(i int) bool { return kfs[i].PacketIndex >= start })
	if first >= len(kfs) || kfs[first].PacketIndex > end {
		return nil
	}
	var starts []demux.Keyframe
	for i := first; i < len(kfs) && kfs[i].PacketIndex <= end; {
		starts = append(starts, kfs[i])
		target := kfs[i].PTSMillis + int64(segmentMS)
		j := i + 1
		for j < len(kfs) && kfs[j].PacketIndex <= end && kfs[j].PTSMillis < target {
			j++
		}
		if j >= len(kfs) || kfs[j].PacketIndex > end {
			break
		}
		i = j
	}
	out := make([]Slice, 0, len(starts))
	for i, kf := range starts {
		sliceEnd := end
		if i+1 < len(starts) {
			sliceEnd = starts[i+1].PacketIndex - 1
		}
		startMS := kf.PTSMillis - basePTS
		if startMS < 0 {
			startMS = 0
		}
		endMS := estimateMS(result, sliceEnd+1)
		if endMS < startMS {
			endMS = startMS
		}
		snap := mappingAt(result.Store, kf.PacketIndex)
		programs := mappingPrograms(snap)
		pids := mappingPIDs(snap)
		out = append(out, Slice{
			StartPacket: kf.PacketIndex, EndPacket: sliceEnd, StartOffset: kf.ByteOffset,
			StartMS: startMS, EndMS: endMS, DurationMS: endMS - startMS,
			Programs: programs, PIDs: pids,
		})
	}
	return out
}

func estimateMS(result *demux.Result, packet uint64) int64 {
	if result.PacketsTotal == 0 {
		return 0
	}
	if packet > result.PacketsTotal {
		packet = result.PacketsTotal
	}
	return result.DurationMS * int64(packet-1) / int64(result.PacketsTotal)
}

func mappingAt(store *psi.Store, packet uint64) psi.MappingSnapshot {
	points := append([]psi.MappingPoint(nil), store.MappingPoints()...)
	sort.SliceStable(points, func(i, j int) bool { return points[i].PacketIndex < points[j].PacketIndex })
	var snap psi.MappingSnapshot
	for _, p := range points {
		if p.PacketIndex > packet {
			break
		}
		snap = p.Snapshot
	}
	if snap.Programs == nil && len(points) > 0 {
		snap = points[0].Snapshot
	}
	return snap
}

func mappingPrograms(snap psi.MappingSnapshot) []uint16 {
	var out []uint16
	seen := make(map[uint16]bool)
	for _, p := range snap.Programs {
		if !seen[p.ProgramNumber] {
			seen[p.ProgramNumber] = true
			out = append(out, p.ProgramNumber)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func mappingPIDs(snap psi.MappingSnapshot) []uint16 {
	var out []uint16
	seen := make(map[uint16]bool)
	for _, p := range snap.Programs {
		for _, es := range p.Streams {
			if !seen[es.PID] {
				seen[es.PID] = true
				out = append(out, es.PID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func playableRanges(s Slice, gaps []demux.CCGap) (bool, []PlayableRange) {
	type span struct{ start, end uint64 }
	var bad []span
	for _, g := range gaps {
		if g.EndPacket < s.StartPacket || g.StartPacket > s.EndPacket {
			continue
		}
		a, b := g.StartPacket, g.EndPacket
		if a < s.StartPacket {
			a = s.StartPacket
		}
		if b > s.EndPacket {
			b = s.EndPacket
		}
		if a <= b {
			bad = append(bad, span{a, b})
		}
	}
	if len(bad) == 0 {
		return false, nil
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].start < bad[j].start })
	merged := []span{bad[0]}
	for _, b := range bad[1:] {
		last := &merged[len(merged)-1]
		if b.start <= last.end+1 {
			if b.end > last.end {
				last.end = b.end
			}
		} else {
			merged = append(merged, b)
		}
	}
	var playable []PlayableRange
	cursor := s.StartPacket
	for _, b := range merged {
		if cursor < b.start {
			playable = append(playable, PlayableRange{StartPacket: cursor, EndPacket: b.start - 1})
		}
		if b.end >= cursor {
			cursor = b.end + 1
		}
	}
	if cursor <= s.EndPacket {
		playable = append(playable, PlayableRange{StartPacket: cursor, EndPacket: s.EndPacket})
	}
	return true, playable
}

// WriteIndex writes deterministic indented JSON.
func WriteIndex(path string, idx Index) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(bs, '\n'), 0o644)
}
