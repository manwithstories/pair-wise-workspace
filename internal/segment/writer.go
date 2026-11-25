package segment

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"medpipe/internal/clock"
	"medpipe/internal/mp4"
)

// Writer emits one planned boundary as a standalone MP4 using stream-safe
// cuts and an atomic temp-file-then-rename commit.
type Writer struct {
	FFmpeg string
	Color  ColorPolicy
	OutDir string
	// Reencode forces transcoding when stream copy cannot satisfy alignment.
	Reencode bool
}

// NewWriter defaults the ffmpeg binary name and creates the output directory.
func NewWriter(outDir string) (*Writer, error) {
	if outDir == "" {
		return nil, clock.E(clock.KindArgument, "segment.write",
			clock.WithDetail("empty output directory"))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, clock.E(clock.KindOther, "segment.write", clock.WithWrap(err))
	}
	return &Writer{FFmpeg: "ffmpeg", OutDir: outDir}, nil
}

// Emit cuts the source window for b into OutDir/seg-NNN.mp4. The cut starts
// at the aligned keyframe; ffmpeg seeks in milliseconds derived from the
// rational ticks without float accumulation.
func (w *Writer) Emit(ctx context.Context, source string, c *mp4.Container, b Boundary) (string, error) {
	final := filepath.Join(w.OutDir, segmentName(b.Index))
	tmp := final + ".tmp"
	_ = os.Remove(tmp) // never reuse a partial temp from an earlier interruption

	colorArgs, err := w.colorArguments(c)
	if err != nil {
		return "", err
	}
	args := w.buildArgs(source, tmp, b, colorArgs)
	cmd := exec.CommandContext(ctx, w.FFmpeg, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp) // clean interrupted output atomically
		if ctx.Err() != nil {
			return "", clock.E(clock.KindCanceled, "segment.write", clock.WithWrap(ctx.Err()))
		}
		return "", clock.E(clock.KindOther, "segment.write",
			clock.WithDetail("ffmpeg cut failed: %s", trimOutput(out)), clock.WithWrap(err))
	}
	if err := syncAndRename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return final, nil
}

func (w *Writer) buildArgs(source, tmp string, b Boundary, colorArgs []string) []string {
	start := secs(b.StartMillis)
	duration := secs(b.EndMillis - b.StartMillis)
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-ss", start,
		"-i", source,
		"-t", duration,
	}
	if w.Reencode {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast",
			"-c:a", "aac")
	} else {
		// -c copy begins at the nearest keyframe at or before -ss, which the
		// planner already aligned; audio packet boundaries are respected.
		args = append(args, "-c", "copy")
	}
	args = append(args, "-movflags", "+faststart+write_colr")
	args = append(args, colorArgs...)
	// The temp file uses a .tmp suffix, so pin the muxer explicitly.
	args = append(args, "-f", "mp4", tmp)
	return args
}

func (w *Writer) colorArguments(c *mp4.Container) ([]string, error) {
	for _, t := range c.Tracks {
		if t.Kind != mp4.KindVideo || t.Color == nil {
			continue
		}
		resolved, err := w.Color.Resolve(t.Color)
		if err != nil {
			return nil, err
		}
		return ffmpegColorArgs(resolved), nil
	}
	return nil, nil
}

// syncAndRename flushes the temp file to disk and atomically moves it into
// place so readers never observe a half-written segment.
func syncAndRename(tmp, final string) error {
	f, err := os.Open(tmp)
	if err != nil {
		return clock.E(clock.KindOther, "segment.write", clock.WithWrap(err))
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return clock.E(clock.KindOther, "segment.write", clock.WithWrap(err))
	}
	if err := f.Close(); err != nil {
		return clock.E(clock.KindOther, "segment.write", clock.WithWrap(err))
	}
	if err := os.Rename(tmp, final); err != nil {
		return clock.E(clock.KindOther, "segment.write", clock.WithWrap(err))
	}
	return nil
}

func segmentName(index int) string {
	const digits = 3
	name := []byte("seg-000.mp4")
	pos := len("seg-") + digits - 1
	for i := 0; i < digits; i++ {
		name[pos-i] = byte('0' + index%10)
		index /= 10
	}
	return string(name)
}

func secs(millis int) string {
	// Integer milliseconds -> fixed "N.mmm" string, no float drift.
	whole := millis / 1000
	frac := millis % 1000
	return uintToStringInt(whole) + "." + pad3(frac)
}

func pad3(v int) string {
	b := []byte{'0', '0', '0'}
	for i := 2; i >= 0; i-- {
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b)
}

func uintToStringInt(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func trimOutput(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}
