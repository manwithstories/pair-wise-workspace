// Command mediacheck is the terminal entry point for the media QC pipeline.
// It generates a deterministic sample, runs it through ingest, inspection and
// recovery, remuxes the result to Matroska and prints the acceptance
// metrics.  The library packages do all the work; this file only parses
// flags, wires the stages and maps outcomes to exit codes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/asticode/go-astiav"

	"mediacheck/internal/ingest"
	"mediacheck/internal/model"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/remux"
	"mediacheck/internal/report"
)

const usage = `mediacheck - broadcast media ingest QC and re-muxing

Usage:
  mediacheck demo [flags]

Flags:
  --profile NAME        clean | damaged | truncated | wrap | slow (default clean)
  --seed N              fixture seed (default 17)
  --packets N           total packet budget for the sample (default 2400)
  --queue N             bounded queue depth in packets (default 32)
  --slow-consumer DUR   artificial consumer delay, e.g. 1ms (default 0)
  --output PATH         write Matroska to PATH (default: no file)
  --frames N            override the video frame count
  --codec NAME          audio codec label for the fixture (opus)
  --mixed-durations     alternate 10/20/60 ms Opus packets
  --no-recovery         disable FEC/PLC/silence filling
  --json                print metrics as JSON after the key=value block

Exit codes: 0 clean, 2 damage found and handled, 1 failure, 130 canceled.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		if len(args) == 0 {
			return 1
		}
		return 0
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "mediacheck: unknown command %q\n\n%s", args[0], usage)
		return 1
	}
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "clean", "sample profile")
	seed := fs.Int64("seed", 17, "fixture seed")
	packets := fs.Int("packets", 2400, "total packet budget")
	queue := fs.Int("queue", 32, "bounded queue depth")
	slow := fs.Duration("slow-consumer", 0, "artificial consumer delay")
	output := fs.String("output", "", "Matroska output path")
	frames := fs.Int("frames", 0, "override video frame count")
	codec := fs.String("codec", "opus", "audio codec label")
	mixed := fs.Bool("mixed-durations", false, "alternate Opus packet durations")
	noRecovery := fs.Bool("no-recovery", false, "disable audio recovery")
	asJSON := fs.Bool("json", false, "print a JSON metric object")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	prof, err := ingest.ParseProfile(*profile)
	if err != nil {
		fmt.Fprintf(stderr, "mediacheck: %v\n", err)
		return 1
	}
	if *packets <= 0 || *queue <= 0 || *seed < 0 || *frames < 0 {
		fmt.Fprintf(stderr, "mediacheck: packets, queue, seed and frames must not be negative\n")
		return 1
	}
	if *slow < 0 {
		fmt.Fprintf(stderr, "mediacheck: slow-consumer must not be negative\n")
		return 1
	}
	parsedCodec, err := model.ParseCodec(*codec)
	if err != nil {
		fmt.Fprintf(stderr, "mediacheck: %v\n", err)
		return 1
	}
	if !parsedCodec.IsAudio() {
		fmt.Fprintf(stderr, "mediacheck: %s is not an audio codec\n", parsedCodec)
		return 1
	}

	cfg, err := demoConfig(*packets, *frames, *seed, *mixed)
	if err != nil {
		fmt.Fprintf(stderr, "mediacheck: %v\n", err)
		return 1
	}
	cfg.Codec = parsedCodec

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, err := ingest.Generate(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "mediacheck: generating fixture: %v\n", err)
		return 1
	}
	injected, err := g.ApplyProfile(prof)
	if err != nil {
		fmt.Fprintf(stderr, "mediacheck: applying profile: %v\n", err)
		return 1
	}

	fixture := g.Fixture()
	var writer *remux.Writer
	if *output != "" {
		if dir := filepath.Dir(*output); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				fmt.Fprintf(stderr, "mediacheck: creating output directory: %v\n", mkErr)
				return 1
			}
		}
		writer, err = remux.Open(*output, fixture.Streams)
		if err != nil {
			fmt.Fprintf(stderr, "mediacheck: opening output: %v\n", err)
			return 1
		}
		defer writer.Abort()
	}

	var sha string
	res, err := pipeline.Run(ctx, g, pipeline.Options{
		QueueItems:   *queue,
		QueueBytes:   64 << 20,
		SlowConsumer: *slow,
		Recovery:     !*noRecovery,
		Emit: func(p model.Packet) error {
			if writer == nil {
				return nil
			}
			return writer.Write(ctx, p)
		},
	})
	if err == nil && writer != nil {
		sha, err = writer.Commit()
	}
	if err != nil {
		fmt.Fprintf(stderr, "mediacheck: pipeline: %v\n", err)
		var astErr astiav.Error
		if errors.Is(err, remux.ErrAborted) && (errors.Is(err, context.Canceled) || errors.Is(err, model.ErrCanceled)) {
			// An aborted write caused by a cancelled context is a cancel, not
			// an internal failure.
			return 130
		}
		if errors.As(err, &astErr) || errors.Is(err, model.ErrUnsupported) {
			fmt.Fprintf(stderr, "mediacheck: media backend error: %v\n", err)
			return 1
		}
		return 1
	}
	if res.Canceled {
		fmt.Fprintf(stderr, "mediacheck: canceled by signal\n")
		return 130
	}

	metrics := report.Build(res, *output, sha)
	extra := []string{
		fmt.Sprintf("profile=%s", prof),
		fmt.Sprintf("codec=%s", parsedCodec),
		fmt.Sprintf("seed=%d", *seed),
		fmt.Sprintf("injected_dropped=%d", len(injected.Dropped)),
		fmt.Sprintf("injected_duplicated=%d", len(injected.Duplicated)),
		fmt.Sprintf("injected_truncated=%d", len(injected.Truncated)),
		fmt.Sprintf("injected_regressed=%d", len(injected.Regressed)),
		fmt.Sprintf("recovery_fec_supported=%t", res.Capabilities.FEC),
		fmt.Sprintf("recovery_plc_supported=%t", res.Capabilities.PLC),
		fmt.Sprintf("recovery_silence_supported=%t", res.Capabilities.Silence),
	}
	report.Print(stdout, metrics, extra)
	if *asJSON {
		for _, line := range report.JSONLines(metrics) {
			fmt.Fprintln(stdout, line)
		}
	}
	return report.ExitCode(metrics)
}

// demoConfig converts the packet budget into a video frame count that leaves
// room for the audio packets covering the same span.
func demoConfig(packets, frames int, seed int64, mixed bool) (ingest.Config, error) {
	cfg := ingest.DefaultConfig(seed)
	cfg.OpusDurationProfile = ""
	if mixed {
		cfg.OpusDurationProfile = "mixed"
	}
	target := packets
	if frames > 0 {
		cfg.VideoFrames = frames
	} else {
		// Audio adds 50 packets per second of video at 25 fps with 20 ms
		// packets, so frames/(1+2) approximates the split.
		cfg.VideoFrames = target / 3
		if cfg.VideoFrames < 1 {
			cfg.VideoFrames = 1
		}
	}
	era, err := cfg.Validate()
	if err != nil {
		return cfg, err
	}
	return era, nil
}
