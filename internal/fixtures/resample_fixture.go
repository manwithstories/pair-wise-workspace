package fixtures

import (
	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// rationalFor returns 1/rate as an exact time base.
func rationalFor(rate int) media.Rational {
	return media.Rational{Num: 1, Den: int64(rate)}
}

// ResampleSpec describes a single PCM asset used by resampling tests.
type ResampleSpec struct {
	Rate    int
	Layout  astiav.ChannelLayout
	Start   int64
	Samples int
	Seed    int64
	Path    string
}

// BuildResampleAsset writes one PCM-only MOV asset and returns its path.
func BuildResampleAsset(spec ResampleSpec) (string, error) {
	p := pcmWrite{
		Rate:     spec.Rate,
		Start:    spec.Start,
		Samples:  spec.Samples,
		Channels: spec.Layout.Channels(),
		Layout:   spec.Layout,
		Generator: func(channel, index int) float64 {
			// Distinct tones per channel so a bad fold is visible.
			return 0.2 * sineAt(spec.Seed+int64(channel), float64(index)/float64(spec.Rate))
		},
	}
	if err := writeAudioOnlyMOV(spec.Path, p, rationalFor(spec.Rate)); err != nil {
		return "", err
	}
	return spec.Path, nil
}
