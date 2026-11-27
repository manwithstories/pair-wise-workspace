package adapter

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"avcore/internal/clock"
)

// QualityMetrics are the objective measurements of one encoded segment
// against the corresponding span of the pristine source.
type QualityMetrics struct {
	PSNRMeanDB float64 `json:"psnr_mean_db"`
	SSIMMean   float64 `json:"ssim_mean"`
}

// Measure renders the matching source span at the segment resolution and
// computes PSNR and SSIM in a single ffmpeg filter_complex pass. The source
// window is positioned with the same integer time base used for encoding.
func (e *FFmpegV1) Measure(ctx context.Context, input string, seg EncodeRequest) (QualityMetrics, error) {
	if err := ValidateInput(input); err != nil {
		return QualityMetrics{}, err
	}
	start := clock.Seconds(seg.StartTicks)
	dur := clock.Seconds(seg.DurationTicks)
	size := strconv.Itoa(seg.Width) + "x" + strconv.Itoa(seg.Height)
	// Scale the pristine reference window to the segment's own resolution
	// before comparison; PSNR/SSIM require identical frame dimensions.
	filter := "[0:v]scale=" + size + ",split=2[a][b];[1:v]split=2[c][d];" +
		"[a][c]ssim=stats_file=-[x];[b][d]psnr=stats_file=-[y]"
	args := []string{
		"-hide_banner",
		"-ss", strconv.FormatFloat(start, 'f', 6, 64),
		"-t", strconv.FormatFloat(dur, 'f', 6, 64), "-i", input,
		"-i", seg.Output,
		"-filter_complex", filter,
		"-map", "[x]", "-map", "[y]", "-f", "null", "-",
	}
	cmd := exec.CommandContext(ctx, e.binary, args...)
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		code := CodeEncodeFailed
		if ctx.Err() != nil {
			code = CodeInterrupted
		}
		return QualityMetrics{}, NewError(code,
			"quality measure failed: "+err.Error()+" | "+combined.String(),
			Source{File: input})
	}
	log := combined.String()
	ssim, ok1 := extractSSIM(log)
	psnr, ok2 := extractPSNR(log)
	if !ok1 || !ok2 {
		return QualityMetrics{}, NewError(CodeEncodeFailed,
			"could not parse PSNR/SSIM from ffmpeg output", Source{File: seg.Output})
	}
	return QualityMetrics{PSNRMeanDB: psnr, SSIMMean: ssim}, nil
}

var (
	// SSIM summary line: "SSIM Y:.. U:.. V:.. All:0.992287 (21.12..)"
	ssimRe = regexp.MustCompile(`All:([0-9.]+)`)
	// PSNR summary line: "...average:41.862681 min:.. max:.."
	psnrRe = regexp.MustCompile(`average:([0-9.]+)`)
)

func extractSSIM(log string) (float64, bool) {
	idx := strings.LastIndex(log, "SSIM Y:")
	if idx < 0 {
		return 0, false
	}
	line := log[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	m := ssimRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

func extractPSNR(log string) (float64, bool) {
	idx := strings.LastIndex(log, "PSNR y:")
	if idx < 0 {
		return 0, false
	}
	line := log[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	m := psnrRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}
