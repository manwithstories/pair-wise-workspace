package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"rcx/internal/report"
)

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	runDir := fs.String("run", "", "本次运行输出目录（含 run.json）")
	baseDir := fs.String("baseline", "", "基线运行输出目录")
	jsonOut := fs.String("json", "", "JSON 报告落盘路径")
	psnrThresh := fs.Float64("psnr-thresh", 1.0, "PSNR 回归阈值（dB）")
	bitrateThresh := fs.Float64("bitrate-thresh", 5.0, "码率偏差阈值（%）")
	if err := fs.Parse(args); err != nil {
		return ExitInvalid
	}
	if *runDir == "" {
		fmt.Fprintln(os.Stderr, "缺少 --run")
		return ExitInvalid
	}
	run, err := report.LoadRun(filepath.Join(*runDir, "run.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取运行报告失败:", err)
		return ExitInvalid
	}
	var diffs []report.SegDiff
	verdict := run.Verdict
	if *baseDir != "" {
		base, err := report.LoadRun(filepath.Join(*baseDir, "run.json"))
		if err != nil {
			// 基线缺失不阻断：仅提示，按本次运行自身指标判定。
			fmt.Fprintln(os.Stderr, "警告: 基线不可用，跳过回归比对:", err)
		} else {
			d, v := report.Compare(run, base, *psnrThresh, *bitrateThresh)
			diffs = d
			if v == "FAIL" {
				verdict = "FAIL"
			}
		}
	}
	run.Verdict = verdict
	report.PrintTable(os.Stdout, run, diffs)
	if *jsonOut != "" {
		if dir := filepath.Dir(*jsonOut); dir != "." {
			os.MkdirAll(dir, 0o755)
		}
		if err := report.SaveJSON(*jsonOut, run); err != nil {
			fmt.Fprintln(os.Stderr, "写 JSON 失败:", err)
			return ExitFailed
		}
		fmt.Println("JSON 报告已写入", *jsonOut)
	}
	if verdict != "PASS" {
		return ExitRegression
	}
	return ExitOK
}
