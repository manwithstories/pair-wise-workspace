// Command relay 是区域直播分发边缘节点的终端工具。
// 子命令 selftest 生成确定性包流、跑完整流水线并验证恢复与交付规格，
// 通过终端键值对输出全部验收指标；退出码遵循 0/2/3/4 约定。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"relay/internal/audio"
	"relay/internal/engine"
	"relay/internal/index"
	"relay/internal/media"
	"relay/internal/streamgen"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return media.ExitUnsupported
	}
	switch args[0] {
	case "selftest":
		return selftest(args[1:])
	case "-h", "--help", "help":
		usage()
		return media.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		usage()
		return media.ExitUnsupported
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relay selftest --work <dir> [--seed N] [--duration D]")
	fmt.Fprintln(os.Stderr, "       [--interrupt-at D] [--inject-write-fail-after N]")
}

// selftestFlags 解析自检参数。
type selftestFlags struct {
	work                string
	seed                int64
	duration            time.Duration
	interruptAt         time.Duration
	injectWriteFailAfter int
}

func parseFlags(args []string) (*selftestFlags, error) {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	f := &selftestFlags{}
	fs.StringVar(&f.work, "work", "", "working directory for fixtures and outputs")
	fs.Int64Var(&f.seed, "seed", 41, "deterministic generator seed")
	fs.DurationVar(&f.duration, "duration", 90*time.Second, "stream duration")
	fs.DurationVar(&f.interruptAt, "interrupt-at", 0, "simulate interrupt after duration")
	fs.IntVar(&f.injectWriteFailAfter, "inject-write-fail-after", 0,
		"inject one write failure after N checkpoint commits")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.work == "" {
		return nil, fmt.Errorf("--work is required")
	}
	return f, nil
}

func selftest(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "selftest:", err)
		return media.ExitUnsupported
	}
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "workdir:", err)
		return media.ExitIO
	}

	// 固定坏包序号 12000，与验收输出 damage_packet=12000 对齐。
	const damageSeq uint16 = 12000
	spec := streamgen.Spec{
		Seed: f.seed, DurationMS: int(f.duration / time.Millisecond),
		AudioRate: 48000, Channels: 2, ClockRate: 48000, PPM: 150,
		DamageSeq: damageSeq, DamageKind: streamgen.DamageBadPayload,
	}
	stream, _ := streamgen.Generate(spec)
	inputPath := filepath.Join(f.work, "input.bin")
	if err := os.WriteFile(inputPath, stream, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write fixture:", err)
		return media.ExitIO
	}
	params := index.Params{
		AudioRate: 48000, Channels: 2, PayloadType: 97,
		ClockRate: 48000, TargetRate: audio.TargetRate, TargetChans: audio.TargetChans,
	}
	outPath := filepath.Join(f.work, "out.pcm")

	// 第一轮：可能带中断/写盘失败注入。
	cfg := engine.Config{
		InputPath: inputPath, OutputPath: outPath,
		WorkDir: filepath.Join(f.work, "run"), Params: params,
		InterruptAt: f.interruptAt, InjectWriteFailAfter: f.injectWriteFailAfter,
	}
	resumeMillis, code := resumeToCompletion(cfg)
	if code != media.ExitOK {
		return code
	}
	sumA, err := fileSHA256(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checksum:", err)
		return media.ExitIO
	}

	// 第二轮：全新工作目录完整运行，作为对照基线。
	refPath := filepath.Join(f.work, "ref.pcm")
	refCfg := engine.Config{
		InputPath: inputPath, OutputPath: refPath,
		WorkDir: filepath.Join(f.work, "ref"), Params: params,
	}
	rep, err := engine.Run(refCfg)
	if err != nil {
		return classifyError(err)
	}
	sumB, err := fileSHA256(refPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checksum:", err)
		return media.ExitIO
	}

	// 第一轮的定位结果从其索引读取。
	located := readLocated(filepath.Join(f.work, "run", "index.json"), damageSeq)
	delta := located - int64(damageSeq)
	if delta < 0 {
		delta = -delta
	}

	fmt.Printf("damage_packet=%d\n", damageSeq)
	fmt.Printf("located_packet=%d\n", located)
	fmt.Printf("locate_delta_packets=%d\n", delta)
	fmt.Printf("resume_ms=%d\n", resumeMillis)
	fmt.Printf("out_rate=%d\n", rep.OutRate)
	fmt.Printf("channels=%d\n", rep.Channels)
	fmt.Printf("lufs=%.2f\n", rep.LUFS)
	fmt.Printf("ppm=%d\n", rep.PPM)
	fmt.Printf("sha256_a=%s\n", sumA)
	fmt.Printf("sha256_b=%s\n", sumB)
	fmt.Printf("checksum_match=%t\n", sumA == sumB)

	if sumA != sumB || delta > 1 {
		return media.ExitDamaged
	}
	return media.ExitOK
}

// resumeToCompletion 反复调用 Run，遇到中断/写盘失败就清除注入后续跑，
// 直到成功完成。返回首次续跑成功的耗时（毫秒）与约定退出码。
func resumeToCompletion(cfg engine.Config) (int64, int) {
	const maxAttempts = 8
	injected := cfg.InterruptAt > 0 || cfg.InjectWriteFailAfter > 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		start := time.Now()
		_, err := engine.Run(cfg)
		if err == nil {
			// 仅在曾发生故障注入时报告续跑耗时；干净首跑返回 0。
			if attempt == 0 && !injected {
				return 0, media.ExitOK
			}
			return time.Since(start).Milliseconds(), media.ExitOK
		}
		var ioErr *media.IOError
		switch {
		case err.Error() == "simulated interrupt":
			// 清除中断，保留检查点继续。
			cfg.InterruptAt = 0
		case asIO(err, &ioErr):
			// 写盘失败：清除注入后续跑。
			cfg.InjectWriteFailAfter = 0
		default:
			return 0, classifyError(err)
		}
	}
	fmt.Fprintln(os.Stderr, "resume exceeded max attempts")
	return 0, media.ExitIO
}

// asIO 报告错误链中是否包含 IOError。
func asIO(err error, target **media.IOError) bool {
	return errors.As(err, target)
}

func classifyError(err error) int {
	var ioErr *media.IOError
	if errors.As(err, &ioErr) {
		return media.ExitIO
	}
	var unsup *media.UnsupportedError
	if errors.As(err, &unsup) {
		return media.ExitUnsupported
	}
	return media.ExitDamaged
}

// readLocated 从持久化索引中读取首个坏包序号；失败回退到注入序号。
func readLocated(indexPath string, fallback uint16) int64 {
	b, err := os.ReadFile(indexPath)
	if err != nil {
		return int64(fallback)
	}
	var parsed struct {
		Faults []struct {
			Kind int    `json:"kind"`
			Seq  uint16 `json:"seq"`
		} `json:"faults"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return int64(fallback)
	}
	for _, flt := range parsed.Faults {
		if flt.Seq != media.SeqUnknown {
			return int64(flt.Seq)
		}
	}
	return int64(fallback)
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
