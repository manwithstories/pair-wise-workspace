// archivecheck 是省级影像档案恢复的纯终端验收入口。
// 退出码：0 全部校验通过；1 输入或边界校验失败；2 参数错误。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"archivecheck/internal/checkpoint"
	"archivecheck/internal/fixture"
	"archivecheck/internal/pipeline"
	"archivecheck/internal/transport"
)

const (
	exitOK     = 0
	exitVerify = 1
	exitArgs   = 2
)

type cliArgs struct {
	frames       int
	duration     string
	bitrates     []string
	driftPPM     int
	wrapAt       uint64
	lossEvery    int
	corruptFrame int
	checkpoint   string
	scenario     string
}

func parseArgs(args []string) (cliArgs, error) {
	fs := flag.NewFlagSet("archivecheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var a cliArgs
	var bitrates, wrap string
	fs.IntVar(&a.frames, "frames", 12000, "生成帧数")
	fs.StringVar(&a.duration, "duration", "2s", "目标分片时长（如 2s）")
	fs.StringVar(&bitrates, "bitrates", "360p,720p", "逗号分隔的表示层，至少两层")
	fs.IntVar(&a.driftPPM, "drift-ppm", 180, "采集时钟漂移（ppm）")
	fs.StringVar(&wrap, "wrap-at", "4294960000", "RTP 时间戳回绕模")
	fs.IntVar(&a.lossEvery, "loss-every", 97, "每 N 个传输位置丢一个包，0 为不丢")
	fs.IntVar(&a.corruptFrame, "corrupt-frame", 8111, "损坏样例帧号，-1 为不注入")
	fs.StringVar(&a.checkpoint, "checkpoint", "", "检查点文件路径，提供则启用续跑")
	fs.StringVar(&a.scenario, "scenario", "demo",
		"demo=成功路径+自生成样例；corrupt|tail|drift=仅跑对应故障路径；resume=续跑演示")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	for _, b := range strings.Split(bitrates, ",") {
		if b = strings.TrimSpace(b); b != "" {
			a.bitrates = append(a.bitrates, b)
		}
	}
	if len(a.bitrates) < 2 {
		return a, fmt.Errorf("至少需要两个表示层，got %q", bitrates)
	}
	if a.frames <= 0 {
		return a, fmt.Errorf("--frames 必须为正数, got %d", a.frames)
	}
	v, err := strconv.ParseUint(wrap, 10, 64)
	if err != nil || v == 0 {
		return a, fmt.Errorf("--wrap-at 非法: %q", wrap)
	}
	a.wrapAt = v
	if _, err := parseDurationNS(a.duration); err != nil {
		return a, err
	}
	switch a.scenario {
	case "demo", "corrupt", "tail", "drift", "resume":
	default:
		return a, fmt.Errorf("--scenario 非法: %q", a.scenario)
	}
	return a, nil
}

// parseDurationNS 只接受整数秒/毫秒/微秒/纳秒后缀，全部换算为整数纳秒。
func parseDurationNS(s string) (int64, error) {
	multipliers := []struct {
		suf string
		mul int64
	}{
		{"ns", 1}, {"us", 1000}, {"ms", 1_000_000}, {"s", 1_000_000_000},
	}
	for _, m := range multipliers {
		if strings.HasSuffix(s, m.suf) {
			v, err := strconv.ParseInt(strings.TrimSuffix(s, m.suf), 10, 64)
			if err != nil || v <= 0 {
				break
			}
			return v * m.mul, nil
		}
	}
	return 0, fmt.Errorf("--duration 非法（示例 2s/500ms）: %q", s)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	a, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return exitArgs
	}
	durNS, _ := parseDurationNS(a.duration)
	base := pipeline.Config{
		Params: fixture.Params{
			Frames:       a.frames,
			Reps:         a.bitrates,
			DriftPPM:     a.driftPPM,
			WrapAt:       a.wrapAt,
			CorruptFrame: -1, // 主验收路径不注入损坏；损坏仅出现在自生成样例
			KeyInterval:  60,
			AudioRate:    48000,
			RTPClock:     90000,
			VFR:          true,
		},
		SegmentTargetNS: durNS,
		BoundaryTolNS:   100_000_000,
		WindowSize:      64,
		Fault: transport.FaultConfig{
			LossEvery:    a.lossEvery,
			ReorderEvery: 131,
			DupeEvery:    53,
		},
		CheckpointPath: a.checkpoint,
	}
	switch a.scenario {
	case "demo":
		return runDemo(base, a)
	case "resume":
		return runResume(base)
	case "corrupt", "tail", "drift":
		return runSingleScenario(base, a.scenario)
	default:
		return exitArgs
	}
}

