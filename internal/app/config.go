// Package app 组合样例生成、摄入、时间线、检查点、渲染和比对，供 CLI 与验收测试共用。
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"mediaarchive/internal/fault"
)

// Config 是 archivecheck 的命令行配置。
type Config struct {
	Scenario       string
	Seed           int64
	Events         int
	DamageAt       int
	InterruptAfter int
	Out            string
	Resume         bool
	Compare        string
}

// ParseArgs 解析参数并把用法错误映射为 fault.KindUsage。
func ParseArgs(args []string) (Config, *fault.Error) {
	fs := flag.NewFlagSet("archivecheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c Config
	fs.StringVar(&c.Scenario, "scenario", "clean", "clean|damage|recovery|fatal")
	fs.Int64Var(&c.Seed, "seed", 41, "确定性随机种子")
	fs.IntVar(&c.Events, "events", 12000, "生成并处理的源事件数，必须为 3 的倍数")
	fs.IntVar(&c.DamageAt, "damage-at", 6001, "损坏定位目标包序号")
	fs.IntVar(&c.InterruptAfter, "interrupt-after", 0, "处理到该事件后原子保存检查点并返回 75")
	fs.StringVar(&c.Out, "out", "", "输出目录")
	fs.BoolVar(&c.Resume, "resume", false, "从输出目录检查点续跑")
	fs.StringVar(&c.Compare, "compare", "", "与另一次正常路径的 hashes.json 比对")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return c, fault.New(fault.KindUsage, "help requested", nil)
		}
		return c, fault.Wrap(fault.KindUsage, "参数解析失败", nil, err)
	}
	if fs.NArg() != 0 {
		return c, fault.New(fault.KindUsage, fmt.Sprintf("存在未识别位置参数: %v", fs.Args()), nil)
	}
	if c.Out == "" {
		return c, fault.New(fault.KindUsage, "必须指定 --out 目录", nil)
	}
	if c.Events < 3 || c.Events > 200000 {
		return c, fault.New(fault.KindUsage, "--events 必须在 3..200000 且为 3 的倍数", nil)
	}
	if c.Events%3 != 0 {
		return c, fault.New(fault.KindUsage, "--events 必须能被 tracks=3 均分", nil)
	}
	if c.DamageAt < 0 || c.DamageAt > c.Events {
		return c, fault.New(fault.KindUsage, "--damage-at 超出事件范围", nil)
	}
	if c.InterruptAfter < 0 || c.InterruptAfter > c.Events {
		return c, fault.New(fault.KindUsage, "--interrupt-after 超出事件范围", nil)
	}
	switch c.Scenario {
	case "clean", "damage", "recovery", "fatal":
	default:
		return c, fault.New(fault.KindUsage, "未知 --scenario；支持 clean/damage/recovery/fatal", nil)
	}
	if c.Resume && c.InterruptAfter > 0 {
		return c, fault.New(fault.KindUsage, "--resume 不能同时指定 --interrupt-after", nil)
	}
	return c, nil
}

// Result 是一次完整运行的可测试结果。
type Result struct {
	Config           Config
	ExitCode         int
	Tracks           int
	Events           int
	ExpectedPacket   int
	LocatedPacket    int
	OffsetError      int64
	CheckpointEvents int
	ResumedEvents    int
	LateReplays      int64
	FramesExported   int
	FramesHash       string
	ManifestHash     string
	StateHash        string
	ParamsHash       string
	RecoveryMS       int64
	CompareMatch     bool
	DamageOffset     int64
	LocatedOffset    int64
	StartedAt        time.Time
}

// paths 集中管理输出文件。
type paths struct {
	out           string
	checkpoint    string
	manifest      string
	hashes        string
	report        string
	compareHashes string
}

func outputPaths(out, compare string) paths {
	return paths{
		out:           out,
		checkpoint:    filepath.Join(out, "checkpoint.json"),
		manifest:      filepath.Join(out, "manifest.json"),
		hashes:        filepath.Join(out, "hashes.json"),
		report:        filepath.Join(out, "report.json"),
		compareHashes: filepath.Join(compare, "hashes.json"),
	}
}

// checkContext 在长阶段之间响应取消。
func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
