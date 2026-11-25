// Command relaycheck 是区域直播分片传输的本地值守分析工具（纯终端）。
//
// 子命令：
//
//	fixtures  生成确定性样例数据
//	analyze   解析 HLS/DASH+fMP4+RTP，支持块边界暂停与断点续跑
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"relaycheck/internal/fixture"
	"relaycheck/internal/report"
	"relaycheck/internal/runner"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(runner.ExitBadInput)
	}
	switch os.Args[1] {
	case "fixtures":
		os.Exit(runFixtures(os.Args[2:]))
	case "analyze":
		os.Exit(runAnalyze(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(runner.ExitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(runner.ExitBadInput)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  relaycheck fixtures --out DIR --seed N")
	fmt.Fprintln(os.Stderr, "  relaycheck analyze --input DIR --index DIR --chunk N --queue N --mem BYTES")
	fmt.Fprintln(os.Stderr, "                      [--pause-after SEQ] [--resume] [--edge]")
}

func runFixtures(args []string) int {
	fs := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	seed := fs.Int64("seed", 17, "deterministic RNG seed")
	edge := fs.Bool("edge", false, "generate into edge-scenario layout (faults injected)")
	if err := fs.Parse(args); err != nil {
		return runner.ExitBadInput
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "--out is required")
		return runner.ExitBadInput
	}
	spec := fixture.DefaultSpec(*seed)
	// clean 与 edge 两个场景目录，便于演示命令直接引用。
	cleanDir := *out + "/clean"
	edgeDir := *out + "/edge"
	if err := fixture.Generate(cleanDir, spec, false); err != nil {
		fmt.Fprintf(os.Stderr, "generate clean: %v\n", err)
		return runner.ExitIOFailure
	}
	if err := fixture.Generate(edgeDir, spec, true); err != nil {
		fmt.Fprintf(os.Stderr, "generate edge: %v\n", err)
		return runner.ExitIOFailure
	}
	if !*edge {
		// 默认仍保证两个目录都生成；--edge 仅为显式语义占位。
	}
	fmt.Printf("fixtures written: %s (clean), %s (edge), segments=%d seed=%d\n",
		cleanDir, edgeDir, spec.SegmentCount, *seed)
	return runner.ExitOK
}

func runAnalyze(args []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	cfg := runner.Config{}
	fs.StringVar(&cfg.InputDir, "input", "", "input directory with playlist.m3u8 and manifest.mpd")
	fs.StringVar(&cfg.IndexPath, "index", "", "checkpoint/index directory")
	fs.IntVar(&cfg.Chunk, "chunk", 4, "checkpoint every N segments")
	fs.IntVar(&cfg.Queue, "queue", 2, "bounded pipeline queue capacity")
	fs.Int64Var(&cfg.MemBytes, "mem", 1<<24, "memory budget in bytes")
	fs.IntVar(&cfg.PauseAfter, "pause-after", 0, "pause after processing segment SEQ (exit 75)")
	fs.BoolVar(&cfg.Resume, "resume", false, "resume from latest valid checkpoint")
	fs.Int64Var(&cfg.OversizeThreshold, "oversize-bytes", 0,
		"if >0, fragments larger than this run under a constrained budget to reproduce oversized faults")
	if err := fs.Parse(args); err != nil {
		return runner.ExitBadInput
	}
	res, err := runner.Run(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "analyze: %v\n", err)
		return res.ExitCode
	}
	report.Render(os.Stdout, res.Summary)
	return res.ExitCode
}
