// pairflow 是多机位 PCM/WAV 采集回传的时间线修复与随机访问索引交付工具，
// 纯终端运行，无图形界面、无在线传输、无系统权限申请。
package main

import (
	"flag"
	"fmt"
	"os"

	"pairflow/internal/clock"
	"pairflow/internal/pipeline"
)

// 退出码：0 完整通过；2 输入/参数错误；3 不可恢复媒体区间；4 索引/清单校验失败。
const (
	exitOK            = 0
	exitInputError    = 2
	exitMediaError    = 3
	exitVerifyError   = 4
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitInputError)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "scan":
		err = runScan(args)
	case "repair":
		err = runRepair(args)
	case "verify":
		err = runVerify(args)
	case "demo":
		err = runDemo(args)
	case "-h", "--help", "help":
		usage()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(exitInputError)
	}
	if err == nil {
		os.Exit(exitOK)
	}
	os.Exit(mapErrorToExit(err))
}

func mapErrorToExit(err error) int {
	se, ok := clock.AsStageError(err)
	if !ok {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitInputError
	}
	loc := fmt.Sprintf("structured_error stage=%s kind=%s device=%q",
		se.Stage, se.Kind, se.DeviceID)
	if se.Offset != clock.NoOffset {
		loc += fmt.Sprintf(" offset=%d", se.Offset)
	}
	if se.Seq != clock.NoSeq {
		loc += fmt.Sprintf(" seq=%d", se.Seq)
	}
	fmt.Fprintln(os.Stderr, loc)
	fmt.Fprintf(os.Stderr, "message=%s\nrecovery=%s\n", se.Message, se.Recovery)
	switch se.Kind {
	case clock.KindIndexSeek, clock.KindManifestAlignment,
		clock.KindCheckpointMismatch:
		return exitVerifyError
	case clock.KindIndeterminate:
		return exitMediaError
	case clock.KindInputTooLarge, clock.KindPathEscape,
		clock.KindUnsupportedPayload, clock.KindTruncatedHeader:
		return exitInputError
	default:
		return exitMediaError
	}
}

func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	out := fs.String("out", "", "输出目录（采集扫描结果与检查点）")
	cache := fs.Int64("cache-bytes", pipeline.MaxTrackCacheBytes, "单轨缓存上限字节")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if *out == "" || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "scan: 需要 --out 与至少一个采集文件")
		os.Exit(exitInputError)
	}
	res, err := pipeline.Run(pipeline.Options{
		OutDir: *out, CacheBytes: *cache, InputFiles: files,
	})
	if err != nil {
		return err
	}
	fmt.Printf("scan complete tracks=%d stage_errors=%d index=%s\n",
		len(res.Tracks), len(res.TrackErrors), res.IndexPath)
	return nil
}

func runRepair(args []string) error {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	out := fs.String("out", "", "输出目录")
	cache := fs.Int64("cache-bytes", pipeline.MaxTrackCacheBytes, "单轨缓存上限字节")
	bitrate := fs.Int64("target-bitrate", 0, "目标码率 bps（写入清单）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if *out == "" || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "repair: 需要 --out 与至少一个采集文件")
		os.Exit(exitInputError)
	}
	res, err := pipeline.Run(pipeline.Options{
		OutDir: *out, CacheBytes: *cache, InputFiles: files,
		TargetBitrate: *bitrate,
	})
	if err != nil {
		return err
	}
	fmt.Printf("repair complete tracks=%d stage_errors=%d manifest=%s\n",
		len(res.Tracks), len(res.TrackErrors), res.ManifestPath)
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := fs.String("input", "", "交付目录（含 delivery.pfidx）")
	manifestPath := fs.String("manifest", "", "manifest.json 路径")
	bitrate := fs.Int64("target-bitrate", 192000, "目标码率 bps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "verify: 需要 --input 与 --manifest")
		os.Exit(exitInputError)
	}
	rep, err := pipeline.VerifyDirectory(*in, *manifestPath, *bitrate)
	if rep != nil {
		// 即便存在不可恢复媒体区间，已算出的指标仍完整输出。
		for _, m := range rep.Metrics {
			printMetrics(m)
		}
	}
	return err
}

func usage() {
	fmt.Fprint(os.Stderr, `pairflow — 多机位 PCM/WAV 时间线修复与随机访问索引

用法:
  pairflow scan   --out DIR [--cache-bytes N] CAPTURE...
  pairflow repair --out DIR [--cache-bytes N] [--target-bitrate BPS] CAPTURE...
  pairflow verify --input DIR --manifest FILE --target-bitrate BPS
  pairflow demo   --seed N --out DIR [--bandwidth-trace steady|changing] [--cache-bytes N]

退出码: 0 通过 / 2 输入或参数错误 / 3 不可恢复媒体区间 / 4 索引或清单校验失败
`)
}
