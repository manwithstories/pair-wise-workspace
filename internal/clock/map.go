package clock

import (
	"errors"
	"fmt"
	"sort"
)

// Sample is one demuxed frame with source-tick decode and presentation times.
type Sample struct {
	// Index is the source-order frame index within its track.
	Index int
	DTS   int64
	PTS   int64
}

// MappedSample is a sample in output timescale.
type MappedSample struct {
	Index    int
	DTS      int64
	PTS      int64
	Duration int64
	// Offset is PTS-DTS; negative values require a version 1 ctts box.
	Offset int64
}

// Stats records the decisions taken while mapping one track.
type Stats struct {
	// Origin is the source-time (output ticks) of the earliest DTS.
	Origin int64
	// NegativeStart reports that Origin was below zero.
	NegativeStart bool
	// Normalized reports that timestamps were shifted by -Origin.
	Normalized bool
	// DuplicateDTS counts samples whose decode delta was zero.
	DuplicateDTS int
	// PTSRegression counts decreases of PTS in source order.
	PTSRegression int
	// NegativeOffsets counts samples with PTS < DTS.
	NegativeOffsets int
	// Rounded counts conversions that were not exact.
	Rounded int
}

// Mapping is the deterministic result of mapping one track.
type Mapping struct {
	Timescale int64
	Shift     int64
	Origin    int64
	Samples   []MappedSample
	Stats     Stats
	// SourceOrder preserves the input indexes in decode order.
	SourceOrder []int
}

type working struct {
	dts, pts int64
	index    int
}

// MapSamples converts and orders samples into MP4 decode order.
//
// The mapping is intentionally explicit about the awkward cases:
//   - duplicate DTS values get the default duration (or zero) and are counted;
//   - PTS that moves backwards in source order is counted but preserved;
//   - PTS < DTS is preserved as a negative composition offset;
//   - non-integral tick conversions are rounded half away from zero;
//   - all DTS values are shifted so the earliest DTS is zero, and the shift is
//     reported so the muxer can emit an empty edit when the origin was positive.
func MapSamples(tb Timebase, timescale int64, samples []Sample, defaultDurationNS int64) (Mapping, error) {
	if timescale <= 0 {
		return Mapping{}, ErrInvalidTimescale
	}
	if defaultDurationNS < 0 {
		return Mapping{}, errors.New("clock: negative default duration")
	}
	m := Mapping{
		Timescale:   timescale,
		Samples:     make([]MappedSample, len(samples)),
		SourceOrder: make([]int, len(samples)),
	}
	ws := make([]working, len(samples))
	var minDTS int64
	var haveMin bool
	for i, s := range samples {
		dts, err := tb.TicksToTimescale(s.DTS, timescale)
		if err != nil {
			return Mapping{}, fmt.Errorf("sample %d dts: %w", i, err)
		}
		pts, err := tb.TicksToTimescale(s.PTS, timescale)
		if err != nil {
			return Mapping{}, fmt.Errorf("sample %d pts: %w", i, err)
		}
		if !exact(tb, s.DTS, timescale) || !exact(tb, s.PTS, timescale) {
			m.Stats.Rounded++
		}
		if i > 0 && s.PTS < samples[i-1].PTS {
			m.Stats.PTSRegression++
		}
		ws[i] = working{dts: dts, pts: pts, index: s.Index}
		if !haveMin || dts < minDTS {
			minDTS, haveMin = dts, true
		}
	}
	m.Origin = minDTS
	m.Shift = -minDTS
	if minDTS < 0 {
		m.Stats.NegativeStart = true
	}
	if minDTS != 0 {
		m.Stats.Normalized = true
	}

	// Stable sort by DTS, then by source index for byte-for-byte determinism.
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].dts != ws[j].dts {
			return ws[i].dts < ws[j].dts
		}
		return ws[i].index < ws[j].index
	})

	defaultDur, err := ticksFromNS(defaultDurationNS, timescale)
	if err != nil {
		return Mapping{}, err
	}
	for i := range ws {
		ws[i].dts += m.Shift
		ws[i].pts += m.Shift
	}
	for i := range ws {
		dur := defaultDur
		if i+1 < len(ws) {
			if d := ws[i+1].dts - ws[i].dts; d > 0 {
				dur = d
			} else {
				m.Stats.DuplicateDTS++
			}
		} else if dur == 0 {
			dur = medianDelta(ws)
		}
		off := ws[i].pts - ws[i].dts
		if off < 0 {
			m.Stats.NegativeOffsets++
		}
		m.Samples[i] = MappedSample{
			Index:    ws[i].index,
			DTS:      ws[i].dts,
			PTS:      ws[i].pts,
			Duration: dur,
			Offset:   off,
		}
		m.SourceOrder[i] = ws[i].index
	}
	return m, nil
}

func exact(tb Timebase, ticks, timescale int64) bool {
	nanos, err := tb.Nanos(ticks)
	if err != nil {
		return false
	}
	return (nanos*timescale)%NanosPerSecond == 0
}

func ticksFromNS(nanos, timescale int64) (int64, error) {
	if nanos == 0 {
		return 0, nil
	}
	if timescale <= 0 {
		return 0, ErrInvalidTimescale
	}
	return NanosToTicks(nanos, timescale), nil
}

func medianDelta(ws []working) int64 {
	var deltas []int64
	for i := 1; i < len(ws); i++ {
		if d := ws[i].dts - ws[i-1].dts; d > 0 {
			deltas = append(deltas, d)
		}
	}
	if len(deltas) == 0 {
		return 0
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	return deltas[len(deltas)/2]
}

// Duration returns the media duration: the sum of sample durations.
func (m Mapping) Duration() int64 {
	var d int64
	for _, s := range m.Samples {
		d += s.Duration
	}
	return d
}
