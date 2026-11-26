package fixture

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"

	"tsproof/internal/codec"
	"tsproof/internal/pes"
	"tsproof/internal/ts"
)

// 演示流几何与节奏参数（固定，保证哈希可复算）。
const (
	FrameW      = 160
	FrameH      = 120
	FrameCount  = 32            // 与验收 keyframe_hit=32/32 对应
	KeyInterval = 4             // 每 4 帧一个关键帧
	AudioBlocks = FrameCount * 2
	PCMPerBlock = 64            // 每块 64 个 16-bit 样本
)

// 27 MHz 时间轴参数。
const (
	baseFrameGap = int64(900_000)  // 30 fps 标称间隔（27MHz tick）
	audioGap     = int64(450_000)  // 音频块间隔
	startPTS     = int64(27_000_000)
)

// CleanStream 是生成器产出的干净流及其已知真值。
type CleanStream struct {
	Packets      [][]byte // 188 字节包
	FramePTS     []int64  // 每帧的 27MHz PTS（真值，供命中率比对）
	KeyIndexes   []int    // 关键帧在帧序列中的下标（真值）
	FrameVFRGap  []int64  // 每帧相对上一帧的间隔（VFR 真值）
	AudioPTS     []int64
}

// vfrPattern 定义可变帧率：相对标称间隔的缩放因子（确定性）。
var vfrPattern = []int64{1, 1, 2, 1, 1, 3, 1, 2}

// GenerateClean 合成固定种子的干净复用流。
func GenerateClean(seed uint64) *CleanStream {
	r := newRNG(seed)
	mx := newMuxer()

	// 周期性 PSI：开头写入 PAT/PMT。
	mx.writeSection(ts.PIDPAT, ts.BuildPAT(TSID, 1,
		[]ts.PATProgram{{ProgramNumber: ProgramNo, PMTPID: PIDPMT}}))
	mx.writeSection(PIDPMT, ts.BuildPMT(ProgramNo, 1, PIDVideo,
		[]ts.PMTStream{
			{StreamType: ts.StreamTypeJPEG, PID: PIDVideo},
			{StreamType: ts.StreamTypePCM, PID: PIDAudio},
		}))

	out := &CleanStream{}
	framePTS := startPTS
	audioPTS := startPTS

	// 交错发出音频块与视频帧，全部包按时间顺序进入复用器。
	for i := 0; i < FrameCount; i++ {
		// 每个画面前放两个音频块。
		for k := 0; k < 2; k++ {
			idx := i*2 + k
			samples := make([]int16, PCMPerBlock)
			for s := range samples {
				samples[s] = r.int16Span(2000)
			}
			pesPayload := codec.EncodePCMBlock(uint32(idx), samples)
			pkt := pes.Encode(pes.StreamIDPCM, audioPTS/300, pesPayload)
			mx.writePESPayload(PIDAudio, pkt, true)
			out.AudioPTS = append(out.AudioPTS, audioPTS)
			audioPTS += audioGap
		}

		gap := baseFrameGap * vfrPattern[i%len(vfrPattern)]
		// VFR 真值由相邻 PTS 差定义：首帧间隔为 0，其后等于本帧相对上一帧的位移。
		if i == 0 {
			out.FrameVFRGap = append(out.FrameVFRGap, 0)
		} else {
			out.FrameVFRGap = append(out.FrameVFRGap, framePTS-out.FramePTS[i-1])
		}
		isKey := i%KeyInterval == 0
		jpegBytes := drawFrameJPEG(uint32(seed), uint32(i), isKey)
		pesPayload := codec.EncodeJPEGBlock(uint32(i), isKey, FrameW, FrameH, jpegBytes)
		pkt := pes.Encode(pes.StreamIDJPEG, framePTS/300, pesPayload)
		mx.writePESPayload(PIDVideo, pkt, true)
		mx.maybePCR(framePTS)

		out.FramePTS = append(out.FramePTS, framePTS)
		if isKey {
			out.KeyIndexes = append(out.KeyIndexes, i)
		}
		framePTS += gap
	}
	out.Packets = mx.out
	return out
}

// drawFrameJPEG 用 imaging 离屏合成一帧确定性渐变图并编码为 JPEG。
// 帧序号与关键帧标记改变色块，使每帧字节可区分但完全可复现。
func drawFrameJPEG(seed, frame uint32, key bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, FrameW, FrameH))
	baseHue := uint8((seed ^ frame*37) & 0xFF)
	for y := 0; y < FrameH; y++ {
		for x := 0; x < FrameW; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: baseHue + uint8(x%128),
				G: uint8(y%200) + uint8(frame%56),
				B: uint8((x + y + int(frame)) % 256),
				A: 255,
			})
		}
	}
	if key {
		// 关键帧左上角画一个亮色标记方块，便于人眼复核帧序列。
		for y := 4; y < 20; y++ {
			for x := 4; x < 20; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	// 标准库 JPEG 编码完全确定性；imaging 在渲染层负责离屏合成。
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	return out.Bytes()
}
