// Package buffer implements an adaptive jitter buffer. Target depth follows
// the observed network jitter within hard bounds and a memory budget, and a
// committed interval is immutable: a packet that arrives after its interval
// was committed is reported as late rather than allowed to rewrite history.
package buffer

import (
	"fmt"
	"sort"

	"mediarun/internal/clock"
)

// Config bounds the adaptive buffer.
type Config struct {
	// MinTargetNS and MaxTargetNS clamp the adaptive target depth.
	MinTargetNS int64
	MaxTargetNS int64
	// InitTargetNS is the starting target before observations arrive.
	InitTargetNS int64
	// MemoryBudgetBytes caps the bytes of payload held by the buffer.
	MemoryBudgetBytes int64
	// CommitHorizonNS is how far behind the newest arrival an interval must
	// be before it can be committed and frozen.
	CommitHorizonNS int64
}

// DefaultConfig: 20ms floor, 1s ceiling, 200ms initial target, 64 MiB of
// payload budget, and a 250ms commit horizon.
func DefaultConfig() Config {
	return Config{
		MinTargetNS:       20_000_000,
		MaxTargetNS:       1_000_000_000,
		InitTargetNS:      200_000_000,
		MemoryBudgetBytes: 64 << 20,
		CommitHorizonNS:   250_000_000,
	}
}

// Sample is one media observation entering the buffer.
type Sample struct {
	Track string
	NS    int64
	Bytes int64
	// Body is optional; the buffer only accounts for its size.
	Body []byte
	// Gap marks an explicit missing interval carried through the pipeline.
	Gap bool
}

// Interval is a committed time range. Once committed it cannot be rewritten.
type Interval struct {
	Track   string
	StartNS int64
	EndNS   int64
	Samples int
	Bytes   int64
	Gap     bool
	// Late reports how many samples arrived after this interval was frozen.
	Late int
}

// Result is what one Push produced.
type Result struct {
	Committed []Interval
	// Late is true when the sample arrived after its interval was frozen.
	Late bool
	// Dropped counts samples shed to respect the memory budget.
	Dropped int
}

// Buffer is an adaptive jitter buffer for a single track.
type Buffer struct {
	cfg         Config
	track       string
	received    []Sample
	newestNS    int64
	committed   []Interval
	lastEnd     int64
	jitterNS    int64
	targetNS    int64
	heldBytes   int64
	dropped     int
	lateCount   int
	started     bool
	committedNS map[int64]struct{}
}

// New creates a jitter buffer for one track.
func New(track string, cfg Config) *Buffer {
	if cfg.MinTargetNS <= 0 || cfg.MaxTargetNS <= 0 || cfg.MaxTargetNS < cfg.MinTargetNS {
		cfg = DefaultConfig()
	}
	if cfg.InitTargetNS < cfg.MinTargetNS {
		cfg.InitTargetNS = cfg.MinTargetNS
	}
	if cfg.InitTargetNS > cfg.MaxTargetNS {
		cfg.InitTargetNS = cfg.MaxTargetNS
	}
	if cfg.MemoryBudgetBytes <= 0 {
		cfg.MemoryBudgetBytes = DefaultConfig().MemoryBudgetBytes
	}
	if cfg.CommitHorizonNS < 0 {
		cfg.CommitHorizonNS = 0
	}
	return &Buffer{cfg: cfg, track: track, targetNS: cfg.InitTargetNS}
}

// TargetNS returns the current adaptive target depth.
func (b *Buffer) TargetNS() int64 { return b.targetNS }

// HeldBytes returns payload bytes currently held.
func (b *Buffer) HeldBytes() int64 { return b.heldBytes }

// LateCount returns samples observed after their interval was committed.
func (b *Buffer) LateCount() int { return b.lateCount }

// Dropped returns samples shed to respect the memory budget.
func (b *Buffer) Dropped() int { return b.dropped }

// Committed returns the immutable committed intervals.
func (b *Buffer) Committed() []Interval { return b.committed }

// Push admits a sample and commits every interval that has aged past the
// commit horizon. The jitter estimate is an exponentially weighted mean of
// the absolute arrival deviation from the nominal spacing.
func (b *Buffer) Push(s Sample) Result {
	res := Result{}
	if s.Track == "" {
		s.Track = b.track
	}
	if b.IsFrozen(s.NS) {
		// The interval covering this timestamp is already frozen; a late
		// packet may be reported but never rewrites committed output.
		b.lateCount++
		for i := range b.committed {
			if s.NS >= b.committed[i].StartNS && s.NS <= b.committed[i].EndNS {
				b.committed[i].Late++
				break
			}
		}
		res.Late = true
		return res
	}
	if !b.started {
		b.started = true
		b.newestNS = s.NS
	} else if s.NS > b.newestNS {
		// Jitter is the deviation of each inter-arrival gap from the
		// track's nominal sample spacing. Only arrivals faster than the
		// nominal spacing count: a hole is a gap to recover, not jitter.
		spacing := b.nominalSpacing()
		delta := s.NS - b.newestNS
		dev := spacing - delta
		if dev < 0 {
			dev = -dev
		}
		// EWMA with alpha = 1/8, integer arithmetic.
		b.jitterNS = b.jitterNS - b.jitterNS/8 + dev/8
		b.newestNS = s.NS
	}
	b.updateTarget()
	// Memory budget: shed the newest uncommitted sample when exceeded.
	size := s.Bytes
	if size < 0 {
		size = 0
	}
	if b.heldBytes+size > b.cfg.MemoryBudgetBytes && len(b.received) > 0 {
		b.dropped++
		res.Dropped = 1
		// Drop the oldest uncommitted sample rather than stalling.
		oldest := b.received[0]
		b.heldBytes -= oldest.Bytes
		b.received = b.received[1:]
	}
	b.received = append(b.received, s)
	b.heldBytes += size
	sort.SliceStable(b.received, func(i, j int) bool { return b.received[i].NS < b.received[j].NS })
	res.Committed = b.commitReady()
	return res
}

