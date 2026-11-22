package pipeline

import (
	"fmt"
	"strings"
	"time"

	"example.com/medialab/archive/internal/checkpoint"
	"example.com/medialab/archive/internal/codec"
	"example.com/medialab/archive/internal/ingest"
	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/sample"
)

// Damage names a fault profile.
type Damage string

const (
	DamageNone      Damage = ""
	DamageMixed     Damage = "mixed"
	DamageBurstLoss Damage = "burst-loss"
)

// Config is the frozen run configuration. Every field that can change output
// is part of the checkpoint config digest.
type Config struct {
	Seed             int64
	DurationUS       int64
	SegmentUS        int64
	LateWindowUS     int64
	Bitrates         string
	Damage           Damage
	LateAtBoundary   bool
	InterruptAfterUS int64 // 0 disables interruption
	Resume           bool
	MaxFillUS        int64
	WorkDir          string
}

// DefaultConfig returns the demo defaults.
func DefaultConfig() Config {
	return Config{
		Seed:         42,
		DurationUS:   120 * 1_000_000,
		SegmentUS:    10 * 1_000_000,
		LateWindowUS: 2 * 1_000_000,
		Bitrates:     "64k,128k",
		MaxFillUS:    3 * 1_000_000,
		WorkDir:      ".work",
	}
}

// Digest freezes the configuration into the checkpoint identity.
func (c Config) Digest(reps []model.Representation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "seed=%d;dur=%d;seg=%d;window=%d;fill=%d;damage=%s;boundary=%t",
		c.Seed, c.DurationUS, c.SegmentUS, c.LateWindowUS, c.MaxFillUS, c.Damage, c.LateAtBoundary)
	for _, r := range reps {
		fmt.Fprintf(&b, ";rep=%s:%d:%s:%d", r.ID, r.Bitrate, r.Codec, r.FrameDurationUS)
	}
	return checkpoint.ConfigDigest(b.String())
}

// Validate checks the run parameters.
func (c Config) Validate() error {
	if c.DurationUS <= 0 {
		return fmt.Errorf("duration must be positive")
	}
	if c.SegmentUS <= 0 || c.SegmentUS > c.DurationUS {
		return fmt.Errorf("segment %d must be in (0, %d]", c.SegmentUS, c.DurationUS)
	}
	if c.LateWindowUS < 0 || c.LateWindowUS > c.SegmentUS {
		return fmt.Errorf("late window %d must be in [0, %d]", c.LateWindowUS, c.SegmentUS)
	}
	if c.InterruptAfterUS < 0 || c.InterruptAfterUS >= c.DurationUS {
		return fmt.Errorf("interrupt-after %d must be in [0, %d)", c.InterruptAfterUS, c.DurationUS)
	}
	if c.Seed <= 0 {
		return fmt.Errorf("seed must be positive")
	}
	if c.MaxFillUS <= 0 {
		return fmt.Errorf("max-fill must be positive")
	}
	if c.WorkDir == "" {
		return fmt.Errorf("work directory is required")
	}
	switch c.Damage {
	case DamageNone, DamageMixed, DamageBurstLoss:
	default:
		return fmt.Errorf("unknown damage profile %q", c.Damage)
	}
	return nil
}

// streamSpec is one representation's generated stream plus its ingest config.
type streamSpec struct {
	rep  model.Representation
	opts sample.Opts
	cfg  ingest.StreamConfig
}

// streams derives one synthetic stream per ladder rung. The 128k rung is AAC
// and the rest are Opus; every rung is parsed independently.
func streams(c Config, reps []model.Representation) ([]*streamSpec, error) {
	var out []*streamSpec
	for _, rep := range reps {
		var opt sample.Opts
		var cfg ingest.StreamConfig
		switch rep.Bitrate {
		case 128000:
			opt = sample.AACStream(c.Seed+1, c.DurationUS, c.SegmentUS, c.LateWindowUS)
			rep.Codec = model.CodecAAC
			rep.FrameDurationUS = 21333
			cfg = ingest.StreamConfig{ID: opt.Stream, Rep: rep.ID, Codec: model.CodecAAC,
				ClockRate: codec.AACRate, AAC: codec.DefaultAACConfig()}
		default:
			opt = sample.OpusStream(c.Seed+int64(rep.Bitrate), c.DurationUS, c.SegmentUS, c.LateWindowUS)
			opt.Stream = model.StreamID(string(rep.ID) + "-opus")
			rep.Codec = model.CodecOpus
			rep.FrameDurationUS = 20000
			cfg = ingest.StreamConfig{ID: opt.Stream, Rep: rep.ID, Codec: model.CodecOpus,
				ClockRate: codec.OpusRate}
		}
		opt.Rep = rep.ID
		out = append(out, &streamSpec{rep: rep, opts: opt, cfg: cfg})
	}
	return out, nil
}

// ParseDuration parses Go-style durations such as 120s or 2s into microseconds.
func ParseDuration(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	us := d.Microseconds()
	if us <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return us, nil
}
