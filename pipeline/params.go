package pipeline

import (
	"fmt"
	"sort"
	"strings"
)

// Damage names a fault to inject into the generated batch.
type Damage string

const (
	DamageTruncate     Damage = "truncate"
	DamageBrokenBox    Damage = "broken-box"
	DamageMissingIndex Damage = "missing-index"
)

// AllDamage lists the supported fault names for help text and validation.
var AllDamage = []Damage{DamageTruncate, DamageBrokenBox, DamageMissingIndex}

// ParseDamage parses a comma-separated fault list. An empty string means no
// injected damage; unknown names are an error rather than silently ignored.
func ParseDamage(s string) ([]Damage, error) {
	var out []Damage
	seen := map[Damage]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "none" {
			continue
		}
		d := Damage(part)
		valid := false
		for _, a := range AllDamage {
			if d == a {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("pipeline: unknown damage %q", part)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// Params configure one run. Defaults come from DefaultParams; the CLI only
// overrides what the operator sets.
type Params struct {
	Seed       int64
	Segments   int // target number of segments
	Damage     []Damage
	TargetDur  int64 // segment target duration, video timescale units
	WindowSize int64 // silence window, audio timescale units
	Threshold  float64
	OutDir     string
	Workers    int
	Retries    int
}

// DefaultParams returns the demo defaults used by the acceptance command.
func DefaultParams() Params {
	return Params{
		Seed:       17,
		Segments:   12,
		TargetDur:  90000, // 1s at the 90 kHz video timescale
		WindowSize: 4800,  // 0.1s at the 48 kHz audio timescale
		Threshold:  500,
		Workers:    4,
		Retries:    2,
	}
}

// Signature is a stable digest of the parameters that affect results. It
// seeds every node's idempotency key, so changing a parameter re-runs the
// affected work instead of reusing stale output.
func (p Params) Signature() string {
	ds := make([]string, len(p.Damage))
	for i, d := range p.Damage {
		ds[i] = string(d)
	}
	sort.Strings(ds)
	return fmt.Sprintf("seed=%d;segments=%d;target=%d;window=%d;thr=%.3f;damage=%s",
		p.Seed, p.Segments, p.TargetDur, p.WindowSize, p.Threshold, strings.Join(ds, "+"))
}
