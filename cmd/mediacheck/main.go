// mediacheck 是广播入库前的媒体规格统一与可复核验收 CLI。
// 纯终端、纯本地：所有素材由内部夹具按种子生成，无外部服务依赖。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"mediacheck/internal/core"
	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
	"mediacheck/internal/report"
)

// 退出码：0 完成且指标通过；2 输入损坏被按预期拒绝；1 运行或指标校验失败。
const (
	exitOK       = 0
	exitFailed   = 1
	exitRejected = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitFailed)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "version":
		fmt.Println("mediacheck v1.0.0")
		os.Exit(exitOK)
	default:
		usage()
		os.Exit(exitFailed)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediacheck demo --seed N --rate 48000 --layout stereo \\")
	fmt.Fprintln(os.Stderr, "  --target-lufs -23 --target-kbps 6000 --out DIR [--inject none|tfdt-regression|...] [--policy reject|rebase]")
}

type demoFlags struct {
	seed       int64
	rate       uint
	layout     string
	targetLUFS float64
	targetKBPS int
	out        string
	inject     string
	policy     string
}

func parseDemoFlags(args []string) (demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var d demoFlags
	fs.Int64Var(&d.seed, "seed", 7, "deterministic fixture seed")
	fs.UintVar(&d.rate, "rate", 48000, "input sample rate (44100/48000)")
	fs.StringVar(&d.layout, "layout", "stereo", "input channel layout (stereo/5.1/7.1)")
	fs.Float64Var(&d.targetLUFS, "target-lufs", -23, "integrated loudness target")
	fs.IntVar(&d.targetKBPS, "target-kbps", 6000, "target packaged bitrate kbps")
	fs.StringVar(&d.out, "out", "", "output directory (required)")
	fs.StringVar(&d.inject, "inject", "none", "corruption injection")
	fs.StringVar(&d.policy, "policy", "reject", "tfdt regression policy (reject/rebase)")
	if err := fs.Parse(args); err != nil {
		return d, err
	}
	if d.out == "" {
		return d, fmt.Errorf("--out is required")
	}
	return d, nil
}

func runDemo(args []string) int {
	d, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flag error: %v\n", err)
		return exitFailed
	}
	layout, err := model.ParseLayout(d.layout)
	if err != nil {
		return printFailure(err)
	}
	if !model.SupportedRate(uint32(d.rate)) {
		return printFailure(&model.MediaError{Category: model.ErrUnsupportedRate, Message: "rate must be 44100 or 48000"})
	}
	policy := core.RegressionPolicy(d.policy)
	if policy != core.RegressionReject && policy != core.RegressionRebase {
		return printFailure(&model.MediaError{Category: model.ErrTFDTRegression, Message: "policy must be reject or rebase"})
	}
	reg, err := core.DefaultRegistry()
	if err != nil {
		return printFailure(err)
	}
	cfg := core.PipelineConfig{
		Seed: d.seed, Rate: uint32(d.rate), Layout: layout,
		TargetRate: model.TargetSampleRate, TargetLayout: model.LayoutStereo,
		TargetLUFS: d.targetLUFS, PeakLimit: 0.95, TargetKBPS: d.targetKBPS,
		Injection: fixture.Injection(sanitizeInject(d.inject)),
		Policy:    policy, OutDir: d.out, FillSilence: true,
		ColorPriority: core.ColorPriority{
			Order:   []string{"9/16/9", "1/1/1"},
			Default: model.ColorMeta{ColorPrimaries: 1, Transfer: 1, Matrix: 1},
		},
	}
	res, err := core.Run(context.Background(), reg, cfg)
	if err != nil {
		if res != nil && res.Rejected {
			// 拒绝路径只输出拒绝 token 与稳定摘要，不打印零值验收窗口，避免误导 grep。
			fmt.Printf("summary_sha256=%s\n", res.Metrics.SummarySHA)
			fmt.Printf("status=rejected error=%s\n", res.Metrics.RejectReason)
			return exitRejected
		}
		return printFailure(err)
	}
	printMetrics(res.Metrics)
	if !acceptancePass(res.Metrics, d.targetKBPS) {
		fmt.Println("status=failed reason=acceptance_thresholds")
		return exitFailed
	}
	fmt.Printf("manifest=%s\n", res.ManifestPath)
	fmt.Println("status=ok")
	return exitOK
}

// acceptancePass 对照硬性验收窗口校验指标。
func acceptancePass(m report.Metrics, targetKBPS int) bool {
	low := float64(targetKBPS) * 0.99
	high := float64(targetKBPS) * 1.01
	if float64(m.ActualKBPS) < low || float64(m.ActualKBPS) > high {
		return false
	}
	if m.BitrateDeltaPct > 1.0 || m.ManifestAlignMS > 1.0 {
		return false
	}
	if m.SamplesIn == 0 || m.SamplesOut == 0 || m.SummarySHA == "" {
		return false
	}
	return true
}

func printMetrics(m report.Metrics) {
	// 同时输出独立 token 与校验行，便于 grep 与人工复核。
	fmt.Printf("target_kbps=%d actual_kbps=%d bitrate_delta_pct=%.3f\n",
		m.TargetKBPS, m.ActualKBPS, m.BitrateDeltaPct)
	fmt.Printf("target_kbps=%d actual_kbps=%d-%d bitrate_delta_pct<=1.0\n",
		m.TargetKBPS, int(float64(m.TargetKBPS)*0.99), int(float64(m.TargetKBPS)*1.01))
	fmt.Printf("samples_in=%d samples_out=%d\n", m.SamplesIn, m.SamplesOut)
	fmt.Printf("samples_in/samples_out=%d/%d\n", m.SamplesIn, m.SamplesOut)
	fmt.Printf("peak_dbfs=%.3f lufs=%.3f color_conflicts=%d\n",
		m.PeakDBFS, m.LUFS, m.ColorConflicts)
	fmt.Printf("manifest_alignment_ms=%.3f manifest_alignment_ms<=1.0\n", m.ManifestAlignMS)
	fmt.Printf("fragment_gaps=%d\n", m.FragmentGaps)
	fmt.Printf("silent_frames=%d clipped_samples=%d short_frames=%d unknown_boxes=%d\n",
		m.SilentFrames, m.ClippedSamples, m.ShortFrames, m.UnknownBoxes)
	fmt.Printf("summary_sha256=%s\n", m.SummarySHA)
}

func printFailure(err error) int {
	if me, ok := model.AsMediaError(err); ok {
		fmt.Printf("status=failed error=%s detail=%s\n", me.Category, me.Message)
	} else {
		fmt.Printf("status=failed error=%v\n", err)
	}
	return exitFailed
}

func sanitizeInject(s string) string {
	if s == "none" {
		return ""
	}
	return strings.TrimSpace(s)
}
