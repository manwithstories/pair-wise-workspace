package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"bcast/internal/fault"
	"bcast/internal/safe"
)

const metricsRel = "state/metrics.json"

// Metrics are the terminal acceptance counters persisted across demo/resume.
type Metrics struct {
	Seed           int `json:"seed"`
	RoundtripOK    int `json:"roundtrip_ok"`
	RoundtripTotal int `json:"roundtrip_total"`
	KeyframeHits   int `json:"keyframe_hit"`
	KeyframeTotal  int `json:"keyframe_total"`
	SegmentSHA256  int `json:"segment_sha256"`
	Mismatch       int `json:"mismatch"`
	Rejected       int `json:"rejected"`
	RTPDuplicate   int `json:"rtp_duplicate"`
	Resumed        int `json:"resumed"`
	Invalidated    int `json:"invalidated"`
}

func loadMetrics(root *safe.Root) Metrics {
	b, err := root.ReadFile(metricsRel)
	if err != nil {
		return Metrics{}
	}
	var m Metrics
	if json.Unmarshal(b, &m) != nil {
		return Metrics{}
	}
	return m
}

func saveMetrics(root *safe.Root, m Metrics) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fault.E(fault.Internal, "cli", "saveMetrics.marshal", err)
	}
	p, err := root.Resolve(metricsRel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fault.E(fault.WriteFailed, "cli", "saveMetrics.mkdir", err).WithPath(metricsRel)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fault.E(fault.WriteFailed, "cli", "saveMetrics.write", err).WithPath(metricsRel)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fault.E(fault.WriteFailed, "cli", "saveMetrics.rename", err).WithPath(metricsRel)
	}
	return nil
}
