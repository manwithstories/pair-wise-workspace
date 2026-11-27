// Command bcast 是播出工程流水线的纯终端入口，无图形界面、无外部服务。
//
// 退出码约定：0 核验通过；2 参数或输入无效；3 边界或校验和不一致。
package main

import (
	"flag"
	"fmt"
	"os"

	"bcast/internal/app"
	"bcast/internal/verify"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		printUsage()
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	work := fs.String("work", "", "工作目录（素材与产物均落在此目录）")
	seed := fs.Int64("seed", 41, "确定性随机种子")
	dur := fs.String("duration", "30m", "素材时长，Go duration（如 30m）")
	faults := fs.String("faults", "all", "故障注入：all 或 none")
	cut := fs.String("resume-cut", "61%", "模拟中断点百分比（0,100)")
	step := fs.String("review-step", "15s", "帧复核步进，Go duration（如 15s）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d, err := app.ParseDuration(*dur)
	if err != nil {
		return failInvalid(err)
	}
	st, err := app.ParseDuration(*step)
	if err != nil {
		return failInvalid(err)
	}
	all, err := app.ParseFaults(*faults)
	if err != nil {
		return failInvalid(err)
	}
	pct, err := app.ParsePercent(*cut)
	if err != nil {
		return failInvalid(err)
	}
	opts := app.Options{
		WorkDir: *work, Seed: *seed, Duration: d, FaultsAll: all,
		ResumeCutPct: pct, ReviewStep: st,
	}
	rep, err := app.RunDemo(opts)
	if err != nil {
		if verify.IsInvalidInput(err) {
			return failInvalid(err)
		}
		if verify.IsBoundaryOrChecksum(err) {
			fmt.Fprintf(os.Stderr, "verification failed: %v\n", err)
			return 3
		}
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 1
	}
	printReport(rep)
	if err := verify.Evaluate(*rep); err != nil {
		fmt.Fprintf(os.Stderr, "verification failed: %v\n", err)
		return 3
	}
	return 0
}

func printReport(r *verify.Report) {
	fmt.Println("== bcast pipeline verification ==")
	fmt.Printf("tracks=%d renditions=%d frames=%d\n", r.Tracks, r.Renditions, r.Frames)
	fmt.Printf("fault_location_error_packets=%d boundary_mismatches=%d cross_layer_deviations=%d\n",
		r.FaultLocationErrorPkts, r.BoundaryMismatches, r.CrossLayerDeviations)
	fmt.Printf("resume_ms=%d block_verification_ok=%t frame_verification_ok=%t\n",
		r.ResumeMS, r.BlockVerificationOK, r.FrameVerificationOK)
	fmt.Printf("pixel_sha256=%s\n", r.PixelSHA256)
	fmt.Printf("full_sha256=%s\n", r.FullSHA256)
	fmt.Printf("resumed_sha256=%s packages_identical=%t\n", r.ResumedSHA256, r.PackagesIdentical)
	if len(r.DriftPPM) > 0 {
		fmt.Printf("drift_ppm=%v\n", r.DriftPPM)
	}
}

func failInvalid(err error) int {
	fmt.Fprintf(os.Stderr, "invalid input: %v\n", err)
	return 2
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: bcast demo --work DIR --seed N --duration 30m --faults all --resume-cut 61% --review-step 15s")
}
