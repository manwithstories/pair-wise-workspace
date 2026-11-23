// Package clock normalises per-track timestamps onto one shared media clock.
// It handles counter wraparound, duplicate sequence numbers and bounded
// reordering, and stops a track when timing cannot be repaired unambiguously.
package clock

import (
	"fmt"

	"medpipe/internal/model"
)

// Track identifies which timeline a record belongs to.
type Track string

const (
	Video Track = "video"
	Audio Track = "audio"
)

// Config tunes normalisation. WrapPeriodNS is the counter modulus; zero
// disables wrap correction. MaxReorderSeq bounds how far out of order a
// record may arrive while still being matched deterministically.
type Config struct {
	WrapPeriodNS  int64
	MaxReorderSeq uint32
}

// DefaultConfig uses a 2^32 ns modulus (matching 32-bit timestamp hardware)
// and a small reorder window.
func DefaultConfig() Config {
	return Config{WrapPeriodNS: 1 << 32, MaxReorderSeq: 4}
}

// TrackState is the observable per-track outcome.
type TrackState struct {
	Stopped          bool
	Wraps            int
	Duplicates       int
	LastSeq          uint32
	LastRaw          int64
	LastNormalized   int64
	Failure          *model.LocatableError
	AffectedStartNS  int64
	AffectedEndNS    int64
	recent           map[uint32]int64
}

// Timeline keeps independent wrap counters but one shared epoch.
type Timeline struct {
	cfg    Config
	tracks map[Track]*TrackState
}

// New constructs an empty timeline.
func New(cfg Config) *Timeline {
	return &Timeline{cfg: cfg, tracks: map[Track]*TrackState{}}
}

// State returns a copy of the track state (nil when never seen).
func (tl *Timeline) State(t Track) *TrackState {
	st := tl.tracks[t]
	if st == nil {
		return nil
	}
	cp := *st
	cp.recent = nil
	return &cp
}

func (tl *Timeline) state(t Track) *TrackState {
	st := tl.tracks[t]
	if st == nil {
		st = &TrackState{recent: map[uint32]int64{}}
		tl.tracks[t] = st
	}
	return st
}

// Normalize maps rawTS plus its sequence number onto the unwrapped shared
// clock. A stopped track keeps returning its terminal failure.
func (tl *Timeline) Normalize(t Track, seq uint32, rawTS int64) (int64, error) {
	st := tl.state(t)
	if st.Stopped {
		return 0, st.Failure
	}
	// Duplicate sequence: reuse the previously resolved instant when the
	// timestamp agrees; otherwise the mapping is ambiguous.
	if norm, ok := st.recent[seq]; ok {
		if rawTS != st.LastRaw && st.LastSeq == seq {
			return 0, tl.stop(st, seq, rawTS, "duplicate seq %d carries conflicting timestamps", seq)
		}
		st.Duplicates++
		return norm, nil
	}
	if len(st.recent) == 0 {
		st.LastSeq, st.LastRaw, st.LastNormalized = seq, rawTS, rawTS
		st.recent[seq] = rawTS
		return rawTS, nil
	}
	// Out-of-order arrival inside the reorder window: the instant must
	// already be known; a brand-new seq behind the head is ambiguous.
	if seq < st.LastSeq {
		if st.LastSeq-seq > tl.cfg.MaxReorderSeq {
			return 0, tl.stop(st, seq, rawTS, "seq %d trails head %d beyond reorder window", seq, st.LastSeq)
		}
		if norm, ok := st.recent[seq]; ok {
			st.Duplicates++
			return norm, nil
		}
		return 0, tl.stop(st, seq, rawTS, "late seq %d has no resolvable timestamp", seq)
	}
	norm := tl.unwrap(st, seq, rawTS)
	if norm <= st.LastNormalized {
		return 0, tl.stop(st, seq, rawTS, "non-monotonic time at seq %d: %d <= %d", seq, norm, st.LastNormalized)
	}
	st.LastSeq, st.LastRaw, st.LastNormalized = seq, rawTS, norm
	tl.remember(st, seq, norm)
	return norm, nil
}

func (tl *Timeline) unwrap(st *TrackState, seq uint32, rawTS int64) int64 {
	period := tl.cfg.WrapPeriodNS
	if period <= 0 || rawTS >= st.LastRaw {
		return st.LastNormalized + (rawTS - st.LastRaw)
	}
	// The counter moved backwards while the sequence advanced: a wrap is the
	// only unambiguous explanation when the jump crosses half the modulus.
	if st.LastRaw-rawTS > period/2 {
		st.Wraps++
		return st.LastNormalized + (period - st.LastRaw) + rawTS
	}
	return st.LastNormalized + (rawTS - st.LastRaw)
}

func (tl *Timeline) remember(st *TrackState, seq uint32, norm int64) {
	st.recent[seq] = norm
	window := tl.cfg.MaxReorderSeq + 1
	for s := range st.recent {
		if seq-s > window {
			delete(st.recent, s)
		}
	}
}

func (tl *Timeline) stop(st *TrackState, seq uint32, rawTS int64, format string, args ...any) error {
	st.Stopped = true
	st.Failure = model.NewError(model.ErrTimeline, "%s", fmt.Sprintf(format, args...)).WithRecord(0, seq)
	st.AffectedStartNS = rawTS
	st.AffectedEndNS = st.LastNormalized
	return st.Failure
}

// Finalize closes a stopped track's affected range at the last timestamp the
// pipeline will still process, giving callers a concrete segment range.
func (tl *Timeline) Finalize(t Track, lastEndNS int64) {
	if st := tl.tracks[t]; st != nil && st.Stopped {
		st.AffectedEndNS = lastEndNS
	}
}

// AVOffset returns audio minus video in nanoseconds; callers fold it into the
// synchronisation metric. Both instants must already be normalized.
func AVOffset(videoNS, audioNS int64) int64 {
	d := audioNS - videoNS
	if d < 0 {
		return -d
	}
	return d
}
