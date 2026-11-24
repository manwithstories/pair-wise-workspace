// Command mediad is the terminal acceptance entry point for the media
// processing kernel. It generates CMAF fixtures in memory, runs the
// fan-out/fan-in pipeline and prints a structured JSON summary.
//
// Exit codes: 0 = demo ran and all thresholds passed, 1 = acceptance metric
// failed, 2 = invalid arguments or input.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"mediaflow/internal/pipeline"
)

const usage = `mediad — 区域直播分发的本地媒体处理验收工具

用法:
  mediad demo [flags]      生成样本、执行扇出/扇入并打印 JSON 摘要
  mediad verify [flags]    执行 demo 并只打印阈值结论

flags:
  --seed int              随机种子，同一种子恢复摘要与损坏区段一致 (default 7)
  --representations int   表示层数量 1..8 (default 3)
  --loss float            RTP 丢包比例 0..0.5 (default 0.03)
  --truncate-every int    每 N 个分片注入一次截断，0 关闭 (default 11)
  --duration int          生成时长秒数 (default 30)
  --fps int               帧率 (default 30)
  --fragment int          分片时长秒数 (default 1)
  --window int            重排窗口包数 (default 64)
  --compact               单行 JSON

退出码: 0 通过, 1 指标未达标, 2 参数或输入无效
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd := args[0]
	switch cmd {
	case "demo", "verify":
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "mediad: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	cfg, compact, err := parseFlags(cmd, args[1:], stderr)
	if err != nil {
		return 2
	}
	summary, err := pipeline.Run(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return 2
	}

	if cmd == "verify" {
		printVerdict(stdout, summary)
	} else {
		if err := writeJSON(stdout, summary, compact); err != nil {
			fmt.Fprintf(stderr, "mediad: encode summary: %v\n", err)
			return 2
		}
	}
	if !summary.Pass {
		return 1
	}
	return 0
}

func parseFlags(cmd string, args []string, stderr io.Writer) (pipeline.Config, bool, error) {
	cfg := pipeline.DefaultConfig()
	fs := flag.NewFlagSet("mediad "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.Int64Var(&cfg.Seed, "seed", cfg.Seed, "random seed")
	fs.IntVar(&cfg.Representations, "representations", cfg.Representations, "number of representations")
	fs.Float64Var(&cfg.Loss, "loss", cfg.Loss, "RTP loss fraction")
	fs.IntVar(&cfg.TruncateEvery, "truncate-every", cfg.TruncateEvery, "inject truncation every N fragments (0 disables)")
	fs.IntVar(&cfg.DurationSec, "duration", cfg.DurationSec, "generated duration in seconds")
	fs.IntVar(&cfg.FPS, "fps", cfg.FPS, "frame rate")
	fs.IntVar(&cfg.FragmentSec, "fragment", cfg.FragmentSec, "fragment duration in seconds")
	fs.IntVar(&cfg.ReorderWindow, "window", cfg.ReorderWindow, "RTP reorder window in packets")
	compact := fs.Bool("compact", false, "single-line JSON output")
	if err := fs.Parse(args); err != nil {
		return cfg, false, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mediad: unexpected argument %q\n", fs.Arg(0))
		return cfg, false, fmt.Errorf("unexpected argument")
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "mediad: invalid configuration: %v\n", err)
		return cfg, false, err
	}
	return cfg, *compact, nil
}

func writeJSON(w io.Writer, s pipeline.Summary, compact bool) error {
	enc := json.NewEncoder(w)
	if !compact {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	return enc.Encode(s)
}

func printVerdict(w io.Writer, s pipeline.Summary) {
	fmt.Fprintf(w, "seed=%d representations=%d\n", s.Seed, s.Representations)
	fmt.Fprintf(w, "sync_error_ms=%.3f (max %.1f)\n", s.SyncErrorMS, s.Thresholds.MaxSyncErrorMS)
	fmt.Fprintf(w, "loss_recovery_rate=%.4f (range %.2f..%.2f)\n", s.LossRecoveryRate, s.Thresholds.MinRecoveryRate, s.Thresholds.MaxRecoveryRate)
	fmt.Fprintf(w, "p95_latency_ms=%.3f (max %.1f) [%s]\n", s.LatencyP95MS, s.Thresholds.MaxP95LatencyMS, s.LatencyScope)
	fmt.Fprintf(w, "boundary_mismatch_count=%d damaged_ranges=%d gop_intervals=%d\n",
		s.BoundaryMismatchCount, s.DamagedCount, len(s.GOPIntervalsMS))
	fmt.Fprintf(w, "recovery_digest=%s\n", s.DeterministicDigest)
	if s.Pass {
		fmt.Fprintln(w, "PASS")
		return
	}
	fmt.Fprintln(w, "FAIL")
	for _, f := range s.Failures {
		fmt.Fprintf(w, "  - %s\n", f)
	}
}
