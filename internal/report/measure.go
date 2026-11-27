package report

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"aircheck/internal/clock"
	"aircheck/internal/plan"
	"aircheck/internal/playlist"
)

// sem bounds concurrent ffmpeg/ffprobe measurements.
var sem = make(chan struct{}, 2)

func run(bin string, args ...string) (string, error) {
	sem <- struct{}{}
	defer func() { <-sem }()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// MeasureRendition measures one output rendition directory containing
// playlist.m3u8 and its segments.
func MeasureRendition(dir, name string, targetBPS int64) (RenditionStats, error) {
	rs := RenditionStats{Name: name, TargetBPS: targetBPS}
	segs, err := playlist.ReadMediaSegments(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		return rs, err
	}
	var totalBytes, totalTicks int64
	list := filepath.Join(dir, ".concat.txt")
	var lb strings.Builder
	for _, s := range segs {
		p := filepath.Join(dir, filepath.Base(s.URI))
		fi, err := os.Stat(p)
		if err != nil {
			return rs, fmt.Errorf("segment %s missing: %w", s.URI, err)
		}
		totalBytes += fi.Size()
		totalTicks += s.DurationTicks
		fmt.Fprintf(&lb, "file '%s'\n", p)
	}
	if err := os.WriteFile(list, []byte(lb.String()), 0o644); err != nil {
		return rs, err
	}
	defer os.Remove(list)
	if totalTicks > 0 {
		rs.ActualBPS = totalBytes * 8 * clock.Timescale / totalTicks
		rs.DriftPct = float64(abs64(rs.ActualBPS-targetBPS)) * 100 / float64(targetBPS)
	}
	if rs.Frames, err = countFrames(list); err != nil {
		return rs, err
	}
	if rs.BlackPct, err = blackPct(list, totalTicks); err != nil {
		return rs, err
	}
	rs.AudioRMSDbFS, rs.AudioPeakDbFS, err = loudness(list)
	return rs, err
}

func countFrames(list string) (int64, error) {
	out, err := run("ffprobe", "-v", "error", "-f", "concat", "-safe", "0",
		"-i", list, "-select_streams", "v:0", "-count_packets",
		"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0")
	if err != nil {
		return 0, fmt.Errorf("frame count: %v", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("frame count parse %q: %v", out, err)
	}
	return n, nil
}

var blackRe = regexp.MustCompile(`black_start:([\d.]+) black_end:([\d.]+)`)

func blackPct(list string, totalTicks int64) (float64, error) {
	out, _ := run("ffmpeg", "-v", "info", "-f", "concat", "-safe", "0",
		"-i", list, "-vf", "blackdetect", "-an", "-f", "null", "-")
	var black float64
	for _, m := range blackRe.FindAllStringSubmatch(out, -1) {
		s, _ := strconv.ParseFloat(m[1], 64)
		e, _ := strconv.ParseFloat(m[2], 64)
		black += e - s
	}
	total := clock.Seconds(totalTicks)
	if total <= 0 {
		return 0, nil
	}
	return black * 100 / total, nil
}

var (
	meanRe = regexp.MustCompile(`mean_volume: (-?[\d.]+) dB`)
	maxRe  = regexp.MustCompile(`max_volume: (-?[\d.]+) dB`)
)

func loudness(list string) (float64, float64, error) {
	out, err := run("ffmpeg", "-v", "info", "-f", "concat", "-safe", "0",
		"-i", list, "-af", "volumedetect", "-vn", "-f", "null", "-")
	if err != nil {
		return 0, 0, fmt.Errorf("volumedetect: %v", err)
	}
	parse := func(re *regexp.Regexp) float64 {
		if m := re.FindStringSubmatch(out); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			return v
		}
		return 0
	}
	return parse(meanRe), parse(maxRe), nil
}

// Alignment computes the maximum deviation between each rendition's
// cumulative playlist timeline and the shared cut plan, in milliseconds.
func Alignment(dirs []string, cp *plan.CutPlan) (float64, error) {
	var maxMs float64
	for _, dir := range dirs {
		segs, err := playlist.ReadMediaSegments(filepath.Join(dir, "playlist.m3u8"))
		if err != nil {
			return 0, err
		}
		if len(segs) != cp.NumSegments() {
			return 0, fmt.Errorf("%s: %d segments, plan has %d", dir, len(segs), cp.NumSegments())
		}
		var cum int64
		for i, s := range segs {
			cum += s.DurationTicks
			dev := clock.Millis(abs64(cum - cp.Cuts[i+1]))
			if dev > maxMs {
				maxMs = dev
			}
		}
	}
	return maxMs, nil
}

// EventCoverage counts plan events not covered by the shortest output
// timeline (in ticks).
func EventCoverage(cp *plan.CutPlan, totalTicks int64) (total, missing int) {
	total = len(cp.Events)
	for _, e := range cp.Events {
		if e.Tick >= totalTicks {
			missing++
		}
	}
	return total, missing
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
