package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"mediad/internal/fixture"
)

type caseReport struct {
	Case                 string            `json:"case"`
	RoundTripPct         float64           `json:"manifest_roundtrip_pct"`
	KeyframeHitPct       float64           `json:"keyframe_hit_pct"`
	KeyframeHits         int               `json:"keyframe_hits"`
	KeyframeTotal        int               `json:"keyframe_determinable"`
	SegmentsOK           int               `json:"segments_ok"`
	SegmentsBad          int               `json:"segments_bad"`
	RecoveredChunks      int               `json:"recovered_chunks"`
	HashMatched          int               `json:"sha256_matched"`
	HashTotal            int               `json:"sha256_total"`
	TimelineGaps         int               `json:"timeline_gaps"`
	KeyframeUndetermined int               `json:"keyframe_undetermined"`
	IndexRebuilt         bool              `json:"index_rebuilt"`
	TimelineJumps        int               `json:"timeline_jumps"`
	RTPPackets           int               `json:"rtp_packets"`
	RTPMonotonic         bool              `json:"rtp_monotonic"`
	SegmentHashes        []segmentEvidence `json:"segment_hashes,omitempty"`
}

type segmentEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Role   string `json:"role"`
	Chunk  int    `json:"chunk"`
}

type demoReport struct {
	Seed  int64        `json:"seed"`
	Cases []caseReport `json:"cases"`
}

func writeReport(dir string, reports []caseReport) error {
	all := make([]caseReport, len(reports))
	copy(all, reports)
	data, err := json.MarshalIndent(demoReport{Cases: all}, "", "  ")
	if err != nil {
		return err
	}
	path, err := fixture.SafeJoin(dir, fixture.ReportName)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".report-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func loadReport(dir string) (demoReport, error) {
	path, err := fixture.SafeJoin(dir, fixture.ReportName)
	if err != nil {
		return demoReport{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return demoReport{}, fmt.Errorf("report.json missing: %w", err)
	}
	var rep demoReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return demoReport{}, fmt.Errorf("report.json malformed: %w", err)
	}
	if len(rep.Cases) == 0 {
		return demoReport{}, errors.New("report.json has no cases")
	}
	return rep, nil
}

func thresholdsPass(reports []caseReport) bool {
	for _, r := range reports {
		if r.Case != string(fixture.CaseNormal) {
			continue
		}
		if r.RoundTripPct < 99 || r.KeyframeHitPct < 99 {
			return false
		}
	}
	// Recovery must reproduce every valid segment hash exactly.
	for _, r := range reports {
		if r.HashTotal > 0 && r.HashMatched != r.HashTotal {
			return false
		}
	}
	return true
}
