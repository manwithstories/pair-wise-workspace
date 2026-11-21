// Command mediacheck is the acceptance entry point for the post-delivery
// media checking pipeline.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Exit codes.
const (
	exitOK       = 0 // processing and metric checks passed
	exitArgs     = 2 // argument error
	exitCorrupt  = 3 // corrupt input, partial results committed
	exitRecovery = 4 // recovery or atomic commit verification failed
)

// Limits bounding local processing (约束).
const (
	maxFragments = 64
	maxLossBurst = 64
	maxBandwidth = 8
)

type config struct {
	seed            int64
	lossBurst       int
	bandwidth       []float64
	crashAfterStage string
	fixtureCase     string
	resume          bool
	workDir         string
	fragments       int
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediacheck demo [--seed N] [--loss-burst N] "+
			"[--bandwidth-mbps a,b,c] [--crash-after-stage STAGE] [--fixture normal|corrupt] "+
			"[--resume] [--work-dir DIR] [--fragments N]")
		return exitArgs
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	lossBurst := fs.Int("loss-burst", 0, "consecutive packet loss burst length")
	bwStr := fs.String("bandwidth-mbps", "3", "comma-separated bandwidth profile")
	crashStage := fs.String("crash-after-stage", "", "simulate a crash after this stage")
	fixtureCase := fs.String("fixture", "normal", "fixture case: normal|corrupt")
	resume := fs.Bool("resume", false, "resume from the existing work directory")
	workDir := fs.String("work-dir", ".mediacheck-work", "journal and output directory")
	fragments := fs.Int("fragments", 8, "fragments per track")
	if err := fs.Parse(args[1:]); err != nil {
		return exitArgs
	}
	cfg := config{
		seed: *seed, lossBurst: *lossBurst, crashAfterStage: *crashStage,
		fixtureCase: *fixtureCase, resume: *resume, workDir: *workDir,
		fragments: *fragments,
	}
	if err := validate(&cfg, *bwStr); err != nil {
		fmt.Fprintf(os.Stderr, "mediacheck: %v\n", err)
		return exitArgs
	}
	return runDemo(cfg)
}

func validate(cfg *config, bwStr string) error {
	for _, part := range strings.Split(bwStr, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("invalid --bandwidth-mbps value %q", part)
		}
		cfg.bandwidth = append(cfg.bandwidth, v)
	}
	if len(cfg.bandwidth) > maxBandwidth {
		return fmt.Errorf("--bandwidth-mbps: at most %d entries", maxBandwidth)
	}
	if cfg.lossBurst < 0 || cfg.lossBurst > maxLossBurst {
		return fmt.Errorf("--loss-burst must be in [0,%d]", maxLossBurst)
	}
	if cfg.fragments < 1 || cfg.fragments > maxFragments {
		return fmt.Errorf("--fragments must be in [1,%d]", maxFragments)
	}
	switch cfg.fixtureCase {
	case "normal", "corrupt":
	default:
		return fmt.Errorf("--fixture must be normal or corrupt")
	}
	if cfg.crashAfterStage != "" {
		ok := false
		for _, s := range stageNames {
			if s == cfg.crashAfterStage {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("--crash-after-stage must be one of %s", strings.Join(stageNames, ","))
		}
	}
	return nil
}
