package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"mediatime/internal/report"
	"mediatime/internal/fixture"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(5)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "help", "-h", "--help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(5)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediatime — 本地 HLS/DASH 媒体时间线验收工具

用法:
  mediatime demo [flags]

参数:
  --seed int          确定性随机种子
  --workers int       并发工作数 (1..16)
  --queue int         待处理队列长度
  --fault string      注入故障: wrap-drift-vfr|missing|truncated|conflict|queue-full
  --cancel-after int  处理 N 个片段后取消 (0=不取消)
  --json              额外输出机器可读 JSON 报告
  --bitrate-pct float 码率偏差阈值百分比 (默认 2.0)
  --align-ms float    对齐误差阈值毫秒 (默认 8.0)

退出码: 0=完整通过 2=输入/清单无效 3=片段损坏(部分报告) 4=取消 5=内部错误
`)
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var (
		seed        = fs.Int("seed", 41, "deterministic seed")
		workers     = fs.Int("workers", 4, "bounded worker count (1..16)")
		queue       = fs.Int("queue", 8, "bounded queue length")
		fault       = fs.String("fault", "", "injected fault")
		cancelAfter = fs.Int("cancel-after", 0, "cancel after N submitted fragments")
		asJSON      = fs.Bool("json", false, "emit machine-readable JSON report")
		bitratePct  = fs.Float64("bitrate-pct", 2.0, "bitrate error threshold percent")
		alignMs     = fs.Float64("align-ms", 8.0, "alignment threshold milliseconds")
	)
	if err := fs.Parse(args); err != nil {
		return 5
	}

	cfg := runConfig{
		seed:        int64(*seed),
		workers:     *workers,
		queue:       *queue,
		fault:       fixture.Fault(*fault),
		cancelAfter: *cancelAfter,
		thresholds: report.Thresholds{
			BitrateErrorPct: *bitratePct,
			AlignmentMaxMs:  *alignMs,
		},
	}
	eng := &engine{cfg: cfg}
	res, err := eng.run(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "internal error: %v\n", err)
		return 5
	}
	if *asJSON {
		raw, jerr := res.metrics.JSON()
		if jerr != nil {
			fmt.Fprintf(os.Stderr, "internal error: %v\n", jerr)
			return 5
		}
		fmt.Println(string(raw))
	}
	fmt.Println(res.metrics.Summary())
	return res.exitCode
}
