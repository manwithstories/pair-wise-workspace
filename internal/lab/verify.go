// Package lab orchestrates the full avlab verification: fixture generation,
// manifest round trips, fragment indexing and hashing, the RTP impairment
// engine and A/V drift measurement, all driven by one seed.
package lab

import (
	"context"
	"fmt"

	"avlab/internal/fixture"
	"avlab/internal/manifest"
	"avlab/internal/media"
	"avlab/internal/transport"
)

// Thresholds are the acceptance gates printed by the CLI.
type Thresholds struct {
	DriftP95MS float64
}

// DefaultThresholds mirrors the documented acceptance contract.
var DefaultThresholds = Thresholds{DriftP95MS: 20}

// Config is the CLI-facing run configuration.
type Config struct {
	Seed       uint64
	Workers    int
	Queue      int
	Packets    uint64
	Repeat     int
	Thresholds Thresholds
}

// Metrics is the deterministic scorecard compared against thresholds.
type Metrics struct {
	RoundtripEqual    string // e.g. "2/2"
	KeyframeHit       string // e.g. "8/8"
	SegmentSHA256Match string // e.g. "8/8"
	DamagedRejected   string // e.g. "4/4"
	BurstRecovered    string // e.g. "16/20"
	DeadlineSkip      uint64
	AVDriftP95MS      float64
	MaxInflight       int
	QueuePeak         int
	RepeatHashEqual   bool
	CancelWorkers     int
	Failures          []string
}

// Run executes the full verification pipeline and returns its metrics.
func Run(ctx context.Context, cfg Config) (*Metrics, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	bundle, err := fixture.Generate(cfg.Seed)
	if err != nil {
		return nil, err
	}
	m := &Metrics{}

	m.RoundtripEqual = checkRoundtrips(bundle)
	m.KeyframeHit, m.SegmentSHA256Match, m.MaxInflight, m.QueuePeak, err =
		indexAndHash(ctx, bundle, cfg.Workers, cfg.Queue)
	if err != nil {
		return nil, err
	}
	m.DamagedRejected = checkDamaged(bundle)

	trace := transport.NewTrace(cfg.Seed, cfg.Packets)
	eng := transport.NewEngine(ctx, transport.EngineConfig{
		Packets: cfg.Packets, Capacity: 512,
		WaitTick: 20, PayloadLen: 24,
	}, trace)
	res, err := eng.Run()
	if err != nil {
		return nil, err
	}
	m.BurstRecovered = fmt.Sprintf("%d/%d",
		res.Jitter.BurstRecovered, res.Trace.BurstDrops)
	m.DeadlineSkip = res.Jitter.DeadlineSkips

	m.AVDriftP95MS = measureDrift(cfg.Seed)
	m.RepeatHashEqual = verifyRepeats(ctx, cfg, bundle)
	m.CancelWorkers = probeCancellation(ctx, cfg.Workers, cfg.Queue)

	evaluate(m, cfg.Thresholds)
	return m, nil
}

func validateConfig(cfg Config) error {
	if cfg.Workers <= 0 || cfg.Workers > media.MaxWorkers {
		return media.NewFault(media.CodeInvalidArgument, false,
			fmt.Sprintf("workers must be in [1,%d]", media.MaxWorkers))
	}
	if cfg.Queue <= 0 || cfg.Queue > media.MaxQueue {
		return media.NewFault(media.CodeInvalidArgument, false,
			fmt.Sprintf("queue must be in [1,%d]", media.MaxQueue))
	}
	if cfg.Packets == 0 || cfg.Repeat <= 0 {
		return media.NewFault(media.CodeInvalidArgument, false,
			"packets and repeat must be positive")
	}
	if cfg.Thresholds.DriftP95MS <= 0 {
		return media.NewFault(media.CodeInvalidArgument, false,
			"drift limit must be positive")
	}
	return nil
}

// checkRoundtrips renders and reparses both manifests.
func checkRoundtrips(b *fixture.Bundle) string {
	ok := 0
	if raw, err := manifest.RenderDASH(b.DASH); err == nil {
		if got, err := manifest.ParseDASH(raw); err == nil &&
			manifest.Compare(b.DASH, got) == nil {
			ok++
		}
	}
	if raw, err := manifest.RenderHLS(b.HLS); err == nil {
		if got, err := manifest.ParseHLS(raw); err == nil &&
			manifest.Compare(b.HLS, got) == nil {
			ok++
		}
	}
	return fmt.Sprintf("%d/2", ok)
}
