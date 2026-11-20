// Command media-audit 是多码率母版分片边界审计工具的终端入口。
// 纯本地运行，无图形界面，所有功能通过子命令参数与终端输出完成。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"media-audit/internal/manifest"
	"media-audit/internal/pipeline"
	"media-audit/internal/stream"
)

// 退出码约定（验收要求）。
const (
	exitOK            = 0 // 完成且校验通过
	exitCorruption    = 2 // 输入或索引损坏已精确定位
	exitInvalidState  = 3 // 恢复状态无效或输出校验不一致
	exitConfigOrIOErr = 4 // 参数或本地 I/O 错误
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitConfigOrIOErr
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "audit":
		return runAudit(args[1:])
	case "version":
		fmt.Println("media-audit 1.0.0")
		return exitOK
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
		usage()
		return exitConfigOrIOErr
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `media-audit — 多码率母版分片边界审计

用法:
  media-audit demo [参数]      自生成临时素材并演示完整审计与恢复
  media-audit audit [参数]     审计指定工作目录的素材
  media-audit version          打印版本

demo 参数:
  --codec <h264|h265|av1>      编码格式（默认 av1）
  --representations <n>        表示层数量（默认 3）
  --frames <n>                 每表示层帧数（默认 18000）
  --block-frames <n>           每扫描块帧数（默认 600）
  --inject-corruption-at <n>   在第 n 帧注入损坏，-1 表示不注入
  --interrupt-after-block <n>  完成第 n 块后模拟中断，-1 表示不中断
  --resume                     启用检查点恢复
  --work-dir <path>            临时输出目录（默认系统临时目录）
`)
}

type demoFlags struct {
	codec          string
	representations int
	frames         int
	blockFrames    int
	corruptAt      int
	interruptBlock int
	resume         bool
	workDir        string
}

func parseDemoFlags(args []string) (*demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	d := &demoFlags{
		codec: "av1", representations: 3, frames: 18000,
		blockFrames: 600, corruptAt: -1, interruptBlock: -1,
	}
	fs.StringVar(&d.codec, "codec", d.codec, "编码格式")
	fs.IntVar(&d.representations, "representations", d.representations, "表示层数量")
	fs.IntVar(&d.frames, "frames", d.frames, "每表示层帧数")
	fs.IntVar(&d.blockFrames, "block-frames", d.blockFrames, "每扫描块帧数")
	fs.IntVar(&d.corruptAt, "inject-corruption-at", d.corruptAt, "损坏注入帧")
	fs.IntVar(&d.interruptBlock, "interrupt-after-block", d.interruptBlock, "中断块号")
	fs.BoolVar(&d.resume, "resume", false, "启用检查点恢复")
	fs.StringVar(&d.workDir, "work-dir", "", "临时输出目录")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return d, nil
}

func runDemo(args []string) int {
	d, err := parseDemoFlags(args)
	if err != nil {
		return exitConfigOrIOErr
	}
	codec, err := stream.ParseCodec(d.codec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return exitConfigOrIOErr
	}
	if d.representations <= 0 || d.frames <= 0 || d.blockFrames <= 0 {
		fmt.Fprintln(os.Stderr, "参数错误: representations/frames/block-frames 必须为正")
		return exitConfigOrIOErr
	}
	workDir := d.workDir
	if workDir == "" {
		workDir, err = os.MkdirTemp("", "media-audit-demo-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "I/O 错误: %v\n", err)
			return exitConfigOrIOErr
		}
	} else {
		workDir, err = filepath.Abs(workDir)
		if err != nil {
			return exitConfigOrIOErr
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "I/O 错误: %v\n", err)
			return exitConfigOrIOErr
		}
	}

	cfg := pipeline.DemoConfig{
		Codec:           codec,
		Representations: d.representations,
		Frames:          d.frames,
		BlockFrames:     d.blockFrames,
		CorruptFrame:    d.corruptAt,
		InterruptBlock:  d.interruptBlock,
		Resume:          d.resume,
		WorkDir:         workDir,
	}

	start := time.Now()
	metrics, m, err := pipeline.Run(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "执行失败: %v\n", err)
		return exitConfigOrIOErr
	}
	metrics.ResumeMS = time.Since(start).Milliseconds()

	// 写出清单并回读校验，校验不一致 => 退出码 3。
	manifestPath := filepath.Join(workDir, "manifest.json")
	if err := manifest.WriteAtomic(manifestPath, m); err != nil {
		fmt.Fprintf(os.Stderr, "清单写出失败: %v\n", err)
		return exitConfigOrIOErr
	}
	got, err := manifest.ReadAtomic(manifestPath)
	if err != nil || got.SHA256 != m.SHA256 {
		fmt.Fprintln(os.Stderr, "输出校验不一致")
		return exitInvalidState
	}

	printMetrics(metrics)
	return metrics.ExitCode
}

// runAudit 是对已落盘素材的审计入口，复用 demo 管线但默认不注入损坏。
func runAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	workDir := fs.String("work-dir", "", "素材与输出工作目录（必填）")
	codecName := fs.String("codec", "av1", "编码格式")
	reps := fs.Int("representations", 3, "表示层数量")
	if err := fs.Parse(args); err != nil {
		return exitConfigOrIOErr
	}
	if *workDir == "" {
		fmt.Fprintln(os.Stderr, "audit 需要 --work-dir")
		return exitConfigOrIOErr
	}
	codec, err := stream.ParseCodec(*codecName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return exitConfigOrIOErr
	}
	abs, err := filepath.Abs(*workDir)
	if err != nil {
		return exitConfigOrIOErr
	}
	cfg := pipeline.DemoConfig{
		Codec: codec, Representations: *reps, Frames: 18000,
		BlockFrames: 600, CorruptFrame: -1, WorkDir: abs, Resume: true,
	}
	metrics, m, err := pipeline.Run(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "审计失败: %v\n", err)
		return exitConfigOrIOErr
	}
	if err := manifest.WriteAtomic(filepath.Join(abs, "manifest.json"), m); err != nil {
		return exitConfigOrIOErr
	}
	printMetrics(metrics)
	return metrics.ExitCode
}

// printMetrics 以稳定的 JSON 行打印验收要求的关键指标。
func printMetrics(m *pipeline.Metrics) {
	out := map[string]interface{}{
		"corrupt_offset_bytes": m.CorruptOffset,
		"location_error_bytes": m.LocationError,
		"resume_ms":            m.ResumeMS,
		"segments":             m.Segments,
		"alignment_max_ms":     m.AlignmentMaxMS,
		"manifest_sha256":      m.ManifestSHA256,
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
}
