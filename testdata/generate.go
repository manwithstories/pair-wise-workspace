// Package testdata 以确定性随机源生成验收所需的全部本地样例：
// 正常 fMP4 分片与 HLS 清单，以及跨回绕加漂移、VFR 重复时间戳、缺片、
// 截断 MP4、冲突序号、取消队列满等边界夹具。
//
// 所有数据随代码生成，不引用任何外部文件；相同种子逐字节可复现。
package testdata

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"mediatime/internal/clock"
)

// Fault 标识一种注入故障。
type Fault string

// 支持的故障类型。
const (
	FaultNone          Fault = ""
	FaultWrapDriftVFR  Fault = "wrap-drift-vfr"
	FaultMissing       Fault = "missing"
	FaultTruncated     Fault = "truncated"
	FaultConflict      Fault = "conflict"
	FaultQueueFull     Fault = "queue-full"
)

// DefaultFragmentCount 是验收演示的标准片段数。
const DefaultFragmentCount = 24

// SampleSpec 描述一个分片内单个样本的时间与大小。
type SampleSpec struct {
	DTS    uint64
	PTS    uint64
	Size   uint32
	IsSync bool
}

// FragmentSpec 是一个分片的完整生成规格。
type FragmentSpec struct {
	Sequence int
	URI      string
	Samples  []SampleSpec
	// TruncateAt >0 时把生成字节截断到该长度，制造 truncated_box。
	TruncateAt int
}

// Scenario 是一次生成的完整场景。
type Scenario struct {
	Seed         int64
	Fragments    int
	TargetBPS    int64
	Fault        Fault
	FragmentSpec []FragmentSpec
	// HLSPlaylist 是配套清单文本。
	HLSPlaylist string
	// ConflictPlaylist 仅在 FaultConflict 下提供，含冲突序号。
	ConflictPlaylist string
}

// Generator 携带确定性随机源与时钟参数。
type Generator struct {
	rng       *rand.Rand
	seed      int64
	tickRate  uint64
}

// NewGenerator 以固定种子构造生成器。
func NewGenerator(seed int64) *Generator {
	return &Generator{
		rng:      rand.New(rand.NewSource(seed)),
		seed:     seed,
		tickRate: clock.TickRate,
	}
}

// nominalDurationTick 是每片段标称时长：1 秒 = 90000 Tick。
const nominalDurationTick = 90_000

// targetFrameTick 是 30fps 下相邻帧标称间隔 3000 Tick。
const targetFrameTick = 3_000

// samplesPerFragment 固定帧率下每秒 30 帧。
const samplesPerFragment = 30

// Generate 按故障类型生成完整场景。
func (g *Generator) Generate(n int, fault Fault) (*Scenario, error) {
	if n <= 0 {
		n = DefaultFragmentCount
	}
	sc := &Scenario{Seed: g.seed, Fragments: n, Fault: fault, TargetBPS: 800_000}
	switch fault {
	case FaultNone, FaultQueueFull:
		g.buildNormal(sc, n)
	case FaultWrapDriftVFR:
		g.buildWrapDriftVFR(sc, n)
	case FaultMissing:
		g.buildMissing(sc, n)
	case FaultTruncated:
		g.buildTruncated(sc, n)
	case FaultConflict:
		g.buildConflict(sc, n)
	default:
		return nil, fmt.Errorf("testdata: unknown fault %q", fault)
	}
	sc.HLSPlaylist = g.renderHLS(sc.FragmentSpec, false)
	if fault == FaultConflict {
		sc.ConflictPlaylist = g.renderHLS(sc.FragmentSpec, true)
	}
	return sc, nil
}

