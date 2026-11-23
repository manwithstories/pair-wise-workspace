// mediaroute is a terminal-only media routing kernel: it scans MP4/MXF
// containers into invalidatable indexes, streams fault-injected RTP, and
// recovers interrupted runs with verified checkpoints. All fixtures are
// generated locally; there are no network or GUI dependencies.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"mediaroute/internal/demo"
	"mediaroute/internal/engine"
	"mediaroute/internal/fault"
	"mediaroute/internal/index"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(int(fault.ExitCode(fault.New(fault.CatBadArgument, "no subcommand"))))
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "scan":
		err = runScan(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fault.New(fault.CatBadArgument, "unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fe := fault.AsError(err)
		fmt.Fprintf(os.Stderr, "error: %s\n", fe.Error())
		os.Exit(fault.ExitCode(err))
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediaroute - local media routing kernel

usage:
  mediaroute demo [flags]      generate fixtures and run the full scenario
  mediaroute scan --input F    scan a local MP4/MXF file and print a summary

demo flags:
  --seed N                deterministic fixture seed (default 41)
  --drop-burst N          number of consecutive RTP packets dropped
  --drop-burst-at N       first sequence number of the burst (default 30)
  --duplicate N           number of duplicated packets
  --reorder-window N      window size for out-of-order delivery
  --bandwidth-mbps S      comma-separated bandwidth stages, e.g. 6,1,9
  --interrupt-after N     simulate interruption after N committed samples
  --resume                resume from the checkpoint after interruption

exit codes: 0 ok, 2 locatable input damage, 3 recovery failure, 4 bad args
`)
}

func runDemo(args []string) error {
	fs := newFlagSet("demo")
	seed := fs.Int64("seed", 41, "fixture seed")
	dropBurst := fs.Int("drop-burst", 0, "burst loss length")
	dropBurstAt := fs.Int("drop-burst-at", 30, "burst loss first sequence")
	duplicate := fs.Int("duplicate", 0, "duplicated packet count")
	reorder := fs.Int("reorder-window", 0, "reorder window")
	bw := fs.String("bandwidth-mbps", "", "comma-separated bandwidth stages")
	interrupt := fs.Int("interrupt-after", 0, "interrupt after N samples")
	resume := fs.Bool("resume", false, "resume after interruption")
	if err := fs.Parse(args); err != nil {
		return fault.New(fault.CatBadArgument, "%v", err)
	}
	stages, err := parseStages(*bw)
	if err != nil {
		return err
	}
	opt := demo.Options{
		Seed:           *seed,
		DropBurst:      *dropBurst,
		DropBurstAt:    *dropBurstAt,
		Duplicate:      *duplicate,
		ReorderWindow:  *reorder,
		BandwidthMbps:  stages,
		InterruptAfter: *interrupt,
		Resume:         *resume,
	}
	if err := validateDemo(opt); err != nil {
		return err
	}
	res, err := demo.Run(opt)
	if err != nil {
		return err
	}
	defer os.RemoveAll(res.WorkDir)
	for _, d := range res.ExtraDirs {
		defer os.RemoveAll(d)
	}
	res.Print()
	for _, p := range res.Phases {
		raw, jerr := p.Summary.JSON()
		if jerr != nil {
			return jerr
		}
		fmt.Printf("--- phase=%s ---\n%s\n", p.Name, string(raw))
	}
	return nil
}

func runScan(args []string) error {
	fs := newFlagSet("scan")
	input := fs.String("input", "", "local MP4/MXF file")
	indexDir := fs.String("index-dir", "", "index directory")
	format := fs.String("format", "", "mp4|mxf|auto")
	if err := fs.Parse(args); err != nil {
		return fault.New(fault.CatBadArgument, "%v", err)
	}
	if *input == "" {
		return fault.New(fault.CatBadArgument, "--input is required")
	}
	dir := *indexDir
	if dir == "" {
		dir = os.TempDir()
	}
	sr, err := engine.Scan(engine.ScanConfig{
		InputPath: *input, Format: *format,
		IndexDir: dir, Params: index.DefaultParams(),
	})
	if err != nil {
		return err
	}
	st := &engine.StreamStats{} // scan-only invocation
	sum := engine.BuildSummary(sr, st, -1)
	raw, err := sum.JSON()
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func parseStages(s string) ([]float64, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v <= 0 {
			return nil, fault.New(fault.CatBadArgument,
				"invalid bandwidth stage %q", p)
		}
		out = append(out, v)
	}
	return out, nil
}

func validateDemo(o demo.Options) error {
	if o.Seed < 0 {
		return fault.New(fault.CatBadArgument, "seed must be non-negative")
	}
	if o.DropBurst < 0 || o.Duplicate < 0 || o.ReorderWindow < 0 {
		return fault.New(fault.CatBadArgument,
			"drop/duplicate/reorder values must be non-negative")
	}
	if o.InterruptAfter < 0 {
		return fault.New(fault.CatBadArgument,
			"interrupt-after must be non-negative")
	}
	return nil
}
