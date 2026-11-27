// Package sync aligns audio and video tracks on the shared rational time
// base. It never fabricates samples: when a pair is out of tolerance the
// alignment is marked unsynchronised and reported, not silently corrected.
package sync

import (
	"sort"

	"pairwise/ingestcheck/internal/clock"
)

// Pair is one audio/video correspondence expressed in each track's native
// ticks. Wall is the shared wall-clock tick assigned by the timeline.
type Pair struct {
	Index    int
	Video    int64
	VideoRt  clock.Rate
	Audio    int64
	AudioRt  clock.Rate
	Wall     int64
	WallRt   clock.Rate
	Fragment uint32
	Track    uint32
}

// Config sets the tolerance used to decide whether a pair is synchronised.
type Config struct {
	ToleranceMs int64 // hard limit; beyond this the pair is unsynchronised
	TargetMs    int64 // soft target reported to the caller
	MaxPairs    int   // bound on retained samples
}

// DefaultConfig targets <=20 ms of p95 skew and rejects >50 ms pairs.
func DefaultConfig() Config { return Config{ToleranceMs: 50, TargetMs: 20, MaxPairs: 4096} }

// Sample is one evaluated alignment.
type Sample struct {
	Pair   Pair
	SkewMs float64
	InSync bool
	Epoch  int
}

// Result summarises an alignment run.
type Result struct {
	Samples        []Sample
	P95Ms          float64
	MaxMs          float64
	Unsynchronized int
	Evaluated      int
	Explicit       int
}

// Aligner consumes pairs and produces the skew statistics.
type Aligner struct {
	cfg      Config
	samples  []Sample
	explicit int
}

// New validates bounds and returns an aligner.
func New(cfg Config) (*Aligner, error) {
	if cfg.ToleranceMs <= 0 || cfg.TargetMs <= 0 || cfg.TargetMs > cfg.ToleranceMs {
		return nil, clock.New(clock.CodeArgument, "sync", "invalid tolerance")
	}
	if cfg.MaxPairs <= 0 {
		return nil, clock.New(clock.CodeArgument, "sync", "invalid pair bound")
	}
	return &Aligner{cfg: cfg}, nil
}

// Evaluate converts both tracks to microseconds and records the absolute
// skew. A pair beyond ToleranceMs is recorded as unsynchronised rather than
// corrected: no sample is ever fabricated to hide it.
func (a *Aligner) Evaluate(p Pair) error {
	if len(a.samples) >= a.cfg.MaxPairs {
		return clock.New(clock.CodeSyncOverLimit, "sync", "pair count exceeds bound")
	}
	vm, err := p.VideoRt.Micros(p.Video)
	if err != nil {
		return clock.Wrap(clock.CodeInternal, "sync.video", -1, err)
	}
	am, err := p.AudioRt.Micros(p.Audio)
	if err != nil {
		return clock.Wrap(clock.CodeInternal, "sync.audio", -1, err)
	}
	diff := vm - am
	if diff < 0 {
		diff = -diff
	}
	skewMs := float64(diff) / 1000.0
	inSync := float64(a.cfg.ToleranceMs) >= skewMs
	a.samples = append(a.samples, Sample{Pair: p, SkewMs: skewMs, InSync: inSync})
	return nil
}

// MarkExplicit records a pair that could not be aligned because a track had
// an explicit gap. Such pairs contribute to the unsynchronised count but are
// excluded from the p95 so fabricated points cannot flatter the result.
func (a *Aligner) MarkExplicit() { a.explicit++ }

// Finish computes the p95 and maximum skew, with deterministic tie-breaking.
func (a *Aligner) Finish() Result {
	sorted := make([]Sample, len(a.samples))
	copy(sorted, a.samples)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SkewMs == sorted[j].SkewMs {
			return sorted[i].Pair.Index < sorted[j].Pair.Index
		}
		return sorted[i].SkewMs < sorted[j].SkewMs
	})
	var max float64
	for _, s := range sorted {
		if s.SkewMs > max {
			max = s.SkewMs
		}
	}
	var p95 float64
	if len(sorted) > 0 {
		idx := (len(sorted) - 1) * 95 / 100
		p95 = sorted[idx].SkewMs
	}
	unsync := 0
	for _, s := range sorted {
		if !s.InSync {
			unsync++
		}
	}
	return Result{
		Samples:        a.samples,
		P95Ms:          p95,
		MaxMs:          max,
		Unsynchronized: unsync,
		Evaluated:      len(a.samples),
		Explicit:       a.explicit,
	}
}
