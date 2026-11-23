// mediaprobe 是媒体处理内核的演示入口：
// 生成本地样例、模拟乱序/抖动/突发丢包、量化恢复质量。
// 退出码：0=全部校验通过 1=处理失败 2=输入损坏 3=缓存或检查点不可信。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"mediakernel/internal/ebml"
	"mediakernel/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "mp4boxes":
		return mp4boxes(args[1:])
	default:
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mediaprobe demo [--seed N] [--loss-burst N] [--bandwidth-kbps N] [--resume] [--workdir DIR]
  mediaprobe mp4boxes FILE`)
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 17, "样例随机种子")
	lossBurst := fs.Int("loss-burst", 12, "突发丢包长度（包）")
	kbps := fs.Int("bandwidth-kbps", 420, "目标带宽 kbps")
	resume := fs.Bool("resume", false, "从最近可信检查点续跑")
	workdir := fs.String("workdir", ".mediaprobe", "工作目录")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	res, err := pipeline.Run(context.Background(), pipeline.Options{
		WorkDir: *workdir, Seed: *seed, LossBurst: *lossBurst,
		BandwidthKbps: *kbps, Resume: *resume,
	})
	if err != nil {
		var te *ebml.TruncatedError
		if errors.As(err, &te) {
			fmt.Printf("error_offset=%d\n", te.Offset)
			fmt.Println("status=corrupt_input")
			return 2
		}
		fmt.Printf("status=error\nerror=%q\n", err.Error())
		return 1
	}
	m := res.Metrics
	fmt.Printf("source_sha256=%s\n", m.SourceSHA256)
	fmt.Printf("recovered_packets=%d\n", m.Recovered)
	fmt.Printf("lost_packets=%d\n", m.Lost)
	fmt.Printf("late_packets=%d\n", m.Late)
	fmt.Printf("target_kbps=%d\n", m.TargetKbps)
	fmt.Printf("actual_kbps=%.2f\n", m.ActualKbps)
	fmt.Printf("bitrate_error_pct=%.2f\n", m.BitrateErrPct)
	fmt.Printf("quality_psnr_db=%.2f\n", m.PSNRdB)
	fmt.Printf("manifest_start_ms=%d\n", m.ManifestStartMs)
	fmt.Printf("timeline_error_ms=%d\n", m.TimelineErrMs)
	fmt.Printf("cache_rejected=%d\n", boolInt(m.CacheRejected))
	fmt.Printf("checkpoint_resumed=%d\n", boolInt(m.Resumed))
	if m.CacheRejected {
		return 3
	}
	return 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
