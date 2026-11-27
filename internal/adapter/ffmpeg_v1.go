package adapter

import (
	"context"
	"os"
	"os/exec"
	"strconv"

	"avcore/internal/clock"
)

// FFmpegV1 is the first-generation ffmpeg encode adapter.
type FFmpegV1 struct {
	binary string
}

// NewFFmpegV1 locates ffmpeg on PATH (or via an explicit binary path).
func NewFFmpegV1(binary string) (*FFmpegV1, error) {
	if binary == "" {
		binary = "ffmpeg"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, NewError(CodeEncodeFailed, "ffmpeg not found: "+err.Error(), Source{})
	}
	return &FFmpegV1{binary: path}, nil
}

func (e *FFmpegV1) AdapterVersion() string { return Version }

// Encode runs one transcode producing a fragmented-MP4 HLS segment. The
// encoder writes to req.Output directly; callers arrange temp-file staging
// and the atomic hash-verified commit (see the state package), keeping the
// adapter free of journal policy. All time arguments derive from the integer
// 90 kHz base and are converted to seconds only at the tool boundary.
func (e *FFmpegV1) Encode(ctx context.Context, req EncodeRequest) (*EncodeResult, error) {
	if err := ValidateInput(req.Input); err != nil {
		return nil, err
	}
	if req.Output == "" || req.TargetBPS <= 0 || req.DurationTicks <= 0 {
		return nil, NewError(CodeBadArgument, "invalid encode request", Source{File: req.Input})
	}
	start := clock.Seconds(req.StartTicks)
	dur := clock.Seconds(req.DurationTicks)
	gop := strconv.Itoa(req.FrameRate.Num / req.FrameRate.Den)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(start, 'f', 6, 64),
		"-t", strconv.FormatFloat(dur, 'f', 6, 64),
		"-i", req.Input,
		"-map_metadata", "-1",
		"-c:v", "libx264", "-preset", "veryfast", "-threads", "1",
		"-b:v", strconv.Itoa(req.TargetBPS),
		"-maxrate", strconv.Itoa(int(float64(req.TargetBPS)*1.2)),
		"-bufsize", strconv.Itoa(req.TargetBPS*2),
		"-pix_fmt", "yuv420p",
		"-g", gop, "-keyint_min", gop, "-force_key_frames", "expr:gte(t,0)",
		"-s", strconv.Itoa(req.Width) + "x" + strconv.Itoa(req.Height),
		"-an",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-y", req.Output,
	}
	if err := e.run(ctx, req.Input, args...); err != nil {
		return nil, err
	}
	info, err := os.Stat(req.Output)
	if err != nil {
		return nil, NewError(CodeEncodeFailed, "output missing after encode: "+err.Error(), Source{File: req.Output})
	}
	if info.Size() == 0 {
		return nil, NewError(CodeEncodeFailed, "encoder produced an empty segment", Source{File: req.Output})
	}
	secs := float64(req.DurationTicks) / float64(clock.TicksPerSecond)
	actual := int(float64(info.Size()*8) / secs)
	return &EncodeResult{
		Output:         req.Output,
		Bytes:          info.Size(),
		DurationTicks:  req.DurationTicks,
		ActualBPS:      actual,
	}, nil
}

// run executes ffmpeg, translating cancellations into the interrupt code.
func (e *FFmpegV1) run(ctx context.Context, src string, args ...string) error {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	if err := cmd.Run(); err != nil {
		code := CodeEncodeFailed
		if ctx.Err() != nil {
			code = CodeInterrupted
		}
		return NewError(code, "ffmpeg failed: "+err.Error(), Source{File: src})
	}
	return nil
}
