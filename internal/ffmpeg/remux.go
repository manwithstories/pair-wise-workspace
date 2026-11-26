package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// writeConcat writes an ffmpeg concat demuxer file. A trailing duplicate of
// the last frame is required by the concat demuxer so the final duration is
// actually applied.
func writeConcat(path, pattern string, count int, durations []float64) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create concat file: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for i := 0; i < count; i++ {
		// image2 muxer numbers the first frame 1, not 0.
		framePath := fmt.Sprintf(pattern, i+1)
		if abs, err := filepath.Abs(framePath); err == nil {
			framePath = abs
		}
		dur := 1.0
		if durations != nil && i < len(durations) && durations[i] > 0 {
			dur = durations[i]
		}
		fmt.Fprintf(w, "file '%s'\nduration %s\n", framePath, strconv.FormatFloat(dur, 'f', 6, 64))
	}
	// Last frame repeated: its duration is ignored, but it flushes the prior.
	last := fmt.Sprintf(pattern, count)
	if abs, err := filepath.Abs(last); err == nil {
		last = abs
	}
	fmt.Fprintf(w, "file '%s'\n", last)
	return w.Flush()
}

// RemuxOptions controls a stream-copy remux driven by the timeline edit list.
type RemuxOptions struct {
	InPath       string
	OutPath      string
	StartSeconds float64
	EndSeconds   float64
	// Missing is set when the declared source file is absent; remux then
	// records the condition without attempting ffmpeg.
	Missing bool
}

// Remux stream-copies one edit segment (no re-encode). It returns
// ErrMissingSource when the declared file does not exist so the transform
// stage can keep processing independent tracks.
func Remux(ctx context.Context, opt RemuxOptions) error {
	bin, _, err := Binaries()
	if err != nil {
		return err
	}
	if opt.Missing {
		return ErrMissingSource
	}
	dur := opt.EndSeconds - opt.StartSeconds
	if dur <= 0 {
		return fmt.Errorf("non-positive edit duration %v", dur)
	}
	args := []string{"-y", "-v", "error",
		"-ss", strconv.FormatFloat(opt.StartSeconds, 'f', 6, 64),
		"-to", strconv.FormatFloat(opt.EndSeconds, 'f', 6, 64),
		"-i", opt.InPath,
		"-c", "copy", "-movflags", "+faststart", opt.OutPath}
	if _, err := run(ctx, bin, args...); err != nil {
		return fmt.Errorf("remux %s: %w", opt.InPath, err)
	}
	return nil
}

// ErrMissingSource signals a declared track file that does not exist.
var ErrMissingSource = fmt.Errorf("declared source file missing")

// ExtractFrame decodes one frame at the given second to a PNG. The seek is
// placed before input for speed; callers verify the actual landing tick
// against the timeline instead of assuming frame-accurate positioning.
func ExtractFrame(ctx context.Context, inPath, outPNG string, atSeconds float64) error {
	bin, _, err := Binaries()
	if err != nil {
		return err
	}
	args := []string{"-y", "-v", "error",
		"-ss", strconv.FormatFloat(atSeconds, 'f', 6, 64),
		"-i", inPath,
		"-frames:v", "1", "-f", "image2", outPNG}
	if _, err := run(ctx, bin, args...); err != nil {
		return fmt.Errorf("extract frame @%.3fs: %w", atSeconds, err)
	}
	return nil
}
