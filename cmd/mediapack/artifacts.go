package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
)

// Artifacts are the durable outputs of each DAG node.  Persisting them is
// what makes skipping safe: a resumed run reuses the recorded result instead
// of keeping state only in memory, and the final report is built from the
// same bytes in both paths.
type artifact[T any] struct {
	Node  string `json:"node"`
	Key   string `json:"key"`
	Value T      `json:"value"`
}

type frameStamp struct {
	Number int    `json:"number"`
	Num    int64  `json:"num"`
	Den    int64  `json:"den"`
	SHA256 string `json:"sha256"`
}

func (f frameStamp) ticks() clock.Ticks { return clock.MustNew(f.Num, f.Den) }

// framesArtifact is one representation's validated frame index, keyed by
// rate value.
type framesArtifact map[string][]frameStamp

type audioArtifact struct {
	Summary string `json:"summary"`
}

type colorArtifact struct {
	Triplet string `json:"triplet"`
}

// mp4Artifact records the round-trip match percentage per rate.
type mp4Artifact map[string]float64

type indexArtifact struct {
	KeyframeHitPct float64 `json:"keyframe_hit_pct"`
	Keyframes      int     `json:"keyframes"`
}

type segmentStamp struct {
	Ordinal  int   `json:"ordinal"`
	StartNum int64 `json:"start_num"`
	StartDen int64 `json:"start_den"`
	EndNum   int64 `json:"end_num"`
	EndDen   int64 `json:"end_den"`
}

func (s segmentStamp) start() clock.Ticks { return clock.MustNew(s.StartNum, s.StartDen) }
func (s segmentStamp) end() clock.Ticks   { return clock.MustNew(s.EndNum, s.EndDen) }

type segmentsArtifact struct {
	Segments []segmentStamp `json:"segments"`
	Summary  string         `json:"summary"`
}

func (rs *runState) artifactPath(node string) string {
	return filepath.Join(rs.opts.WorkDir, "artifacts", node+".json")
}

func writeArtifact[T any](rs *runState, node, key string, value T) error {
	path := rs.artifactPath(node)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fault.Wrap(fault.ClassInternal, node, "*", "artifact-dir-failed",
			"cannot create artifact directory", err)
	}
	data, err := json.MarshalIndent(artifact[T]{Node: node, Key: key, Value: value}, "", "  ")
	if err != nil {
		return fault.Wrap(fault.ClassInternal, node, "*", "artifact-encode-failed",
			"cannot encode artifact", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fault.Wrap(fault.ClassInternal, node, "*", "artifact-write-failed",
			"cannot write artifact", err)
	}
	return os.Rename(tmp, path)
}

// readArtifact loads a node's artifact and rejects one written for a
// different idempotency key: a stale artifact must never masquerade as the
// output of the current configuration.
func readArtifact[T any](rs *runState, node, key string) (T, error) {
	var zero T
	path := rs.artifactPath(node)
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, fault.Wrap(fault.ClassInternal, node, "*", "artifact-missing",
			fmt.Sprintf("artifact for %s is unavailable", node), err)
	}
	var a artifact[T]
	if err := json.Unmarshal(data, &a); err != nil {
		return zero, fault.Wrap(fault.ClassInternal, node, "*", "artifact-corrupt",
			"artifact is not valid JSON", err)
	}
	if a.Key != key {
		return zero, fault.New(fault.ClassInternal, node, "*", "artifact-stale",
			fmt.Sprintf("artifact for %s belongs to a different configuration", node))
	}
	return a.Value, nil
}
