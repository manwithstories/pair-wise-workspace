package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"rcx/internal/rc"
)

func cmdPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	input := fs.String("input", "", "输入 .yuv 文件")
	fps := fs.Int("fps", 25, "帧率")
	segSec := fs.Int("segment", 2, "段时长（秒）")
	bitrate := fs.String("bitrate", "", "目标码率，如 900k")
	mode := fs.String("mode", rc.ModeCapped, "模式: cbr|capped")
	width := fs.Int("width", 320, "宽")
	height := fs.Int("height", 240, "高")
	out := fs.String("out", "", "输出 plan.json")
	if err := fs.Parse(args); err != nil {
		return ExitInvalid
	}
	if *input == "" || *out == "" || *bitrate == "" {
		fmt.Fprintln(os.Stderr, "缺少必填参数 --input/--bitrate/--out")
		return ExitInvalid
	}
	bps, err := parseBitrate(*bitrate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitInvalid
	}
	info, err := os.Stat(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取输入失败:", err)
		return ExitInvalid
	}
	frameSize := int64(*width) * int64(*height) * 3 / 2
	if info.Size()%frameSize != 0 {
		fmt.Fprintf(os.Stderr, "文件大小 %d 与分辨率 %dx%d 不匹配\n", info.Size(), *width, *height)
		return ExitInvalid
	}
	nFrames := int(info.Size() / frameSize)
	plan, err := rc.BuildPlan(*input, *width, *height, *fps, *segSec, nFrames, bps, *mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数非法:", err)
		return ExitInvalid
	}
	data, _ := json.MarshalIndent(plan, "", "  ")
	if dir := filepath.Dir(*out); dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写计划失败:", err)
		return ExitFailed
	}
	fmt.Printf("已生成 %s：frames=%d segments=%d mode=%s bitrate=%dbps 段长=%d帧\n",
		*out, nFrames, len(plan.Segments), *mode, bps, plan.SegmentFrames)
	return ExitOK
}
