package config

import (
	"errors"
	"testing"
)

func validSpec() Spec {
	return Spec{
		Version:       Version,
		Width:         320,
		Height:        180,
		FPS:           30,
		Frames:        600,
		PixelFormat:   "yuv420p",
		YUVFile:       "sample.yuv",
		TargetBPS:     1200000,
		GOP:           30,
		SegmentFrames: 60,
	}
}

func TestSpecValidate(t *testing.T) {
	s := validSpec()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.FrameBytes != 86400 || s.FileBytes != 51840000 {
		t.Fatalf("unexpected sizes: %d %d", s.FrameBytes, s.FileBytes)
	}
}

func TestSpecRejectsBoundsAndBudget(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Spec)
		want error
	}{
		{"odd-width", func(s *Spec) { s.Width = 321 }, ErrInvalidSpec},
		{"too-tall", func(s *Spec) { s.Height = 2000 }, ErrInvalidSpec},
		{"zero-fps", func(s *Spec) { s.FPS = 0 }, ErrInvalidSpec},
		{"too-many-frames", func(s *Spec) { s.Frames = MaxFrames + 1 }, ErrInvalidSpec},
		{"tiny-budget", func(s *Spec) { s.TargetBPS = 1000 }, ErrInvalidSpec},
		{"huge-frame-budget", func(s *Spec) { s.TargetBPS = MaxTargetBPS; s.FPS = 1 }, ErrBudget},
		{"bad-gop", func(s *Spec) { s.GOP = 601 }, ErrInvalidSpec},
		{"path-yuv", func(s *Spec) { s.YUVFile = "../x.yuv" }, ErrInvalidSpec},
		{"wrong-file-size", func(s *Spec) { s.FileBytes = 1 }, ErrInvalidSpec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.edit(&s)
			if err := s.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
