// Package audit computes the delivery acceptance metrics: bit-rate
// deviation, SSIM against the source, decode error count, recovered span
// count, manifest alignment error and the repeat-run output hash.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"presspack/internal/core"
)

// Thresholds for a passing delivery.
const (
	MaxBitrateErrorPct = 15.0
	MinSSIMAvg         = 0.90
	MaxAlignMs         = 34 // one frame at 29.97 fps, the loosest supported rate
)

// Metrics is the acceptance summary printed on the terminal.
type Metrics struct {
	Cameras            int
	Variants           int
	SegmentsPerVariant int
	BitrateErrorPct    float64
	SSIMAvg            float64
	DecodeErrors       int
	RecoveredSpans     int
	MaxAlignMs         int64
	RepeatHash         string // "match" or "mismatch"
}

// Pass reports whether every acceptance threshold holds.
func (m Metrics) Pass() bool {
	return m.BitrateErrorPct <= MaxBitrateErrorPct &&
		m.SSIMAvg >= MinSSIMAvg &&
		m.DecodeErrors == 0 &&
		m.MaxAlignMs <= MaxAlignMs &&
		m.RepeatHash == "match"
}

// Print writes the metrics as key=value lines.
func (m Metrics) Print(w io.Writer) {
	fmt.Fprintf(w, "cameras=%d\n", m.Cameras)
	fmt.Fprintf(w, "variants=%d\n", m.Variants)
	fmt.Fprintf(w, "segments_per_variant=%d\n", m.SegmentsPerVariant)
	fmt.Fprintf(w, "bitrate_error_pct=%.2f\n", m.BitrateErrorPct)
	fmt.Fprintf(w, "ssim_avg=%.3f\n", m.SSIMAvg)
	fmt.Fprintf(w, "decode_errors=%d\n", m.DecodeErrors)
	fmt.Fprintf(w, "recovered_spans=%d\n", m.RecoveredSpans)
	fmt.Fprintf(w, "max_align_ms=%d\n", m.MaxAlignMs)
	fmt.Fprintf(w, "repeat_hash=%s\n", m.RepeatHash)
}

// MeasureBitrateError returns the worst |actual-target|/target percentage
// across the given variant directories. actual = media bytes / duration.
func MeasureBitrateError(variantDirs map[string]string, targets map[string]int, duration core.Time) (float64, error) {
	if duration.Ticks <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	worst := 0.0
	for name, dir := range variantDirs {
		target, ok := targets[name]
		if !ok {
			continue
		}
		var bytes int64
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, ".m4s") || strings.HasSuffix(p, ".mp4") {
				info, err := d.Info()
				if err != nil {
					return err
				}
				bytes += info.Size()
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		actual := float64(bytes*8) / duration.Seconds()
		pct := (actual - float64(target)) / float64(target) * 100
		if pct < 0 {
			pct = -pct
		}
		if pct > worst {
			worst = pct
		}
	}
	return worst, nil
}

var ssimRe = regexp.MustCompile(`All:([0-9.]+)`)

// MeasureSSIM runs ffmpeg's ssim filter of the encoded playlist against the
// source and returns the mean All: score. Streams end at the shorter input.
func MeasureSSIM(ctx context.Context, src, playlist string) (float64, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "info",
		"-i", playlist, "-i", src,
		"-lavfi", "ssim", "-f", "null", "-")
	out, _ := cmd.CombinedOutput()
	matches := ssimRe.FindAllSubmatch(out, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("no SSIM score in ffmpeg output: %s", tail(out))
	}
	v, err := strconv.ParseFloat(string(matches[len(matches)-1][1]), 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// CountDecodeErrors decodes a playlist to null and counts ffmpeg error
// lines; zero means a clean decode.
func CountDecodeErrors(ctx context.Context, playlist string) (int, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-i", playlist, "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	if err != nil && n == 0 {
		return 0, fmt.Errorf("decode run failed: %w", err)
	}
	return n, nil
}

// HashTree hashes every file under root (relative path + content) into one
// digest. Two runs producing the same digest are bit-identical deliveries.
func HashTree(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		h.Write([]byte(rel))
		h.Write([]byte{0})
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		if _, err := io.CopyBuffer(h, f, buf); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func tail(b []byte) string {
	s := string(b)
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}
