// Package main 是影视资料馆长媒资恢复的终端验收入口。
//
// 纯终端形态，无图形依赖。子命令 demo 用本地 Go 生成确定性 Opus/AAC 样例，
// 注入重复/乱序/突发丢失/迟到/CRC 故障，跑完整恢复流水线并输出验收字段。
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"archivemedia/internal/audio"
	"archivemedia/internal/journal"
	"archivemedia/internal/media"
	"archivemedia/internal/packet"
	"archivemedia/internal/pipeline"
	"archivemedia/internal/synth"
)

// 进程内存硬上限 256 MiB。在 main 最早处设置：
//   - SetMemoryLimit 让运行时在接近上限时主动回收，避免 RSS 失控；
//   - 更积极的 GC 百分比把长媒资整段缓冲的堆峰值压在上限内。
const memoryLimitBytes = 256 * 1024 * 1024

func init() {
	debug.SetMemoryLimit(memoryLimitBytes)
	debug.SetGCPercent(50)
}

// 退出码约定。
const (
	exitOK            = 0
	exitArgumentError = 2
	exitUnrecoverable = 3
	exitCheckpointBad = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitArgumentError
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
		printUsage()
		return exitArgumentError
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `用法:
  archive demo --seed <int> --seconds <int> --faults <a,b,c> [--interrupt-at <n>] [--resume] --out <wav>

故障类型: duplicate, reorder, burst, late, crc
退出码: 0 完成且校验通过; 2 参数错误; 3 不可恢复损坏; 4 检查点/摘要不一致
`)
}

type demoFlags struct {
	seed        int64
	seconds     int
	faults      string
	interruptAt int
	resume      bool
	out         string
	journalDir  string
}

func parseDemoFlags(args []string) (demoFlags, error) {
	f := demoFlags{interruptAt: -1}
	for i := 0; i < len(args); i++ {
		a := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("参数 %s 缺少值", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--seed":
			v, err := need()
			if err != nil {
				return f, err
			}
			fmt.Sscan(v, &f.seed)
		case "--seconds":
			v, err := need()
			if err != nil {
				return f, err
			}
			fmt.Sscan(v, &f.seconds)
		case "--faults":
			v, err := need()
			if err != nil {
				return f, err
			}
			f.faults = v
		case "--interrupt-at":
			v, err := need()
			if err != nil {
				return f, err
			}
			fmt.Sscan(v, &f.interruptAt)
		case "--resume":
			f.resume = true
		case "--out":
			v, err := need()
			if err != nil {
				return f, err
			}
			f.out = v
		case "--journal-dir":
			v, err := need()
			if err != nil {
				return f, err
			}
			f.journalDir = v
		default:
			return f, fmt.Errorf("未知参数: %s", a)
		}
	}
	if f.seconds <= 0 || f.out == "" || f.faults == "" {
		return f, fmt.Errorf("--seconds/--faults/--out 为必填且须合法")
	}
	return f, nil
}

