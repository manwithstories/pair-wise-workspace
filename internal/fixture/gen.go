// Package fixture 以固定种子确定性生成多声道 PCM/WAV 块与损坏样例，
// 全程纯本地合成，不采集或引用任何外部媒体文件。
package fixture

import (
	"bytes"
	"encoding/binary"
	"os"

	"medpipe/internal/clock"
	"medpipe/internal/index"
	"medpipe/internal/ingest"
)

// Spec 描述一次样例生成的几何参数。
type Spec struct {
	Seed       int64
	Tracks     int
	Seconds    int
	SampleRate int
	FPS        int
	BlockFrames int64 // 每个 WAV 块覆盖的采样帧数
	Bits       int
}

// Plan 是生成结果，供流水线消费。
type Plan struct {
	Spec        Spec
	TotalFrames int64
	BlockCount  int
	Paths       []string
}

// lcg 是确定性线性同余生成器，保证跨平台逐字节一致。
type lcg struct{ state uint64 }

func newLCG(seed int64) *lcg { return &lcg{state: uint64(seed)*6364136223846793005 + 1442695040888963407} }

func (g *lcg) next() uint64 {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return g.state
}

func (g *lcg) int16Between(lo, hi int16) int16 {
	span := int64(hi) - int64(lo) + 1
	return lo + int16(g.next()%uint64(span))
}

// Generate 在 dir 下生成 block_%06d.wav 序列，返回生成计划。
func Generate(dir string, spec Spec) (*Plan, error) {
	if spec.Tracks <= 0 || spec.Seconds <= 0 || spec.SampleRate <= 0 || spec.BlockFrames <= 0 {
		return nil, clock.NewError(clock.CodeInvalidParam, -1, "fixture: 非法生成参数 %+v", spec)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, clock.WrapError(clock.CodeInvalidParam, -1, err, "创建样例目录失败")
	}
	total := int64(spec.Seconds) * int64(spec.SampleRate)
	plan := &Plan{Spec: spec, TotalFrames: total}
	g := newLCG(spec.Seed)
	var frame int64
	var id int64
	for frame < total {
		n := spec.BlockFrames
		if rest := total - frame; rest < n {
			n = rest
		}
		pcm := synthFrames(g, n, spec.Tracks, frame)
		name := "block_" + pad6(id) + ".wav"
		full, err := index.SafeJoin(dir, name)
		if err != nil {
			return nil, err
		}
		if err := writeWAV(full, spec, pcm, int(n)); err != nil {
			return nil, err
		}
		plan.Paths = append(plan.Paths, full)
		frame += n
		id++
	}
	plan.BlockCount = int(id)
	return plan, nil
}

// synthFrames 合成交错 16bit PCM：每轨确定性波形，轨道 0 周期性满量程
// 以便产生可复现的削波证据。
func synthFrames(g *lcg, frames int64, tracks int, globalStart int64) []byte {
	buf := make([]byte, frames*int64(tracks)*2)
	for f := int64(0); f < frames; f++ {
		for c := 0; c < tracks; c++ {
			var v int16
			switch {
			case c == 0 && (globalStart+f)%97 == 0:
				v = 32767 // 周期性满量程，制造确定性削波样本
			case c%2 == 0:
				v = int16((int64(g.int16Between(-8000, 8000)) + sine(globalStart+f, c)) / 2)
			default:
				v = g.int16Between(-4000, 4000)
			}
			off := (f*int64(tracks) + int64(c)) * 2
			binary.LittleEndian.PutUint16(buf[off:], uint16(v))
		}
	}
	return buf
}

func sine(phase int64, track int) int64 {
	// 整数正弦近似：三角波，避免浮点，保证跨平台一致。
	period := int64(64 + track*16)
	p := phase % period
	tri := p
	if p > period/2 {
		tri = period - p
	}
	return tri*2000/(period/2) - 2000
}

func writeWAV(path string, spec Spec, pcm []byte, frames int) error {
	align := spec.Tracks * spec.Bits / 8
	var b bytes.Buffer
	le := binary.LittleEndian
	dataLen := uint32(len(pcm))
	riffLen := uint32(4 + 8 + 16 + 8 + dataLen)
	b.WriteString("RIFF")
	binary.Write(&b, le, riffLen)
	b.WriteString("WAVEfmt ")
	binary.Write(&b, le, uint32(16))
	binary.Write(&b, le, uint16(1))
	binary.Write(&b, le, uint16(spec.Tracks))
	binary.Write(&b, le, uint32(spec.SampleRate))
	binary.Write(&b, le, uint32(spec.SampleRate*align))
	binary.Write(&b, le, uint16(align))
	binary.Write(&b, le, uint16(spec.Bits))
	b.WriteString("data")
	binary.Write(&b, le, dataLen)
	b.Write(pcm)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		return clock.WrapError(clock.CodeInvalidParam, -1, err, "写 WAV 失败")
	}
	_ = frames
	return nil
}

// Corrupt 按故障模式在已生成样例上施加损坏，返回受影响块号。
func Corrupt(dir string, plan *Plan, fault ingest.Fault) (int64, error) {
	switch fault {
	case ingest.FaultConflict:
		// 冲突在网络层注入，文件层无需损坏。
		return -1, nil
	case "truncate":
		return truncateBlock(dir, plan, false)
	case "half-frame":
		return truncateBlock(dir, plan, true)
	case "bad-header":
		path := plan.Paths[0]
		raw, err := os.ReadFile(path)
		if err != nil {
			return -1, clock.WrapError(clock.CodeInvalidHeader, 0, err, "读取块失败")
		}
		copy(raw[0:4], "NOPE")
		return 0, os.WriteFile(path, raw, 0o644)
	default:
		return -1, nil
	}
}

// truncateBlock 截断中间块：half=true 制造半个采样帧（必须拒绝），
// 否则截断到完整采样边界（保留有效样本并统计补偿）。
func truncateBlock(dir string, plan *Plan, half bool) (int64, error) {
	id := int64(len(plan.Paths) / 2)
	if id >= int64(len(plan.Paths)) {
		id = int64(len(plan.Paths)) - 1
	}
	path, err := index.SafeJoin(dir, "block_"+pad6(id)+".wav")
	if err != nil {
		return id, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return id, clock.WrapError(clock.CodeInvalidHeader, id, err, "读取块失败")
	}
	align := plan.Spec.Tracks * plan.Spec.Bits / 8
	dataOff := int64(44) // 标准 16bit fmt PCM 头
	avail := int64(len(raw)) - dataOff
	keepFrames := avail/int64(align) - 2 // 至少砍掉 2 个完整帧
	if keepFrames < 1 {
		keepFrames = 1
	}
	keep := keepFrames * int64(align)
	if half {
		keep++ // 多留 1 字节 -> 尾部出现半个采样帧
	}
	return id, os.WriteFile(path, raw[:dataOff+keep], 0o644)
}

func pad6(v int64) string {
	d := []byte("000000")
	s := []byte(itoa(v))
	copy(d[len(d)-len(s):], s)
	return string(d)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
