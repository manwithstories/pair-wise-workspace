// Command avrepair repairs synthetic HLS/DASH capture timelines. It is a
// pure terminal tool: no network, no graphics, no external services.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"avrepair/internal/fixture"
	"avrepair/internal/index"
	"avrepair/internal/media"
	"avrepair/internal/pipeline"
)

// Exit codes are part of the tool contract.
const (
	exitOK          = 0
	exitCorrupt     = 2
	exitInternal    = 3
	exitInterrupted = 75
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitInternal
	}
	switch args[0] {
	case "fixture":
		return runFixture(args[1:])
	case "repair":
		return runRepair(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "seek":
		return runSeek(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "avrepair: unknown command %q\n", args[0])
		usage()
		return exitInternal
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: avrepair <command> [flags]

commands:
  fixture --dir DIR                 generate normal, lossy and corrupt cases
  repair  --input PATH --state DIR --output DIR [--interrupt-after STAGE] [--resume]
  verify  --index FILE [--output DIR]
  seek    --index FILE --track ID --time-ms N`)
}

func runFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	dir := fs.String("dir", "", "target directory")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "avrepair fixture: --dir is required")
		return exitInternal
	}
	if err := fixture.Generate(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "avrepair fixture: %v\n", err)
		return exitInternal
	}
	fmt.Printf("fixture_dir=%s\n", *dir)
	for _, name := range []string{fixture.CaseNormal, fixture.CaseLossy, fixture.CaseCorrupt} {
		fmt.Printf("case=%s path=%s\n", name, filepath.Join(*dir, name))
	}
	return exitOK
}

func runRepair(args []string) int {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	var opt pipeline.Options
	fs.StringVar(&opt.Input, "input", "", "case directory or manifest file")
	fs.StringVar(&opt.StateDir, "state", "", "state directory for checkpoints")
	fs.StringVar(&opt.OutputDir, "output", "", "output directory")
	fs.StringVar(&opt.InterruptAt, "interrupt-after", "", "stop cleanly after a stage")
	fs.BoolVar(&opt.Resume, "resume", false, "resume from the last complete checkpoint")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	if opt.Input == "" || opt.StateDir == "" || opt.OutputDir == "" {
		fmt.Fprintln(os.Stderr, "avrepair repair: --input, --state and --output are required")
		return exitInternal
	}
	rep, err := pipeline.Run(opt)
	if err != nil {
		var fault *media.FaultError
		if errors.Is(err, pipeline.ErrInterrupted) {
			if rep != nil {
				printReport(rep)
			}
			fmt.Fprintf(os.Stderr, "avrepair repair: %v\n", err)
			return exitInterrupted
		}
		if errors.As(err, &fault) || isFault(err) {
			if rep != nil {
				printReport(rep)
			}
			fmt.Fprintf(os.Stderr, "avrepair repair: %v\n", err)
			return exitCorrupt
		}
		fmt.Fprintf(os.Stderr, "avrepair repair: %v\n", err)
		return exitInternal
	}
	printReport(rep)
	if rep.Quarantined > 0 {
		return exitCorrupt
	}
	return exitOK
}

// isFault reports whether err wraps one of the typed media/segment faults.
func isFault(err error) bool {
	for _, target := range []error{
		media.ErrLimitExceeded, media.ErrUnsafeURI, media.ErrPathTraversal,
		media.ErrInvalidTimescale, media.ErrTimelineTooLong, media.ErrInvalidRange,
		media.ErrNoManifest, media.ErrAmbiguousTimestamp,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func printReport(rep *pipeline.Report) {
	fields := []struct {
		key string
		val any
	}{
		{"input", rep.Input},
		{"state_dir", rep.StateDir},
		{"output_dir", rep.OutputDir},
		{"manifest", rep.Manifest},
		{"input_digest", rep.InputDigest},
		{"tracks", rep.Tracks},
		{"segments", rep.Segments},
		{"segments_valid", rep.SegmentsValid},
		{"quarantined_segments", rep.Quarantined},
		{"seek_index_entries", rep.IndexEntries},
		{"av_sync_max_ms", round(rep.AVSyncMaxMS)},
		{"loss_recovery_pct", round(rep.LossRecoveryPct)},
		{"p95_segment_ms", round(rep.P95SegmentMS)},
		{"recovered_packets", rep.RecoveredPackets},
		{"unrecovered_groups", rep.Unrecovered},
		{"corrections", rep.Corrections},
		{"reused_stages", rep.ReusedStages},
		{"reused_stage_names", rep.ReusedNames},
		{"completed_stages", rep.CompletedStages},
		{"result_sha256", rep.ResultSHA256},
		{"index_path", rep.IndexPath},
		{"interrupted", rep.Interrupted},
		{"resumed", rep.Resumed},
	}
	for _, f := range fields {
		b, err := json.Marshal(f.val)
		if err != nil {
			continue
		}
		fmt.Printf("%s=%s\n", f.key, string(b))
	}
	if len(rep.Warnings) > 0 {
		b, _ := json.Marshal(rep.Warnings)
		fmt.Printf("warnings=%s\n", b)
	}
}

// round trims floating point noise so terminal output stays stable.
func round(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	path := fs.String("index", "", "index file")
	outDir := fs.String("output", "", "output directory to verify references against")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "avrepair verify: --index is required")
		return exitInternal
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avrepair verify: %v\n", err)
		return exitInternal
	}
	idx, err := index.Decode(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avrepair verify: %v\n", err)
		return exitCorrupt
	}
	if err := idx.Validate(*outDir); err != nil {
		fmt.Fprintf(os.Stderr, "avrepair verify: %v\n", err)
		return exitCorrupt
	}
	fmt.Printf("index_entries=%d\ntracks=%d\nverified=true\n", len(idx.Entries), len(idx.Tracks))
	return exitOK
}

func runSeek(args []string) int {
	fs := flag.NewFlagSet("seek", flag.ContinueOnError)
	path := fs.String("index", "", "index file")
	track := fs.String("track", "", "track id")
	timeMS := fs.Int64("time-ms", 0, "target time in milliseconds")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	if *path == "" || *track == "" {
		fmt.Fprintln(os.Stderr, "avrepair seek: --index and --track are required")
		return exitInternal
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avrepair seek: %v\n", err)
		return exitInternal
	}
	idx, err := index.Decode(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avrepair seek: %v\n", err)
		return exitCorrupt
	}
	e, ok := idx.Seek(*track, media.NewRational(*timeMS, 1000))
	if !ok {
		fmt.Fprintf(os.Stderr, "avrepair seek: no entry at or before %dms on %s\n", *timeMS, *track)
		return exitCorrupt
	}
	b, _ := json.Marshal(e)
	fmt.Printf("seek=%s\n", b)
	return exitOK
}
