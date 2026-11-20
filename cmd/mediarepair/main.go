// Command mediarepair 是纯终端媒体时间线修复工具。
//
// 子命令：
//
//	fixture 生成自包含夹具（MP4 / RTP dump / 时间线 sidecar / 截断样例）
//	repair  解析、修复时间戳、重建关键帧索引并输出报告
//	verify  校验输入结构并在失败时定位原始字节偏移
//
// 退出码：0 通过；1 素材或校验失败；2 参数错误；3 运行时 I/O 或取消。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"mediarepair/internal/fault"
)

const usage = `mediarepair — 多轨媒体时间线修复与关键帧索引

用法:
  mediarepair fixture [--out DIR] [--pattern MODE] [--video N] [--audio N]
  mediarepair repair  --input FILE [--timeline FILE] [--report text|json]
                      [--buffer N] [--slow-consumer-ms N] [--policy clamp|keep]
                      [--drift-ppm N] [--resume-from SEG] [--out FILE]
  mediarepair verify  --input FILE [--timeline FILE]

退出码: 0 通过 / 1 素材或校验失败 / 2 参数错误 / 3 运行时 I/O 或取消
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "fixture":
		return cmdFixture(args[1:], stdout, stderr)
	case "repair":
		return cmdRepair(args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n\n%s", args[0], usage)
		return 2
	}
}

// exitCodeOf 把错误映射为约定的退出码。
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		switch fe.Stage {
		case fault.StageIO:
			return 3
		case fault.StageParse, fault.StageRepair, fault.StageIndex, fault.StageVerify, fault.StageReport:
			return 1
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 3
	}
	return 1
}

// reportError 输出错误的人类可读定位行。
func reportError(stderr io.Writer, err error) {
	var fe *fault.Error
	if errors.As(err, &fe) {
		fmt.Fprintf(stderr, "error_stage=%s source_offset=%d track=%d recoverable=%v message=%q\n",
			fe.Stage, fe.Offset, fe.TrackID, fe.Recoverable, fe.Msg)
		return
	}
	fmt.Fprintf(stderr, "error=%q\n", err.Error())
}

// asFault 提取错误链中的 *fault.Error。
func asFault(err error) (*fault.Error, bool) { return fault.As(err) }

// defaultTimelinePath 返回输入同目录下的 timeline.json。
func defaultTimelinePath(input string) string {
	dir := "."
	for i := len(input) - 1; i >= 0; i-- {
		if input[i] == '/' || input[i] == '\\' {
			dir = input[:i]
			break
		}
	}
	return dir + "/timeline.json"
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
