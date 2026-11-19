package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"pairflow/internal/clock"
	"pairflow/internal/pipeline"
	"pairflow/internal/rtp"
	"pairflow/internal/testkit"
)

// runDemo 自生成乱序/重复/突发丢失/截断头/回绕/缓存超限样例并跑通全流水线，
// 输出可复现摘要与失败样例定位信息。
func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "确定性随机种子")
	out := fs.String("out", "", "输出目录")
	trace := fs.String("bandwidth-trace", "changing", "带宽轨迹 steady|changing")
	cache := fs.Int64("cache-bytes", pipeline.MaxTrackCacheBytes, "单轨缓存上限字节")
	bitrate := fs.Int64("target-bitrate", 192000, "目标码率 bps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "demo: 需要 --out")
		os.Exit(exitInputError)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	fx := testkit.GenerateFixture(*seed, *trace)

	// 每个设备一个采集文件，路径全部限定在输出目录内。
	var files []string
	for _, cap := range fx.Captures {
		name := "capture-" + cap.Spec.DeviceID + ".pfc"
		p, err := pipeline.SafeJoin(*out, name)
		if err != nil {
			return err
		}
		if err := writeCapture(p, cap.Records); err != nil {
			return err
		}
		files = append(files, p)
	}

	res, err := pipeline.Run(pipeline.Options{
		OutDir: *out, CacheBytes: *cache, InputFiles: files,
		TargetBitrate: *bitrate,
	})
	// 即使存在可恢复媒体错误，也先输出摘要，再决定退出码。
	printDemoSummary(fx, res)
	if err != nil {
		return err
	}
	return nil
}

func writeCapture(path string, recs []rtp.CaptureRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return clock.Wrap("demo", clock.KindPathEscape, "", 0, clock.NoSeq,
			"create capture failed", "检查输出目录", err)
	}
	defer f.Close()
	if err := rtp.WriteCaptureHeader(f, time.Now().UnixNano()); err != nil {
		return err
	}
	for _, r := range recs {
		if err := rtp.WriteCaptureRecord(f, r); err != nil {
			return err
		}
	}
	return f.Sync()
}

// printDemoSummary 输出验收要求的可复现摘要与失败样例定位。
func printDemoSummary(fx *testkit.Fixture, res *pipeline.RunResult) {
	s := fx.Spec
	fmt.Printf("seed=%d bandwidth_trace=%s tracks=%d\n", s.Seed, s.BandwidthTrace, len(s.Devices))
	fmt.Printf("duplicate_packets=%d\n", s.DuplicatePackets)
	fmt.Printf("duplicate_conflicts=%d\n", s.ConflictPackets)
	fmt.Printf("reordered_packets=%d\n", s.ReorderSwaps)
	var burst int64
	var repaired, checkpoints int64
	var truncated, drift, lost []string
	for _, t := range res.Tracks {
		st := t.ReordererStats()
		burst += sumGapSamples(st.Gaps)
		checkpoints += st.CacheCheckpoints
		repaired += int64(t.RepairedCount())
	}
	for _, b := range s.TruncatedBlocks {
		truncated = append(truncated, fmt.Sprintf("%s#%d", b.DeviceID, b.Index))
	}
	for _, b := range s.DriftBlocks {
		drift = append(drift, fmt.Sprintf("%s#%d", b.DeviceID, b.Index))
	}
	for _, b := range s.LostBlocks {
		lost = append(lost, fmt.Sprintf("%s#%d(seq=%d)", b.DeviceID, b.Index, b.RTPSeq))
	}
	fmt.Printf("burst_loss_samples=%d\n", burst)
	fmt.Printf("repaired_timestamps=%d\n", repaired)
	fmt.Printf("cache_checkpoints=%d\n", checkpoints)
	fmt.Printf("ts_wrap_devices=%v seq_wrap_devices=%v\n", s.TSWrapDevices, s.SeqWrapDevices)
	fmt.Printf("truncated_blocks=%v\n", truncated)
	fmt.Printf("drift_blocks=%v\n", drift)
	fmt.Printf("lost_blocks=%v\n", lost)
	if res != nil {
		fmt.Printf("stage_errors=%d index=%s manifest=%s\n",
			len(res.TrackErrors), res.IndexPath, res.ManifestPath)
		for _, e := range res.TrackErrors {
			fmt.Printf("  failure stage=%s kind=%s device=%q offset=%d seq=%d recovery=%s\n",
				e.Stage, e.Kind, e.DeviceID, e.Offset, e.Seq, e.Recovery)
		}
	}
}

func sumGapSamples(gaps []rtp.Gap) int64 {
	var n int64
	for _, g := range gaps {
		n += g.EstSamples
	}
	return n
}
