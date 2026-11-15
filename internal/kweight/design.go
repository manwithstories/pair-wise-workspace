// Package kweight implements the two-stage K-weighting filter from
// ITU-R BS.1770. Coefficients are derived for the actual sample rate.
package kweight

import (
	"errors"
	"fmt"
	"math"
	"math/cmplx"
)

const (
	minRate = 32000
	maxRate = 192000
)

var ErrUnsupportedSampleRate = errors.New("unsupported_sample_rate")

// Biquad coefficients use the normalized form:
// H(z) = (b0 + b1*z^-1 + b2*z^-2) / (1 + a1*z^-1 + a2*z^-2).
type Coefficients struct {
	B0, B1, B2 float64
	A1, A2     float64
}

type Design struct {
	Shelf    Coefficients
	HighPass Coefficients
}

// ForRate derives the shelf and high-pass stages for fs. Rates outside
// 32 kHz through 192 kHz are rejected as required by the delivery spec.
func ForRate(fs int) (Design, error) {
	if fs < minRate || fs > maxRate {
		return Design{}, fmt.Errorf("%w: %d Hz", ErrUnsupportedSampleRate, fs)
	}
	// ITU-R BS.1770-4 shelf stage parameters.
	const (
		shelfF0 = 1681.974450955533
		shelfG  = 3.999843853973347
		shelfQ  = 0.7071752369554196
		hpF0    = 38.13547087602444
		hpQ     = 0.5003270373238773
	)

	k := math.Tan(math.Pi * shelfF0 / float64(fs))
	vh := math.Pow(10, shelfG/20)
	vb := math.Pow(vh, 0.4996667741545416)
	a0 := 1 + k/shelfQ + k*k
	shelf := Coefficients{
		B0: (vh + vb*k/shelfQ + k*k) / a0,
		B1: 2 * (k*k - vh) / a0,
		B2: (vh - vb*k/shelfQ + k*k) / a0,
		A1: 2 * (k*k - 1) / a0,
		A2: (1 - k/shelfQ + k*k) / a0,
	}

	// The high-pass numerator is intentionally not divided by a0. This is
	// the BS.1770 definition and gives the standard +0.7 dB gain near 1 kHz.
	k = math.Tan(math.Pi * hpF0 / float64(fs))
	a0 = 1 + k/hpQ + k*k
	highPass := Coefficients{
		B0: 1,
		B1: -2,
		B2: 1,
		A1: 2 * (k*k - 1) / a0,
		A2: (1 - k/hpQ + k*k) / a0,
	}

	d := Design{Shelf: shelf, HighPass: highPass}
	if err := d.Validate(); err != nil {
		return Design{}, err
	}
	return d, nil
}

// Validate checks coefficient finiteness and that both biquad sections have
// all poles strictly inside the unit circle.
func (d Design) Validate() error {
	if err := validateStage(d.Shelf); err != nil {
		return fmt.Errorf("shelf: %w", err)
	}
	if err := validateStage(d.HighPass); err != nil {
		return fmt.Errorf("high-pass: %w", err)
	}
	return nil
}

func validateStage(c Coefficients) error {
	values := []float64{c.B0, c.B1, c.B2, c.A1, c.A2}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("non-finite coefficient")
		}
	}
	// Roots of z^2 + a1*z + a2.
	disc := cmplx.Sqrt(complex(c.A1*c.A1-4*c.A2, 0))
	p1 := (-complex(c.A1, 0) + disc) / 2
	p2 := (-complex(c.A1, 0) - disc) / 2
	if cmplx.Abs(p1) >= 1 || cmplx.Abs(p2) >= 1 {
		return fmt.Errorf("unstable poles %.9g, %.9g", cmplx.Abs(p1), cmplx.Abs(p2))
	}
	return nil
}

// MagnitudeAt returns the linear K-weighting magnitude at freq Hz. It is
// used by fixture calibration and tests.
func (d Design) MagnitudeAt(fs int, freq float64) float64 {
	z := cmplx.Rect(1, -2*math.Pi*freq/float64(fs))
	return cmplx.Abs(stageResponse(d.Shelf, z) * stageResponse(d.HighPass, z))
}

func stageResponse(c Coefficients, z complex128) complex128 {
	return (complex(c.B0, 0) + complex(c.B1, 0)*z + complex(c.B2, 0)*z*z) /
		(complex(1, 0) + complex(c.A1, 0)*z + complex(c.A2, 0)*z*z)
}
