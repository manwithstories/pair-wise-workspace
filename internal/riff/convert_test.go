package riff_test

import (
	"encoding/binary"
	"math"
	"testing"

	"loudness/internal/riff"
)

func TestConvertDepths(t *testing.T) {
	tests := []struct {
		name string
		fmt  riff.Format
		raw  []byte
		want []float64
	}{
		{"pcm8", riff.Format{Tag: 1, BitsPerSample: 8}, []byte{0, 128, 255}, []float64{-1, 0, 127.0 / 128}},
		{"pcm16", riff.Format{Tag: 1, BitsPerSample: 16}, []byte{0x00, 0x80, 0xff, 0x7f}, []float64{-1, 32767.0 / 32768}},
		{"pcm24", riff.Format{Tag: 1, BitsPerSample: 24}, []byte{0x00, 0x00, 0x80, 0xff, 0xff, 0x7f}, []float64{-1, 8388607.0 / 8388608}},
		{"pcm32", riff.Format{Tag: 1, BitsPerSample: 32}, []byte{0, 0, 0, 0x80, 0xff, 0xff, 0xff, 0x7f}, []float64{-1, 2147483647.0 / 2147483648}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := make([]float64, len(tc.want))
			nans, err := riff.ConvertSamples(tc.raw, tc.fmt, dst)
			if err != nil {
				t.Fatal(err)
			}
			if nans != 0 {
				t.Fatalf("nans = %d", nans)
			}
			for i := range tc.want {
				if math.Abs(dst[i]-tc.want[i]) > 1e-12 {
					t.Fatalf("sample %d = %.12f, want %.12f", i, dst[i], tc.want[i])
				}
			}
		})
	}
}

func TestConvertFloatCountsNaNAndInfinity(t *testing.T) {
	raw := make([]byte, 16)
	binary.LittleEndian.PutUint32(raw[0:], math.Float32bits(float32(math.NaN())))
	binary.LittleEndian.PutUint32(raw[4:], math.Float32bits(float32(math.Inf(1))))
	binary.LittleEndian.PutUint32(raw[8:], math.Float32bits(0.25))
	binary.LittleEndian.PutUint32(raw[12:], math.Float32bits(-0.5))
	dst := make([]float64, 4)
	nans, err := riff.ConvertSamples(raw, riff.Format{Tag: 3, BitsPerSample: 32}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if nans != 2 {
		t.Fatalf("nans = %d, want 2", nans)
	}
	if dst[0] != 0 || dst[1] != 0 || dst[2] != 0.25 || dst[3] != -0.5 {
		t.Fatalf("converted = %v", dst)
	}
}
