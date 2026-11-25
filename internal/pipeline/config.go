package pipeline

import (
	"errors"
	"fmt"
	"time"

	"avcheck/internal/ebml"
	"avcheck/internal/media"
	"avcheck/internal/report"
	"avcheck/internal/timeline"
	"avcheck/internal/transport"
)

// Config is the full run configuration.
type Config struct {
	Seed        int64
	TargetBps   int64
	MemoryBytes int64
	Limits      ebml.Limits
	Transport   transport.Config
	Sync        timeline.Options
	// Link is the simulated cellular impairment profile. The runner reseeds
	// it per track so tracks do not share a loss pattern.
	Link transport.LinkConfig
	// Deadline bounds wall-clock processing. Zero uses DefaultDeadline.
	Deadline time.Duration
}

// HardMaxDeadline is the absolute upper bound on a run's wall-clock budget.
const HardMaxDeadline = 5 * time.Minute

// DefaultDeadline is the processing time limit used when none is given.
const DefaultDeadline = 30 * time.Second

// DefaultConfig returns the CLI defaults.
func DefaultConfig() Config {
	return Config{
		TargetBps:   900_000,
		MemoryBytes: transport.HardMaxBufferBytes,
		Limits:      ebml.DefaultLimits(),
		Transport:   transport.DefaultConfig(),
		Sync:        timeline.Options{},
		Link: transport.LinkConfig{
			GroupSize: 16,
			JitterNs:  15_000_000,
		},
	}
}

// Validate checks every configurable bound before work begins. The memory
// budget may only lower the transport queue cap, never raise it above the
// 8 MiB hard ceiling.
func (c Config) Validate() error {
	if c.TargetBps <= 0 || c.TargetBps > 1_000_000_000 {
		return fmt.Errorf("pipeline: target bps %d outside (0,1e9]", c.TargetBps)
	}
	if c.MemoryBytes <= 0 || c.MemoryBytes > transport.HardMaxBufferBytes {
		return fmt.Errorf("pipeline: memory budget %d outside (0,%d]",
			c.MemoryBytes, transport.HardMaxBufferBytes)
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	if err := c.Transport.Validate(); err != nil {
		return err
	}
	if c.Link.JitterNs < 0 || c.Link.JitterNs > transport.HardMaxLatencyNs {
		return fmt.Errorf("pipeline: link jitter %d outside [0,%d]", c.Link.JitterNs, transport.HardMaxLatencyNs)
	}
	if c.Deadline < 0 || c.Deadline > HardMaxDeadline {
		return fmt.Errorf("pipeline: deadline %v outside [0,%v]", c.Deadline, HardMaxDeadline)
	}
	return nil
}

// WithMemoryBudget returns cfg with the transport queue cap tightened to the
// memory budget.
func (c Config) WithMemoryBudget() Config {
	if c.MemoryBytes > 0 && c.MemoryBytes < c.Transport.MaxBufferBytes {
		c.Transport.MaxBufferBytes = c.MemoryBytes
	}
	return c
}

// ErrCanceled is returned when the context ends before commit.
var ErrCanceled = errors.New("pipeline: run canceled")

// RunResult is the committed outcome.
type RunResult struct {
	Manifest     *report.Manifest
	Faults       *media.Faults
	Committed    bool
	ManifestPath string
	// OutputBytes is the container payload analysed.
	OutputBytes int64
}

// Runner executes one analysis of one document.
type Runner struct {
	cfg   Config
	fault *media.Faults
}

// New creates a runner.
func New(cfg Config) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.WithMemoryBudget()
	return &Runner{cfg: cfg, fault: &media.Faults{}}, nil
}
