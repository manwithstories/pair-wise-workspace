// Package rewrap performs keyframe-aligned direct MP4 copy and decoded composite encoding.
package rewrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strconv"

	"github.com/abema/go-mp4"

	"bcast/internal/fault"
	"bcast/internal/safe"
)

// Result describes a committed MP4 artifact.
type Result struct {
	PathRel string `json:"path"`
	SHA256  string `json:"sha256"`
	Samples int    `json:"samples"`
}

// CopyCompatible rewraps a keyframe-aligned range without decoding. The caller
// is responsible for ensuring startFrame is on a sync sample.
func CopyCompatible(ctx context.Context, root *safe.Root, srcRel, outRel string, startFrame, frameCount, fps int) (*Result, error) {
	if frameCount <= 0 || fps <= 0 {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "CopyCompatible", errors.New("frame count and fps must be positive"))
	}
	src, err := root.Resolve(srcRel)
	if err != nil {
		return nil, err
	}
	if _, err := root.MkdirAll(parentRel(outRel)); err != nil {
		return nil, err
	}
	final, err := root.Resolve(outRel)
	if err != nil {
		return nil, err
	}
	tmpRel := outRel + ".tmp"
	tmp, err := root.Resolve(tmpRel)
	if err != nil {
		return nil, err
	}
	when := rationalSeconds(startFrame, fps)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-ss", when, "-i", src,
		"-frames:v", strconv.Itoa(frameCount), "-c", "copy",
		"-map_metadata", "-1", "-fflags", "+bitexact", "-f", "mp4", "-y", tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "CopyCompatible.ffmpeg",
			fmt.Errorf("%w: %s", err, stderr.String())).WithPath(srcRel)
	}
	samples, err := ProbeSampleCount(tmp)
	if err != nil {
		return nil, err
	}
	sum, err := root.CommitTemp(tmpRel, outRel)
	if err != nil {
		return nil, err
	}
	if samples == 0 {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "CopyCompatible", errors.New("rewrapped mp4 has no samples")).WithPath(outRel)
	}
	_ = final
	return &Result{PathRel: outRel, SHA256: sum, Samples: samples}, nil
}

// EncodeRawRGB encodes bounded raw RGB frames received from frames. The channel
// capacity supplies the backpressure bound; this function never accumulates the
// entire segment beyond the frames queued by its caller.
func EncodeRawRGB(ctx context.Context, root *safe.Root, outRel string, width, height, fps int, frames <-chan []byte) (*Result, error) {
	if width <= 0 || height <= 0 || fps <= 0 {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "EncodeRawRGB", errors.New("invalid dimensions or fps"))
	}
	if _, err := root.MkdirAll(parentRel(outRel)); err != nil {
		return nil, err
	}
	tmpRel := outRel + ".tmp"
	tmp, err := root.Resolve(tmpRel)
	if err != nil {
		return nil, err
	}
	size := fmt.Sprintf("%dx%d", width, height)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-s", size, "-r", strconv.Itoa(fps), "-i", "-",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-profile:v", "baseline",
		"-g", "2", "-keyint_min", "2", "-sc_threshold", "0", "-bf", "0",
		"-map_metadata", "-1", "-fflags", "+bitexact", "-flags:v", "+bitexact",
		"-f", "mp4", "-y", tmp)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "EncodeRawRGB.stdin", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "EncodeRawRGB.start",
			fmt.Errorf("%w: %s", err, stderr.String())).WithPath(outRel)
	}
	frameSize := width * height * 3
	var writeErr error
	for fr := range frames {
		if len(fr) != frameSize {
			writeErr = fault.E(fault.RewrapFailed, "rewrap", "EncodeRawRGB.frameSize",
				fmt.Errorf("frame size %d want %d", len(fr), frameSize)).WithPath(outRel)
			break
		}
		if _, err := stdin.Write(fr); err != nil {
			writeErr = fault.E(fault.RewrapFailed, "rewrap", "EncodeRawRGB.write", err).WithPath(outRel)
			break
		}
	}
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if writeErr != nil {
		return nil, writeErr
	}
	if waitErr != nil {
		return nil, fault.E(fault.RewrapFailed, "rewrap", "EncodeRawRGB.ffmpeg",
			fmt.Errorf("%w: %s", waitErr, stderr.String())).WithPath(outRel)
	}
	samples, err := ProbeSampleCount(tmp)
	if err != nil {
		return nil, err
	}
	sum, err := root.CommitTemp(tmpRel, outRel)
	if err != nil {
		return nil, err
	}
	return &Result{PathRel: outRel, SHA256: sum, Samples: samples}, nil
}

// ProbeSampleCount uses go-mp4 to validate a produced MP4 and count samples.
func ProbeSampleCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fault.E(fault.RewrapFailed, "rewrap", "Probe.open", err).WithPath(path)
	}
	defer f.Close()
	info, err := mp4.Probe(f)
	if err != nil {
		return 0, fault.E(fault.RewrapFailed, "rewrap", "Probe.mp4", err).WithPath(path)
	}
	n := 0
	for _, tr := range info.Tracks {
		n += len(tr.Samples)
	}
	if n == 0 {
		return 0, fault.E(fault.RewrapFailed, "rewrap", "Probe.empty", errors.New("no samples")).WithPath(path)
	}
	return n, nil
}

func rationalSeconds(frame, fps int) string {
	r := new(big.Rat).SetFrac(big.NewInt(int64(frame)), big.NewInt(int64(fps)))
	return r.FloatString(6)
}

func parentRel(rel string) string {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' {
			if i == 0 {
				return "."
			}
			return rel[:i]
		}
	}
	return "."
}
