// aircheck 电视台母版验收工具：纯终端形态，无图形界面依赖。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"aircheck/internal/pipeline"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "demo":
		return cmdDemo(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  aircheck run  --manifest M --src DIR --out DIR --target-kbps N [--strict] [--resume] [--fps N]
  aircheck demo --case all|normal|wrap_drift|badhash|gap|truncated --target-kbps 900 --out DIR`)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "JSONL 清单路径")
	src := fs.String("src", "", "源帧根目录")
	out := fs.String("out", "", "输出目录（仅写入此目录）")
	kbps := fs.Float64("target-kbps", 900, "目标码率 kbps")
	strict := fs.Bool("strict", false, "坏哈希帧严格拒绝（退出码 2）")
	resume := fs.Bool("resume", false, "从检查点续跑")
	fps := fs.Float64("fps", 0, "输出帧率（0=最高优先级轨）")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	opts := pipeline.Options{
		ManifestPath: *manifest, SourceRoot: *src, OutDir: *out,
		TargetKbps: *kbps, Strict: *strict, Resume: *resume, FPS: *fps,
	}
	res, err := pipeline.Run(context.Background(), opts)
	if err != nil {
		var ee *pipeline.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "error:", ee.Err)
			return ee.Code
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("frames=%d gap_frames=%d bad_frames=%d align_max_ms=%.3f "+
		"actual_kbps=%.1f rate_delta_pct=%.2f psnr_mean_db=%.2f psnr_min_db=%.2f "+
		"resumed=%v output_sha=%s\n",
		res.Frames, res.GapFrames, res.BadFrames, res.AlignMaxMs,
		res.Rate.ActualKbps, res.Rate.DeltaPct, res.Rate.PSNRMeanDB, res.Rate.PSNRMinDB,
		res.Resumed, res.OutputSHA)
	return 0
}
