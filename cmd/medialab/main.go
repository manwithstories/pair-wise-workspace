// Command medialab is the thin terminal adapter for the media lab pipeline.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"medialab/internal/demo"
	"medialab/internal/format"
	"medialab/internal/metadata"
	"medialab/internal/model"
	"medialab/internal/sample"
)

const (
	exitOK       = 0
	exitUsage    = 2
	exitRejected = 3
	exitInternal = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "inspect":
		return runInspect(args[1:], stdout, stderr)
	case "gen-fixtures":
		return runGenFixtures(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printHelp(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printHelp(stderr)
		return exitUsage
	}
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 17, "deterministic generation seed")
	burstLoss := fs.Int("burst-loss", 7, "consecutive packets to lose (0..8 recoverable scenario)")
	bwRaw := fs.String("bandwidth-kbps", "900,280,1200", "comma-separated bandwidth ladder")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", fs.Args())
		return exitUsage
	}
	ladder, err := parseBandwidth(*bwRaw)
	if err != nil {
		fmt.Fprintf(stderr, "invalid --bandwidth-kbps: %v\n", err)
		return exitUsage
	}
	if *burstLoss < 0 || *burstLoss > 8 {
		fmt.Fprintf(stderr, "invalid --burst-loss: must be between 0 and 8\n")
		return exitUsage
	}
	result, err := demo.Run(demo.Options{Seed: *seed, BurstLoss: *burstLoss, BandwidthKbps: ladder})
	if err != nil {
		fmt.Fprintf(stderr, "internal_error: %v\n", err)
		return exitInternal
	}
	fmt.Fprint(stdout, demo.Render(result))
	if !result.ExpectedPathsComplete() {
		fmt.Fprintln(stderr, "internal_error: one or more deterministic acceptance invariants failed")
		return exitInternal
	}
	return exitOK
}

func parseBandwidth(raw string) ([]int, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("empty list")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 8 {
		return nil, errors.New("at most 8 ladder rungs")
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", p, err)
		}
		if v <= 0 || v > 1_000_000 {
			return nil, fmt.Errorf("bandwidth %d out of range 1..1000000", v)
		}
		out = append(out, v)
	}
	return out, nil
}

func runInspect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("file", "", "MP4 or MXF metadata file to inspect")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "inspect requires exactly --file <path>")
		return exitUsage
	}
	info, err := os.Stat(*path)
	if err != nil {
		fmt.Fprintf(stderr, "cannot read --file: %v\n", err)
		return exitUsage
	}
	if info.Size() > format.MaxContainerBytes {
		fmt.Fprintf(stdout, "safe_reject code=input_too_large offset=0 size=%d\n", info.Size())
		return exitRejected
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(stderr, "read failed: %v\n", err)
		return exitInternal
	}
	c, perr := parseByExtension(*path, data)
	if perr != nil {
		f, _ := model.AsFault(perr)
		if f != nil {
			fmt.Fprintf(stdout, "safe_reject stage=%s code=%s track=%d first_error_offset=%d\n", f.Stage, f.Code, f.TrackID, f.Offset)
		} else {
			fmt.Fprintf(stdout, "safe_reject first_error_offset=-1 error=%v\n", perr)
		}
		return exitRejected
	}
	resolved := metadata.Resolve(c)
	fmt.Fprintf(stdout, "accepted format=%s tracks=%d metadata_faults=%d\n", c.Format, len(c.Tracks), len(resolved.Faults))
	for _, rt := range resolved.Tracks {
		t := rt.Track
		status := "valid"
		if !rt.Valid {
			status = "isolated"
		}
		fmt.Fprintf(stdout, "track id=%d kind=%s %dx%d rotation=%d keyframes=%d metadata=%s color_priority=%s\n",
			t.ID, t.Kind, t.Width, t.Height, rt.Rotation, len(t.Keyframes), status, rt.Color.Priority)
		if rt.Fault != nil {
			fmt.Fprintf(stdout, "track_rejected stage=%s code=%s track=%d first_error_offset=%d\n",
				rt.Fault.Stage, rt.Fault.Code, rt.Fault.TrackID, rt.Fault.Offset)
		}
	}
	if len(resolved.Faults) > 0 {
		return exitRejected
	}
	return exitOK
}

func parseByExtension(path string, data []byte) (*format.Container, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mxf":
		return format.ParseMXF(data)
	case ".mp4", ".m4v", ".mov":
		return format.ParseMP4(data)
	default:
		if c, err := format.ParseMP4(data); err == nil {
			return c, nil
		}
		return format.ParseMXF(data)
	}
}

func runGenFixtures(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen-fixtures", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "fixtures/generated", "directory for deterministic generated fixtures")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	files, err := sample.WriteFixtures(*out)
	if err != nil {
		fmt.Fprintf(stderr, "internal_error: %v\n", err)
		return exitInternal
	}
	for _, f := range files {
		fmt.Fprintf(stdout, "generated=%s\n", f)
	}
	return exitOK
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "usage: medialab <demo|inspect|gen-fixtures> [flags]")
	fmt.Fprintln(w, "  demo --seed N --burst-loss N --bandwidth-kbps a,b,c")
	fmt.Fprintln(w, "  inspect --file path.(mp4|mxf)")
	fmt.Fprintln(w, "  gen-fixtures --out fixtures/generated")
	fmt.Fprintln(w, "exit codes: 0 ok, 2 invalid arguments, 3 input safely rejected, 1 internal error")
}