func (b *Buffer) committedStart() int64 {
	if len(b.committed) == 0 {
		return 0
	}
	return b.committed[len(b.committed)-1].StartNS
}

// nominalSpacing approximates the expected sample spacing from the target
// depth; it only shapes the jitter estimate, never commit decisions.
func (b *Buffer) nominalSpacing() int64 {
	n := int64(len(b.received))
	if n < 2 {
		return 0
	}
	span := b.received[n-1].NS - b.received[0].NS
	if span <= 0 {
		return 0
	}
	return span / (n - 1)
}

func (b *Buffer) updateTarget() {
	// Target tracks the jitter estimate plus a safety multiple, clamped.
	want := b.jitterNS*4 + b.cfg.MinTargetNS
	if want < b.cfg.MinTargetNS {
		want = b.cfg.MinTargetNS
	}
	if want > b.cfg.MaxTargetNS {
		want = b.cfg.MaxTargetNS
	}
	// Smooth the change so the target does not thrash.
	b.targetNS = b.targetNS - b.targetNS/4 + want/4
	if b.targetNS < b.cfg.MinTargetNS {
		b.targetNS = b.cfg.MinTargetNS
	}
	if b.targetNS > b.cfg.MaxTargetNS {
		b.targetNS = b.cfg.MaxTargetNS
	}
}

// commitReady freezes intervals whose end is at least the commit horizon
// behind the newest arrival. It never touches a frozen interval.
func (b *Buffer) commitReady() []Interval {
	if len(b.received) == 0 {
		return nil
	}
	cutoff := b.newestNS - b.cfg.CommitHorizonNS
	var out []Interval
	keep := b.received[:0]
	for _, s := range b.received {
		if s.NS > cutoff {
			keep = append(keep, s)
			continue
		}
		if len(out) > 0 && out[len(out)-1].Track == s.Track &&
			s.NS <= out[len(out)-1].EndNS {
			out[len(out)-1].Samples++
			out[len(out)-1].Bytes += s.Bytes
			if s.Gap {
				out[len(out)-1].Gap = true
			}
			continue
		}
		out = append(out, Interval{
			Track:   s.Track,
			StartNS: s.NS,
			EndNS:   s.NS,
			Samples: 1,
			Bytes:   s.Bytes,
			Gap:     s.Gap,
		})
		b.heldBytes -= s.Bytes
	}
	b.received = keep
	for _, iv := range out {
		b.appendCommitted(iv)
	}
	return out
}

// Flush commits everything still held, ignoring the horizon. Called at end of
// stream; already committed intervals are untouched.
func (b *Buffer) Flush() []Interval {
	if len(b.received) == 0 {
		return nil
	}
	var out []Interval
	for _, s := range b.received {
		if len(out) > 0 && s.NS <= out[len(out)-1].EndNS {
			out[len(out)-1].Samples++
			out[len(out)-1].Bytes += s.Bytes
			if s.Gap {
				out[len(out)-1].Gap = true
			}
			continue
		}
		out = append(out, Interval{
			Track: s.Track, StartNS: s.NS, EndNS: s.NS,
			Samples: 1, Bytes: s.Bytes, Gap: s.Gap,
		})
		b.heldBytes -= s.Bytes
	}
	b.received = nil
	for _, iv := range out {
		b.appendCommitted(iv)
	}
	return out
}

// appendCommitted records a frozen interval.
func (b *Buffer) appendCommitted(iv Interval) {
	b.committed = append(b.committed, iv)
	if iv.EndNS > b.lastEnd {
		b.lastEnd = iv.EndNS
	}
}

// CommittedRange returns the first committed start and last committed end.
func (b *Buffer) CommittedRange() (int64, int64, int, error) {
	if len(b.committed) == 0 {
		return 0, 0, 0, clock.New(clock.KindUnrecoverable, clock.CodeSyncInputShort,
			b.track, "no committed intervals")
	}
	return b.committed[0].StartNS, b.committed[len(b.committed)-1].EndNS,
		len(b.committed), nil
}

// IsFrozen reports whether ns falls inside an already committed interval.
// Intervals are contiguous per track, so a single range check suffices.
func (b *Buffer) IsFrozen(ns int64) bool {
	if len(b.committed) == 0 {
		return false
	}
	return ns >= b.committed[0].StartNS && ns <= b.lastEnd
}

// Describe renders the buffer for the terminal summary.
func (b *Buffer) Describe() string {
	return fmt.Sprintf("track=%s target_ms=%d held_bytes=%d committed=%d dropped=%d late=%d",
		b.track, b.targetNS/1_000_000, b.heldBytes, len(b.committed), b.dropped, b.lateCount)
}
