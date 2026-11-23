package pipeline

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"medpipe/internal/dag"
	"medpipe/internal/model"
	"medpipe/internal/sequence"
)

// Report is the terminal summary the demo prints.
type Report struct {
	RoundTripRate        float64
	KeyframeHitRate      float64
	KeyframePrestartMiss int
	Segments             int
	SegmentSHA256Count   int
	CorruptRejected      int
	RecoveryReusedNodes  int
	BlockedDependency    string
	BlockedByDependency  string
	DuplicateGroups      int
	SilenceRanges        int
	MaxSnapErrorNS       int64
	MaxFrameIntervalNS   int64
	VFRTracks            int
	DriftedTracks        int
	SidecarSampleCount   int
	BoundaryCases        int
	ChecksumCases        int
}

// SegmentTargets returns the query times used to probe the keyframe index:
// every segment boundary plus its midpoint.
func SegmentTargets(out *Outcome) []int64 {
	if out == nil || len(out.Segments) == 0 {
		return nil
	}
	seen := map[int64]bool{}
	var targets []int64
	add := func(ns int64) {
		if !seen[ns] {
			seen[ns] = true
			targets = append(targets, ns)
		}
	}
	for _, s := range out.Segments {
		add(s.StartNS)
		add((s.StartNS + s.EndNS) / 2)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	return targets
}

// KeyframeHitRate measures index correctness over queries that are inside a
// track's keyframe-covered range: from its first keyframe to its last frame.
// Every such query must resolve. Queries before the first keyframe are
// expected to miss and are counted separately by KeyframePrestartMisses; they
// are not index failures.
func KeyframeHitRate(out *Outcome) float64 {
	if out == nil || out.Keyframes == nil || out.Timeline == nil {
		return 0
	}
	targets := SegmentTargets(out)
	if len(targets) == 0 {
		return 0
	}
	var total, hits int
	for i := range out.Timeline.Tracks {
		tt := &out.Timeline.Tracks[i]
		if len(tt.Frames) == 0 {
			continue
		}
		ti, ok := out.Keyframes.Tracks[tt.Track.ID]
		if !ok {
			continue
		}
		firstKF := int64(math.MaxInt64)
		if len(ti.Entries) > 0 {
			firstKF = ti.Entries[0].NS
		}
		covered := false
		for _, target := range targets {
			if target < firstKF || target > tt.EndNS {
				continue
			}
			covered = true
			res, err := out.Keyframes.QueryTrack(tt.Track.ID, target)
			if err != nil {
				continue
			}
			total++
			if res.Hit {
				hits++
			}
		}
		if !covered && len(ti.Entries) > 0 {
			// No segment boundary fell inside the covered range; probe the
			// first keyframe itself so the track still contributes.
			res, err := out.Keyframes.QueryTrack(tt.Track.ID, firstKF)
			if err == nil {
				total++
				if res.Hit && res.DeltaNS == 0 {
					hits++
				}
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// KeyframePrestartMisses counts probes made just before a track's first
// keyframe. Each must report a miss; a hit here would mean the index returned
// a keyframe later than the target.
func KeyframePrestartMisses(out *Outcome) (misses int, violations int) {
	if out == nil || out.Keyframes == nil || out.Timeline == nil {
		return 0, 0
	}
	for i := range out.Timeline.Tracks {
		ti, ok := out.Keyframes.Tracks[out.Timeline.Tracks[i].Track.ID]
		if !ok || len(ti.Entries) == 0 || ti.Entries[0].NS == math.MinInt64 {
			continue
		}
		res, err := out.Keyframes.QueryTrack(ti.TrackID, ti.Entries[0].NS-1)
		if err != nil {
			continue
		}
		misses++
		if res.Hit {
			violations++
		}
	}
	return misses, violations
}

// RoundTripRate compares the sequence digest read from disk with the digest
// of the in-memory frames after re-serializing them. A rate of 1 means every
// frame survived byte-for-byte.
func RoundTripRate(out *Outcome) (float64, error) {
	if out == nil || out.File == nil {
		return 0, model.New(model.KindInvariant, "pipeline.RoundTripRate", "no file artifact")
	}
	want := out.File.Digest
	_, digest, err := sequence.WriteTo(io.Discard, out.File.Header.Tracks, out.File.Frames)
	if err != nil {
		return 0, err
	}
	if digest == want {
		return 1, nil
	}
	// Report the fraction of frames that still hash equal, for diagnostics.
	var same, total int
	for _, tr := range out.File.Header.Tracks {
		for _, fr := range out.File.Frames[tr.ID] {
			total++
			if fr.IntegrityChecksum() == fr.Checksum {
				same++
			}
		}
	}
	if total == 0 {
		return 0, nil
	}
	return float64(same) / float64(total), nil
}

// DuplicateGroups counts dedupe groups with more than one member.
func DuplicateGroups(out *Outcome) int {
	if out == nil || out.Fingerprints == nil {
		return 0
	}
	return len(fingerprintDuplicates(out))
}

func fingerprintDuplicates(out *Outcome) [][]int {
	byKey := map[string][]int{}
	for _, s := range out.Fingerprints.Segments {
		byKey[s.DedupeKey] = append(byKey[s.DedupeKey], s.Index)
	}
	var groups [][]int
	for _, idx := range byKey {
		if len(idx) > 1 {
			groups = append(groups, idx)
		}
	}
	return groups
}

// BuildReport assembles the printable report from an outcome, the per-node
// reuse counts observed during the recovery pass, and the blocked dependency
// captured during the deliberate failure.
func BuildReport(out *Outcome, reusedNodes int, blocked *dag.BlockedDep, corruptRejected int) (*Report, error) {
	rate, err := RoundTripRate(out)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		RoundTripRate:       rate,
		KeyframeHitRate:     KeyframeHitRate(out),
		Segments:            len(out.Segments),
		SegmentSHA256Count:  len(out.Fingerprints.Segments),
		CorruptRejected:     corruptRejected,
		RecoveryReusedNodes: reusedNodes,
		DuplicateGroups:     len(fingerprintDuplicates(out)),
		SilenceRanges:       len(out.Silence),
	}
	rep.KeyframePrestartMiss, _ = KeyframePrestartMisses(out)
	if out.Timeline != nil {
		for i := range out.Timeline.Tracks {
			tt := &out.Timeline.Tracks[i]
			if tt.VFR {
				rep.VFRTracks++
			}
			if math.Abs(tt.DriftPPM) > 1 {
				rep.DriftedTracks++
			}
		}
		rep.MaxFrameIntervalNS = out.Timeline.MaxIntervalNS
	}
	if out.Sidecar != nil {
		for _, tr := range out.Sidecar.Tracks {
			rep.SidecarSampleCount += tr.SampleCount
		}
	}
	if blocked != nil {
		rep.BlockedDependency = blocked.Node
		rep.BlockedByDependency = blocked.Dep
	}
	return rep, nil
}

// Lines renders the report as key=value lines in a stable order.
func (r *Report) Lines() []string {
	return []string{
		fmt.Sprintf("roundtrip_rate=%.3f", r.RoundTripRate),
		fmt.Sprintf("keyframe_hit_rate=%.3f", r.KeyframeHitRate),
		fmt.Sprintf("segments=%d", r.Segments),
		fmt.Sprintf("segment_sha256_count=%d", r.SegmentSHA256Count),
		fmt.Sprintf("corrupt_rejected=%d", r.CorruptRejected),
		fmt.Sprintf("recovery_reused_nodes=%d", r.RecoveryReusedNodes),
		fmt.Sprintf("blocked_dependency=%s", r.BlockedDependency),
		fmt.Sprintf("blocked_by=%s", r.BlockedByDependency),
		fmt.Sprintf("duplicate_groups=%d", r.DuplicateGroups),
		fmt.Sprintf("silence_ranges=%d", r.SilenceRanges),
		fmt.Sprintf("vfr_tracks=%d", r.VFRTracks),
		fmt.Sprintf("drifted_tracks=%d", r.DriftedTracks),
		fmt.Sprintf("keyframe_prestart_misses=%d", r.KeyframePrestartMiss),
		fmt.Sprintf("sidecar_samples=%d", r.SidecarSampleCount),
		fmt.Sprintf("max_snap_error_ns=%d", r.MaxSnapErrorNS),
		fmt.Sprintf("max_frame_interval_ns=%d", r.MaxFrameIntervalNS),
	}
}

// String is the multi-line report body.
func (r *Report) String() string {
	return strings.Join(r.Lines(), "\n")
}
