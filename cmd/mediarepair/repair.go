package main

import (
	"fmt"
	"io"
	"os"

	"mediarepair/internal/pipeline"
	"mediarepair/internal/repair"
	"mediarepair/internal/report"
)

// cmdRepair 运行完整流水线并按报告格式输出。
func cmdRepair(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("repair")
	input := fs.String("input", "", "输入 MP4 路径（必填）")
	timeline := fs.String("timeline", "", "时间线 sidecar 路径（默认与输入同目录 timeline.json）")
	format := fs.String("report", "text", "报告格式: text|json")
	buffer := fs.Int("buffer", 8, "有界队列容量")
	slow := fs.Int("slow-consumer-ms", 0, "消费者每样本延迟毫秒，用于演示背压")
	policy := fs.String("policy", "clamp", "修复策略: clamp|keep")
	drift := fs.Int64("drift-ppm", 0, "时钟漂移修正 ppm")
	resume := fs.String("resume-from", "", "从最近验证边界（片段 ID）续跑")
	outPath := fs.String("out", "", "把修复后样本流写入文件（可选）")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return 2
	}
	if *input == "" {
		fmt.Fprintln(stderr, "参数错误: --input 必填")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "参数错误: 未知报告格式 %q\n", *format)
		return 2
	}
	if *buffer < 1 {
		fmt.Fprintf(stderr, "参数错误: --buffer 必须 >= 1\n")
		return 2
	}
	if *slow < 0 {
		fmt.Fprintf(stderr, "参数错误: --slow-consumer-ms 必须 >= 0\n")
		return 2
	}
	strat := repair.DefaultStrategy()
	switch *policy {
	case "clamp":
	case "keep":
		strat = repair.AuditStrategy()
	default:
		fmt.Fprintf(stderr, "参数错误: 未知策略 %q\n", *policy)
		return 2
	}
	strat.DriftPPM = *drift

	var sink io.Writer
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(stderr, "error_stage=io message=%q\n", err.Error())
			return 3
		}
		defer f.Close()
		sink = f
	}

	ctx, cancel := signalContext()
	defer cancel()
	out, err := pipeline.Run(pipeline.Options{
		Input: *input, TimelinePath: *timeline, Buffer: *buffer, SlowConsumer: *slow,
		Strategy: strat, ResumeFrom: *resume, SampleSink: sink, Context: ctx,
	})
	if err != nil {
		if out != nil && out.Result != nil {
			printReport(stdout, out.Result, *format)
		} else if *format == "json" {
			// 输入未能进入流水线：仍输出机器可读的失败定位。
			printOpenFailure(stdout, *input, err)
		}
		reportError(stderr, err)
		return exitCodeOf(err)
	}
	if out.Result == nil {
		fmt.Fprintln(stderr, "error=流水线未产生报告")
		return 1
	}
	if *outPath != "" {
		out.Result.RepairedInput = *outPath
	}
	printReport(stdout, out.Result, *format)
	// 验收阈值：关键帧命中率与往返一致性；片段数必须 >= 3。
	if err := accept(out.Result); err != nil {
		fmt.Fprintf(stderr, "验收未通过: %v\n", err)
		return 1
	}
	return 0
}

// printOpenFailure 输出打开/解析阶段失败的最小 JSON 报告。
func printOpenFailure(w io.Writer, input string, err error) {
	r := &report.Result{Schema: "mediarepair/report/v1", Input: input}
	if fe, ok := asFault(err); ok {
		r.MarkFailure(string(fe.Stage), fe.Offset, fe.Recoverable, fe.Error())
	} else {
		r.MarkFailure("io", -1, false, err.Error())
	}
	data, mErr := r.MarshalIndent()
	if mErr != nil {
		return
	}
	w.Write(data)
	w.Write([]byte("\n"))
}

// accept 检查验收阈值。
func accept(r *report.Result) error {
	if r.RoundtripMatch < 0.99 {
		return fmt.Errorf("roundtrip_match=%.4f < 0.99", r.RoundtripMatch)
	}
	if r.KeyframeHitRate < 0.98 {
		return fmt.Errorf("keyframe_hit_rate=%.4f < 0.98", r.KeyframeHitRate)
	}
	if len(r.SegmentHashes) < 3 {
		return fmt.Errorf("segment_hashes=%d < 3", len(r.SegmentHashes))
	}
	return nil
}

// printReport 输出文本摘要或完整 JSON。
func printReport(w io.Writer, r *report.Result, format string) {
	if format == "json" {
		data, err := r.MarshalIndent()
		if err != nil {
			fmt.Fprintf(w, "{\"error\":%q}\n", err.Error())
			return
		}
		w.Write(data)
		w.Write([]byte("\n"))
		return
	}
	fmt.Fprintln(w, r.SummaryText())
	fmt.Fprintf(w, "input=%s buffer_capacity=%d\n", r.Input, r.BufferCapacity)
	for _, t := range r.Tracks {
		fmt.Fprintf(w, "track=%d kind=%s timescale=%d samples=%d wraps=%d duplicates=%d regressions=%d clip_jumps=%d keyframes=%d hit_rate=%.4f\n",
			t.TrackID, t.Kind, t.Timescale, t.Samples, t.Wraps, t.Duplicates, t.Regressions, t.ClipJumps,
			t.Keyframes, t.HitRate)
	}
	for _, s := range r.Segments {
		fmt.Fprintf(w, "segment=%s track=%d transition=%v samples=%d repairs=%d ticks=[%d,%d)\n",
			s.ID, s.TrackID, s.Transition, s.Samples, s.Repairs, s.StartTick, s.EndTick)
	}
	for _, h := range r.SegmentHashes {
		fmt.Fprintf(w, "sha256 segment=%s digest=%s\n", h.ID, h.Digest)
	}
	if r.FailedStage != "" {
		fmt.Fprintf(w, "error_stage=%s source_offset=%d recoverable=%v\n", r.FailedStage, r.FailedOffset, r.Recoverable)
	}
}
