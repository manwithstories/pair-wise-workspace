// Package fixtures generates deterministic, damaged MPEG-TS samples and truth.
package fixtures

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tsaudit/internal/ts"
)

// Config controls deterministic fixture generation.
type Config struct {
	Out         string
	Truth       string
	Seed        int64
	Programs    int
	PATVersions int
	PMTVersions int
	Drop        int
	CCJump      int
	CRCErrors   int
	DurationMS  int
}

// TableTruth is the expected PAT/PMT version ledger.
type TableTruth struct {
	VersionChanges int `json:"version_changes"`
}

// PMTTruth is expected per-program PMT versioning.
type PMTTruth struct {
	Program        int `json:"program"`
	VersionChanges int `json:"version_changes"`
}

// ContinuityTruth is the expected continuity result after injections.
type ContinuityTruth struct {
	LossPackets int `json:"loss_packets"`
	CCJumps     int `json:"cc_jumps"`
	Duplicates  int `json:"duplicates"`
}

// OrphanPMTTruth identifies a dangling PAT reference.
type OrphanPMTTruth struct {
	Program uint16 `json:"program"`
	PID     uint16 `json:"pid"`
}

// Truth is the compact verification contract for a generated fixture.
type Truth struct {
	Version      int              `json:"version"`
	Seed         int64            `json:"seed"`
	Programs     int              `json:"programs"`
	DurationMS   int              `json:"duration_ms"`
	PacketsTotal int64            `json:"packets_total"`
	PAT          TableTruth       `json:"pat"`
	PMT          []PMTTruth       `json:"pmt"`
	Continuity   ContinuityTruth  `json:"continuity"`
	CRCErrors    int              `json:"crc_errors"`
	OrphanPMTs   []OrphanPMTTruth `json:"orphan_pmts"`
	Injected     InjectionTruth   `json:"injected"`
}

// InjectionTruth records the requested fault counts.
type InjectionTruth struct {
	Drop        int `json:"drop"`
	CCJump      int `json:"cc_jump"`
	CRCErrors   int `json:"crc_errors"`
	PATVersions int `json:"pat_versions"`
	PMTVersions int `json:"pmt_versions"`
}

// Generate writes the TS and truth files using go-astits packet serialization.
func Generate(cfg Config) (*Truth, error) {
	if cfg.Programs < 1 {
		return nil, fmt.Errorf("programs must be >= 1")
	}
	if cfg.PATVersions < 1 || cfg.PMTVersions < 1 {
		return nil, fmt.Errorf("table versions must be >= 1")
	}
	if cfg.DurationMS <= 0 {
		cfg.DurationMS = 12000
	}
	specs, truth, dropped, jumps := buildSpecs(cfg)
	if dropped != cfg.Drop {
		return nil, fmt.Errorf("cannot inject %d drops: only %d packets are available", cfg.Drop, dropped)
	}
	if jumps != cfg.CCJump {
		return nil, fmt.Errorf("cannot inject %d CC jumps: only %d positions are available", cfg.CCJump, jumps)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Out), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(cfg.Out)
	if err != nil {
		return nil, fmt.Errorf("create fixture: %w", err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	w := ts.NewWriter(bw)
	for i := range specs {
		s := &specs[i]
		if err := w.Write(s.PID, s.CC, s.PUSI, s.Payload, s.Adapt); err != nil {
			f.Close()
			return nil, fmt.Errorf("write packet %d: %w", i, err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	truth.PacketsTotal = int64(len(specs))
	if cfg.Truth != "" {
		if err := writeTruth(cfg.Truth, truth); err != nil {
			return nil, err
		}
	}
	return truth, nil
}

func writeTruth(path string, truth *Truth) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
