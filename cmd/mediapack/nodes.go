package main

import (
	"fmt"
	"os"

	"mediapack/internal/audio"
	"mediapack/internal/color"
	"mediapack/internal/dag"
	"mediapack/internal/fault"
	"mediapack/internal/frames"
	"mediapack/internal/index"
	"mediapack/internal/mp4"
	"mediapack/internal/segments"
)

// buildNodes assembles the pipeline.  Node names are stable because the
// persisted state, the artifacts and the blocked-chain report all key off
// them.  Each node writes its result as an artifact before reporting
// success, so a skipped node restores exactly what it computed.
func (rs *runState) buildNodes() ([]dag.Node, error) {
	cfg := rs.configHash()
	inputs := rs.fixtureInputs()
	allDeps := []string{"frames", "audio", "color", "mp4", "index", "segments", "hash"}

	nodes := []dag.Node{
		rs.framesNode(cfg, inputs),
		rs.audioNode(cfg, inputs),
		rs.colorNode(cfg, inputs),
		rs.mp4Node(cfg, inputs),
		rs.indexNode(cfg, inputs),
		rs.segmentsNode(cfg, inputs),
		rs.hashNode(cfg, inputs),
	}
	nodes = append(nodes, dag.Node{
		Name: "manifest", Deps: allDeps, Inputs: inputs, Config: cfg,
		Run: func() (string, error) {
			data, err := rs.manifestBytes(cfg)
			if err != nil {
				return "", err
			}
			if err := writeFileAtomic(rs.manifestPath(), data); err != nil {
				return "", fault.Wrap(fault.ClassInternal, "manifest", "*", "write-failed",
					"cannot write manifest", err)
			}
			return digest(data), nil
		},
	})
	return nodes, nil
}

func (rs *runState) framesNode(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "frames", Inputs: inputs, Config: cfg, Run: func() (string, error) {
		art := framesArtifact{}
		for _, rate := range rs.bundle.Rates {
			valid, err := frames.Validate(&rate.Manifest, rate.Payloads, frames.DefaultLimits())
			if err != nil {
				return "", err
			}
			name := fmt.Sprintf("%d", rate.Value)
			for _, f := range valid {
				art[name] = append(art[name], frameStamp{Number: f.Number,
					Num: f.Ticks.Num(), Den: f.Ticks.Den(), SHA256: f.SHA256})
			}
		}
		if err := writeArtifact(rs, "frames", cfg, art); err != nil {
			return "", err
		}
		return digest(jsonBytes(art)), nil
	}}
}

func (rs *runState) audioNode(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "audio", Inputs: inputs, Config: cfg, Run: func() (string, error) {
		res, err := audio.Resolve(rs.bundle.Audio, audioConfig(rs.opts.Policies.Audio), audio.DefaultLimits())
		if err != nil {
			return "", err
		}
		art := audioArtifact{Summary: res.Summary()}
		if err := writeArtifact(rs, "audio", cfg, art); err != nil {
			return "", err
		}
		return digest(jsonBytes(art)), nil
	}}
}

func (rs *runState) colorNode(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "color", Inputs: inputs, Config: cfg, Run: func() (string, error) {
		res, err := color.Resolve(rs.bundle.Color, colorConfig(rs.opts.Policies.Color), color.DefaultLimits())
		if err != nil {
			return "", err
		}
		art := colorArtifact{Triplet: res.Triplet.String()}
		if err := writeArtifact(rs, "color", cfg, art); err != nil {
			return "", err
		}
		return digest(jsonBytes(art)), nil
	}}
}

func (rs *runState) mp4Node(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "mp4", Inputs: inputs, Config: cfg, Run: func() (string, error) {
		art := mp4Artifact{}
		for _, rate := range rs.bundle.Rates {
			data, err := readFile(rate.MP4Path)
			if err != nil {
				return "", err
			}
			rt, err := mp4.Check(data, mp4.DefaultLimits())
			if err != nil {
				return "", err
			}
			if !rt.ByteEqual || rt.MatchPct != 100 {
				return "", fault.New(fault.ClassInternal, "mp4", fmt.Sprintf("rate-%d", rate.Value),
					"roundtrip-mismatch", fmt.Sprintf("round trip match %.2f%%", rt.MatchPct))
			}
			rs.summaries[rate.Value] = rt.Summary
			art[fmt.Sprintf("%d", rate.Value)] = rt.MatchPct
		}
		if err := writeArtifact(rs, "mp4", cfg, art); err != nil {
			return "", err
		}
		return digest(jsonBytes(art)), nil
	}}
}

