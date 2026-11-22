package audio

import (
	"fmt"
	"math"

	"mediad/internal/fault"
)

// Layout describes channel semantics for a decoded buffer.
type Layout int

const (
	LayoutMono   Layout = 1
	LayoutStereo Layout = 2
	// LayoutLFE marks 5.1 layouts; only the front L/R pair is downmixed.
	Layout5Point1 Layout = 6
)

// UpmixMonoToStereo duplicates mono to both channels with equal-power gains.
func UpmixMonoToStereo(samples []float64) []float64 {
	out := make([]float64, len(samples)*2)
	g := 1 / math.Sqrt2
	for i, v := range samples {
		out[i*2] = v * g
		out[i*2+1] = v * g
	}
	return out
}

// DownmixToStereo folds supported layouts into stereo. 5.1 uses the standard
// L + 0.707C + 0.707Ls / R + 0.707C + 0.707Rs matrix with the LFE dropped.
func DownmixToStereo(samples []float64, channels int) ([]float64, error) {
	if channels == 2 {
		return append([]float64(nil), samples...), nil
	}
	if channels == 1 {
		return UpmixMonoToStereo(samples), nil
	}
	if len(samples)%channels != 0 {
		return nil, ErrInvalidLayout
	}
	if channels < 6 {
		// Unknown intermediate layouts: take the first two channels rather
		// than inventing a matrix.
		frames := len(samples) / channels
		out := make([]float64, frames*2)
		for i := 0; i < frames; i++ {
			out[i*2] = samples[i*channels]
			out[i*2+1] = samples[i*channels+1]
		}
		return out, nil
	}
	frames := len(samples) / channels
	out := make([]float64, frames*2)
	const c = 0.70710678
	for i := 0; i < frames; i++ {
		base := i * channels
		l := samples[base]
		r := samples[base+1]
		center := samples[base+2]
		ls := samples[base+4]
		rs := samples[base+5]
		out[i*2] = l + c*center + c*ls
		out[i*2+1] = r + c*center + c*rs
	}
	return out, nil
}

// Normalize converts one source segment into the target format. Undecodable
// segments yield a Gap result with the source timeline preserved, so the
// caller can record the hole without shifting later segments.
func Normalize(src Source, target Target, seq uint64, rep string) (Result, *fault.List) {
	var faults fault.List
	loc := fault.Location{Segment: src.URI, Stream: rep, Offset: -1}
	res := Result{
		URI: src.URI, RepresentationID: rep, StartTicks: src.StartTicks,
		DurationTicks: src.DurationTicks, SampleRate: target.SampleRate, Channels: target.Channels,
	}
	if !src.Decodable || src.SampleRate <= 0 || src.Channels <= 0 {
		faults.Add(fault.MustNew(fault.KindUndecodableAudio, loc,
			fmt.Sprintf("segment is not decodable (rate=%d channels=%d)", src.SampleRate, src.Channels), true, true))
		res.Gap = true
		res.MeasuredLUFS = math.Inf(-1)
		return res, &faults
	}
	if target.SampleRate != CanonicalSampleRate {
		faults.Add(fault.MustNew(fault.KindLimitExceeded, loc,
			fmt.Sprintf("unsupported target rate %d", target.SampleRate), false, false))
		res.Gap = true
		return res, &faults
	}
	samples := src.Samples
	channels := src.Channels
	// Channel layout first, at the source rate.
	stereo, err := DownmixToStereo(samples, channels)
	if err != nil {
		faults.Add(fault.MustNew(fault.KindUndecodableAudio, loc, err.Error(), true, true))
		res.Gap = true
		return res, &faults
	}
	channels = 2
	resampled, _, err := Resample(stereo, channels, src.SampleRate, target.SampleRate)
	if err != nil {
		faults.Add(fault.MustNew(fault.KindUndecodableAudio, loc, err.Error(), true, true))
		res.Gap = true
		return res, &faults
	}
	measured := LoudnessLUFS(resampled, target.SampleRate, channels)
	peak := PeakDBFS(resampled)
	gain := GainForTarget(measured, target.LoudnessLUFS, peak, target.PeakCeilingDBFS)
	gainDB := ApplyGain(resampled, gain)
	res.Samples = resampled
	res.MeasuredLUFS = measured
	res.GainDB = gainDB
	res.PeakDBFS = PeakDBFS(resampled)
	// The emitted duration comes from the sample count, but the source
	// timeline duration is retained when present so gaps stay aligned.
	if dur := DurationTicks(resampled, target.SampleRate, channels); src.DurationTicks == 0 {
		res.DurationTicks = dur
	}
	_ = seq
	return res, &faults
}

// NormalizeAll normalises a batch and reports timeline gaps at the resulting
// canonical positions. Gap positions are derived from the source start, not
// from accumulated output, so a gap never shifts later segments.
func NormalizeAll(srcs []Source, target Target) ([]Result, []fault.Fault) {
	var results []Result
	var faults []fault.Fault
	for _, src := range srcs {
		res, fl := Normalize(src, target, 0, src.RepresentationID)
		faults = append(faults, fl.All()...)
		if res.Gap && res.DurationTicks > 0 {
			faults = append(faults, fault.MustNew(fault.KindUndecodableAudio,
				fault.Location{Segment: src.URI, Stream: src.RepresentationID, Offset: -1},
				fmt.Sprintf("timeline gap at %d for %d ticks", res.StartTicks, res.DurationTicks), true, true))
		}
		results = append(results, res)
	}
	return results, faults
}
