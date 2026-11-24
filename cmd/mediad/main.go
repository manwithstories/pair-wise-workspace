// Command mediad 是广播节目交付流水线的纯终端入口，
// 提供本地自包含演示（demo）与捕获文件批处理（batch）两个子命令。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"media-pipeline/internal/engine"
	"media-pipeline/internal/fault"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return fault.ExitInput
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "batch":
		return runBatch(args[1:])
	case "-h", "--help", "help":
		usage()
		return fault.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return fault.ExitInput
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediad — 广播节目交付流水线（纯本地、无外部服务）

用法:
  mediad demo  --seed N --out DIR [--queue N] [--resume]
  mediad batch --in DIR --out DIR [--queue N]

退出码: 0 全部校验通过 | 2 输入损坏/参数错误 |
        3 不可恢复的检查点/校验和冲突 | 1 内部处理失败
`)
}

// demoFlags 解析 demo 子命令参数。
type demoFlags struct {
	seed   uint64
	out    string
	queue  int
	resume bool
}

func parseDemo(args []string) (demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var d demoFlags
	fs.Uint64Var(&d.seed, "seed", 0, "确定性随机种子")
	fs.StringVar(&d.out, "out", "", "输出目录（用户明确给定的本地目录）")
	fs.IntVar(&d.queue, "queue", 4, "有界管线队列容量")
	fs.BoolVar(&d.resume, "resume", false, "演示中断后从检查点续跑")
	if err := fs.Parse(args); err != nil {
		return d, err
	}
	if d.out == "" {
		return d, fmt.Errorf("--out is required")
	}
	if d.queue <= 0 {
		return d, fmt.Errorf("--queue must be > 0")
	}
	return d, nil
}

func runDemo(args []string) int {
	d, err := parseDemo(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		return fault.ExitInput
	}
	if err := ensureLocalDir(d.out); err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		return fault.ExitInput
	}
	cfg := engine.DefaultConfig()
	cfg.Queue = d.queue

	res, err := engine.Run(d.seed, d.out, cfg, d.resume)
	if err != nil {
		return reportError(err)
	}
	if err := writeArtifacts(d.out, res); err != nil {
		return reportError(err)
	}
	printScenarioLines(res)
	printSummary(res)
	return fault.ExitOK
}

// ensureLocalDir 校验并创建输出目录，输入路径限定在用户明确给定的本地目录。
func ensureLocalDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("empty output directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve output dir: %w", err)
	}
	// 拒绝指向远程/非本地形态的路径，保持纯本地约束。
	if strings.HasPrefix(abs, "//") || strings.Contains(abs, "://") {
		return fmt.Errorf("output must be a local directory: %s", abs)
	}
	return os.MkdirAll(abs, 0o755)
}

func reportError(err error) int {
	code := fault.ExitCode(err)
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return code
}
