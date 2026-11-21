// mediadeliver 是音乐节多机位交付流水线的验收入口。
// 用法：mediadeliver demo --seed 42 --sample-rate 48000 --channels 2 \
//        --target-lufs -16 --rtp-window 32
// 退出码：0 三条路径及阈值均通过；1 指标不达标；2 参数或输入错误。
package main

import (
	"flag"
	"fmt"
	"os"

	"mediadeliver/internal/fixture"
	"mediadeliver/internal/pipeline"
	"mediadeliver/internal/report"
)

const peakCeiling = -1.0 // dBTP

func main() { os.Exit(run(os.Args)) }

func run(args []string) int {
	if len(args) < 2 || args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediadeliver demo [--seed N] [--sample-rate R] [--channels C] [--target-lufs L] [--rtp-window W]")
		return 2
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "fixture seed")
	rate := fs.Int64("sample-rate", 48000, "target sample rate")
	ch := fs.Int("channels", 2, "target channels")
	lufs := fs.Float64("target-lufs", -16, "target loudness")
	window := fs.Int("rtp-window", 32, "rtp reorder window")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if *rate <= 0 || *ch <= 0 || *ch > 8 || *window <= 0 || *lufs >= 0 {
		fmt.Fprintln(os.Stderr, "invalid parameters")
		return 2
	}
	cfg := pipeline.Config{SampleRate: *rate, Channels: *ch, TargetLUFS: *lufs,
		PeakCeiling: peakCeiling, RTPWindow: *window, RTPClockRate: 48000}
	set := fixture.Generate(*seed, 4, 2.0)

	ok := true
	ok = runNormal(cfg, set) && ok
	ok = runCorrupt(cfg, set) && ok
	ok = runRecover(cfg, set) && ok
	if !ok {
		return 1
	}
	return 0
}

func summarize(path string, res *pipeline.Result) report.Summary {
	rec := 0
	for _, s := range res.Segments {
		if s.Recovered {
			rec++
		}
	}
	return report.Summary{
		Path:               path,
		SyncErrorMs:        res.SyncErrorMs,
		PacketRecoveryRate: res.RTPStats.RecoveryRate(),
		P95LatencyMs:       report.P95Ms(res.Latencies),
		ParsedSegments:     len(res.Playlist.Segments),
		RecoveredSegments:  rec,
		FirstErrorOffset:   res.Faults.FirstOffset(),
		OutputSHA256:       res.OutputDigest(),
		Faults:             res.Faults,
	}
}

func emit(s report.Summary) {
	if err := report.WriteJSON(os.Stdout, s); err != nil {
		fmt.Fprintln(os.Stderr, "write summary:", err)
	}
}

func runNormal(cfg pipeline.Config, set fixture.Set) bool {
	j := pipeline.Job{ManifestName: "live.m3u8", ManifestData: set.ManifestGood,
		Segments: set.Segments, RTPPackets: set.RTPGood}
	res := pipeline.Run(cfg, j, nil)
	s := summarize("normal", res)
	emit(s)
	pass := s.SyncErrorMs <= 5 && s.PacketRecoveryRate >= 0.80 && s.P95LatencyMs <= 250
	fmt.Fprintf(os.Stderr, "normal path: pass=%v\n", pass)
	return pass
}

func runCorrupt(cfg pipeline.Config, set fixture.Set) bool {
	segs := append([]pipeline.SegmentData(nil), set.Segments...)
	segs[set.CorruptIndex].Fragment = set.Corrupt
	j := pipeline.Job{ManifestName: "live.m3u8", ManifestData: set.ManifestGood,
		Segments: segs, RTPPackets: set.RTPGood}
	res := pipeline.Run(cfg, j, nil)
	s := summarize("corrupt", res)
	emit(s)
	pass := s.FirstErrorOffset >= 0
	fmt.Fprintf(os.Stderr, "corrupt path: pass=%v\n", pass)
	return pass
}

func runRecover(cfg pipeline.Config, set fixture.Set) bool {
	j := pipeline.Job{ManifestName: "live.m3u8", ManifestData: set.ManifestTrunc,
		Segments: set.Segments, RTPPackets: set.RTPLossy}
	j.Segments[set.ShortTailIdx].Audio = set.ShortTailAudio
	res := pipeline.Run(cfg, j, nil)
	// 重复运行：前段复用且摘要一致
	res2 := pipeline.Run(cfg, j, res)
	s := summarize("recover", res)
	emit(s)
	pass := s.RecoveredSegments >= 1 &&
		s.PacketRecoveryRate >= 0.80 &&
		res.OutputDigest() == res2.OutputDigest() &&
		res2.Segments[0].Reused
	fmt.Fprintf(os.Stderr, "recover path: pass=%v\n", pass)
	return pass
}
