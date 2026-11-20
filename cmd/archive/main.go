// Command archive restores historical programs from impaired RTP/RTCP
// packet streams: it parses and repairs packets onto a shared nanosecond
// timeline, mixes audio/video tracks, exports offscreen frame segments with
// per-segment hashes, maintains a chunked keyframe index, and supports
// checkpointed resume. It is terminal-only and uses purely local fixtures.
//
// Exit codes:
//
//	0  full verification passed
//	2  input or index corruption / digest mismatch
//	3  invalid parameters
//	4  cancellation or write failure with checkpoints preserved
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"archive/internal/model"
	"archive/internal/pipeline"
	"archive/internal/testdata"
)

const (
	exitOK       = 0
	exitCorrupt  = 2
	exitParam    = 3
	exitCanceled = 4
)

const usage = `archive - local RTP/RTCP media archive restoration

usage:
  archive demo   --tracks N --packets N --reorder-window N --chunk-packets N
                 --queue N --slow-writer-ms N --out DIR [--seed N] [--scenario S] [--json]
  archive verify --out DIR

scenarios: clean, reorder_dup, burst_loss, truncated, corrupt_rtcp, archive

exit codes: 0 ok | 2 input/index corrupt | 3 invalid params | 4 canceled/write failed
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitParam
	}
	switch args[0] {
	case "demo":
		return runDemo(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return exitParam
	}
}

// exitFor maps a pipeline error onto the contracted process exit code.
func exitFor(err error) int {
	var me *model.Error
	if !errors.As(err, &me) {
		return exitCorrupt
	}
	switch me.Kind {
	case model.KindParam:
		return exitParam
	case model.KindCanceled, model.KindIO:
		return exitCanceled
	case model.KindInput, model.KindIndex, model.KindPacket:
		return exitCorrupt
	default:
		return exitCorrupt
	}
}

func parseDemoFlags(args []string) (pipeline.Config, bool, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	cfg := pipeline.Config{}
	var scenario string
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON summary")
	fs.IntVar(&cfg.Tracks, "tracks", 2, "number of media tracks")
	fs.IntVar(&cfg.Packets, "packets", 12000, "total RTP packets across tracks")
	fs.IntVar(&cfg.ReorderWindow, "reorder-window", 64, "bounded reorder window (packets)")
	fs.IntVar(&cfg.ChunkPackets, "chunk-packets", 1024, "keyframe index chunk size (packets)")
	fs.IntVar(&cfg.Queue, "queue", 128, "bounded inter-stage channel capacity")
	fs.IntVar(&cfg.SlowWriterMs, "slow-writer-ms", 2, "simulated slow writer delay per frame (ms)")
	fs.StringVar(&cfg.OutDir, "out", "", "output directory")
	fs.Int64Var(&cfg.Seed, "seed", 41, "deterministic fixture seed")
	fs.StringVar(&scenario, "scenario", string(testdata.ScenarioArchive), "impairment scenario")
	if err := fs.Parse(args); err != nil {
		return cfg, false, err
	}
	cfg.Scenario = testdata.Scenario(scenario)
	cfg.DriftLimitNS = 50_000_000
	return cfg, *jsonOut, nil
}