// runDemo 执行主验收、三类自生成故障样例与续跑一致性核验。
func runDemo(base pipeline.Config, a cliArgs) int {
	fmt.Println("== 主验收路径（成功路径）==")
	main, err := pipeline.Run(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "主验收运行失败: %v\n", err)
		return exitVerify
	}
	printReport(main)
	if !main.Published {
		return exitVerify
	}

	fmt.Println("\n== 自生成故障样例（故障路径不发布清单，等效退出码 1）==")
	samples := []struct {
		name   string
		mutate func(*pipeline.Config)
	}{
		{"single_frame_corrupt", func(c *pipeline.Config) { c.Params.CorruptFrame = a.corruptFrame }},
		{"tail_truncation", func(c *pipeline.Config) { c.TailTruncate = true }},
		{"drift_jump", func(c *pipeline.Config) { c.CrossModuleFault = true }},
	}
	for _, sc := range samples {
		c := base
		c.CheckpointPath = ""
		sc.mutate(&c)
		r, err := pipeline.Run(c)
		fmt.Printf("[%s] ", sc.name)
		switch {
		case err != nil:
			fmt.Printf("error=%v published=false exit_code=1\n", err)
		case r.FirstError != nil:
			fmt.Printf("error_frame=%d fault=%s published=false exit_code=1\n",
				r.FirstError.Seq, r.FirstError.Fault)
		case !r.Published:
			fmt.Printf("rejected=%s boundary_mismatches=%d published=false exit_code=1\n",
				r.RejectedReason, r.BoundaryMiss)
		default:
			fmt.Printf("recovered=true manifest=%s\n", r.ManifestHash)
		}
	}

	fmt.Println("\n== 检查点续跑一致性 ==")
	resumed := runResumeQuiet(base)
	if resumed != main.ManifestHash {
		fmt.Fprintf(os.Stderr, "续跑哈希不一致: %s != %s\n", resumed, main.ManifestHash)
		return exitVerify
	}
	fmt.Printf("resume_manifest_sha256 %s\n", resumed)
	fmt.Println("resume_hash_matches    true")
	return exitOK
}

// runSingleScenario 仅运行一个故障路径并按规则非零退出。
func runSingleScenario(base pipeline.Config, scenario string) int {
	c := base
	switch scenario {
	case "corrupt":
		c.Params.CorruptFrame = base.Params.Frames / 2
	case "tail":
		c.TailTruncate = true
	case "drift":
		c.CrossModuleFault = true
	}
	r, err := pipeline.Run(c)
	if err == pipeline.ErrCheckpointMismatch {
		fmt.Fprintf(os.Stderr, "拒绝续跑: %v\n", err)
		return exitVerify
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
		return exitVerify
	}
	printReport(r)
	if r.Published {
		fmt.Fprintln(os.Stderr, "故障路径不应发布清单")
		return exitVerify
	}
	return exitVerify
}

// runResume 演示真实断点续跑并打印续跑清单哈希。
func runResume(base pipeline.Config) int {
	hash := runResumeQuiet(base)
	if hash == "" {
		return exitVerify
	}
	fmt.Printf("resume_manifest_sha256 %s\n", hash)
	return exitOK
}

// runResumeQuiet 写入匹配检查点后二次运行，返回续跑清单哈希。
func runResumeQuiet(base pipeline.Config) string {
	dir, err := os.MkdirTemp("", "archivecheck-cp-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	cpPath := filepath.Join(dir, "checkpoint.json")
	inSum := fixture.InputDigest(base.Params)
	pmSum := pipeline.ParamsHash(base)
	if err := checkpoint.Save(cpPath, checkpoint.State{
		InputDigest:  hexEncode(inSum[:]),
		ParamsDigest: hexEncode(pmSum[:]),
		FramesDone:   base.Params.Frames,
		Stage:        "segment",
	}); err != nil {
		return ""
	}
	c := base
	c.CheckpointPath = cpPath
	r, err := pipeline.Run(c)
	if err != nil || !r.CheckpointUsed || !r.Published {
		return ""
	}
	return r.ManifestHash
}

func hexEncode(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0x0f]
	}
	return string(out)
}

func printReport(r pipeline.Report) {
	fmt.Printf("sync_error_ms       %.6f\n", float64(r.SyncErrorNS)/1e6)
	fmt.Printf("loss_recovery_rate  %.4f\n", r.RecoveryRate)
	fmt.Printf("p95_latency_ms      %.6f\n", float64(r.P95LatencyNS)/1e6)
	fmt.Printf("segments            %d\n", r.Segments)
	fmt.Printf("boundary_mismatches %d\n", r.BoundaryMiss)
	fmt.Printf("manifest_sha256     %s\n", r.ManifestHash)
	if r.FirstError != nil {
		fmt.Printf("first_error_frame   %d\n", r.FirstError.Seq)
		fmt.Printf("first_error_fault   %s\n", r.FirstError.Fault)
	}
	if r.RejectedReason != "" {
		fmt.Printf("rejected_reason     %s\n", r.RejectedReason)
	}
}
