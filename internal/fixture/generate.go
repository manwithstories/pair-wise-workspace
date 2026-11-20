// Package fixture 生成确定性的合成裸码流、MP4 sidecar 与关键帧索引，
// 不引用任何仓库外素材。所有伪随机来自固定种子的 LCG，保证可重复。
package fixture

import (
	"crypto/sha256"
	"encoding/hex"

	"media-audit/internal/sidecar"
	"media-audit/internal/stream"
)

// Spec 描述一条表示层的生成参数。
type Spec struct {
	Codec         stream.Codec
	TrackID       int
	Frames        int
	Timescale     uint32 // 通常 90000
	NominalStep   uint32 // 恒定帧率每帧滴答
	KeyframeEvery int
	JitterTicks   int32 // 每帧时间戳的确定性抖动（滴答），用于多表示层微偏
	Seed          uint64
	// VFRPattern 非空时按该模式循环修改帧步长（模拟真实可变帧率）。
	VFRPattern []uint32
}

// lcg 是数值配方（Numerical Recipes）线性同余生成器。
type lcg struct{ state uint64 }

func (g *lcg) next() uint64 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return g.state
}

// bitstreamBuilder 累积 Annex-B / 低开销 OBU 字节。
type bitstreamBuilder struct {
	buf []byte
}

var startCode = []byte{0, 0, 0, 1}

func (b *bitstreamBuilder) raw(p []byte) { b.buf = append(b.buf, p...) }

// BuildStream 生成裸码流并返回字节与帧元数据。帧净荷带有序号，
// 保证不同帧内容不同、块校验和可区分。
func BuildStream(spec Spec) ([]byte, []sidecar.FrameMeta) {
	rng := lcg{state: spec.Seed + uint64(spec.TrackID)*7919}
	bb := &bitstreamBuilder{}
	frames := make([]sidecar.FrameMeta, 0, spec.Frames)
	var ts64 uint64
	var rawTS uint32

	for i := 0; i < spec.Frames; i++ {
		keyframe := i%spec.KeyframeEvery == 0
		step := uint64(spec.NominalStep)
		if len(spec.VFRPattern) > 0 {
			step = uint64(spec.VFRPattern[i%len(spec.VFRPattern)])
		}
		if i > 0 {
			ts64 += step
		}
		// 抖动是每帧的固定微偏（不累计），模拟编码器打点的小幅相位差，
		// 避免各表示层时间轴整体漂移。
		rawTS = uint32(ts64 + uint64(spec.JitterTicks))
		frames = append(frames, sidecar.FrameMeta{RawTS: rawTS, Keyframe: keyframe})
		bb.writeAU(spec.Codec, i, keyframe, rng.next())
	}
	return bb.buf, frames
}

func (b *bitstreamBuilder) writeAU(codec stream.Codec, idx int, keyframe bool, rnd uint64) {
	switch codec {
	case stream.H264:
		if idx == 0 {
			b.nal(0x67, []byte("sps")) // SPS
			b.nal(0x68, []byte("pps")) // PPS
		}
		h := byte(0x61) // non-IDR slice
		if keyframe {
			h = 0x65 // IDR
		}
		b.nal(h, framePayload(idx, rnd))
	case stream.H265:
		if idx == 0 {
			b.nal2(33, []byte("vps")) // VPS
			b.nal2(34, []byte("sps")) // SPS
			b.nal2(35, []byte("pps")) // PPS
		}
		t := byte(1) // TRAIL_R
		if keyframe {
			t = 19 // IDR_W_RADL
		}
		b.nal2(t, framePayload(idx, rnd))
	case stream.AV1:
		if idx == 0 {
			b.obu(1, false, []byte("sequence-header"))
		}
		// 整个 OBU 净荷（2 字节合成帧头 + 帧数据）统一施加竞争防止，
		// 杜绝帧头前缀零字节与净荷拼接出伪起始码。
		raw := append([]byte{0, 0}, rawFramePayload(idx, rnd)...)
		if keyframe {
			raw[0] |= 0x80
		}
		b.obu(6, false, emulationPrevent(raw)) // FRAME OBU
	}
}

func (b *bitstreamBuilder) nal(header byte, payload []byte) {
	b.raw(startCode)
	b.raw([]byte{header})
	b.raw(payload)
}

// nal2 写 2 字节 H.265 NAL 头：首字节 = forbidden(0)+type(6)+layer_id_high(1)，
// 第二字节 = layer_id_low(6)+temporal_id+1(2)。类型在首字节高 6 位。
func (b *bitstreamBuilder) nal2(nalType byte, payload []byte) {
	b.raw(startCode)
	b.raw([]byte{nalType << 1, 0x01})
	b.raw(payload)
}

// obu 写低开销 AV1 OBU：header + 1 字节 leb128 长度 + payload。
func (b *bitstreamBuilder) obu(obuType byte, forbidden bool, payload []byte) {
	h := (obuType & 0x0F) << 3
	h |= 0x02 // has_size
	if forbidden {
		h |= 0x80
	}
	b.raw(startCode)
	b.raw([]byte{h, byte(len(payload))})
	b.raw(payload)
}

// framePayload 生成带帧序号的确定性净荷，并施加 Annex-B 竞争防止
// （emulation prevention），保证净荷内不出现裸 00 00 00/01/02/03，
// 从而不会被扫描器误判为起始码。
func framePayload(idx int, rnd uint64) []byte {
	return emulationPrevent(rawFramePayload(idx, rnd))
}

// rawFramePayload 生成 8 字节确定性净荷，末字节恒非零。
func rawFramePayload(idx int, rnd uint64) []byte {
	return []byte{
		byte(idx), byte(idx >> 8), byte(idx >> 16), byte(idx >> 24),
		byte(rnd), byte(rnd >> 8), byte(rnd >> 16), byte(rnd>>24) | 0x01,
	}
}

// emulationPrevent 在 00 00 之后插入 03，模拟真实编码器的 RBSP->EBSP 转换。
func emulationPrevent(b []byte) []byte {
	out := make([]byte, 0, len(b)+4)
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 0x03 {
			out = append(out, 0x03)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// BuildMeta 由码流与帧表组装 sidecar 元数据。
func BuildMeta(spec Spec, data []byte, frames []sidecar.FrameMeta) *sidecar.Meta {
	sum := sha256.Sum256(data)
	return &sidecar.Meta{
		Codec:         string(spec.Codec),
		TrackID:       spec.TrackID,
		Timescale:     spec.Timescale,
		NominalStep:   spec.NominalStep,
		KeyframeEvery: spec.KeyframeEvery,
		Frames:        frames,
		StreamSize:    int64(len(data)),
		StreamSHA256:  hex.EncodeToString(sum[:]),
	}
}