// buildNormal 生成固定帧率、码率贴近 800kbps 的正常分片序列。
func (g *Generator) buildNormal(sc *Scenario, n int) {
	for seq := 0; seq < n; seq++ {
		base := uint64(seq) * nominalDurationTick
		spec := FragmentSpec{Sequence: seq, URI: fmt.Sprintf("seg%d.m4s", seq)}
		for k := 0; k < samplesPerFragment; k++ {
			dts := base + uint64(k)*targetFrameTick
			// 目标 800kbps、30fps → 每帧约 3333 字节，加入 ±2% 确定性抖动。
			size := uint32(3333 + g.rng.Intn(134) - 67)
			spec.Samples = append(spec.Samples, SampleSpec{
				DTS: dts, PTS: dts + 900, Size: size, IsSync: k == 0,
			})
		}
		sc.FragmentSpec = append(sc.FragmentSpec, spec)
	}
}

// renderHLS 把分片规格渲染为 HLS 清单文本。
func (g *Generator) renderHLS(specs []FragmentSpec, conflict bool) string {
	out := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"
	out += fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:0\n")
	for i, sp := range specs {
		if i == 0 {
			out += "#EXT-X-DISCONTINUITY\n"
		}
		out += "#EXTINF:1.0,\n" + sp.URI + "\n"
	}
	if conflict && len(specs) > 1 {
		// 注入一个回退媒体序号造成重复序号冲突。
		out += "#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\nconflict.m4s\n"
	}
	out += "#EXT-X-ENDLIST\n"
	return out
}

// BuildFragmentBytes 把单个分片规格编码为 fMP4 字节（moof+mdat 布局）。
func BuildFragmentBytes(spec FragmentSpec) []byte {
	n := len(spec.Samples)
	trunPayload := 4 + 4 + 4 + n*12 // flags/count/dataOffset + 每样本 3 字段
	trunBox := 8 + trunPayload
	tfhdBox := 8 + 8
	tfdtBox := 8 + 12
	trafBox := 8 + tfhdBox + tfdtBox + trunBox
	moofBox := 8 + trafBox

	var totalSize uint32
	for _, s := range spec.Samples {
		totalSize += s.Size
	}

	var b []byte
	appendBox := func(typ string, payload []byte) {
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
		copy(hdr[4:], typ)
		b = append(b, hdr...)
		b = append(b, payload...)
	}

	// traf > tfhd
	{
		p := make([]byte, 8)
		binary.BigEndian.PutUint32(p, 0x020000) // default-base-is-moof
		binary.BigEndian.PutUint32(p[4:], 1)    // trackID
		traf := box("tfhd", p)
		// tfdt v1
		tfdtP := make([]byte, 12)
		binary.BigEndian.PutUint32(tfdtP, 0x01000000)
		binary.BigEndian.PutUint64(tfdtP[4:], spec.Samples[0].DTS)
		traf = append(traf, box("tfdt", tfdtP)...)
		// trun
		trunP := make([]byte, 0, trunPayload)
		flags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400)
		trunP = binary.BigEndian.AppendUint32(trunP, flags)
		trunP = binary.BigEndian.AppendUint32(trunP, uint32(n))
		trunP = binary.BigEndian.AppendUint32(trunP, 8) // dataOffset 指向 mdat 负载
		var prevDTS uint64
		for i, s := range spec.Samples {
			dur := uint32(targetFrameTick)
			if i > 0 {
				dur = uint32(s.DTS - prevDTS)
			}
			prevDTS = s.DTS
			trunP = binary.BigEndian.AppendUint32(trunP, dur)
			trunP = binary.BigEndian.AppendUint32(trunP, s.Size)
			sflag := uint32(0)
			if !s.IsSync {
				sflag = 0x00010000
			}
			trunP = binary.BigEndian.AppendUint32(trunP, sflag)
		}
		traf = append(traf, box("trun", trunP)...)
		appendBox("moof", box("traf", traf))
	}

	payload := make([]byte, totalSize)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	appendBox("mdat", payload)
	_ = moofBox
	if spec.TruncateAt > 0 && spec.TruncateAt < len(b) {
		b = b[:spec.TruncateAt]
	}
	return b
}

func box(typ string, payload []byte) []byte {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], typ)
	return append(hdr, payload...)
}
