// Package audit independently re-scans repaired outputs, replays RTP
// recovery, probes digest invalidation and checks random seek points
// against input sample boundaries. It does not trust the run report.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"archivectl/internal/clock"
	"archivectl/internal/index"
	"archivectl/internal/layout"
	"archivectl/internal/pipeline"
	"archivectl/internal/repair"
	"archivectl/internal/scan"
)

// Thresholds from the acceptance contract.
const (
	MaxAVSyncMS   = 1.0
	MinLossRecov  = 0.90
	MaxSegmentP95 = 40.0
	SeekCheckCount = 24
)

// Metrics is one clean/damaged verification result group.
type Metrics struct {
	Group              string   `json:"group"`
	AVSyncMaxMS        float64  `json:"av_sync_max_ms"`
	LossRecovery       float64  `json:"loss_recovery"`
	SegmentP95MS       float64  `json:"segment_p95_ms"`
	IndexSHA256        string   `json:"index_sha256"`
	RepeatMatch        bool     `json:"repeat_match"`
	DigestInvalidated  bool     `json:"digest_invalidated"`
	UnrecoveredGaps    int      `json:"unrecovered_gaps"`
	UnrecoveredPackets int      `json:"unrecovered_gap_packets,omitempty"`
	SeekChecks         int      `json:"seek_checks"`
	PayloadMatch       bool     `json:"payload_match"`
	InputDigestMatch   bool     `json:"input_digest_match"`
	StructuralFaults   int      `json:"structural_faults_detected"`
	MetadataConflicts  int      `json:"metadata_conflicts"`
	Pass               bool     `json:"pass"`
	FailReasons        []string `json:"fail_reasons,omitempty"`
}

// Verify independently checks one group. The returned Metrics is
// populated even when the returned error signals failure.
func Verify(root, group string, repeat int) (*Metrics, error) {
	m := &Metrics{Group: group, PayloadMatch: true}
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	saved, err := index.Load(layout.IndexPath(root, group))
	if err != nil {
		return m, fmt.Errorf("audit: load index: %w", err)
	}
	if d, err := saved.Digest(); err == nil {
		m.IndexSHA256 = d
	}
	m.MetadataConflicts = len(saved.MetadataConflicts)

	// Repeat the deterministic index build and require every digest to
	// match each other and the saved index.
	if repeat < 2 {
		repeat = 2
	}
	digests := make([]string, 0, repeat)
	var analysis *pipeline.Analysis
	for k := 0; k < repeat; k++ {
		idx, a, err := pipeline.BuildIndex(root, group)
		if err != nil {
			return m, err
		}
		d, err := idx.Digest()
		if err != nil {
			return m, err
		}
		digests = append(digests, d)
		analysis = a
	}
	allMatch := true
	for _, d := range digests {
		if d != digests[0] {
			allMatch = false
		}
	}
	savedDigest, _ := saved.Digest()
	m.RepeatMatch = allMatch && digests[0] == savedDigest
	if !m.RepeatMatch {
		fail("repeated index digests do not match saved index")
	}
	m.InputDigestMatch = analysis != nil && analysis.InputDigest == saved.InputSHA256
	if !m.InputDigestMatch {
		fail("input SHA-256 in index does not match a fresh tree digest")
	}

	// Digest invalidation probe on a disposable copy.
	inv, err := probeDigestInvalidation(root, group, saved.InputSHA256)
	if err != nil {
		fail("digest invalidation probe error: %v", err)
	}
	m.DigestInvalidated = inv
	if !m.DigestInvalidated {
		fail("input mutation did not invalidate digest")
	}

	// Independent RTP replay.
	rtpRep, err := replayRTP(root, group)
	if err != nil {
		fail("rtp replay: %v", err)
	} else {
		m.LossRecovery = rtpRep.LossRecovery
		m.UnrecoveredGaps = len(rtpRep.UnrecoveredGaps)
		for _, g := range rtpRep.UnrecoveredGaps {
			m.UnrecoveredPackets += g.PacketCount
		}
		if len(saved.RTP) > 0 {
			if saved.RTP[0].Recovered != rtpRep.Recovered || saved.RTP[0].TotalLost != rtpRep.TotalLost {
				fail("index RTP summary disagrees with independent replay")
			}
		}
		switch group {
		case "damaged":
			if m.UnrecoveredGaps != 1 {
				fail("damaged profile must archive exactly 1 unrecovered gap, got %d", m.UnrecoveredGaps)
			}
		default:
			if m.UnrecoveredGaps != 0 {
				fail("clean profile must have no unrecovered gaps, got %d", m.UnrecoveredGaps)
			}
		}
	}

	// Re-scan outputs for AV sync, p95 and random seek boundaries.
	syncWorst, p95, checks, payloadOK, err := verifyChunks(root, group, saved)
	if err != nil {
		return m, err
	}
	m.AVSyncMaxMS = round3(syncWorst)
	m.SegmentP95MS = round3(p95)
	m.SeekChecks = checks
	m.PayloadMatch = payloadOK
	if !payloadOK {
		fail("a seek boundary disagreed with the input sample or payload")
	}

	// Structural fixture detection.
	if n := checkStructuralFixture(root, group); n > 0 {
		m.StructuralFaults = n
	}
	if group == "damaged" && m.StructuralFaults == 0 {
		fail("truncated trun fixture was not detected as structural damage")
	}
	if group == "damaged" && m.MetadataConflicts == 0 {
		fail("conflicting metadata fixture was not recorded")
	}
	if group == "clean" && m.MetadataConflicts != 0 {
		fail("clean profile unexpectedly has metadata conflicts")
	}

	// Threshold evaluation.
	if m.AVSyncMaxMS > MaxAVSyncMS {
		fail("av_sync_max_ms=%.4f > %.1f", m.AVSyncMaxMS, MaxAVSyncMS)
	}
	if m.LossRecovery+1e-9 < MinLossRecov {
		fail("loss_recovery=%.4f < %.2f", m.LossRecovery, MinLossRecov)
	}
	if m.SegmentP95MS > MaxSegmentP95 {
		fail("segment_p95_ms=%.3f > %d", m.SegmentP95MS, MaxSegmentP95)
	}
	if !shaRE(m.IndexSHA256) {
		fail("index_sha256 is not 64 hex chars")
	}

	if len(problems) > 0 {
		m.Pass = false
		m.FailReasons = problems
		return m, errors.New(strings.Join(problems, "; "))
	}
	m.Pass = true
	return m, nil
}

func replayRTP(root, group string) (*repair.RTPReport, error) {
	f, err := os.Open(layout.CapturePath(root, group))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pkts, err := repair.ParseCapture(f)
	if err != nil {
		return nil, err
	}
	var groups []repair.FECGroup
	if b, err := os.ReadFile(layout.FECPath(root, group)); err == nil {
		if err := json.Unmarshal(b, &groups); err != nil {
			return nil, err
		}
	}
	return repair.RepairRTP(pkts, groups), nil
}

func checkStructuralFixture(root, group string) int {
	p := layout.StructuralPath(root, group)
	rep, err := scan.ScanFile(p, "structural")
	if err != nil || rep == nil || rep.Record == nil {
		return 0
	}
	for _, f := range rep.Record.Faults {
		if f.Code == "trun_truncated" {
			return 1
		}
	}
	return 0
}

// rationalMS converts native ticks to milliseconds exactly enough for
// the threshold comparison.
func rationalMS(base int64, timescale uint32) float64 {
	return clock.Timebase{Timescale: timescale}.Millis(base)
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }
