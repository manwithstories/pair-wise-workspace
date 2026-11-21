package main

import (
	"fmt"

	"mediapack/internal/hash"
	"mediapack/internal/segments"
)

// hashArtifact is the durable form of the per-segment manifest.
type hashArtifact struct {
	SegmentTicks string        `json:"segment_ticks"`
	Segments     []hashSegment `json:"segments"`
	Blocked      bool          `json:"blocked"`
}

type hashSegment struct {
	Ordinal         int      `json:"ordinal"`
	Start           string   `json:"start"`
	End             string   `json:"end"`
	Frames          int      `json:"frames"`
	SHA256          string   `json:"sha256"`
	Representations []string `json:"representations"`
}

func asHashArtifact(m *hash.Manifest) hashArtifact {
	out := hashArtifact{SegmentTicks: m.SegmentTicks, Blocked: m.Blocked}
	for _, e := range m.Segments {
		out.Segments = append(out.Segments, hashSegment{Ordinal: e.Ordinal, Start: e.Start,
			End: e.End, Frames: e.Frames, SHA256: e.SHA256, Representations: e.Representations})
	}
	return out
}

func (a hashArtifact) manifest() *hash.Manifest {
	m := &hash.Manifest{SegmentTicks: a.SegmentTicks, Blocked: a.Blocked}
	for _, e := range a.Segments {
		m.Segments = append(m.Segments, hash.Entry{Ordinal: e.Ordinal, Start: e.Start,
			End: e.End, Frames: e.Frames, SHA256: e.SHA256, Representations: e.Representations})
	}
	return m
}

// hashSegments builds and hashes every planned segment using only artifacts.
func hashSegments(rs *runState, cfg string) (hashArtifact, error) {
	plan, err := ensurePlan(rs, cfg)
	if err != nil {
		return hashArtifact{}, err
	}
	frameArt, err := readArtifact[framesArtifact](rs, "frames", cfg)
	if err != nil {
		return hashArtifact{}, err
	}
	var segs []hash.Segment
	for _, seg := range plan.Segments {
		var all []hash.Frame
		for _, rate := range rs.bundle.Rates {
			name := fmt.Sprintf("%d", rate.Value)
			for _, f := range frameArt[name] {
				ticks := f.ticks()
				if ticks.Cmp(seg.Start) >= 0 && ticks.Cmp(seg.End) < 0 {
					all = append(all, hash.Frame{Track: name, Sample: f.Number,
						Payload: rate.Payloads[f.Number], SHA256: f.SHA256})
				}
			}
		}
		segs = append(segs, hash.Segment{Ordinal: seg.Ordinal, Start: seg.Start, End: seg.End, Frames: all})
	}
	var reps []string
	for _, r := range rs.bundle.Rates {
		reps = append(reps, fmt.Sprintf("%d", r.Value))
	}
	m, err := hash.Build(segs, reps, rs.frameTicks, hash.DefaultLimits())
	if err != nil {
		return hashArtifact{}, err
	}
	return asHashArtifact(m), nil
}

// ensurePlan restores the segment plan when the segments node was skipped.
func ensurePlan(rs *runState, cfg string) (*segments.Plan, error) {
	if rs.plan != nil {
		return rs.plan, nil
	}
	art, err := readArtifact[segmentsArtifact](rs, "segments", cfg)
	if err != nil {
		return nil, err
	}
	plan := &segments.Plan{Primary: fmt.Sprintf("%d", rs.bundle.Rates[0].Value)}
	for _, s := range art.Segments {
		plan.Segments = append(plan.Segments, segments.Segment{Ordinal: s.Ordinal, Start: s.start(), End: s.end()})
	}
	rs.plan = plan
	return plan, nil
}

// manifestBytes returns the exact bytes written to manifest.json, whether the
// hash node ran in this invocation or an earlier one.
func (rs *runState) manifestBytes(cfg string) ([]byte, error) {
	art, err := readArtifact[hashArtifact](rs, "hash", cfg)
	if err != nil {
		return nil, err
	}
	return jsonBytes(art), nil
}

// manifestView returns the manifest recorded by the hash node, whether that
// node ran in this invocation or was skipped because it was already done.
func (rs *runState) manifestView(cfg string) (hashArtifact, error) {
	return readArtifact[hashArtifact](rs, "hash", cfg)
}
