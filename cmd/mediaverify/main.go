// mediaverify 批量音频交付校验工具：扫描建索引、流式执行、
// 输出机器可读 JSON 行。退出码：0 成功，2 错误，3 质量门限失败。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mediaverify/internal/audio"
	"mediaverify/internal/fault"
	"mediaverify/internal/fixture"
	"mediaverify/internal/manifest"
	"mediaverify/internal/report"
	"mediaverify/internal/run"
	"mediaverify/internal/scan"
)

const (
	exitOK      = 0
	exitError   = 2
	exitQuality = 3
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mediaverify <demo|scan|run> [flags]")
		return exitError
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "scan":
		return cmdScan(args[1:])
	case "run":
		return cmdRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		return exitError
	}
}

// fail 按错误类型映射退出码：响度/峰值门限 → 3，其余可定位错误 → 2。
func fail(err error) int {
	var ge *audio.GainError
	if errors.As(err, &ge) {
		fmt.Fprintln(os.Stderr, "quality gate:", ge.Err)
		return exitQuality
	}
	if fe, ok := fault.As(err); ok {
		fmt.Fprintln(os.Stderr, "error:", fe)
	} else {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return exitError
}

// qualityGate 正常样例门限：码率偏差 ≤10%，时间对齐误差 ≤20ms。
func qualityGate(m report.Metrics) int {
	if m.BitrateErrorPct > 10 || m.BitrateErrorPct < -10 {
		fmt.Fprintf(os.Stderr, "quality gate: bitrate error %.2f%% exceeds 10%%\n", m.BitrateErrorPct)
		return exitQuality
	}
	if m.ManifestErrorMs > 20 {
		fmt.Fprintf(os.Stderr, "quality gate: manifest error %.2f ms exceeds 20 ms\n", m.ManifestErrorMs)
		return exitQuality
	}
	return exitOK
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "fixture seed")
	target := fs.Int("target-kbps", 256, "target bitrate")
	rate := fs.Int("rate", 48000, "output sample rate")
	ch := fs.Int("channels", 2, "output channels")
	loud := fs.Float64("loudness", -16, "target loudness (LUFS)")
	loss := fs.Int("loss-burst", 4, "RTP loss burst size")
	bw := fs.Int("bandwidth-kbps", 900, "read bandwidth limit")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	dir, err := os.MkdirTemp("", "mediaverify-demo-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)

	// 1. 生成夹具（正常节目 + 损坏样例）
	opts := fixture.DefaultOptions(*seed)
	opts.LossBurst = *loss
	prog, err := fixture.Generate(dir, opts)
	if err != nil {
		return fail(err)
	}
	if _, err := fixture.GenerateCorrupt(filepath.Join(dir, "corrupt"), opts); err != nil {
		return fail(err)
	}

	// 2. 扫描建索引
	mpd, _ := os.ReadFile(prog.MPDPath)
	mp, err := manifest.ParseDASH(prog.MPDPath, strings.NewReader(string(mpd)))
	if err != nil {
		return fail(err)
	}
	idx, err := scan.Build(mp, dir, prog.MPDPath)
	if err != nil {
		return fail(err)
	}
	indexPath := filepath.Join(dir, "index.json")
	if err := idx.Save(indexPath); err != nil {
		return fail(err)
	}

	params := run.Params{
		Dir: dir, ManifestPath: prog.MPDPath, IndexPath: indexPath,
		OutputPath: filepath.Join(dir, "out.mp4"), CheckpointPath: filepath.Join(dir, "checkpoint.json"),
		TargetKbps: *target, OutRate: *rate, OutChannels: *ch, TargetLUFS: *loud,
		BandwidthKbps: *bw, BandwidthDropAt: len(prog.Packets) / 2, BandwidthDropKbps: *bw / 10,
		Packets: prog.Packets,
		Sleep:   func(time.Duration) {}, // 演示不真实等待；限速节奏由测试覆盖
	}

	// 3. 正常样例：流式执行并输出 JSON 行
	res, err := run.Execute(params)
	if err != nil {
		return fail(err)
	}
	res.Metrics.Scenario = "normal"
	fmt.Println(res.Metrics.JSON())
	code := qualityGate(res.Metrics)

	// 4. 损坏样例：截断分段 → 可定位错误（预期内，作为演示输出）
	corruptDir := filepath.Join(dir, "corrupt", "trunc")
	cparams := params
	cparams.Dir = corruptDir
	cparams.ManifestPath = filepath.Join(corruptDir, "prog.mpd")
	cparams.IndexPath = filepath.Join(corruptDir, "index.json")
	cparams.OutputPath = filepath.Join(corruptDir, "out.mp4")
	cparams.CheckpointPath = filepath.Join(corruptDir, "checkpoint.json")
	cparams.Packets = nil
	cmpd, _ := os.ReadFile(cparams.ManifestPath)
	cmp, cerr := manifest.ParseDASH(cparams.ManifestPath, strings.NewReader(string(cmpd)))
	if cerr == nil {
		var cidx *scan.Index
		cidx, cerr = scan.Build(cmp, corruptDir, cparams.ManifestPath)
		if cerr == nil {
			cerr = cidx.Save(cparams.IndexPath)
		}
	}
	cm := report.Metrics{Scenario: "corrupt", TargetKbps: *target, ResumedFromSeg: -1}
	if cerr != nil {
		cm.Error = cerr.Error()
		cm.RecoveryAction = "fail"
	} else if _, rerr := run.Execute(cparams); rerr != nil {
		cm.Error = rerr.Error()
		if fe, ok := fault.As(rerr); ok {
			cm.RecoveryAction = string(fe.Recovery)
		}
	}
	fmt.Println(cm.JSON())

	// 5. 恢复样例：中断后续跑，展示摘要校验与续跑边界
	rparams := params
	rparams.OutputPath = filepath.Join(dir, "out_resume.mp4")
	rparams.CheckpointPath = filepath.Join(dir, "checkpoint_resume.json")
	rparams.StopAfterSegments = 2
	if _, err := run.Execute(rparams); !errors.Is(err, run.ErrInterrupted) {
		return fail(fmt.Errorf("expected interruption, got %v", err))
	}
	rparams.StopAfterSegments = 0
	rparams.Resume = true
	rres, err := run.Execute(rparams)
	if err != nil {
		return fail(err)
	}
	rres.Metrics.Scenario = "resumed"
	fmt.Println(rres.Metrics.JSON())

	return code
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "DASH MPD or HLS m3u8 path")
	dir := fs.String("dir", ".", "segment directory")
	out := fs.String("index", "index.json", "index output path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	b, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fail(err)
	}
	var prog *manifest.Program
	if strings.HasSuffix(*manifestPath, ".mpd") {
		prog, err = manifest.ParseDASH(*manifestPath, strings.NewReader(string(b)))
	} else {
		prog, err = manifest.ParseHLS(*manifestPath, strings.NewReader(string(b)))
	}
	if err != nil {
		return fail(err)
	}
	idx, err := scan.Build(prog, *dir, *manifestPath)
	if err != nil {
		return fail(err)
	}
	if err := idx.Save(*out); err != nil {
		return fail(err)
	}
	fmt.Printf(`{"scanned_segments":%d,"input_digest":%q}`+"\n", len(idx.Segments), idx.InputDigest)
	return exitOK
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "manifest path")
	dir := fs.String("dir", ".", "segment directory")
	index := fs.String("index", "index.json", "index path")
	out := fs.String("out", "out.mp4", "output path")
	ckpt := fs.String("checkpoint", "", "checkpoint path")
	resume := fs.Bool("resume", false, "resume from checkpoint")
	target := fs.Int("target-kbps", 256, "target bitrate")
	rate := fs.Int("rate", 48000, "output sample rate")
	ch := fs.Int("channels", 2, "output channels")
	loud := fs.Float64("loudness", -16, "target loudness")
	lossStop := fs.Bool("loss-stop", false, "stop on loss burst instead of filling silence")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	res, err := run.Execute(run.Params{
		Dir: *dir, ManifestPath: *manifestPath, IndexPath: *index,
		OutputPath: *out, CheckpointPath: *ckpt,
		TargetKbps: *target, OutRate: *rate, OutChannels: *ch,
		TargetLUFS: *loud, LossStop: *lossStop, Resume: *resume,
	})
	if err != nil {
		return fail(err)
	}
	fmt.Println(res.Metrics.JSON())
	return qualityGate(res.Metrics)
}