func (rs *runState) indexNode(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "index", Deps: []string{"mp4"}, Inputs: inputs, Config: cfg,
		Run: func() (string, error) {
			if err := rs.ensureSummaries(); err != nil {
				return "", err
			}
			art := indexArtifact{}
			for _, rate := range rs.bundle.Rates {
				tab, err := index.Build(rs.summaries[rate.Value], 1, fmt.Sprintf("%d", rate.Value), index.DefaultLimits())
				if err != nil {
					return "", err
				}
				art.Keyframes += len(tab.Keyframes)
			}
			art.KeyframeHitPct = rs.keyframeHitPct()
			if err := writeArtifact(rs, "index", cfg, art); err != nil {
				return "", err
			}
			return digest(jsonBytes(art)), nil
		}}
}

func (rs *runState) segmentsNode(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "segments", Deps: []string{"index", "audio", "color"}, Inputs: inputs, Config: cfg,
		Run: func() (string, error) {
			if err := rs.ensureSummaries(); err != nil {
				return "", err
			}
			reps, err := rs.representations()
			if err != nil {
				return "", err
			}
			plan, err := segments.Align(reps, rs.frameTicks, segments.DefaultLimits())
			if err != nil {
				return "", err
			}
			rs.plan = plan
			art := segmentsArtifact{Summary: plan.Summary()}
			for _, seg := range plan.Segments {
				art.Segments = append(art.Segments, segmentStamp{
					Ordinal: seg.Ordinal, StartNum: seg.Start.Num(), StartDen: seg.Start.Den(),
					EndNum: seg.End.Num(), EndDen: seg.End.Den()})
			}
			if err := writeArtifact(rs, "segments", cfg, art); err != nil {
				return "", err
			}
			return digest(jsonBytes(art)), nil
		}}
}

// hashNode computes the per-segment digests.  It reads the frames and
// segments from their artifacts rather than shared memory: on a resumed run
// those nodes are skipped, and their artifacts are the only record.
func (rs *runState) hashNode(cfg string, inputs []string) dag.Node {
	return dag.Node{Name: "hash", Deps: []string{"segments", "frames"}, Inputs: inputs, Config: cfg,
		Run: func() (string, error) {
			out, err := hashSegments(rs, cfg)
			if err != nil {
				return "", err
			}
			if err := writeArtifact(rs, "hash", cfg, out); err != nil {
				return "", err
			}
			return digest(jsonBytes(out)), nil
		}}
}

// ensureSummaries re-parses MP4 files only when the in-memory summaries were
// lost because the mp4 node was skipped on a resumed run.
func (rs *runState) ensureSummaries() error {
	if len(rs.summaries) > 0 {
		return nil
	}
	for _, rate := range rs.bundle.Rates {
		data, err := readFile(rate.MP4Path)
		if err != nil {
			return err
		}
		rt, err := mp4.Check(data, mp4.DefaultLimits())
		if err != nil {
			return err
		}
		rs.summaries[rate.Value] = rt.Summary
	}
	return nil
}

// representations builds the alignment inputs from the MP4 summaries.
func (rs *runState) representations() ([]segments.Representation, error) {
	if err := rs.ensureSummaries(); err != nil {
		return nil, err
	}
	var reps []segments.Representation
	for _, rate := range rs.bundle.Rates {
		summary := rs.summaries[rate.Value]
		if summary == nil {
			return nil, fault.New(fault.ClassDependency, "segments", fmt.Sprintf("rate-%d", rate.Value),
				"missing-summary", "MP4 summary is unavailable").WithBlockedBy("dag:mp4")
		}
		tab, err := index.Build(summary, 1, fmt.Sprintf("%d", rate.Value), index.DefaultLimits())
		if err != nil {
			return nil, err
		}
		frameDuration, err := tab.FrameDuration()
		if err != nil {
			return nil, err
		}
		reps = append(reps, segments.Representation{
			Name: fmt.Sprintf("%d", rate.Value), Rate: rate.Value,
			FrameDuration: frameDuration, Keyframes: tab,
		})
	}
	return reps, nil
}

func (rs *runState) manifestPath() string {
	return rs.opts.WorkDir + "/manifest.json"
}

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(fault.ClassInput, "mp4", "*", "read-failed",
			"cannot read fixture file", err)
	}
	return data, nil
}
