package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"rcx/internal/fixture"
)

func cmdGen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	scene := fs.String("scene", "motion", "场景: gradient|texture|motion|noise")
	seed := fs.Int64("seed", 1, "随机种子")
	frames := fs.Int("frames", 100, "帧数")
	width := fs.Int("width", 320, "宽（8 的倍数）")
	height := fs.Int("height", 240, "高（8 的倍数）")
	out := fs.String("out", "", "输出 .yuv 文件")
	if err := fs.Parse(args); err != nil {
		return ExitInvalid
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "缺少 --out")
		return ExitInvalid
	}
	cfg := fixture.Config{Scene: *scene, Seed: *seed, Width: *width, Height: *height, Frames: *frames}
	framesList, err := fixture.Generate(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数非法:", err)
		return ExitInvalid
	}
	if dir := filepath.Dir(*out); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "创建目录失败:", err)
			return ExitInvalid
		}
	}
	if err := fixture.WriteFile(*out, framesList); err != nil {
		fmt.Fprintln(os.Stderr, "写文件失败:", err)
		return ExitFailed
	}
	fmt.Printf("已生成 %s：scene=%s seed=%d %dx%d frames=%d (%.1f MB)\n",
		*out, *scene, *seed, *width, *height, *frames,
		float64(len(framesList)*fixture.FrameSize(*width, *height))/1e6)
	return ExitOK
}
