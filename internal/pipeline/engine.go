// Package pipeline wires transport recovery, CMAF parsing, the shared media
// clock, A/V sync, the resumable DAG and the checksum manifest into one
// deterministic run. The CLI is a thin adapter over Build/Run/Verify.
package pipeline

import (
	"errors"
	"fmt"

	"relayctl/internal/cmaf"
	"relayctl/internal/clock"
	"relayctl/internal/dag"
	"relayctl/internal/fixture"
	"relayctl/internal/sync"
	"relayctl/internal/transport"
)

// Node names used in DAG keys, journal records and blocking reports.
const (
	NodeIngest  = "ingest"
	NodeRecover = "recover"
	NodeParse   = "parse"
	NodeSync    = "sync"
	NodePack    = "pack"
	NodeVerify  = "verify"
)

// Config controls one pipeline run.
type Config struct {
	InputDir   string
	StateDir   string
	PauseAfter string
	// Damaged selects the truncated stream (true) vs the recoverable one.
	Damaged bool
}

// Status is the machine-readable result rendered by the CLI.
type Status struct {
	Recovered      int    `json:"recovered"`
	Unrecoverable  int    `json:"unrecoverable"`
	SyncOffsetMS   int64  `json:"sync_offset_ms"`
	SyncConfidence string `json:"sync_confidence"`
	ResumeMS       int64  `json:"resume_ms"`
	SHA256         string `json:"sha256"`
	RepeatEqual    bool   `json:"repeat_equal"`
	DamageExpected int64  `json:"damage_expected_offset,omitempty"`
	DamageFound    int64  `json:"damage_found_offset,omitempty"`
	DamageDelta    int64  `json:"damage_delta,omitempty"`
	FirstBlocked   string `json:"first_blocked,omitempty"`
	ExitCode       int    `json:"-"`
}

// Sentinel errors mapped to process exit codes by the CLI.
var (
	ErrDamaged       = errors.New("pipeline: damaged input located")
	ErrUnrecoverable = errors.New("pipeline: unrecoverable packet loss")
	ErrBlocked       = errors.New("pipeline: packaging blocked")
)

// Engine holds the loaded scenario and shared subsystems for one input.
type Engine struct {
	scenario *fixture.Scenario
	cfg      Config
	mc       *clock.MediaClock
	journal  *dag.Journal

	// node-shared state
	jitter         *transport.JitterBuffer
	recovery       recoveryState
	fragments      []*cmaf.Fragment
	syncResult     sync.Result
	damageOff      int64
	manifestDigest string
}

// recoveryState is the recover node's output, consumed by parse/pack.
type recoveryState struct {
	Recovered     int
	Unrecoverable []uint16
	Present       map[uint16]bool
}

// NewEngine loads a scenario manifest and journal from disk.
func NewEngine(cfg Config) (*Engine, error) {
	sc, err := LoadScenario(cfg.InputDir)
	if err != nil {
		return nil, err
	}
	j, err := dag.OpenJournal(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	mc := clock.New()
	for _, tr := range sc.Manifest.Tracks {
		if err := mc.AddTrack(clock.TrackConfig{
			TrackID: tr.ID, Timescale: tr.Timescale, Kind: string(tr.Kind),
		}); err != nil {
			return nil, err
		}
	}
	return &Engine{scenario: sc, cfg: cfg, mc: mc, journal: j}, nil
}

// stream returns the fragment bytes this run operates on.
func (e *Engine) stream() []byte {
	if e.cfg.Damaged && e.scenario.DamagedCMAF != nil {
		return e.scenario.DamagedCMAF
	}
	return e.scenario.CMAF
}

// parseFragments runs the CMAF parser; a BoxError becomes the damage status
// without ever silently repairing the bytes.
func (e *Engine) parseFragments() ([]*cmaf.Fragment, int64, error) {
	frags, _, err := cmaf.ParseFragmentStream(e.stream())
	if err != nil {
		off, ok := cmaf.DamageOffset(err)
		if !ok {
			return nil, 0, fmt.Errorf("pipeline: parse failure without offset: %w", err)
		}
		return nil, off, fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	return frags, 0, nil
}

var _ = transport.DefaultBufferCap
var _ = sync.DefaultThresholdMS
