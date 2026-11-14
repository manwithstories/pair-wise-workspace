// rcx 是车机端教学视频编码质控工具：夹具生成、分片规划、两遍编码、回归校验。
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 退出码约定。
const (
	ExitOK         = 0 // 通过
	ExitRegression = 2 // 质量回归
	ExitFailed     = 3 // 任务失败，可续跑
	ExitInvalid    = 4 // 参数或状态非法
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return ExitInvalid
	}
	var code int
	switch args[0] {
	case "gen":
		code = cmdGen(args[1:])
	case "plan":
		code = cmdPlan(args[1:])
	case "encode":
		code = cmdEncode(args[1:])
	case "verify":
		code = cmdVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", args[0])
		usage()
		return ExitInvalid
	}
	return code
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法: rcx <子命令> [参数]
  gen     生成确定性 YUV420 夹具
  plan    按帧率与段时长切分并规划码率
  encode  两遍码率控制编码（支持断点续跑）
  verify  聚合报告并与基线回归比对`)
}

// parseBitrate 解析 "900k" / "1.5m" / 纯数字 形式的码率。
func parseBitrate(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := float64(1)
	if strings.HasSuffix(s, "k") {
		mult = 1e3
		s = strings.TrimSuffix(s, "k")
	} else if strings.HasSuffix(s, "m") {
		mult = 1e6
		s = strings.TrimSuffix(s, "m")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("非法码率 %q", s)
	}
	return int64(v * mult), nil
}
