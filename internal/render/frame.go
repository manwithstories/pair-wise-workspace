// Package render exports offscreen review frames and composites transitions.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/provenance"
	"bcast/internal/safe"
)

const (
	Width      = 160
	Height     = 120
	Channels   = 3
	FrameBytes = Width * Height * Channels
	FrameRate  = 8
)

// Frame is one decoded RGB24 frame. It is exactly FrameBytes bytes.
type Frame struct {
	Index       int
	PTS         model.Rational
	RGB         []byte
	SourceAsset string
	SourceFrame int64
}

// DecodeRGBRange invokes FFmpeg offscreen and streams exactly count RGB24
// frames. Only one frame-sized buffer is live at a time inside the callback.
func DecodeRGBRange(ctx context.Context, ffmpegBin, path string, startFrame, count int, onFrame func(Frame) error) error {
	if count <= 0 {
		return fault.E(fault.RenderFailed, "render", "DecodeRGBRange", errors.New("frame count must be positive"))
	}
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	expr := fmt.Sprintf("between(n\\,%d\\,%d)", startFrame, startFrame+count-1)
	cmd := exec.CommandContext(ctx, ffmpegBin, "-v", "error", "-i", path,
		"-vf", "select="+expr+",setpts=N/FRAME_RATE/TB",
		"-vsync", "0", "-frames:v", strconv.Itoa(count),
		"-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fault.E(fault.RenderFailed, "render", "DecodeRGBRange.pipe", err).WithPath(path)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 8192}
	if err := cmd.Start(); err != nil {
		return fault.E(fault.RenderFailed, "render", "DecodeRGBRange.start", err).WithPath(path)
	}

	buf := make([]byte, FrameBytes)
	for i := 0; i < count; i++ {
		if _, err := io.ReadFull(stdout, buf); err != nil {
			_ = cmd.Wait()
			return fault.E(fault.RenderFailed, "render", "DecodeRGBRange.read", err).WithPath(path).WithSegment(startFrame + i)
		}
		rgb := append([]byte(nil), buf...)
		fr := Frame{Index: i, SourceFrame: int64(startFrame + i), RGB: rgb}
		if onFrame != nil {
			if err := onFrame(fr); err != nil {
				_ = cmd.Wait()
				return err
			}
		}
	}
	// Drain any unexpected output before waiting.
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return fault.E(fault.RenderFailed, "render", "DecodeRGBRange.ffmpeg",
			fmt.Errorf("%w: %s", err, stderr.String())).WithPath(path)
	}
	return nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len() < l.max {
		remain := l.max - l.buf.Len()
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = l.buf.Write(p)
	}
	return len(p), nil
}

// WritePPM writes a binary P6 image. PPM keeps review evidence readable with
// no window, browser or platform-specific imaging dependency.
func WritePPM(path string, rgb []byte) error {
	if len(rgb) != FrameBytes {
		return fault.E(fault.RenderFailed, "render", "WritePPM", fmt.Errorf("rgb length %d != %d", len(rgb), FrameBytes)).WithPath(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fault.E(fault.WriteFailed, "render", "WritePPM.mkdir", err).WithPath(path)
	}
	f, err := os.Create(path)
	if err != nil {
		return fault.E(fault.WriteFailed, "render", "WritePPM.create", err).WithPath(path)
	}
	header := fmt.Sprintf("P6\n%d %d\n255\n", Width, Height)
	if _, err := f.WriteString(header); err != nil {
		_ = f.Close()
		return fault.E(fault.WriteFailed, "render", "WritePPM.header", err).WithPath(path)
	}
	if _, err := f.Write(rgb); err != nil {
		_ = f.Close()
		return fault.E(fault.WriteFailed, "render", "WritePPM.pixels", err).WithPath(path)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fault.E(fault.WriteFailed, "render", "WritePPM.sync", err).WithPath(path)
	}
	if err := f.Close(); err != nil {
		return fault.E(fault.WriteFailed, "render", "WritePPM.close", err).WithPath(path)
	}
	return nil
}

// ExportRange decodes count frames and writes them as review PPMs.
func ExportRange(ctx context.Context, root *safe.Root, sourceRel, dirRel, namePrefix, asset string, start, count int) ([]provenance.FrameHash, error) {
	src, err := root.Resolve(sourceRel)
	if err != nil {
		return nil, err
	}
	dir, err := root.MkdirAll(dirRel)
	if err != nil {
		return nil, err
	}
	out := make([]provenance.FrameHash, 0, count)
	err = DecodeRGBRange(ctx, "ffmpeg", src, start, count, func(fr Frame) error {
		name := fmt.Sprintf("%s-%03d.ppm", namePrefix, fr.SourceFrame)
		p := filepath.Join(dir, name)
		if err := WritePPM(p, fr.RGB); err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root.Path(), p)
		if rerr != nil {
			return rerr
		}
		out = append(out, provenance.FrameHash{
			Index:       len(out),
			PTS:         model.Rational{N: fr.SourceFrame, D: FrameRate},
			SHA256:      provenance.HashBytes(fr.RGB),
			SourceAsset: asset,
			SourceFrame: fr.SourceFrame,
			SourceRange: &model.SourceInterval{
				Asset: asset,
				Start: model.Rational{N: fr.SourceFrame, D: FrameRate},
				End:   model.Rational{N: fr.SourceFrame + 1, D: FrameRate},
			},
		})
		_ = rel
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Dissolve returns exact integer alpha blending of two equally sized frames.
// alpha is a/b where 0 yields a and a==b yields b.
func Dissolve(a, b []byte, alpha model.Rational) ([]byte, error) {
	if len(a) != FrameBytes || len(b) != FrameBytes {
		return nil, fault.E(fault.RenderFailed, "render", "Dissolve", errors.New("frame size mismatch"))
	}
	if alpha.D <= 0 || alpha.N < 0 || alpha.N > alpha.D {
		return nil, fault.E(fault.RenderFailed, "render", "Dissolve", errors.New("alpha must be in [0,1]"))
	}
	out := make([]byte, FrameBytes)
	num, den := alpha.N, alpha.D
	for i := 0; i < FrameBytes; i++ {
		av := int64(a[i])
		bv := int64(b[i])
		v := av + (bv-av)*num/den
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		out[i] = byte(v)
	}
	return out, nil
}
