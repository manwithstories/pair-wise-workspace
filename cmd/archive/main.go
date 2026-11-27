// Command archive runs the local HLS/DASH recovery pipeline against
// fixture-generated material. It is a pure terminal tool: every behaviour is
// driven by flags and the result is printed as key=value lines. Exit codes
// follow the archive contract: 0 success, 1 parameter/invariant error,
// 2 strict mode met an unrecoverable segment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"archive/internal/manifest"
	"archive/internal/model"
	"archive/internal/pipeline"
	"archive/internal/report"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 1
	}
}

type demoFlags struct {
	format         string
	seed           int64
	targetKbps     int
	bandwidth      string
	duplicateEvery int
	reorderWindow  int
	lossBurst      int
	memoryBytes    int
	strict         bool
	interruptAfter int
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f demoFlags
	fs.StringVar(&f.format, "format", "both", "playlist format: hls|dash|both")
	fs.Int64Var(&f.seed, "seed", 41, "deterministic fixture seed")
	fs.IntVar(&f.targetKbps, "target-kbps", 1800, "target representation bitrate in kbps")
	fs.StringVar(&f.bandwidth, "bandwidth-kbps", "900,4200,1200", "comma-separated bandwidth samples in kbps")
	fs.IntVar(&f.duplicateEvery, "duplicate-every", 0, "duplicate every Nth media packet (0 disables)")
	fs.IntVar(&f.reorderWindow, "reorder-window", 0, "sliding reorder window size (0 disables)")
	fs.IntVar(&f.lossBurst, "loss-burst", 0, "packets dropped per loss burst on damaged cases")
	fs.IntVar(&f.memoryBytes, "memory-bytes", model.MaxQueueBytes, "bounded queue memory ceiling")
	fs.BoolVar(&f.strict, "strict", false, "abort with code 2 on any unrecoverable segment")
	fs.IntVar(&f.interruptAfter, "interrupt-after", 0, "cancel ingest after N packets then resume")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	bw, err := parseBandwidth(f.bandwidth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return 1
	}
	formats, err := parseFormats(f.format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return 1
	}
	if err := validateFlags(f); err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return 1
	}

	// Three semantic cases per format: clean, fully-repairable, damaged.
	kinds := []pipeline.CaseKind{
		pipeline.CaseNormal, pipeline.CaseRepaired, pipeline.CaseDamaged,
	}
	var summaries []report.Summary
	for _, format := range formats {
		for _, kind := range kinds {
			cfg := pipeline.Config{
				Kind: kind, Format: format, Seed: f.seed,
				TargetKbps: f.targetKbps, BandwidthKbps: bw,
				DuplicateEvery: f.duplicateEvery, ReorderWindow: f.reorderWindow,
				LossBurst: f.lossBurst, MemoryBytes: f.memoryBytes,
				Strict: f.strict, InterruptAfter: f.interruptAfter,
			}
			summary, err := pipeline.Run(context.Background(), cfg)
			if errors.Is(err, model.ErrStrictUnrecovered) {
				// Strict mode: still render the locatable summary, then exit 2.
				summaries = append(summaries, summary)
				fmt.Print(report.RenderTable(summaries))
				fmt.Fprintf(os.Stderr, "\nstrict mode: unrecoverable segment in case %q\n", summary.Case)
				return 2
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "invariant error: %v\n", err)
				return 1
			}
			summaries = append(summaries, summary)
		}
	}

	fmt.Print(report.RenderTable(summaries))
	if err := assertAcceptance(summaries, int64(f.memoryBytes)); err != nil {
		fmt.Fprintf(os.Stderr, "acceptance invariant failed: %v\n", err)
		return 1
	}
	return 0
}

// assertAcceptance enforces the numeric acceptance contract across the
// non-damaged cases: bitrate delta within 2%, perfect CRC, sync error at
// most 20 ms, and every summary inside the memory ceiling. Damaged cases
// are only required to report at least one gap and a degraded status.
func assertAcceptance(ss []report.Summary, memLimit int64) error {
	for _, s := range ss {
		if s.PeakBytes > memLimit {
			return fmt.Errorf("case %s peak %d exceeds memory %d", s.Case, s.PeakBytes, memLimit)
		}
		isDamaged := strings.HasPrefix(s.Case, string(pipeline.CaseDamaged))
		if isDamaged {
			if s.Gaps == 0 || s.Status != report.StatusDegraded {
				return fmt.Errorf("damaged case %s must show gaps and degraded status", s.Case)
			}
			continue
		}
		if absFloat(s.DeltaPct) > 2.0 {
			return fmt.Errorf("case %s bitrate delta %.2f%% exceeds 2%%", s.Case, s.DeltaPct)
		}
		if s.QualityCRCPct != 100.0 {
			return fmt.Errorf("case %s CRC %.2f%% is not 100", s.Case, s.QualityCRCPct)
		}
		if s.SyncErrorMs > 20.0 {
			return fmt.Errorf("case %s sync error %.2f ms exceeds 20", s.Case, s.SyncErrorMs)
		}
		if s.Gaps != 0 {
			return fmt.Errorf("case %s expected gap-free, got %d gaps", s.Case, s.Gaps)
		}
	}
	return nil
}

func parseBandwidth(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid bandwidth sample %q", p)
		}
		if v <= 0 {
			return nil, fmt.Errorf("bandwidth sample must be positive, got %v", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one bandwidth sample is required")
	}
	return out, nil
}

func parseFormats(s string) ([]manifest.Format, error) {
	switch s {
	case "hls":
		return []manifest.Format{manifest.FormatHLS}, nil
	case "dash":
		return []manifest.Format{manifest.FormatDASH}, nil
	case "both":
		return []manifest.Format{manifest.FormatHLS, manifest.FormatDASH}, nil
	default:
		return nil, fmt.Errorf("format must be hls, dash or both, got %q", s)
	}
}

func validateFlags(f demoFlags) error {
	if f.seed < 0 {
		return fmt.Errorf("seed must be non-negative")
	}
	if f.targetKbps <= 0 {
		return fmt.Errorf("target-kbps must be positive")
	}
	if f.memoryBytes <= 0 || f.memoryBytes > model.MaxQueueBytes {
		return fmt.Errorf("memory-bytes must be within (0,%d]", model.MaxQueueBytes)
	}
	if f.duplicateEvery < 0 || f.reorderWindow < 0 || f.lossBurst < 0 {
		return fmt.Errorf("fault injection flags must be non-negative")
	}
	if f.reorderWindow > model.MaxRepairWindow {
		return fmt.Errorf("reorder-window must not exceed %d", model.MaxRepairWindow)
	}
	return nil
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: archive demo [flags]")
	fmt.Fprintln(w, "flags:")
	fmt.Fprintln(w, "  --format hls|dash|both")
	fmt.Fprintln(w, "  --seed int")
	fmt.Fprintln(w, "  --target-kbps int")
	fmt.Fprintln(w, "  --bandwidth-kbps csv")
	fmt.Fprintln(w, "  --duplicate-every int")
	fmt.Fprintln(w, "  --reorder-window int")
	fmt.Fprintln(w, "  --loss-burst int")
	fmt.Fprintln(w, "  --memory-bytes int")
	fmt.Fprintln(w, "  --strict")
	fmt.Fprintln(w, "  --interrupt-after int")
	fmt.Fprintln(w, "exit codes: 0 ok, 1 parameter/invariant error, 2 strict unrecoverable")
}
