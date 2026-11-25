package audio

import (
	"math"

	"relaycheck/internal/media"
)

// LoudnessReport is the deterministic measurement/normalization summary.
type LoudnessReport struct {
	IntegratedLUFS float64 `json:"integrated_lufs"`
	PeakDBFS       float64 `json:"peak_dbfs"`
	AppliedGainDB  float64 `json:"applied_gain_db"`
	Limited        bool    `json:"limited"`
}

const lufsOffset = -0.691 // BS.1770-style offset for unweighted channel sum

func normalizedMeanSquare(p *PCM) (meanSquare, peak float64) {
	if p == nil || len(p.Samples) == 0 {
		return 0, 0
	}
	var sumSq float64
	var peakS float64
	for _, s := range p.Samples {
		v := float64(s) / 32768.0
		sumSq += v * v
		a := math.Abs(v)
		if a > peakS {
			peakS = a
		}
	}
	return sumSq / float64(len(p.Samples)), peakS
}

// MeasureLoudness returns an integrated-loudness estimate (LUFS) and peak dBFS.
func MeasureLoudness(p *PCM) LoudnessReport {
	ms, peak := normalizedMeanSquare(p)
	lufs := -120.0
	if ms > 1e-12 {
		lufs = lufsOffset + 10*math.Log10(ms)
	}
	peakDB := -120.0
	if peak > 0 {
		peakDB = 20 * math.Log10(peak)
	}
	return LoudnessReport{IntegratedLUFS: round2(lufs), PeakDBFS: round2(peakDB)}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// NormalizeOptions configures gain normalization and limiting.
type NormalizeOptions struct {
	TrackID            int64
	TargetLUFS         float64
	CeilingDBFS        float64
	RejectClippedInput bool
	ClippingRun        int
	Range              media.TimeRange
}

// NormalizeLoudness measures, applies a scalar gain to the target integrated
// loudness, then brickwall-limits at the ceiling so output never full-scale
// clips. With RejectClippedInput it errors on an already-clipped source.
func NormalizeLoudness(p *PCM, opts NormalizeOptions) (*PCM, LoudnessReport, *media.Fault) {
	if p == nil || len(p.Samples) == 0 {
		return nil, LoudnessReport{}, media.NewFault(media.CodeEmptyChannel, "empty pcm in loudness stage").
			WithTrack(opts.TrackID).WithRange(opts.Range.Start, opts.Range.End)
	}
	if opts.RejectClippedInput {
		if f := DetectClipping(p, opts.TrackID, opts.ClippingRun, opts.Range); f != nil {
			return nil, LoudnessReport{}, f
		}
	}

	before := MeasureLoudness(p)
	gainDB := opts.TargetLUFS - before.IntegratedLUFS
	// Silence cannot be made louder meaningfully; leave as silence.
	if before.IntegratedLUFS <= -119 {
		gainDB = 0
	}
	gain := math.Pow(10, gainDB/20)

	ceilingDB := opts.CeilingDBFS
	if ceilingDB == 0 {
		ceilingDB = -1.0
	}
	ceiling := int64(32767 * math.Pow(10, ceilingDB/20))
	if ceiling < 1 {
		ceiling = 1
	}

	out := &PCM{Rate: p.Rate, Channels: p.Channels, Samples: make([]int16, len(p.Samples))}
	limited := false
	for i, s := range p.Samples {
		v := int64(math.Round(float64(s) * gain))
		if v > ceiling {
			v = ceiling
			limited = true
		} else if v < -ceiling {
			v = -ceiling
			limited = true
		}
		out.Samples[i] = clamp16(v)
	}
	rep := MeasureLoudness(out)
	rep.AppliedGainDB = round2(gainDB)
	rep.Limited = limited
	return out, rep, nil
}
