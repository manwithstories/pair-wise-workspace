package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asticode/go-astiav"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `mediacore - 跨节点媒体处理内核验证器

用法:
  mediacore verify [flags]

标志:
  --self-generate          由 Go 代码生成夹具（不读取外部媒体）
  --segments N             生成/处理的秒数（默认 12）
  --slow-consumer D        每个片段的消费者延迟，如 40ms
  --memory-limit SIZE      队列内存上限，如 64MiB
  --fault KIND             truncate | timebase | stale-index
  --simulate-crash-after N 处理 N 个片段后模拟崩溃
  --resume                 从已确认检查点恢复
  --work-dir DIR           夹具与索引工作目录（默认临时目录）
  --keep                   保留工作目录
  --input PATH             使用既有媒体文件而非自生成
  --index-dir DIR          索引缓存目录（默认工作目录下 index）

退出码: 0 完成；2 损坏样例已隔离并报告；1 内部或输入输出错误。
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 1
	}
	switch args[0] {
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 1
	}
}

// verifyFlags is the parsed verify request.
type verifyFlags struct {
	selfGenerate bool
	segments     int
	slowConsumer time.Duration
	memoryLimit  string
	fault        string
	crashAfter   int
	resume       bool
	workDir      string
	keep         bool
	input        string
	indexDir     string
}

func parseVerifyFlags(args []string, stderr io.Writer) (verifyFlags, error) {
	var f verifyFlags
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&f.selfGenerate, "self-generate", false, "generate fixtures from Go code")
	fs.IntVar(&f.segments, "segments", 12, "seconds to generate and process")
	fs.DurationVar(&f.slowConsumer, "slow-consumer", 0, "per-segment consumer delay")
	fs.StringVar(&f.memoryLimit, "memory-limit", "64MiB", "queue memory budget")
	fs.StringVar(&f.fault, "fault", "", "truncate|timebase|stale-index")
	fs.IntVar(&f.crashAfter, "simulate-crash-after", 0, "simulate a crash after N segments")
	fs.BoolVar(&f.resume, "resume", false, "resume from the last acknowledged checkpoint")
	fs.StringVar(&f.workDir, "work-dir", "", "working directory")
	fs.BoolVar(&f.keep, "keep", false, "keep the working directory")
	fs.StringVar(&f.input, "input", "", "existing media file")
	fs.StringVar(&f.indexDir, "index-dir", "", "index cache directory")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.segments <= 0 || f.segments > 600 {
		return f, fmt.Errorf("--segments must be in [1,600]")
	}
	if f.crashAfter < 0 {
		return f, fmt.Errorf("--simulate-crash-after must be >= 0")
	}
	switch f.fault {
	case "", "truncate", "timebase", "stale-index":
	default:
		return f, fmt.Errorf("unknown --fault %q", f.fault)
	}
	if !f.selfGenerate && f.input == "" {
		return f, fmt.Errorf("either --self-generate or --input is required")
	}
	return f, nil
}

// ParseSize parses suffixes KiB/MiB/GiB and plain bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	units := []struct {
		suffix string
		mul    int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q: %w", s, err)
			}
			if n <= 0 {
				return 0, fmt.Errorf("size must be positive: %q", s)
			}
			return n * u.mul, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	f, err := parseVerifyFlags(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "mediacore: %v\n", err)
		return 1
	}
	limit, err := ParseSize(f.memoryLimit)
	if err != nil {
		fmt.Fprintf(stderr, "mediacore: --memory-limit: %v\n", err)
		return 1
	}
	// FFmpeg diagnostics would drown the terminal report; errors are still
	// surfaced through return values.
	astiav.SetLogLevel(astiav.LogLevelQuiet)

	rep, err := execute(f, limit, stderr)
	if rep != nil {
		rep.print(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mediacore: %v\n", err)
		return 1
	}
	if rep != nil && rep.Quarantined > 0 {
		return 2
	}
	return 0
}
