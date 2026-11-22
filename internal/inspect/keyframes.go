package inspect

import (
	"fmt"

	"mediacheck/internal/model"
)

// Sample is one normalized media sample on the unified ms timeline.
type Sample struct {
	Track   string
	StartMs int64
	DurMs   int64
	IsSync  bool // keyframe / random-access point
	Size    int
	Corrupt bool // generator-flagged damaged sample
}

// IntervalViolation describes a keyframe-spacing breach with the
// boundary samples immediately before and after the offending span.
type IntervalViolation struct {
	Track    string `json:"track"`
	StartMs  int64  `json:"start_ms"`
	EndMs    int64  `json:"end_ms"`
	GapMs    int64  `json:"gap_ms"`
	PrevSync int64  `json:"prev_sync_ms"`
	NextSync int64  `json:"next_sync_ms"`
	LimitMs  int64  `json:"limit_ms"`
}

// TrackReport holds per-track inspection findings.
type TrackReport struct {
	Track        string              `json:"track"`
	SampleCount  int                 `json:"sample_count"`
	DurationMs   int64               `json:"duration_ms"`
	Violations   []IntervalViolation `json:"keyframe_violations"`
	NegativeDur  int                 `json:"negative_duration_count"`
	MissingSync  bool                `json:"missing_sync"`
	CorruptSpans []Span              `json:"corrupt_spans"`
}

// Span is a half-open millisecond range.
type Span struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
}

// KeyframePolicy configures spacing checks.
type KeyframePolicy struct {
	MaxIntervalMs int64 // maximum allowed sync-to-sync distance
}

// DefaultKeyframePolicy mirrors common broadcast 2 s GOP limits.
func DefaultKeyframePolicy() KeyframePolicy {
	return KeyframePolicy{MaxIntervalMs: 2500}
}

// AnalyzeTrack validates sample durations, corrupt spans and keyframe
// spacing. Samples must arrive in timeline order per track.
func AnalyzeTrack(samples []Sample, policy KeyframePolicy) TrackReport {
	rep := TrackReport{Track: "", Violations: []IntervalViolation{}}
	if len(samples) == 0 {
		return rep
	}
	rep.Track = samples[0].Track
	rep.SampleCount = len(samples)

	var lastSync int64
	haveSync := false
	var corrupt *Span

	flushCorrupt := func(end int64) {
		if corrupt != nil {
			corrupt.EndMs = end
			rep.CorruptSpans = append(rep.CorruptSpans, *corrupt)
			corrupt = nil
		}
	}

	for _, s := range samples {
		if s.Track != rep.Track {
			continue
		}
		rep.DurationMs = s.StartMs + s.DurMs
		if s.DurMs < 0 {
			rep.NegativeDur++
		}
		if s.Corrupt {
			if corrupt == nil {
				corrupt = &Span{StartMs: s.StartMs, EndMs: s.StartMs + s.DurMs}
			} else {
				corrupt.EndMs = s.StartMs + s.DurMs
			}
		} else {
			flushCorrupt(s.StartMs)
		}
		if !s.IsSync {
			continue
		}
		if !haveSync {
			haveSync = true
			lastSync = s.StartMs
			continue
		}
		gap := s.StartMs - lastSync
		if gap > policy.MaxIntervalMs {
			rep.Violations = append(rep.Violations, IntervalViolation{
				Track:    rep.Track,
				StartMs:  lastSync,
				EndMs:    s.StartMs,
				GapMs:    gap,
				PrevSync: lastSync,
				NextSync: s.StartMs,
				LimitMs:  policy.MaxIntervalMs,
			})
		}
		lastSync = s.StartMs
	}
	flushCorrupt(rep.DurationMs)

	if !haveSync {
		rep.MissingSync = true
	}
	return rep
}

// Faults converts a track report into model faults with stable IDs.
func TrackFaults(rep TrackReport, idFn func() int) []model.Fault {
	out := make([]model.Fault, 0, len(rep.Violations)+len(rep.CorruptSpans)+1)
	for _, v := range rep.Violations {
		out = append(out, model.Fault{
			ID: idFn(), Kind: model.KindKeyframeGap, Severity: model.SeverityError,
			Track: rep.Track, StartMs: v.StartMs, EndMs: v.EndMs,
			Detail:   fmt.Sprintf("keyframe gap %dms > %dms", v.GapMs, v.LimitMs),
			Recovery: model.RecPending,
		})
	}
	for _, sp := range rep.CorruptSpans {
		out = append(out, model.Fault{
			ID: idFn(), Kind: model.KindCorruptSample, Severity: model.SeverityError,
			Track: rep.Track, StartMs: sp.StartMs, EndMs: sp.EndMs,
			Detail:   "one or more corrupt samples",
			Recovery: model.RecPending,
		})
	}
	if rep.MissingSync {
		out = append(out, model.Fault{
			ID: idFn(), Kind: model.KindMissingKeyframe, Severity: model.SeverityError,
			Track: rep.Track, StartMs: 0, EndMs: rep.DurationMs,
			Detail:   "track contains no sync sample",
			Recovery: model.RecPending,
		})
	}
	return out
}
