package flow

import (
	"fmt"
	"sort"

	"mediaroute/internal/clock"
)

// ChunkResult is the outcome of one worker building one segment chunk.
type ChunkResult struct {
	Job       Job
	OutPath   string // empty when the chunk failed or was not written
	SHA256    string
	Bytes     int64
	Digest    string // sample-table digest of the written chunk
	Recovered bool   // source representation carried recovered faults
	Err       error
}

// SegmentDecision is the fan-in verdict for one (camera, segment) group.
type SegmentDecision struct {
	Camera      string
	Segment     int
	Delivered   []ChunkResult
	Quarantined []ChunkResult
	Mismatch    bool
	Deviation   int64 // max boundary spread across representations, 90 kHz ticks
}

// FanIn collects chunk results per (camera, segment) and cross-checks
// representation boundaries at every cut point on the shared clock.
// Groups whose boundary spread exceeds the tolerance are mismatches;
// the deviating representations are quarantined, the rest is delivered.
type FanIn struct {
	Tolerance   int64 // 90 kHz ticks
	ExpectPerGroup int // results expected per (camera, segment) group

	groups map[string]map[int][]ChunkResult
}

// NewFanIn creates a FanIn expecting expectPerGroup results per segment.
func NewFanIn(tolerance int64, expectPerGroup int) *FanIn {
	if tolerance <= 0 {
		tolerance = clock.Rate // 1 s default
	}
	return &FanIn{
		Tolerance:      tolerance,
		ExpectPerGroup: expectPerGroup,
		groups:         map[string]map[int][]ChunkResult{},
	}
}

// Add records one result and returns a decision once its group is
// complete (or nil while waiting for more results).
func (f *FanIn) Add(r ChunkResult) *SegmentDecision {
	cam := r.Job.Camera
	seg := r.Job.Segment
	if f.groups[cam] == nil {
		f.groups[cam] = map[int][]ChunkResult{}
	}
	f.groups[cam][seg] = append(f.groups[cam][seg], r)
	if len(f.groups[cam][seg]) < f.ExpectPerGroup {
		return nil
	}
	results := f.groups[cam][seg]
	delete(f.groups[cam], seg)
	d := f.decide(cam, seg, results)
	return &d
}

// Flush decides every incomplete group with the results available
// (end of stream). Decisions are ordered by camera then segment.
func (f *FanIn) Flush() []SegmentDecision {
	var out []SegmentDecision
	cams := make([]string, 0, len(f.groups))
	for cam := range f.groups {
		cams = append(cams, cam)
	}
	sort.Strings(cams)
	for _, cam := range cams {
		segs := make([]int, 0, len(f.groups[cam]))
		for seg := range f.groups[cam] {
			segs = append(segs, seg)
		}
		sort.Ints(segs)
		for _, seg := range segs {
			out = append(out, f.decide(cam, seg, f.groups[cam][seg]))
		}
	}
	f.groups = map[string]map[int][]ChunkResult{}
	return out
}

// decide cross-checks the segment start clocks of all representations.
func (f *FanIn) decide(cam string, seg int, results []ChunkResult) SegmentDecision {
	d := SegmentDecision{Camera: cam, Segment: seg}
	var ok []ChunkResult
	for _, r := range results {
		if r.Err != nil || r.OutPath == "" {
			d.Quarantined = append(d.Quarantined, r) // failed chunk: isolate
			continue
		}
		ok = append(ok, r)
	}
	if len(ok) == 0 {
		d.Mismatch = len(results) > 0
		return d
	}
	lo, hi := ok[0].Job.StartClock, ok[0].Job.StartClock
	for _, r := range ok[1:] {
		lo = min64(lo, r.Job.StartClock)
		hi = max64(hi, r.Job.StartClock)
	}
	d.Deviation = hi - lo
	if d.Deviation <= f.Tolerance {
		d.Delivered = ok
		return d
	}
	// Boundary mismatch: quarantine the representations that deviate
	// from the group median, deliver the rest.
	d.Mismatch = true
	starts := make([]int64, len(ok))
	for i, r := range ok {
		starts[i] = r.Job.StartClock
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	median := starts[len(starts)/2]
	for _, r := range ok {
		if abs64(r.Job.StartClock-median) > f.Tolerance {
			d.Quarantined = append(d.Quarantined, r)
		} else {
			d.Delivered = append(d.Delivered, r)
		}
	}
	return d
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// String summarizes a decision for terminal output.
func (d SegmentDecision) String() string {
	return fmt.Sprintf("camera=%s segment=%d delivered=%d quarantined=%d deviation_ticks=%d",
		d.Camera, d.Segment, len(d.Delivered), len(d.Quarantined), d.Deviation)
}