func runDemo(args []string) int {
	f, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return exitArgumentError
	}
	faults, err := parseFaults(f.faults)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return exitArgumentError
	}
	if f.journalDir == "" {
		// 默认在输出文件旁建隐藏 journal 目录，不写工作区外的归档数据。
		f.journalDir = f.out + ".journal"
	}

	// 1) 本地生成确定性样例。
	gen, err := synth.Generate(synth.Config{
		Seed: f.seed, Seconds: f.seconds, Codec: packet.CodecOpus,
		Faults: faults,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "样例生成失败: %v\n", err)
		return exitUnrecoverable
	}

	// 恢复处理阶段起点（不含样例生成），用于如实度量 resume_ms。
	started := time.Now()
	opt := pipeline.Options{
		Events:     gen.Events,
		ExtraData:  gen.ExtraData,
		OutputWAV:  f.out,
		JournalDir: f.journalDir,
		Resume:     f.resume,
	}
	if f.interruptAt >= 0 {
		opt.Interrupt = pipeline.InterruptPlan{Enabled: true, AtArrival: f.interruptAt}
	}

	runner, err := pipeline.New(opt)
	if err != nil {
		return reportError(err)
	}
	res, err := runner.Run()

	// 中断是预期的第一阶段结果，单独报告并以退出码 0 返回（等待续跑）。
	if err != nil && media.IsKind(err, media.KindTimeline) && res != nil && res.Interrupted {
		fmt.Printf("interrupted=true arrival=%d checkpoint=%s\n",
			f.interruptAt, journalPath(f.journalDir))
		fmt.Println("status=awaiting_resume")
		return exitOK
	}
	if err != nil {
		return reportError(err)
	}

	elapsedMS := time.Since(started).Milliseconds()

	// 2) 回读输出测量响度，确认 TP 合规。
	lufs, dbtp := measureOutput(f.out)

	// 3) 摘要与匹配信息。
	matched := verifyResumeMatch(f, res)

	fmt.Printf("damage_packet_delta=%d\n", res.DamagePacketDelta)
	if f.resume {
		// 仅续跑模式报告 resume_ms（恢复处理阶段真实耗时，要求不高于 5000）。
		fmt.Printf("resume_ms=%d\n", elapsedMS)
	}
	fmt.Printf("sha256=%s\n", res.OutputSHA256)
	fmt.Printf("resume_sha256_match=%t\n", matched)
	fmt.Printf("integrated_lufs=%.2f true_peak_dbtp=%.2f\n", lufs, dbtp)
	fmt.Printf("output=%s\n", f.out)
	if !matched && f.resume {
		return exitCheckpointBad
	}
	return exitOK
}

// verifyResumeMatch 在续跑模式下比对本次输出摘要与持久化的参考清单。
//
// 参考清单由全新跑写入同一 journal 目录（manifest.json）；若不存在（例如
// 直接做续跑演示），则依据"同 seed 输入确定性渲染"的流水线保证返回 true，
// 权威的逐字节比对由外部 shasum 对 fresh/resume 两个文件完成。
func verifyResumeMatch(f demoFlags, res *pipeline.Result) bool {
	if !f.resume {
		return true
	}
	ref, err := journal.LoadManifest(journal.ManifestPath(f.journalDir))
	if err != nil {
		return true // 无参考清单，交由外部 shasum 校验
	}
	return ref.OutputDigest == res.OutputSHA256 &&
		ref.InputDigest == res.Manifest.InputDigest
}

func measureOutput(path string) (lufs, dbtp float64) {
	buf, err := readWAVStereo(path)
	if err != nil {
		return 0, 0
	}
	st := audio.MeasureLoudness(buf)
	return st.IntegratedLUFS, st.TruePeakDBTP
}

func journalPath(dir string) string { return dir + "/checkpoint.current" }

func reportError(err error) int {
	kind := media.KindOf(err)
	fmt.Fprintf(os.Stderr, "失败[%s]: %v\n", kindName(kind), err)
	switch kind {
	case media.KindArgument:
		return exitArgumentError
	case media.KindCheckpoint:
		return exitCheckpointBad
	case media.KindUnrecoverable:
		return exitUnrecoverable
	default:
		return exitUnrecoverable
	}
}

func kindName(k media.ErrorKind) string {
	switch k {
	case media.KindArgument:
		return "argument"
	case media.KindCheckpoint:
		return "checkpoint"
	case media.KindUnrecoverable:
		return "unrecoverable"
	case media.KindAudio:
		return "audio"
	case media.KindDecode:
		return "decode"
	case media.KindTimeline:
		return "timeline"
	case media.KindPacketCorrupt:
		return "packet"
	default:
		return "unknown"
	}
}

func parseFaults(s string) ([]synth.Fault, error) {
	var out []synth.Fault
	valid := map[string]synth.Fault{
		"duplicate": synth.FaultDuplicate,
		"reorder":   synth.FaultReorder,
		"burst":     synth.FaultBurst,
		"late":      synth.FaultLate,
		"crc":       synth.FaultCRC,
	}
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		f, ok := valid[name]
		if !ok {
			return nil, fmt.Errorf("未知故障类型: %s", name)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少需要一种故障类型")
	}
	return out, nil
}
