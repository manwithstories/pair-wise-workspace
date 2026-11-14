package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tsprobe/internal/demux"
	"tsprobe/internal/fixture"
	"tsprobe/internal/psi"
	"tsprobe/internal/report"
	"tsprobe/internal/slicer"
	"tsprobe/internal/tspacket"
)

const (
	exitOK       = 0
	exitUsage    = 2
	exitInput    = 3
	exitNoTables = 4
	exitNoSlices = 5
	exitOutput   = 6
	exitMismatch = 7
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "gen":
		return runGen(args[1:], stdout, stderr)
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "slice":
		return runSlice(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: tsprobe <gen|scan|slice|verify> [flags]")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

func parseFlags(fs *flag.FlagSet, args []string) (bool, int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, exitOK
		}
		return false, exitUsage
	}
	if fs.NArg() != 0 {
		return false, exitUsage
	}
	return true, 0
}

func runGen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("gen", stderr)
	fs.SetOutput(stderr)
	out := fs.String("out", "testdata/sample.ts", "output TS path (out/ or testdata/)")
	programs := fs.Int("programs", 2, "number of PAT programs")
	duration := fs.Duration("duration", 12*time.Second, "stream duration")
	loss := fs.Int("loss", 7, "packets to omit from video PID")
	ccjump := fs.Int("ccjump", 3, "continuity-counter jumps")
	versions := fs.Int("versions", 4, "unique PAT versions")
	if ok, code := parseFlags(fs, args); !ok {
		return code
	}
	if *programs < 1 || *duration <= 0 || *loss < 0 || *ccjump < 0 || *versions < 1 || *versions > 32 {
		fmt.Fprintln(stderr, "invalid generator parameters")
		return exitUsage
	}
	if err := checkOutput(*out); err != nil {
		fmt.Fprintln(stderr, err)
		return exitOutput
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(stderr, err)
		return exitOutput
	}
	file, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitOutput
	}
	summary, genErr := fixture.Generate(file, fixture.Options{
		Programs: *programs, Duration: *duration, Loss: *loss, CCJumps: *ccjump, Versions: *versions,
	})
	closeErr := file.Close()
	if genErr != nil {
		fmt.Fprintln(stderr, genErr)
		return exitOutput
	}
	if closeErr != nil {
		fmt.Fprintln(stderr, closeErr)
		return exitOutput
	}
	fmt.Fprintf(stdout, "generated %s packets=%d video=%d pmt=%d pat_versions=%d pmt_versions=%d lost=%d cc_jumps=%d\n",
		*out, summary.Packets, summary.VideoPackets, summary.PMTPackets,
		summary.PATVersions, summary.PMTVersions, summary.Lost, summary.CCJumps)
	return exitOK
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("scan", stderr)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "input TS path")
	jsonPath := fs.String("json", "", "optional JSON output path (out/ or testdata/)")
	text := fs.Bool("text", false, "print text summary instead of JSON")
	if ok, code := parseFlags(fs, args); !ok {
		return code
	}
	if *in == "" {
		fmt.Fprintln(stderr, "-in is required")
		return exitUsage
	}
	if *jsonPath != "" {
		if err := checkOutput(*jsonPath); err != nil {
			fmt.Fprintln(stderr, err)
			return exitOutput
		}
	}
	file, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitInput
	}
	defer file.Close()

	reader := tspacket.NewReader(file, 8)
	store := psi.NewStore()
	dmx := demux.New(store)
	for {
		packet, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			if errors.Is(err, tspacket.ErrEmptyInput) || errors.Is(err, tspacket.ErrNoSync) {
				return exitInput
			}
			return exitInput
		}
		if _, err := dmx.Process(packet); err != nil {
			fmt.Fprintln(stderr, err)
			return exitInput
		}
	}
	packetStats := reader.Stats()
	demuxStats := dmx.Stats()
	if packetStats.Packets == 0 {
		fmt.Fprintln(stderr, "input contains no packets")
		return exitInput
	}
	if !store.HasPAT() || (demuxStats.PSISections > 0 && demuxStats.CRCFailures == demuxStats.PSISections) {
		fmt.Fprintln(stderr, "no valid PAT or all PSI CRCs failed")
		return exitNoTables
	}
	summary := report.Build(store, packetStats, demuxStats, nil, 0)
	if err := writeJSONFile(*jsonPath, summary); err != nil {
		fmt.Fprintln(stderr, err)
		return exitOutput
	}
	if *text {
		if err := summary.WriteText(stdout, store, packetStats); err != nil {
			return exitOutput
		}
	} else if err := summary.WriteJSON(stdout); err != nil {
		return exitOutput
	}
	return exitOK
}

func writeJSONFile(path string, value any) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func runSlice(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("slice", stderr)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "input TS path")
	outDir := fs.String("out-dir", "", "clip output directory (out/ or testdata/)")
	minDuration := fs.Duration("min-duration", 2*time.Second, "minimum clip duration")
	indexPath := fs.String("index", "", "index JSON output path (out/ or testdata/)")
	if ok, code := parseFlags(fs, args); !ok {
		return code
	}
	if *in == "" || *outDir == "" || *indexPath == "" {
		fmt.Fprintln(stderr, "-in, -out-dir and -index are required")
		return exitUsage
	}
	if *minDuration <= 0 {
		fmt.Fprintln(stderr, "min-duration must be positive")
		return exitUsage
	}
	if err := checkOutput(*outDir); err != nil {
		fmt.Fprintln(stderr, err)
		return exitOutput
	}
	if err := checkOutput(*indexPath); err != nil {
		fmt.Fprintln(stderr, err)
		return exitOutput
	}
	slices, err := slicer.Slice(*in, *outDir, *indexPath, *minDuration)
	if err != nil {
		fmt.Fprintln(stderr, err)
		switch {
		case errors.Is(err, slicer.ErrNoSlices):
			return exitNoSlices
		case errors.Is(err, slicer.ErrNoTables):
			return exitNoTables
		case errors.Is(err, tspacket.ErrEmptyInput), errors.Is(err, tspacket.ErrNoSync),
			errors.Is(err, os.ErrNotExist):
			return exitInput
		default:
			return exitOutput
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(slicer.Index{Slices: slices}); err != nil {
		return exitOutput
	}
	return exitOK
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify", stderr)
	fs.SetOutput(stderr)
	indexPath := fs.String("index", "", "index JSON path")
	dir := fs.String("dir", "", "clip directory")
	if ok, code := parseFlags(fs, args); !ok {
		return code
	}
	if *indexPath == "" || *dir == "" {
		fmt.Fprintln(stderr, "-index and -dir are required")
		return exitUsage
	}
	mismatches, details, err := slicer.Verify(*indexPath, *dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitMismatch
	}
	for _, detail := range details {
		fmt.Fprintln(stderr, detail)
	}
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(map[string]int{"mismatches": mismatches}); err != nil {
		return exitOutput
	}
	if mismatches > 0 {
		return exitMismatch
	}
	return exitOK
}

func checkOutput(path string) error {
	if path == "" {
		return errors.New("empty output path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, root := range []string{filepath.Join(cwd, "out"), filepath.Join(cwd, "testdata")} {
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("output path %q must be inside out/ or testdata/", path)
}
