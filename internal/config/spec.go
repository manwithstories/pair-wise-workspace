// Package config defines the immutable job specification shared by the
// fixture generator, scheduler and reporting layers.
package config

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
)

const (
	Version          = 1
	MinDimension     = 16
	MaxWidth         = 1920
	MaxHeight        = 1080
	MinFPS           = 1
	MaxFPS           = 120
	MaxFrames        = 108000
	MinTargetBPS     = 64000
	MaxTargetBPS     = 100000000
	MinGOP           = 1
	MaxGOP           = 600
	MaxSegmentFrames = 36000
	MinFrameBits     = 128
	MaxFrameBits     = 32 * 1024 * 1024
)

var (
	ErrInvalidSpec = errors.New("invalid spec")
	ErrBudget      = errors.New("frame budget out of range")
)

// Spec is both the fixture manifest and the submitted transcode job. Optional
// manifest fields have conservative defaults when omitted.
type Spec struct {
	Version       int    `json:"version"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	Frames        int    `json:"frames"`
	PixelFormat   string `json:"pix_fmt"`
	Scene         string `json:"scene,omitempty"`
	Seed          int64  `json:"seed,omitempty"`
	YUVFile       string `json:"yuv_file"`
	FrameBytes    int64  `json:"frame_bytes"`
	FileBytes     int64  `json:"file_bytes"`
	SHA256        string `json:"sha256,omitempty"`
	TargetBPS     int64  `json:"target_bps"`
	GOP           int    `json:"gop"`
	SegmentFrames int    `json:"segment_frames"`
}

// Defaults fills fields that a fixture manifest commonly omits. Explicit CLI
// flags are applied by the caller after loading the manifest.
func (s *Spec) Defaults() {
	if s.Version == 0 {
		s.Version = Version
	}
	if s.PixelFormat == "" {
		s.PixelFormat = "yuv420p"
	}
	if s.TargetBPS == 0 {
		s.TargetBPS = 1200000
	}
	if s.GOP == 0 {
		s.GOP = 30
	}
	if s.SegmentFrames == 0 {
		s.SegmentFrames = 60
	}
}

// ExpectedFrameBytes returns the exact byte count of one 8-bit YUV420p frame.
func ExpectedFrameBytes(width, height int) int64 {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return 0
	}
	return int64(width) * int64(height) * 3 / 2
}

// Validate checks all hard bounds, manifest size arithmetic, and the per-frame
// budget. It deliberately does not inspect the filesystem.
func (s *Spec) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidSpec, s.Version)
	}
	if s.Width < MinDimension || s.Width > MaxWidth || s.Width%2 != 0 {
		return fmt.Errorf("%w: width must be even in [%d,%d]", ErrInvalidSpec, MinDimension, MaxWidth)
	}
	if s.Height < MinDimension || s.Height > MaxHeight || s.Height%2 != 0 {
		return fmt.Errorf("%w: height must be even in [%d,%d]", ErrInvalidSpec, MinDimension, MaxHeight)
	}
	if s.FPS < MinFPS || s.FPS > MaxFPS {
		return fmt.Errorf("%w: fps must be in [%d,%d]", ErrInvalidSpec, MinFPS, MaxFPS)
	}
	if s.Frames < 1 || s.Frames > MaxFrames {
		return fmt.Errorf("%w: frames must be in [1,%d]", ErrInvalidSpec, MaxFrames)
	}
	if s.TargetBPS < MinTargetBPS || s.TargetBPS > MaxTargetBPS {
		return fmt.Errorf("%w: target_bps must be in [%d,%d]", ErrInvalidSpec, MinTargetBPS, MaxTargetBPS)
	}
	frameBits := float64(s.TargetBPS) / float64(s.FPS)
	if frameBits < MinFrameBits || frameBits > MaxFrameBits {
		return fmt.Errorf("%w: %.1f bits/frame is outside [%d,%d]", ErrBudget, frameBits, MinFrameBits, MaxFrameBits)
	}
	if s.GOP < MinGOP || s.GOP > MaxGOP || s.GOP > s.Frames {
		return fmt.Errorf("%w: gop must be in [1,%d] and not exceed frames", ErrInvalidSpec, min(MaxGOP, s.Frames))
	}
	if s.SegmentFrames < 1 || s.SegmentFrames > MaxSegmentFrames || s.SegmentFrames > s.Frames {
		return fmt.Errorf("%w: segment_frames must be in [1,%d] and not exceed frames", ErrInvalidSpec, min(MaxSegmentFrames, s.Frames))
	}
	if s.PixelFormat != "yuv420p" {
		return fmt.Errorf("%w: only yuv420p is supported", ErrInvalidSpec)
	}
	if s.YUVFile == "" || s.YUVFile != filepath.Base(s.YUVFile) || strings.ContainsAny(s.YUVFile, `/\\`) {
		return fmt.Errorf("%w: yuv_file must be a plain basename", ErrInvalidSpec)
	}
	expectedFrame := ExpectedFrameBytes(s.Width, s.Height)
	expectedFile := expectedFrame * int64(s.Frames)
	if s.FrameBytes != 0 && s.FrameBytes != expectedFrame {
		return fmt.Errorf("%w: frame_bytes=%d, want %d", ErrInvalidSpec, s.FrameBytes, expectedFrame)
	}
	if s.FileBytes != 0 && s.FileBytes != expectedFile {
		return fmt.Errorf("%w: file_bytes=%d, want %d", ErrInvalidSpec, s.FileBytes, expectedFile)
	}
	s.FrameBytes = expectedFrame
	s.FileBytes = expectedFile
	return nil
}

// Duration is the media duration represented by the stream.
func (s *Spec) Duration() time.Duration {
	if s.FPS <= 0 {
		return 0
	}
	return time.Duration(math.Round(float64(s.Frames) / float64(s.FPS) * float64(time.Second)))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
