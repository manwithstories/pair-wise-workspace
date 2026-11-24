package dsp

import (
	"fmt"

	"mediad/internal/pcm"
)

// Canonical 5.1 channel order used by generated fixtures:
// 0=L, 1=R, 2=C, 3=LFE, 4=Ls, 5=Rs.
const (
	ChL = iota
	ChR
	ChC
	ChLFE
	ChLs
	ChRs
)

// StereoMatrix is an output-channel by input-channel coefficient matrix.
type StereoMatrix [2][]float64

// Explicit unity-row-L1 matrices. Keeping each row's absolute coefficient sum
// at or below one prevents the matrix itself from creating clipping.
var (
	MatrixMono = StereoMatrix{
		{1.0}, // L = M
		{1.0}, // R = M
	}
	MatrixStereo = StereoMatrix{
		{1, 0},
		{0, 1},
	}
	Matrix51 = StereoMatrix{
		// L = .500 L + .250 C + .125 LFE + .125 Ls
		{0.500, 0.000, 0.250, 0.125, 0.125, 0.000},
		// R = .500 R + .250 C + .125 LFE + .125 Rs
		{0.000, 0.500, 0.250, 0.125, 0.000, 0.125},
	}
)

// MatrixFor returns the declared matrix for 1, 2, or 6 input channels.
func MatrixFor(channels int) (StereoMatrix, error) {
	switch channels {
	case 1:
		return MatrixMono, nil
	case 2:
		return MatrixStereo, nil
	case 6:
		return Matrix51, nil
	default:
		return StereoMatrix{}, fmt.Errorf("unsupported channel layout %d", channels)
	}
}

// NormalizeLayout converts mono, stereo, or SMPTE-order 5.1 audio to stereo.
func NormalizeLayout(in *pcm.Audio) (*pcm.Audio, error) {
	if in == nil {
		return nil, fmt.Errorf("nil audio")
	}
	matrix, err := MatrixFor(in.Channels)
	if err != nil {
		return nil, err
	}
	for outCh, row := range matrix {
		if len(row) != in.Channels {
			return nil, fmt.Errorf("matrix row %d width %d != channels %d", outCh, len(row), in.Channels)
		}
	}

	out, err := pcm.NewAudio(in.SampleRate, 2, in.Frames)
	if err != nil {
		return nil, err
	}
	for frame := 0; frame < in.Frames; frame++ {
		for outCh := 0; outCh < 2; outCh++ {
			v := 0.0
			for inCh, g := range matrix[outCh] {
				v += g * in.Samples[frame*in.Channels+inCh]
			}
			out.Samples[frame*2+outCh] = v
		}
	}
	return out, nil
}
