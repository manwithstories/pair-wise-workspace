// Command tsaudit performs offline MPEG-TS health checks and slicing.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"tsaudit/internal/fixtures"
	"tsaudit/internal/report"
	"tsaudit/internal/slicer"
)

const (
	exitOK      = 0
	exitUsageIO = 1
	exitStrict  = 2
	exitVerify  = 3
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsageIO
	}
	switch args[0] {
	case "fixtures":
		return runFixtures(args[1:], stdout, stderr)
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "slice":
		return runSlice(args[1:], stdout, stderr)
	case "verify":
		return runVerifyCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitUsageIO
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `tsaudit - offline MPEG-TS audit

usage:
  tsaudit fixtures gen --out FILE --truth FILE [--seed N] [--programs N]
      [--pat-versions N] [--pmt-versions N] [--drop N] [--cc-jump N]
      [--crc-errors N] [--duration-ms N]
  tsaudit scan --in FILE [--json] [--out FILE] [--strict]
  tsaudit slice --in FILE --program N --segment-ms N --out-dir DIR [--index FILE]
  tsaudit verify --truth FILE --report FILE`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runFixtures(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "gen" {
		fmt.Fprintln(stderr, "fixtures requires gen")
		return exitUsageIO
	}
	fs := newFlagSet("fixtures gen", stderr)
	cfg := fixtures.Config{}
	fs.StringVar(&cfg.Out, "out", "", "output TS file")
	fs.StringVar(&cfg.Truth, "truth", "", "truth JSON file")
	fs.Int64Var(&cfg.Seed, "seed", 42, "deterministic seed")
	fs.IntVar(&cfg.Programs, "programs", 2, "program count")
	fs.IntVar(&cfg.PATVersions, "pat-versions", 4, "PAT version count")
	fs.IntVar(&cfg.PMTVersions, "pmt-versions", 5, "PMT version count")
	fs.IntVar(&cfg.Drop, "drop", 0, "injected packet drops")
	fs.IntVar(&cfg.CCJump, "cc-jump", 0, "injected continuity jumps")
	fs.IntVar(&cfg.CRCErrors, "crc-errors", 2, "injected PSI CRC errors")
	fs.IntVar(&cfg.DurationMS, "duration-ms", 12000, "stream duration in milliseconds")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsageIO
	}
	if cfg.Out == "" || cfg.Truth == "" || cfg.Drop < 0 || cfg.CCJump < 0 || cfg.CRCErrors < 0 {
		fmt.Fprintln(stderr, "--out and --truth are required; injection counts must be non-negative")
		return exitUsageIO
	}
	truth, err := fixtures.Generate(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "fixtures gen:", err)
		return exitUsageIO
	}
	fmt.Fprintf(stdout, "generated %s: packets=%d pat_changes=%d crc_errors=%d\n",
		cfg.Out, truth.PacketsTotal, truth.PAT.VersionChanges, truth.CRCErrors)
	return exitOK
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("scan", stderr)
	in := fs.String("in", "", "input TS file")
	asJSON := fs.Bool("json", false, "emit the full JSON report")
	out := fs.String("out", "", "write JSON report to file")
	strict := fs.Bool("strict", false, "exit 2 when packet loss is detected")
	if err := fs.Parse(args); err != nil {
		return exitUsageIO
	}
	if *in == "" {
		fmt.Fprintln(stderr, "--in is required")
		return exitUsageIO
	}
	r, err := report.ScanFile(*in)
	if err != nil {
		fmt.Fprintln(stderr, "scan:", err)
		return exitUsageIO
	}
	if *out != "" {
		if err := r.WriteJSON(*out); err != nil {
			fmt.Fprintln(stderr, "scan:", err)
			return exitUsageIO
		}
	}
	if *asJSON {
		if *out == "" {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(r); err != nil {
				fmt.Fprintln(stderr, "scan:", err)
				return exitUsageIO
			}
		}
	} else {
		printSummary(stdout, r)
	}
	if *strict && r.Continuity.LossPackets > 0 {
		return exitStrict
	}
	return exitOK
}

