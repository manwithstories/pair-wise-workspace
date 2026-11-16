// mp4loc is a scriptable MP4 demux and keyframe random-access tool.
//
// Subcommands: fixture, inspect, seek, export.
// Exit codes: 0 ok, 2 argument/input error, 3 structure validation
// failure, 4 target not locatable, 5 export/write failure.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"mp4loc/internal/box"
	"mp4loc/internal/export"
	"mp4loc/internal/fixture"
	"mp4loc/internal/hash"
	"mp4loc/internal/seek"
)

const (
	exitOK        = 0
	exitUsage     = 2
	exitStructure = 3
	exitLocate    = 4
	exitWrite     = 5
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	var err error
	code := exitOK
	switch args[0] {
	case "fixture":
		code, err = cmdFixture(args[1:])
	case "inspect":
		code, err = cmdInspect(args[1:])
	case "seek":
		code, err = cmdSeek(args[1:])
	case "export":
		code, err = cmdExport(args[1:])
	default:
		usage()
		return exitUsage
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	return code
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: mp4loc <command> [flags]
  fixture  generate a deterministic test MP4
  inspect  parse and summarize a file (--format text|json)
  seek     locate nearest keyframe at/before a time, dump window hashes
  export   write a JSON Lines manifest`)
}

// classify maps an error to its exit code.
func classify(err error, fallback int) int {
	if err == nil {
		return exitOK
	}
	if box.IsStructure(err) {
		return exitStructure
	}
	var le *seek.LocateError
	if errors.As(err, &le) {
		return exitLocate
	}
	return fallback
}

func cmdFixture(args []string) (int, error) {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	p := fixture.Params{}
	fs.StringVar(&p.Out, "out", "", "output MP4 path (required)")
	fs.IntVar(&p.VideoFrames, "video-frames", 240, "video sample count")
	fs.IntVar(&p.AudioFrames, "audio-frames", 480, "audio sample count")
	fs.BoolVar(&p.EditList, "edit-list", false, "emit an edit list with an empty leading segment")
	fs.BoolVar(&p.TimestampWrap, "timestamp-wrap", false, "shift media origin to force 32-bit wrap")
	fs.Int64Var(&p.Seed, "seed", 1, "deterministic content seed")
	fs.StringVar(&p.Inject, "inject", "", "fault injection: truncate|bad-offset|no-stss|bad-elst")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if err := fixture.Generate(p); err != nil {
		return exitUsage, err
	}
	fmt.Printf("WROTE: %s\n", p.Out)
	return exitOK, nil
}

// analyzeOrCode runs export.Analyze and maps errors to exit codes.
func analyzeOrCode(input string) (*export.Analysis, int, error) {
	if input == "" {
		return nil, exitUsage, errors.New("--input is required")
	}
	a, err := export.Analyze(input)
	if err != nil {
		return nil, classify(err, exitUsage), err
	}
	return a, exitOK, nil
}

func cmdInspect(args []string) (int, error) {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	input := fs.String("input", "", "input MP4 path (required)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	a, code, err := analyzeOrCode(*input)
	if err != nil {
		return code, err
	}
	defer a.Close()
	switch *format {
	case "text":
		fmt.Print(a.Report.Text())
	case "json":
		b, err := a.Report.JSON()
		if err != nil {
			return exitWrite, err
		}
		fmt.Println(string(b))
	default:
		return exitUsage, fmt.Errorf("unknown --format %q", *format)
	}
	return exitOK, nil
}

func cmdSeek(args []string) (int, error) {
	fs := flag.NewFlagSet("seek", flag.ContinueOnError)
	input := fs.String("input", "", "input MP4 path (required)")
	trackID := fs.Uint("track", 0, "track ID (required)")
	target := fs.Float64("time", 0, "target presentation time in seconds")
	before := fs.Int("before", 0, "samples before the landing keyframe")
	after := fs.Int("after", 0, "samples after the landing keyframe")
	hashAlg := fs.String("hash", "none", "none|sha256")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if *hashAlg != "none" && *hashAlg != "sha256" {
		return exitUsage, fmt.Errorf("unknown --hash %q", *hashAlg)
	}
	a, code, err := analyzeOrCode(*input)
	if err != nil {
		return code, err
	}
	defer a.Close()
	tr := a.Movie.TrackByID(uint32(*trackID))
	if tr == nil {
		return exitUsage, fmt.Errorf("no track with id %d", *trackID)
	}
	tl := a.Timelines[tr.ID]
	landing, err := seek.Locate(tl, *target, *before, *after)
	if err != nil {
		return classify(err, exitLocate), err
	}
	recs, err := hash.Samples(a.File, a.FileSize, tr, tl, landing.Window,
		*hashAlg == "sha256", 4,
		func(s *box.SampleRecord) string {
			return seek.FrameType(s, tr.Table.HasSTSS, tr.IsVideo())
		})
	if err != nil {
		return classify(err, exitStructure), err
	}
	fmt.Printf("LANDING_SAMPLE: %d\n", landing.Sample)
	fmt.Printf("LANDING_PTS: %d\n", landing.PTS)
	fmt.Printf("LANDING_PTS_SECONDS: %.3f\n", landing.PTSSeconds)
	fmt.Printf("FRAME_TYPE: %s\n", seek.FrameType(&tr.Table.Samples[landing.Sample], tr.Table.HasSTSS, tr.IsVideo()))
	fmt.Printf("FILE_OFFSET: %d\n", landing.Offset)
	fmt.Printf("SAMPLE_SIZE: %d\n", landing.Size)
	fmt.Printf("WINDOW_COUNT: %d\n", len(recs))
	for i, r := range recs {
		fmt.Printf("SAMPLE[%d].INDEX: %d\n", i, r.Index)
		fmt.Printf("SAMPLE[%d].FILE_OFFSET: %d\n", i, r.Offset)
		fmt.Printf("SAMPLE[%d].SAMPLE_SIZE: %d\n", i, r.Size)
		fmt.Printf("SAMPLE[%d].DTS: %d\n", i, r.DTS)
		fmt.Printf("SAMPLE[%d].PTS: %d\n", i, r.PTS)
		fmt.Printf("SAMPLE[%d].FRAME_TYPE: %s\n", i, r.FrameType)
		if r.SHA256 != "" {
			fmt.Printf("SAMPLE[%d].SHA256: %s\n", i, r.SHA256)
		}
	}
	return exitOK, nil
}

func cmdExport(args []string) (int, error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	input := fs.String("input", "", "input MP4 path (required)")
	out := fs.String("out", "", "output JSONL path (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if *out == "" {
		return exitUsage, errors.New("--out is required")
	}
	a, code, err := analyzeOrCode(*input)
	if err != nil {
		return code, err
	}
	defer a.Close()
	f, err := os.Create(*out)
	if err != nil {
		return exitWrite, err
	}
	werr := export.WriteJSONL(f, a)
	cerr := f.Close()
	if werr != nil {
		return exitWrite, werr
	}
	if cerr != nil {
		return exitWrite, cerr
	}
	fmt.Printf("WROTE: %s\n", *out)
	return exitOK, nil
}
