package pipeline

import (
	"mediasim/internal/clock"
	"mediasim/internal/fixture"
)

// Config selects the fixture, transport faults and output directory.
type Config struct {
	Fixture    string
	Seed       int64
	OutDir     string
	LossBurst  int   // packets dropped per burst window
	BurstEvery int   // burst window size in packets
	JitterMs   int   // jitter buffer target delay
	JitterMax  int64 // deterministic jitter amplitude in ms
	Window     int   // RTP reorder window in packets
	MaxPackets int   // jitter buffer cap
	Gates      Gates
}

// Defaults fills zero values with the acceptance-contract defaults.
func (c *Config) Defaults() {
	if c.Window < 0 {
		return // leave it invalid; Validate reports it
	}
	if c.Window == 0 {
		c.Window = 16
	}
	if c.MaxPackets < 0 {
		return
	}
	if c.MaxPackets == 0 {
		c.MaxPackets = 64
	}
	if c.BurstEvery <= 0 {
		c.BurstEvery = 17
	}
	if c.Fixture == "" {
		c.Fixture = "normal"
	}
	if c.OutDir == "" {
		c.OutDir = "./artifacts"
	}
	if c.Gates == (Gates{}) {
		c.Gates = DefaultGates()
	}
}

// Validate rejects impossible parameters before any file is written.
func (c Config) Validate() error {
	if c.LossBurst < 0 || c.JitterMs < 0 || c.Window <= 0 || c.MaxPackets <= 0 || c.BurstEvery <= 0 {
		return clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone,
			"invalid transport parameters: loss=%d jitter=%d window=%d max=%d every=%d",
			c.LossBurst, c.JitterMs, c.Window, c.MaxPackets, c.BurstEvery)
	}
	if c.Window > 4096 || c.MaxPackets > 4096 {
		return clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone, "window/max packets exceed 4096")
	}
	if _, err := fixture.PresetSpec(c.Fixture); err != nil {
		return clock.Errorf(clock.CatArgument, clock.Span{}, clock.RecoveredNone, "%v", err)
	}
	return nil
}
