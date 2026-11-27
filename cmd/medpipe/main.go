// medpipe 是纯终端的多声道 PCM/WAV 流水线：生成样例、模拟弱网、
// 流式重排合轨、离屏渲染 JPEG 帧序列并写出交付复核清单。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"medpipe/internal/clock"
	"medpipe/internal/fixture"
	"medpipe/internal/ingest"
	"medpipe/internal/manifest"
	"medpipe/internal/pipeline"
)

// 退出码约定：0 验证通过；2 输入损坏或冲突；75 保留检查点后中断。
const (
	exitOK          = 0
	exitCorrupt     = 2
	exitInterrupted = 75
)

type demoFlags struct {
	seed      int64
	tracks    int
	seconds   int
	fps       int64
	targetBPS int64
	out       string
	fault     string
	stopAfter int
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitCorrupt)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "resume":
		err = runResume(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		usage()
		os.Exit(exitCorrupt)
	}
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	switch clock.CodeOf(err) {
	case clock.CodeInterrupted:
		os.Exit(exitInterrupted)
	case clock.CodeInvalidHeader, clock.CodeConflict, clock.CodeChecksum,
		clock.CodeTruncated, clock.CodeOversize, clock.CodePathRejected,
		clock.CodeCheckpoint, clock.CodeLatePacket:
		os.Exit(exitCorrupt)
	default:
		os.Exit(exitCorrupt)
	}
}

func parseFlags(args []string) (demoFlags, error) {
	f := demoFlags{seed: 41, tracks: 3, seconds: 4, fps: 10, targetBPS: 1_000_000}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return f, clock.NewError(clock.CodeInvalidParam, -1, "无法解析参数: %s", a)
		}
		if i+1 >= len(args) {
			return f, clock.NewError(clock.CodeInvalidParam, -1, "参数 %s 缺少值", a)
		}
		val := args[i+1]
		i++
		switch a {
		case "--seed":
			f.seed, _ = strconv.ParseInt(val, 10, 64)
		case "--tracks":
			f.tracks, _ = strconv.Atoi(val)
		case "--seconds":
			f.seconds, _ = strconv.Atoi(val)
		case "--fps":
			f.fps, _ = strconv.ParseInt(val, 10, 64)
		case "--target-bps":
			f.targetBPS, _ = strconv.ParseInt(val, 10, 64)
		case "--out":
			f.out = val
		case "--fault":
			f.fault = val
		case "--stop-after":
			f.stopAfter, _ = strconv.Atoi(val)
		default:
			return f, clock.NewError(clock.CodeInvalidParam, -1, "未知参数: %s", a)
		}
	}
	if f.out == "" {
		return f, clock.NewError(clock.CodeInvalidParam, -1, "必须指定 --out")
	}
	return f, nil
}

func runDemo(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	inputDir := filepath.Join(f.out, "input")
	spec := fixture.Spec{
		Seed: f.seed, Tracks: f.tracks, Seconds: f.seconds, SampleRate: 48000,
		FPS: int(f.fps), BlockFrames: 4800, Bits: 16,
	}
	plan, err := fixture.Generate(inputDir, spec)
	if err != nil {
		return err
	}
	// 文件级损坏注入（截断/半帧/坏头）；网络级故障走 Fault 枚举。
	switch ingest.Fault(f.fault) {
	case ingest.FaultTruncate, ingest.FaultHalfFrame, ingest.FaultBadHeader:
		if _, err := fixture.Corrupt(inputDir, plan, ingest.Fault(f.fault)); err != nil {
			return err
		}
	}
	opts := pipeline.Options{
		Seed: f.seed, Tracks: f.tracks, Seconds: f.seconds, FPSNum: f.fps, FPSDen: 1,
		TargetBPS: f.targetBPS, OutDir: f.out, InputDir: inputDir,
		Fault: ingest.Fault(f.fault), StopAfter: f.stopAfter,
	}
	// 先持久化运行参数，保证中断后 resume 能复原完全相同的配置。
	if err := pipeline.SaveOptions(f.out, opts); err != nil {
		return err
	}
	r, err := pipeline.New(opts)
	if err != nil {
		return err
	}
	res, err := r.Run()
	if err != nil {
		if clock.CodeOf(err) == clock.CodeInterrupted {
			fmt.Printf("status=interrupted blocks=%d checkpoint=%s\n",
				f.stopAfter, filepath.Join(f.out, "checkpoint.json"))
		}
		return err
	}
	printSummary(res)
	return nil
}

func runResume(args []string) error {
	out, err := requireOut(args)
	if err != nil {
		return err
	}
	// 从已保存的运行参数恢复（run.json 由 demo 写出）。
	opts, err := pipeline.LoadOptions(out)
	if err != nil {
		return err
	}
	opts.OutDir = out
	opts.InputDir = filepath.Join(out, "input")
	opts.Resume = true
	opts.StopAfter = 0
	r, err := pipeline.New(opts)
	if err != nil {
		return err
	}
	res, err := r.Run()
	if err != nil {
		return err
	}
	printSummary(res)
	return nil
}

func runVerify(args []string) error {
	dir, err := requireOut(args)
	if err != nil {
		return err
	}
	mf, err := manifest.Load(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	if err := mf.Verify(dir); err != nil {
		return err
	}
	fmt.Printf("verify=ok frames=%d frame_sha256=%s\n", mf.Summary.Frames, mf.Summary.FrameSHA256)
	fmt.Printf("clipped_samples=%d missing_samples=%d manifest_max_error_samples=%d\n",
		mf.Summary.ClippedSamples, mf.Summary.MissingSamples, mf.Summary.MaxErrorSamples)
	return nil
}

func printSummary(res *pipeline.Result) {
	s := res.Summary
	fmt.Printf("frames=%d\n", s.Frames)
	fmt.Printf("target_bps=%d actual_bps=%d deviation_pct=%.2f\n",
		s.TargetBPS, s.ActualBPS, s.DeviationPct)
	fmt.Printf("psnr_db=%.2f clipped_samples=%d missing_samples=%d\n",
		s.PSNRMean, s.ClippedSamples, s.MissingSamples)
	fmt.Printf("manifest_max_error_samples=%d frame_sha256=%s\n",
		s.MaxErrorSamples, s.FrameSHA256)
	fmt.Printf("compensated_frames=%d\n", s.CompensatedFrames)
	if len(res.Network.Dropped) > 0 {
		fmt.Printf("network_dropped=%d reordered=%d\n",
			len(res.Network.Dropped), res.Network.Reordered)
	}
}

func requireOut(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			return args[i+1], nil
		}
		if args[i] == "--out" && i+1 < len(args) {
			return args[i+1], nil
		}
	}
	return "", clock.NewError(clock.CodeInvalidParam, -1, "必须指定 --dir/--out")
}

func usage() {
	fmt.Fprint(os.Stderr, `medpipe — 多声道 PCM/WAV 离屏流水线

用法:
  medpipe demo   --seed N --tracks N --seconds N --fps N --target-bps N --out DIR [--fault M] [--stop-after N]
  medpipe resume --out DIR
  medpipe verify --dir DIR

故障模式: reorder | burst-loss | flapping | duplicate | conflict | truncate | half-frame | bad-header
退出码: 0=通过 2=损坏/冲突 75=保留检查点后中断
`)
}
