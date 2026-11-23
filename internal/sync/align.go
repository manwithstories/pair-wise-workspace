// Package sync estimates audio/video drift and aligns segments with a
// bounded strategy that never silently drops out-of-tolerance units.
package sync

import (
	"math"

	"github.com/example/mediad/internal/clock"
	"github.com/example/mediad/internal/model"
)

// Config bounds the alignment strategy.
type Config struct {
	// ToleranceMicros is the maximum drift corrected by shifting units
	// on the timeline; beyond it the units are flagged, not dropped.
	ToleranceMicros clock.Micros
	// MaxShiftMicros caps the absolute correction applied to any unit.
	MaxShiftMicros clock.Micros
}

// DefaultConfig mirrors a tight live-to-post tolerance of 20 ms.
func DefaultConfig() Config {
	return Config{ToleranceMicros: 20_000, MaxShiftMicros: 20_000}
}

// Point is one timestamped observation from either track.
type Point struct {
	TrackID uint32
	Time    clock.Micros
	IsAudio bool
}

// Estimate measures the mean audio-minus-video offset across paired
// observations. Inputs must be pre-sorted by time on the unified clock.
func Estimate(video, audio []Point) (clock.Micros, error) {
	if len(video) == 0 || len(audio) == 0 {
		return 0, model.New(model.KindArgument, "sync", "estimate",
			"both video and audio points are required")
	}
	var sum int64
	pairs := 0
	for _, a := range audio {
		// Nearest preceding (or coincident) video point.
		i := nearestIndex(video, a.Time)
		sum += int64(a.Time - video[i].Time)
		pairs++
	}
	if pairs == 0 {
		return 0, model.New(model.KindArgument, "sync", "estimate",
			"no pair-able observations")
	}
	return clock.Micros(math.Round(float64(sum) / float64(pairs))), nil
}

func nearestIndex(video []Point, t clock.Micros) int {
	lo, hi := 0, len(video)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if video[mid].Time <= t {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// Decision classifies one unit after alignment.
type Decision string

const (
	DecisionAligned   Decision = "aligned"
	DecisionTolerance Decision = "out-of-tolerance"
)

// AlignedUnit is a unit plus the correction applied to it.
type AlignedUnit struct {
	TrackID     uint32
	Original    clock.Micros
	Corrected   clock.Micros
	Shift       clock.Micros
	IsAudio     bool
	Decision    Decision
	WithinTol   bool
}

// Align shifts audio units by -offset so they coincide with video. The
// correction is bounded by MaxShiftMicros; units whose required shift
// exceeds ToleranceMicros are marked out-of-tolerance and retained —
// the pipeline never silently discards media.
func Align(units []Point, offset clock.Micros, cfg Config) []AlignedUnit {
	out := make([]AlignedUnit, 0, len(units))
	for _, u := range units {
		shift := clock.Micros(0)
		if u.IsAudio {
			shift = clampShift(-offset, cfg.MaxShiftMicros)
		}
		corrected := u.Time + shift
		drift := abs64(int64(offset))
		decision := DecisionAligned
		within := drift <= int64(cfg.ToleranceMicros)
		if u.IsAudio && !within {
			decision = DecisionTolerance
		}
		out = append(out, AlignedUnit{
			TrackID:   u.TrackID,
			Original:  u.Time,
			Corrected: corrected,
			Shift:     shift,
			IsAudio:   u.IsAudio,
			Decision:  decision,
			WithinTol: within || !u.IsAudio,
		})
	}
	return out
}

// RejectOutOfTolerance returns a classified error naming the first
// unit that could not be aligned inside the tolerance. It is the
// explicit alternative to silently dropping it.
func RejectOutOfTolerance(aligned []AlignedUnit) error {
	for _, a := range aligned {
		if a.Decision == DecisionTolerance {
			return model.New(model.KindConflict, "sync", "align",
				"unit exceeds alignment tolerance; retained for manual review")
		}
	}
	return nil
}

func clampShift(shift, max clock.Micros) clock.Micros {
	if shift > max {
		return max
	}
	if shift < -max {
		return -max
	}
	return shift
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// OffsetMillis exposes the estimated drift in milliseconds for reports.
func OffsetMillis(offset clock.Micros) int64 { return int64(offset / 1000) }
