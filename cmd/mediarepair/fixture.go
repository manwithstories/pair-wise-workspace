package main

import (
	"fmt"
	"io"

	"mediarepair/internal/fixture"
)

// cmdFixture 生成自包含夹具并打印路径摘要。
func cmdFixture(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("fixture")
	out := fs.String("out", "./tmp", "夹具输出目录")
	pattern := fs.String("pattern", string(fixture.PatternClean), "时间戳模式: clean|clipjump|drift|wrap|duplicate|regression")
	video := fs.Int("video", 120, "视频样本数 [8,4096]")
	audio := fs.Int("audio", 60, "音频样本数 [0,4096]")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "参数错误: 多余的参数 %v\n", fs.Args())
		return 2
	}
	spec := fixture.Spec{
		VideoSamples: *video, AudioSamples: *audio,
		VideoTimescale: 90_000, AudioTimescale: 48_000, Pattern: fixture.Pattern(*pattern),
	}
	if err := spec.Validate(); err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return 2
	}
	files, err := fixture.Generate(*out, spec)
	if err != nil {
		reportError(stderr, err)
		return exitCodeOf(err)
	}
	fmt.Fprintf(stdout, "fixture_dir=%s\n", *out)
	fmt.Fprintf(stdout, "timeline=%s\ncorrupt=%s\nsidecar=%s\nrtp_dump=%s\n",
		files.Timeline, files.Corrupt, files.Sidecar, files.RTPDump)
	fmt.Fprintf(stdout, "pattern=%s video_samples=%d audio_samples=%d\n", spec.Pattern, spec.VideoSamples, spec.AudioSamples)
	for _, kind := range fixture.CorruptKinds {
		fmt.Fprintf(stdout, "corrupt kind=%s mp4=%s timeline=%s\n", kind, files.Corrupts[kind], files.CorruptSides[kind])
	}
	return 0
}
