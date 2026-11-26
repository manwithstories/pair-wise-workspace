// Command archive 是归档媒体处理内核的终端验收适配器。
//
// 子命令：
//
//	archive demo   --dir DIR --preset all --interrupt-after N
//	archive resume --dir DIR
//	archive verify --dir DIR --max-bitrate-delta P --max-align-ms M
//
// 退出码：0 通过；2 参数/输入无效；3 模拟中断可续跑；4 不可恢复媒体错误；
// 5 量化门限失败。纯终端形态，不开窗口、不依赖浏览器、不申请设备权限。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"archivemedia/internal/fixture"
	"archivemedia/internal/manifest"
	"archivemedia/internal/model"
	"archivemedia/internal/pipeline"
)

// 退出码常量（与任务验收约定一致）。
const (
	exitOK            = 0
	exitInvalidInput  = 2
	exitInterrupted   = 3
	exitUnrecoverable = 4
	exitThreshold     = 5
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitInvalidInput
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", args[0])
		usage()
		return exitInvalidInput
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: archive {demo|resume|verify} [flags]")
}

// emit 打印一行 JSON；序列化失败时回退为错误事件，绝不输出空白行。
func emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"emit_error": err.Error()})
	}
	fmt.Println(string(b))
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory (required)")
	preset := fs.String("preset", "all", "fixture preset (all)")
	interrupt := fs.Int("interrupt-after", 0, "simulate interrupt after N committed segments")
	if err := fs.Parse(args); err != nil {
		return exitInvalidInput
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "demo: --dir is required")
		return exitInvalidInput
	}
	if *preset != "all" {
		fmt.Fprintf(os.Stderr, "demo: unsupported preset %q\n", *preset)
		return exitInvalidInput
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: bad dir: %v\n", err)
		return exitInvalidInput
	}

	// 每次 demo 重建样例；既有检查点由 resume 复用，demo 中断后可直接 resume。
	sources, err := fixture.GenerateAll(absDir)
	if err != nil {
		return mapError(err)
	}
	mkv, err := fixture.GenerateMKV(absDir)
	if err != nil {
		return mapError(err)
	}
	sources = append(sources, mkv)

	cfg := pipeline.DefaultConfig(absDir)
	cfg.InterruptAfter = *interrupt
	runner, err := pipeline.New(cfg)
	if err != nil {
		return mapError(err)
	}
	res, err := runner.Run(sources)
	printOutcomes(res)
	if res.Report != nil {
		emit(res.Report)
	}
	if err != nil {
		return mapError(err)
	}
	emit(map[string]string{"event": "demo_complete", "manifest": res.ManifestSha})
	return exitOK
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory (required)")
	if err := fs.Parse(args); err != nil {
		return exitInvalidInput
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "resume: --dir is required")
		return exitInvalidInput
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return exitInvalidInput
	}
	// 续跑重新生成确定性样例（字节一致），已提交片段经幂等键短路。
	sources, err := fixture.GenerateAll(absDir)
	if err != nil {
		return mapError(err)
	}
	mkv, err := fixture.GenerateMKV(absDir)
	if err != nil {
		return mapError(err)
	}
	sources = append(sources, mkv)

	runner, err := pipeline.New(pipeline.DefaultConfig(absDir))
	if err != nil {
		return mapError(err)
	}
	res, err := runner.Run(sources)
	printOutcomes(res)
	if res.Report != nil {
		emit(res.Report)
	}
	if err != nil {
		return mapError(err)
	}
	emit(map[string]string{"event": "resume_complete", "manifest": res.ManifestSha})
	return exitOK
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory (required)")
	maxDelta := fs.Float64("max-bitrate-delta", 8, "max bitrate delta percent")
	maxAlignMs := fs.Int64("max-align-ms", 1, "max alignment error in ms")
	minPsnrDb := fs.Float64("min-psnr-db", 32, "min acceptable PSNR in dB")
	if err := fs.Parse(args); err != nil {
		return exitInvalidInput
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "verify: --dir is required")
		return exitInvalidInput
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return exitInvalidInput
	}
	m, err := manifest.Read(absDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		return exitInvalidInput
	}
	// 从已原子提交的清单重算核验指标（只读）。
	report, err := verifyFromManifest(absDir, m, *maxDelta, *maxAlignMs, *minPsnrDb)
	if err != nil {
		emit(report)
		return mapError(err)
	}
	emit(report)
	manifestSha, _ := manifest.ContentSha256(*m)
	emit(map[string]string{"event": "verify_complete", "manifest": manifestSha})
	return exitOK
}

// printOutcomes 逐素材打印处理结局 JSON 行（含损坏恢复与隔离信息）。
func printOutcomes(res *pipeline.Result) {
	for i := range res.Outcomes {
		emit(res.Outcomes[i])
	}
}

// mapError 把内核分类错误映射到约定退出码。
func mapError(err error) int {
	switch model.KindOf(err) {
	case model.KindInterrupted:
		return exitInterrupted
	case model.KindUnsupportedFormat, model.KindCorruptMedia:
		return exitUnrecoverable
	case model.KindThreshold:
		return exitThreshold
	default:
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitInvalidInput
	}
}
