// Command review aligns multi-camera review timelines, merges subtitle and
// timecode tracks, exports deterministic review frames and resumes
// interrupted batches from checkpoints. Pure terminal UI.
//
// Exit codes: 0 complete and verified; 2 located input errors but
// continued; 3 checkpoint or output verification failed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"review/internal/clock"
	"review/internal/fixture"
	"review/internal/run"
	"review/internal/safepath"
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: review <demo|resume> [flags]")
		return 64
	}
	sig := watchSignals()
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:], sig)
	case "resume":
		return cmdResume(args[1:], sig)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		return 64
	}
}

func watchSignals() *atomic.Bool {
	var sig atomic.Bool
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		sig.Store(true)
		fmt.Fprintln(os.Stderr, "interrupt: finishing current checkpoint, state stays resumable")
	}()
	return &sig
}

func cmdDemo(args []string, sig *atomic.Bool) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", "./demo-work", "work directory (recreated)")
	cams := fs.Int("cams", 4, "number of cameras")
	fps := fs.String("fps", "30000/1001", "frame rate: 24, 25, 30 or 30000/1001")
	blockFrames := fs.Int64("block-frames", 120, "frames per block")
	inject := fs.String("inject", "", "comma-separated damage: truncated-srt,damaged-mp4,incomplete-index")
	interruptAfter := fs.Int("interrupt-after-block", 0, "stop after N newly completed blocks (resumable)")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	rate, err := clock.ParseRate(*fps)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 64
	}
	abs, err := filepath.Abs(*work)
	if err != nil || abs == "/" || len(abs) < 8 {
		fmt.Fprintln(os.Stderr, "unsafe work directory")
		return 64
	}
	os.RemoveAll(abs)
	cfg := run.Config{Cams: *cams, FPS: *fps, BlockFrames: *blockFrames,
		FramesPerCam: 8 * *blockFrames, DayWrap: true}
	if *inject != "" {
		cfg.Inject = strings.Split(*inject, ",")
	}
	if err := fixture.Generate(abs, cfg.Cams, rate, cfg.FramesPerCam, cfg.Inject); err != nil {
		fmt.Fprintln(os.Stderr, "generate fixtures:", err)
		return 1
	}
	if err := saveConfig(abs, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sum, err := run.Run(abs, cfg, *interruptAfter, sig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		return 1
	}
	sum.PrintText(os.Stdout)
	if sum.Status == "interrupted" {
		fmt.Printf("interrupted: resume with `go run ./cmd/review resume --work %s --json`\n", *work)
		return 0
	}
	return sum.ExitCode()
}

func cmdResume(args []string, sig *atomic.Bool) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	work := fs.String("work", "./demo-work", "work directory")
	asJSON := fs.Bool("json", false, "print JSON summary")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	abs, err := filepath.Abs(*work)
	if err != nil {
		return 64
	}
	cfg, err := loadConfig(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		return 3
	}
	sum, err := run.Run(abs, cfg, 0, sig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		return 1
	}
	if *asJSON {
		sum.PrintJSON(os.Stdout)
	} else {
		sum.PrintText(os.Stdout)
	}
	return sum.ExitCode()
}

func saveConfig(work string, cfg run.Config) error {
	p, err := safepath.Join(work, "config.json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func loadConfig(work string) (run.Config, error) {
	var cfg run.Config
	p, err := safepath.Join(work, "config.json")
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}
