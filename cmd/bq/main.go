// Command bq is the terminal entry point for the broadcast material QC
// pipeline. It generates self-describing fixtures (demo) and verifies an
// existing manifest (verify), printing JSON evidence and using the documented
// exit codes: 0 pass, 1 threshold exceeded, 2 unrecoverable input, 64 args.
package main

import (
	"fmt"
	"os"
)

const usage = `bq — 播出素材时序核对与再封装工具

用法:
  bq demo   --minutes N --seed S [--inject-loss R] [--inject-wrap]
            [--inject-dup] [--inject-trunc] [--anchor-conflicts N] --out DIR
  bq verify --manifest PATH [--json]

退出码:
  0  通过        1  指标超限      2  不可恢复输入      64  参数错误
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(64)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n%s", os.Args[1], usage)
		os.Exit(64)
	}
	if err != nil {
		if ce, ok := err.(*cliError); ok {
			fmt.Fprintln(os.Stderr, ce.Error())
			os.Exit(ce.code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type cliError struct {
	code int
	msg  string
}

func (e *cliError) Error() string { return e.msg }

func argError(format string, args ...any) error {
	return &cliError{code: 64, msg: "参数错误: " + fmt.Sprintf(format, args...)}
}

type flags struct {
	values map[string]string
	bools  map[string]bool
}

func parseFlags(args []string, wantValue map[string]bool) (*flags, error) {
	f := &flags{values: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if len(tok) < 3 || tok[:2] != "--" {
			return nil, argError("无法识别的参数 %q", tok)
		}
		key := tok[2:]
		if wantValue[key] {
			if i+1 >= len(args) {
				return nil, argError("--%s 缺少取值", key)
			}
			f.values[key] = args[i+1]
			i++
		} else {
			f.bools[key] = true
		}
	}
	return f, nil
}
