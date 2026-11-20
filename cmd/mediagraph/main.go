// mediagraph 演示区域直播分发片段处理：RTP/字幕/时间码解析进入统一时间基准，
// 经轨道依赖图执行切分、静音检测与指纹去重，结果写入检查点与溯源记录。
// 纯终端形态，仅处理本地自生成样例，无外部服务依赖。
//
// 退出码：0 完整成功；2 可恢复输入损坏（已输出定位）；
// 3 检查点或校验失败；4 参数错误。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"mediagraph/internal/app"
	"mediagraph/internal/pipeline"
)

func main() { os.Exit(run(os.Args[1:])) }

// output 为终端输出：摘要指标 + 本轮耗时（不计入清单，保证清单确定）。
type output struct {
	app.Summary
	ResumeMs    int64 `json:"resume_ms"`
	Interrupted bool  `json:"interrupted"`
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 4
	}
	switch args[0] {
	case "demo":
		fs := flag.NewFlagSet("demo", flag.ContinueOnError)
		seed := fs.Int64("seed", 42, "fixture seed (deterministic)")
		workdir := fs.String("workdir", "", "working directory (required)")
		interrupt := fs.Int("interrupt-after", -1, "interrupt after N completed steps (-1: never)")
		window := fs.Int64("window-ms", 4000, "split window in milliseconds")
		minWin := fs.Int64("min-window-ms", 1500, "minimum window in milliseconds")
		if err := fs.Parse(args[1:]); err != nil {
			return 4
		}
		if *workdir == "" {
			fmt.Fprintln(os.Stderr, "demo: --workdir is required")
			return 4
		}
		cfg := &pipeline.Config{Workdir: *workdir, Seed: *seed,
			InterruptAfter: *interrupt, WindowMs: *window, MinWindowMs: *minWin}
		start := time.Now()
		sum, interrupted, err := app.Demo(cfg, *interrupt)
		if err != nil {
			return fail(err)
		}
		return printOutput(sum, interrupted, time.Since(start))
	case "resume":
		fs := flag.NewFlagSet("resume", flag.ContinueOnError)
		workdir := fs.String("workdir", "", "working directory (required)")
		if err := fs.Parse(args[1:]); err != nil {
			return 4
		}
		if *workdir == "" {
			fmt.Fprintln(os.Stderr, "resume: --workdir is required")
			return 4
		}
		start := time.Now()
		sum, interrupted, err := app.Resume(*workdir)
		if err != nil {
			return fail(err)
		}
		return printOutput(sum, interrupted, time.Since(start))
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		fixture := fs.String("fixture", "all", "good|corrupt|all")
		format := fs.String("format", "text", "json|text")
		seed := fs.Int64("seed", 42, "fixture seed (deterministic)")
		workdir := fs.String("workdir", "", "directory for fixtures (default: temp dir)")
		if err := fs.Parse(args[1:]); err != nil {
			return 4
		}
		if *format != "json" && *format != "text" {
			fmt.Fprintln(os.Stderr, "verify: --format must be json or text")
			return 4
		}
		res, err := app.Verify(*fixture, *seed, *workdir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verify:", err)
			return 4
		}
		if *format == "json" {
			raw, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(raw))
		} else {
			fmt.Printf("fixture=%s ok=%v first_error_offset=%d first_error_file=%s recovered_cues=%d\n",
				res.Fixture, res.OK, res.FirstErrorOffset, res.FirstErrorFile, res.RecoveredCues)
			for file, n := range res.Issues {
				fmt.Printf("  %s: %d issue(s)\n", file, n)
			}
		}
		if !res.OK {
			return 2 // 可恢复输入损坏，已输出定位
		}
		return 0
	default:
		usage()
		return 4
	}
}

func printOutput(sum app.Summary, interrupted bool, elapsed time.Duration) int {
	raw, _ := json.MarshalIndent(output{
		Summary:     sum,
		ResumeMs:    elapsed.Milliseconds(),
		Interrupted: interrupted,
	}, "", "  ")
	fmt.Println(string(raw))
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	if errors.Is(err, app.ErrCheckpoint) {
		return 3
	}
	return 1
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mediagraph demo   --seed N --workdir DIR [--interrupt-after N] [--window-ms N] [--min-window-ms N]
  mediagraph resume --workdir DIR
  mediagraph verify --fixture good|corrupt|all [--format json|text] [--seed N] [--workdir DIR]`)
}
