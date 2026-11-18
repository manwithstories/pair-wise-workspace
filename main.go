package main

import (
	"fmt"
	"os"
)

// 退出码契约：0 成功；2 输入错误；3 数据不足以可靠拟合；4 读写失败。
const (
	exitOK         = 0
	exitInput      = 2
	exitUnreliable = 3
	exitIO         = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitInput
	}
	switch args[0] {
	case "generate":
		return cmdGenerate(args[1:])
	case "analyze":
		return cmdAnalyze(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", args[0])
		usage()
		return exitInput
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `avsync — 独立时钟起始偏差与漂移分析工具

用法:
  avsync generate --seed <int> --seconds <int> --out <file.jsonl>
      生成含计数器回绕与异常锚点的样例 JSONL（真值随终端输出给出）
  avsync analyze --in <file.jsonl> [--json <out.json>] [--csv <out.csv>]
                 [--tb-num <int> --tb-den <int>]
      解绕、配对、拟合 a = αv + β，并输出校正时间戳序列
      目标时间基默认 1/1000000000（纳秒），最近偶数舍入

退出码: 0 成功 | 2 输入错误 | 3 不可可靠拟合 | 4 读写失败`)
}

// ---- 子命令占位，后续批次替换为真实实现 ----

func cmdGenerate(args []string) int {
	fmt.Fprintln(os.Stderr, "generate: not implemented yet")
	return exitInput
}

func cmdAnalyze(args []string) int {
	fmt.Fprintln(os.Stderr, "analyze: not implemented yet")
	return exitInput
}