func printSummary(w io.Writer, r *report.Report) {
	fmt.Fprintf(w, "file=%s packets=%d packet_size=%d sync_losses=%d crc_errors=%d\n",
		r.File, r.PacketsTotal, r.PacketSize, r.SyncLosses, r.CRCErrors)
	if r.PAT != nil {
		fmt.Fprintf(w, "PAT tsid=%d version=%d changes=%d programs=%d\n",
			r.PAT.TransportStreamID, r.PAT.Version, r.PAT.VersionChanges, len(r.PAT.Programs))
	}
	for _, p := range r.PMTs {
		fmt.Fprintf(w, "PMT program=%d pid=0x%x version=%d changes=%d pcr_pid=0x%x streams=%d\n",
			p.Program, p.PID, p.Version, p.VersionChanges, p.PCRPID, len(p.Streams))
	}
	fmt.Fprintf(w, "continuity loss=%d cc_jumps=%d duplicates=%d\n",
		r.Continuity.LossPackets, r.Continuity.CCJumps, r.Continuity.Duplicates)
	fmt.Fprintf(w, "pid_map=%d orphan_pids=%d zero_packet_es=%d orphan_pmts=%d\n",
		len(r.PIDMap), len(r.OrphanPIDs), len(r.ZeroPacketES), len(r.OrphanPMTs))
}

func runSlice(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("slice", stderr)
	cfg := slicer.Config{}
	var program uint
	fs.StringVar(&cfg.In, "in", "", "input TS file")
	fs.UintVar(&program, "program", 0, "program number")
	fs.IntVar(&cfg.SegmentMS, "segment-ms", 0, "segment duration in milliseconds")
	fs.StringVar(&cfg.OutDir, "out-dir", "", "segment output directory")
	fs.StringVar(&cfg.IndexPath, "index", "", "index JSON file")
	if err := fs.Parse(args); err != nil {
		return exitUsageIO
	}
	cfg.Program = uint16(program)
	if cfg.In == "" || cfg.Program == 0 || cfg.SegmentMS <= 0 || cfg.OutDir == "" {
		fmt.Fprintln(stderr, "--in, --program, --segment-ms and --out-dir are required")
		return exitUsageIO
	}
	idx, err := slicer.Run(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "slice:", err)
		return exitUsageIO
	}
	fmt.Fprintf(stdout, "segments=%d out_dir=%s\n", len(idx.Segments), cfg.OutDir)
	return exitOK
}

func runVerifyCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify", stderr)
	truthPath := fs.String("truth", "", "truth JSON file")
	reportPath := fs.String("report", "", "scan JSON report")
	if err := fs.Parse(args); err != nil {
		return exitUsageIO
	}
	if *truthPath == "" || *reportPath == "" {
		fmt.Fprintln(stderr, "--truth and --report are required")
		return exitUsageIO
	}
	diff, err := verifyFiles(*truthPath, *reportPath)
	if err != nil {
		fmt.Fprintln(stderr, "verify:", err)
		return exitUsageIO
	}
	if diff != nil {
		fmt.Fprintf(stdout, "MISMATCH %s expected=%v actual=%v\n", diff.field, diff.expected, diff.actual)
		return exitVerify
	}
	fmt.Fprintln(stdout, "OK truth and report agree")
	return exitOK
}

func verifyFiles(truthPath, reportPath string) (*difference, error) {
	truth, err := loadTruth(truthPath)
	if err != nil {
		return nil, err
	}
	r, err := loadReport(reportPath)
	if err != nil {
		return nil, err
	}
	return compareTruth(truth, r), nil
}

func loadTruth(path string) (*fixtures.Truth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var truth fixtures.Truth
	if err := json.Unmarshal(b, &truth); err != nil {
		return nil, err
	}
	return &truth, nil
}

func loadReport(path string) (*report.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
