package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"aircheck/internal/plan"
	"aircheck/internal/playlist"
	"aircheck/internal/report"
)

// errGates signals quality-gate failure; the report was already printed.
var errGates = errors.New("quality gates failed")

func execFFProbe(path string) string {
	out, _ := exec.Command("ffprobe", "-v", "error", "-show_entries",
		"format=bit_rate", "-of", "csv=p=0", path).Output()
	return string(out)
}

// cmdAudit measures the packaged output against the scan index and the
// quality gates, printing the JSON report.
func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	input := fs.String("input", "", "delivery package directory")
	output := fs.String("output", "", "packaged output directory")
	cache := fs.String("cache", "", "cache directory (optional)")
	_ = fs.Bool("json", false, "JSON output (default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		return fmt.Errorf("--input and --output are required")
	}
	cacheDir := *cache
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "aircheck-audit-cache")
	}
	res, _, err := scanOrLoad(*input, cacheDir)
	if err != nil {
		return err // fault (truncated, malformed, ...) -> exit 1 with JSON
	}
	cp, err := plan.Build(res)
	if err != nil {
		return err
	}
	rep := &report.Report{Package: res.Package}
	var dirs []string
	var minTotal int64 = -1
	for _, r := range res.Renditions {
		dir := filepath.Join(*output, r.Name)
		st, err := report.MeasureRendition(dir, r.Name, r.TargetBPS)
		if err != nil {
			return fmt.Errorf("measure %s: %w", r.Name, err)
		}
		rep.Renditions = append(rep.Renditions, st)
		dirs = append(dirs, dir)
		total, err := playlistTotal(dir)
		if err != nil {
			return err
		}
		if minTotal < 0 || total < minTotal {
			minTotal = total
		}
	}
	rep.MaxAlignmentMs, err = report.Alignment(dirs, cp)
	if err != nil {
		return err
	}
	rep.EventsTotal, rep.EventMissing = report.EventCoverage(cp, minTotal)
	rep.Finalize()
	printJSON(rep)
	if !rep.OK {
		return errGates
	}
	return nil
}

// playlistTotal sums declared segment durations of one rendition (ticks).
func playlistTotal(dir string) (int64, error) {
	segs, err := playlist.ReadMediaSegments(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		return 0, err
	}
	var t int64
	for _, s := range segs {
		t += s.DurationTicks
	}
	return t, nil
}
