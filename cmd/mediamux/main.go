// mediamux 终端验收入口：demo 子命令串联生成、解析、抖动缓冲、
// 同步与验证，输出 JSON 行并按质量门限返回退出码。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediamux/ingest"
	"mediamux/manifest"
	"mediamux/media"
	"mediamux/pipeline"
	"mediamux/testdata"
	"mediamux/transport"
)

// 退出码。
const (
	exitOK       = 0 // 校验通过
	exitQuality  = 1 // 质量指标超限
	exitUsage    = 2 // 输入或参数错误
	exitPipeline = 3 // 不可恢复流水线故障
)

// summary 是 demo 的汇总 JSON 行。
type summary struct {
	TargetBps              int64    `json:"target_bps"`
	ActualBps              int64    `json:"actual_bps"`
	BitrateDeviationPct    float64  `json:"bitrate_deviation_pct"`
	PacketLoss             int      `json:"packet_loss"`
	RecoveredPackets       int      `json:"recovered_packets"`
	BufferPeakBytes        int      `json:"buffer_peak_bytes"`
	SyncErrorMsP95         int64    `json:"sync_error_ms_p95"`
	ManifestAlignmentMsMax int64    `json:"manifest_alignment_ms_max"`
	SubtitleValid          int      `json:"subtitle_valid"`
	SubtitleInvalid        int      `json:"subtitle_invalid"`
	ErrorOffset            int64    `json:"error_offset"`
	Checks                 []string `json:"checks"`
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "用法: mediamux demo --seed N --target-bitrate BPS "+
			"--buffer-ms MS --memory-mb MB --slow-consumer-ms MS [--duration-ms MS] [--queue-cap N]")
		os.Exit(exitUsage)
	}
	os.Exit(runDemo(os.Args[2:]))
}

// item 在流水线后段传递：RTP 交付或字幕事件。
type item struct {
	del *transport.Delivery
	sub *ingest.Event
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 17, "确定性随机种子")
	targetBps := fs.Int64("target-bitrate", 2400000, "目标码率 bps")
	bufferMs := fs.Int64("buffer-ms", 180, "抖动缓冲窗口上限 ms")
	memoryMB := fs.Int64("memory-mb", 32, "总内存预算 MiB")
	slowMs := fs.Int64("slow-consumer-ms", 0, "慢消费者每条处理延时 ms")
	durationMs := fs.Int("duration-ms", 8000, "生成流时长 ms")
	queueCap := fs.Int("queue-cap", 16, "阶段间队列容量")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *targetBps <= 0 || *bufferMs <= 0 || *memoryMB <= 0 || *slowMs < 0 || *durationMs < 1000 {
		fmt.Fprintln(os.Stderr, "参数非法：码率/缓冲/内存/时长必须为正")
		return exitUsage
	}

	stream := testdata.RTPStream(*seed, *durationMs, *targetBps)
	srtData := testdata.SRT(*seed, *durationMs)

	mp4Data, err := testdata.MP4(stream.FrameSizes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成 MP4 失败:", err)
		return exitPipeline
	}
	tmp, err := os.MkdirTemp("", "mediamux")
	if err != nil {
		fmt.Fprintln(os.Stderr, "临时目录失败:", err)
		return exitPipeline
	}
	defer os.RemoveAll(tmp)
	mp4Path := filepath.Join(tmp, "demo.mp4")
	if err := os.WriteFile(mp4Path, mp4Data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "写入 MP4 失败:", err)
		return exitPipeline
	}
	tracks, err := manifest.ReadMP4(mp4Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 MP4 失败:", err)
		return exitPipeline
	}

	// 抖动缓冲容量来自内存预算（一半预算预留给缓冲）
	jbCap := int((*memoryMB << 20) / 2 / testdata.MaxPacket)
	if jbCap < 8 {
		jbCap = 8
	}
	jb := transport.NewJitterBuffer(jbCap, testdata.ClockRate,
		media.MillisToTicks(*bufferMs/4), media.MillisToTicks(*bufferMs))

	col := &collector{aligner: pipeline.NewAligner()}
	sub := &subState{errOffset: -1}
	stages := []pipeline.Stage{
		sourceStage(stream.Packets),
		jitterStage(jb),
		subtitleStage(srtData, sub),
		col.sink(*slowMs),
	}
	if err := pipeline.Run(context.Background(), *queueCap, stages...); err != nil {
		fmt.Fprintln(os.Stderr, "流水线故障:", err)
		return exitPipeline
	}

	jstats := jb.Stats()
	actualBps := int64(float64(col.deliveredBytes*8) / (float64(*durationMs) / 1000))
	dev := float64(actualBps-*targetBps) / float64(*targetBps) * 100
	if dev < 0 {
		dev = -dev
	}
	rep := col.aligner.Report()

	m := &manifest.Manifest{
		Tracks: tracks, Gaps: col.gaps, Subtitles: col.subs,
		TargetBps: *targetBps, ActualBps: actualBps,
	}
	frameDur := media.FrameRate25.FrameDuration()
	var checkNames []string
	for _, c := range m.Validate(frameDur) {
		s := "ok"
		if !c.OK {
			s = "FAIL"
		}
		checkNames = append(checkNames, c.Name+":"+s)
	}

	emit(summary{
		TargetBps: *targetBps, ActualBps: actualBps,
		BitrateDeviationPct:    dev,
		PacketLoss:             jstats.Lost,
		RecoveredPackets:       jstats.Recovered,
		BufferPeakBytes:        jstats.PeakBytes,
		SyncErrorMsP95:         media.Millis(rep.P95),
		ManifestAlignmentMsMax: media.Millis(m.AlignmentError()),
		SubtitleValid:          sub.valid,
		SubtitleInvalid:        sub.invalid,
		ErrorOffset:            sub.errOffset,
		Checks:                 checkNames,
	})
	for _, fr := range runFixtures() {
		emit(fr)
	}

	if dev > 5 || int64(jstats.PeakBytes) >= (*memoryMB<<20) ||
		rep.P95 > media.MillisToTicks(40) || m.AlignmentError() > frameDur ||
		rep.Incomplete || !m.AllOK() {
		return exitQuality
	}
	return exitOK
}

func emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "JSON 编码失败:", err)
		return
	}
	fmt.Println(string(b))
}

