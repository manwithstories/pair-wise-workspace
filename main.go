package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

// 退出码契约：0 成功；2 输入错误；3 数据不足以可靠拟合；4 读写失败。
const (
	exitOK         = 0
	exitInput      = 2
	exitUnreliable = 3
	exitIO         = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitInput
	}
	switch args[0] {
	case "generate":
		return cmdGenerate(args[1:])
	case "analyze":
		return cmdAnalyze(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", args[0])
		usage()
		return exitInput
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `avsync — 独立时钟起始偏差与漂移分析工具

用法:
  avsync generate --seed <int> --seconds <int> --out <file.jsonl>
      生成含计数器回绕与异常锚点的样例 JSONL（真值随终端输出给出）
  avsync analyze --in <file.jsonl> [--json <out.json>] [--csv <out.csv>]
                 [--tb-num <int> --tb-den <int>]
      解绕、配对、拟合 a = αv + β，并输出校正时间戳序列
      目标时间基默认 1/1000000000（纳秒），最近偶数舍入

退出码: 0 成功 | 2 输入错误 | 3 不可可靠拟合 | 4 读写失败`)
}

// ---- 子命令占位，后续批次替换为真实实现 ----

func cmdGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 0, "随机种子（确定性 LCG）")
	seconds := fs.Int("seconds", 0, "样例时长（秒）")
	out := fs.String("out", "", "输出 JSONL 路径")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *seconds <= 0 || *out == "" {
		fmt.Fprintln(os.Stderr, "generate: --seconds > 0 and --out are required")
		return exitInput
	}
	truth, err := Generate(GenConfig{Seed: *seed, Seconds: *seconds, OutPath: *out})
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		if _, ok := err.(genArgError); ok {
			return exitInput
		}
		return exitIO
	}
	fmt.Printf("out=%s\n", *out)
	fmt.Printf("truth_start_offset_ns=%d\n", truth.BetaNS)
	fmt.Printf("truth_drift_ppm=%.3f\n", truth.DriftPPM)
	fmt.Printf("records video=%d audio=%d anchors=%d bad_anchors=%d wraps video=%d audio=%d\n",
		truth.VideoCount, truth.AudioCount, truth.Anchors, truth.BadAnchors,
		truth.VideoWraps, truth.AudioWraps)
	return exitOK
}

func cmdAnalyze(args []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	in := fs.String("in", "", "输入 JSONL 路径（- 表示标准输入）")
	jsonOut := fs.String("json", "", "校正序列 JSON 导出路径（可选）")
	csvOut := fs.String("csv", "", "校正序列 CSV 导出路径（可选）")
	tbNum := fs.Int64("tb-num", 1, "目标时间基分子（秒/tick），默认 1")
	tbDen := fs.Int64("tb-den", 1_000_000_000, "目标时间基分母（秒/tick），默认 1e9 即纳秒")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "analyze: --in is required")
		return exitInput
	}
	tb := Timebase{Num: *tbNum, Den: *tbDen}
	if !tb.Valid() {
		fmt.Fprintf(os.Stderr, "analyze: illegal target timebase %d/%d\n", *tbNum, *tbDen)
		return exitInput
	}
	summary, err := Analyze(AnalyzeOptions{
		InPath: *in, JSONOut: *jsonOut, CSVOut: *csvOut, Target: tb,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "analyze: %v\n", err)
		switch {
		case errors.Is(err, ErrUnreliable):
			return exitUnreliable
		case errors.Is(err, ErrInput):
			return exitInput
		default:
			return exitIO
		}
	}
	printSummary(summary)
	return exitOK
}

// printSummary 输出验收所需四项指标；多段（重启）时逐段给出偏移/漂移/残差。
func printSummary(s *Summary) {
	if len(s.Epochs) == 1 {
		f := s.Fits[s.Epochs[0]]
		betaNS, _ := ratToInt64Even(f.Beta)
		fmt.Printf("start_offset_ns=%d\n", betaNS)
		fmt.Printf("drift_ppm=%.3f\n", f.PPM)
		fmt.Printf("residual_p95_ns=%d\n", f.ResP95NS)
		fmt.Printf("corrected_count=%d\n", s.CorrectedCount)
		return
	}
	for i, ep := range s.Epochs {
		f := s.Fits[ep]
		betaNS, _ := ratToInt64Even(f.Beta)
		fmt.Printf("segment=%d epoch=%d start_offset_ns=%d drift_ppm=%.3f residual_p95_ns=%d inliers=%d/%d\n",
			i, ep, betaNS, f.PPM, f.ResP95NS, f.Inliers, f.Pairs)
	}
	fmt.Printf("corrected_count=%d\n", s.CorrectedCount)
}
