package fingerprint

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strconv"

	"medpipe/internal/clock"
)

// TimeRange selects a media window in milliseconds.
type TimeRange struct {
	StartMillis int
	DurationMs  int
}

// NormalizeVideo extracts a fixed number of grayscale frames for one window.
// FFmpeg handles scaling and pixel-format normalization, so source
// resolution and color format do not affect the frame bytes beyond content.
func NormalizeVideo(ctx context.Context, path string, streamIndex int,
	tr TimeRange, geo FrameGeometry) ([][]byte, error) {
	if tr.DurationMs <= 0 || tr.StartMillis < 0 {
		return nil, clock.E(clock.KindArgument, "fingerprint.video",
			clock.WithDetail("invalid time range: %+v", tr))
	}
	rate := float64(geo.FramesPerWindow) / (float64(tr.DurationMs) / 1000.0)
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-ss", secs(tr.StartMillis),
		"-i", path,
		"-map", "0:v:" + itoa(streamIndex),
		"-t", secs(tr.DurationMs),
		"-vf", "fps=fps=" + formatFloat(rate) +
			",scale=" + itoa(geo.Width) + ":" + itoa(geo.Height) +
			",format=gray",
		"-frames:v", itoa(geo.FramesPerWindow),
		"-f", "rawvideo", "-pix_fmt", "gray", "pipe:1",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, clock.E(clock.KindOther, "fingerprint.video", clock.WithWrap(err))
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, clock.E(clock.KindCanceled, "fingerprint.video", clock.WithWrap(ctx.Err()))
		}
		return nil, clock.E(clock.KindOther, "fingerprint.video", clock.WithWrap(err))
	}
	frameSize := geo.Width * geo.Height
	frames := make([][]byte, 0, geo.FramesPerWindow)
	buf := make([]byte, frameSize)
	for len(frames) < geo.FramesPerWindow {
		_, err := io.ReadFull(stdout, buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			cmd.Wait()
			if ctx.Err() != nil {
				return nil, clock.E(clock.KindCanceled, "fingerprint.video", clock.WithWrap(ctx.Err()))
			}
			return nil, clock.E(clock.KindOther, "fingerprint.video", clock.WithWrap(err))
		}
		frame := append([]byte(nil), buf...)
		frames = append(frames, frame)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, clock.E(clock.KindCanceled, "fingerprint.video", clock.WithWrap(ctx.Err()))
		}
		return nil, clock.E(clock.KindCorrupt, "fingerprint.video",
			clock.WithDetail("ffmpeg failed: %s", trim(stderr.Bytes())), clock.WithWrap(err))
	}
	return frames, nil
}

// NormalizeMonoPCM downmixes/resamples are already done by the audio layer;
// this trims or pads PCM deterministically to a fixed sample count so equal
// content windows hash equally even if block boundaries differ.
func NormalizeMonoPCM(pcm []int16, targetSamples int) []int16 {
	out := make([]int16, targetSamples)
	n := copy(out, pcm)
	for i := n; i < targetSamples; i++ {
		out[i] = 0 // short windows zero-pad deterministically
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func secs(millis int) string {
	return strconv.FormatFloat(float64(millis)/1000.0, 'f', -1, 64)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func trim(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(bytes.TrimSpace(b))
}
