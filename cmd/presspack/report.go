package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"presspack/internal/core"
	"presspack/internal/ingest"
)

// CameraReport captures one camera's delivery outcome for the audit trail.
type CameraReport struct {
	Name               string
	Delivered          bool
	Error              string `json:",omitempty"`
	DurationTicks      int64
	Faults             []core.Fault
	Records            []ingest.RecoveryRecord
	SegmentsPerVariant int
	MaxAlignMs         int64
	SubtitleFitMs      int64
}

// Report is the persisted result of one pipeline run (report.json).
type Report struct {
	Seed         int
	Seconds      int
	SegmentMs    int64
	Targets      []int
	Cameras      []CameraReport
	DeliveryHash string
}

func (r *Report) recoveredSpans() int {
	n := 0
	for _, c := range r.Cameras {
		n += len(c.Records)
	}
	return n
}

func (r *Report) delivered() []CameraReport {
	var out []CameraReport
	for _, c := range r.Cameras {
		if c.Delivered {
			out = append(out, c)
		}
	}
	return out
}

func (r *Report) partial() bool {
	return len(r.delivered()) != len(r.Cameras)
}

func writeReport(dir string, r *Report) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".report.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "report.json"))
}

func readReport(dir string) (*Report, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
