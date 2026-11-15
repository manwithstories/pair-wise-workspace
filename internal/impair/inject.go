// Package impair deterministically simulates delayed, reordered, lost and
// duplicated RTP packets for local verification.
package impair

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

// Config uses fractional loss and percentage values for reorder/duplicate.
type Config struct {
	Delay          time.Duration
	MaxExtraDelay  time.Duration
	ReorderPercent float64
	Loss           float64
	DuplicatePct   float64
	Seed           int64
}

// Delivery is one packet scheduled for a particular wall-clock deadline.
type Delivery struct {
	Packet  *rtp.Packet
	Due     time.Time
	Arrival time.Time
}

// Injector is safe to use from one goroutine. It is intentionally deterministic.
type Injector struct {
	cfg        Config
	rng        *rand.Rand
	dropped    atomic.Uint64
	duplicated atomic.Uint64
}

// New creates an injector with validation.
func New(cfg Config) (*Injector, error) {
	if cfg.Delay < 0 || cfg.MaxExtraDelay < 0 {
		return nil, fmt.Errorf("delay cannot be negative")
	}
	if cfg.ReorderPercent < 0 || cfg.ReorderPercent > 100 ||
		cfg.Loss < 0 || cfg.Loss > 1 ||
		cfg.DuplicatePct < 0 || cfg.DuplicatePct > 100 {
		return nil, fmt.Errorf("impairment percentages out of range")
	}
	if cfg.MaxExtraDelay == 0 {
		cfg.MaxExtraDelay = 62 * time.Millisecond
	}
	return &Injector{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}, nil
}

// ParseSpec parses delay=20ms,reorder=5,loss=0.01,duplicate=0,seed=7.
func ParseSpec(spec string) (Config, error) {
	cfg := Config{MaxExtraDelay: 62 * time.Millisecond}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return cfg, nil
	}
	for _, field := range strings.Split(spec, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			return cfg, fmt.Errorf("invalid impair field %q", field)
		}
		switch key {
		case "delay":
			d, err := time.ParseDuration(value)
			if err != nil {
				return cfg, fmt.Errorf("delay: %w", err)
			}
			cfg.Delay = d
		case "reorder":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return cfg, fmt.Errorf("reorder: %w", err)
			}
			cfg.ReorderPercent = v
		case "loss":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return cfg, fmt.Errorf("loss: %w", err)
			}
			cfg.Loss = v
		case "duplicate":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return cfg, fmt.Errorf("duplicate: %w", err)
			}
			cfg.DuplicatePct = v
		case "seed":
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("seed: %w", err)
			}
			cfg.Seed = v
		default:
			return cfg, fmt.Errorf("unknown impair field %q", key)
		}
	}
	return cfg, nil
}

// Inject returns zero or more deliveries. Dropped and duplicated counters are
// monotonic and exposed for reporting.
func (i *Injector) Inject(packet *rtp.Packet, now time.Time) []Delivery {
	if i.rng.Float64() < i.cfg.Loss {
		i.dropped.Add(1)
		return nil
	}
	extra := time.Duration(0)
	if i.rng.Float64()*100 < i.cfg.ReorderPercent {
		extra = minDuration(i.cfg.MaxExtraDelay, 10*time.Millisecond+
			time.Duration(i.rng.Int63n(int64(i.cfg.MaxExtraDelay)+1)))
	}
	base := now.Add(i.cfg.Delay + extra)
	arrival := deterministicArrival(packet.Timestamp).Add(i.cfg.Delay + extra)
	deliveries := []Delivery{{Packet: Clone(packet), Due: base, Arrival: arrival}}
	if i.rng.Float64()*100 < i.cfg.DuplicatePct {
		i.duplicated.Add(1)
		duplicateOffset := time.Duration(i.rng.Intn(5)+1) * time.Millisecond
		deliveries = append(deliveries, Delivery{
			Packet:  Clone(packet),
			Due:     base.Add(duplicateOffset),
			Arrival: arrival.Add(duplicateOffset),
		})
	}
	return deliveries
}

// Snapshot returns deterministic counters for this injector.
func (i *Injector) Snapshot() (dropped, duplicated uint64) {
	return i.dropped.Load(), i.duplicated.Load()
}

// Clone makes a packet safe to mutate independently per egress.
func Clone(packet *rtp.Packet) *rtp.Packet {
	clone := *packet
	clone.CSRC = append([]uint32(nil), packet.CSRC...)
	clone.Extensions = append([]rtp.Extension(nil), packet.Extensions...)
	clone.Payload = append([]byte(nil), packet.Payload...)
	clone.Raw = append([]byte(nil), packet.Raw...)
	return &clone
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func deterministicArrival(timestamp uint32) time.Time {
	nanos := int64(timestamp) * int64(time.Second) / 48000
	return time.Unix(0, nanos)
}
