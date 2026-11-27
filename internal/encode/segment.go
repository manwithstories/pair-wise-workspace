package encode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fashionweek/mediad/internal/media"
)

// SegmentRequest encodes one closed [Start, Start+Duration) interval as a
// single fMP4 segment plus its initialization section.
type SegmentRequest struct {
	Input        string
	WorkDir      string // scratch directory for this one encode
	StartSec     float64
	DurationSec  float64
	Width        int
	Height       int
	VideoBitrate string
	AudioBitrate string
}

// SegmentOutput points at the freshly encoded init section and media segment.
type SegmentOutput struct {
	Init    string
	Segment string
}

// EncodeSegment transcodes one interval deterministically. Output-side seeking
// (-ss after -i) decodes from the source and resets the clock so the first
// encoded frame is forced to an IDR, guaranteeing every segment starts on a
// keyframe regardless of the source GOP layout.
func (r *Runner) EncodeSegment(ctx context.Context, req SegmentRequest) (*SegmentOutput, error) {
	if err := os.MkdirAll(req.WorkDir, 0o755); err != nil {
		return nil, media.New(media.PhaseEncode, media.KindTool, req.WorkDir, -1,
			"cannot create segment work dir").Wrap(err)
	}
	pattern := req.WorkDir + "/seg-%03d.m4s"
	playlist := req.WorkDir + "/.seg.m3u8"
	// Fixed-precision timestamps keep the argument vector (and therefore the
	// encoder output) byte-stable across repeated and resumed runs.
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", req.Input,
		"-ss", fmt.Sprintf("%.6f", req.StartSec),
		"-t", fmt.Sprintf("%.6f", req.DurationSec),
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "medium",
		"-b:v", req.VideoBitrate, "-pix_fmt", "yuv420p",
		"-g", "240", "-keyint_min", "240", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,0)",
		"-c:a", "aac", "-b:a", req.AudioBitrate, "-ar", "48000", "-ac", "2",
		"-fflags", "+bitexact", "-flags", "+bitexact",
		"-map_metadata", "-1",
		"-f", "hls", "-hls_time", "10", "-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", pattern,
		playlist,
	}
	if err := r.run(ctx, args, req.Input); err != nil {
		return nil, err
	}
	segs, err := listSegments(req.WorkDir)
	if err != nil {
		return nil, err
	}
	if len(segs) != 1 {
		return nil, media.New(media.PhaseEncode, media.KindBounds, req.Input, -1,
			fmt.Sprintf("expected exactly one segment, got %d", len(segs)))
	}
	init := filepath.Join(req.WorkDir, "init.mp4")
	if _, err := os.Stat(init); err != nil {
		return nil, media.New(media.PhaseEncode, media.KindTruncated, init, -1,
			"init section missing after segment encode").Wrap(err)
	}
	return &SegmentOutput{Init: init, Segment: segs[0]}, nil
}

// VersionLine returns just the compact "ffmpeg x.y" token for reports.
func (r *Runner) VersionLine() string {
	parts := strings.SplitN(r.version, " ", 3)
	if len(parts) >= 2 {
		return strings.Join(parts[:2], " ")
	}
	return r.version
}
