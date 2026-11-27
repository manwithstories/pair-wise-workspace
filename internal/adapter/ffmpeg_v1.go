package adapter

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"

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
		// Tight VBV with CBR NAL HRD keeps short one-second segments
		// close to the target even when their duration is small.
		"-maxrate", strconv.Itoa(req.TargetBPS),
		"-bufsize", strconv.Itoa(req.TargetBPS/2),
		"-x264-params", "nal-hrd=cbr",
		"-pix_fmt", "yuv420p",
		// Fixed GOP aligns every rendition to the same cut grid. No explicit
		// force_key_frames: re-forcing an IDR at t=0 inflates the first
		// segment and defeats the tight VBV rate control.
		"-g", gop, "-keyint_min", gop,
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
	// Actual bitrate is the encoded video stream rate (the conformance
	// target), excluding fragmented-MP4 container overhead.
	actual, err := streamBitrate(e, req.Output)
	if err != nil {
		return nil, err
	}
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

// streamBitrate derives the encoded elementary-stream bitrate by summing the
// video packet sizes with ffprobe and dividing by the stream duration. This
// excludes fragmented-MP4 muxer overhead, matching the meaning of -b:v.
func streamBitrate(e *FFmpegV1, path string) (int, error) {
	probe := strings.TrimSuffix(e.binary, "ffmpeg") + "ffprobe"
	if _, err := exec.LookPath(probe); err != nil {
		probe = "ffprobe"
	}
	cmd := exec.Command(probe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=size", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, NewError(CodeProbeFailed, "bitrate probe failed: "+err.Error(), Source{File: path})
	}
	var streamBytes int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, perr := strconv.ParseInt(line, 10, 64)
		if perr != nil {
			return 0, NewError(CodeProbeFailed, "bad packet size "+line, Source{File: path})
		}
		streamBytes += n
	}
	dur, derr := probeDuration(probe, path)
	if derr != nil || dur <= 0 {
		return 0, NewError(CodeProbeFailed, "cannot determine stream duration", Source{File: path})
	}
	return int(float64(streamBytes*8) / dur), nil
}

func probeDuration(probe, path string) (float64, error) {
	cmd := exec.Command(probe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=duration", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}
