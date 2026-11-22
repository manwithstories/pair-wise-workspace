// Package fixture 生成正常、损坏和边界样例：DASH/HLS 清单、PCM 分段、
// 以及带重复/乱序/突发丢失/时间戳回退的 RTP 包流。全部数据由种子确定生成。
package fixture

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"mediaverify/internal/clock"
)

// PCMHeader 是自定义分段头：magic + rate + channels + frames。
const PCMHeaderSize = 20
const pcmMagic = "MVPCM01\x00"

// Packet 是一个待发送的 RTP 包参数（由运行层封装为 pion/rtp）。
type Packet struct {
	Seq     uint16
	TS      uint32
	Payload []byte // 一帧交错 s16le PCM
}

// Options 夹具参数。
type Options struct {
	Seed      int64
	Rate      int // 源采样率
	Channels  int // 源声道数
	Segments  int // 分段数
	SegFrames int // 每分段帧数（20ms/帧）
	LossBurst int // 突发丢失包数
	Amp       float64
}

// DefaultOptions 默认夹具参数。
func DefaultOptions(seed int64) Options {
	return Options{Seed: seed, Rate: 48000, Channels: 2, Segments: 4, SegFrames: 100, LossBurst: 4, Amp: 0.25}
}

// FrameSamples 每帧每声道采样数（20ms）。
func (o Options) FrameSamples() int { return o.Rate / 50 }

// Program 生成结果。
type Program struct {
	Dir          string
	MPDPath      string
	HLSPath      string
	SegmentFiles []string
	Packets      []Packet // 已施加损伤的 RTP 流
	Opts         Options
}

// sinePCM 生成确定性的正弦 PCM（s16le 交错）。
func sinePCM(rate, channels, framesPerSeg, segIdx int, amp float64) []byte {
	n := framesPerSeg * rate / 50
	out := make([]byte, 0, n*channels*2)
	base := segIdx * n
	for i := 0; i < n; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(base+i)/float64(rate)))
		for c := 0; c < channels; c++ {
			out = append(out, byte(v), byte(v>>8))
		}
	}
	return out
}

// WritePCMSegment 写自定义 PCM 分段文件。
func WritePCMSegment(path string, rate, channels, frames int, payload []byte) error {
	h := make([]byte, PCMHeaderSize)
	copy(h, pcmMagic)
	put32(h[8:], uint32(rate))
	put32(h[12:], uint32(channels))
	put32(h[16:], uint32(frames))
	return os.WriteFile(path, append(h, payload...), 0o644)
}

// ReadPCMSegment 读回 PCM 分段；头部不合法返回错误。
func ReadPCMSegment(b []byte) (rate, channels, frames int, payload []byte, err error) {
	if len(b) < PCMHeaderSize || string(b[:7]) != pcmMagic[:7] {
		return 0, 0, 0, nil, fmt.Errorf("bad PCM segment header")
	}
	rate = int(get32(b[8:]))
	channels = int(get32(b[12:]))
	frames = int(get32(b[16:]))
	payload = b[PCMHeaderSize:]
	want := frames * rate / 50 * channels * 2
	if len(payload) != want {
		return 0, 0, 0, nil, fmt.Errorf("truncated segment: %d payload bytes, want %d", len(payload), want)
	}
	return rate, channels, frames, payload, nil
}

func put32(b []byte, v uint32) { b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24) }
func get32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// Generate 在 dir 下生成完整正常节目（清单+分段+受损 RTP 流）。
func Generate(dir string, o Options) (*Program, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := &Program{Dir: dir, Opts: o}
	fs := o.FrameSamples()
	// 分段文件
	for s := 0; s < o.Segments; s++ {
		name := fmt.Sprintf("seg_%d.m4s", s+1)
		if err := WritePCMSegment(filepath.Join(dir, name), o.Rate, o.Channels,
			o.SegFrames, sinePCM(o.Rate, o.Channels, o.SegFrames, s, o.Amp)); err != nil {
			return nil, err
		}
		p.SegmentFiles = append(p.SegmentFiles, name)
	}
	// DASH MPD
	segDur := int64(o.SegFrames) * int64(fs) // timescale=rate 单位
	mpd := fmt.Sprintf(`<?xml version="1.0"?>
<MPD><Period><AdaptationSet>
  <Representation id="a1" bandwidth="128000">
    <SegmentTemplate media="seg_$Number$.m4s" initialization="init.mp4" timescale="%d">
      <SegmentTimeline><S t="0" d="%d" r="%d"/></SegmentTimeline>
    </SegmentTemplate>
  </Representation>
</AdaptationSet></Period></MPD>`, o.Rate, segDur, o.Segments-1)
	p.MPDPath = filepath.Join(dir, "prog.mpd")
	if err := os.WriteFile(p.MPDPath, []byte(mpd), 0o644); err != nil {
		return nil, err
	}
	// HLS（中间含 2 个 discontinuity，均有显式 PDT 映射）
	secPerSeg := float64(o.SegFrames) / 50
	hls := "#EXTM3U\n#EXT-X-TARGETDURATION:3\n#EXT-X-MAP:URI=\"init.mp4\"\n"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wall := 0.0
	for s := 0; s < o.Segments; s++ {
		if s == 1 || s == 3 {
			wall += 10 // discontinuity：统一时钟跳变
			hls += "#EXT-X-DISCONTINUITY\n"
			hls += "#EXT-X-PROGRAM-DATE-TIME:" +
				base.Add(time.Duration(wall*float64(time.Second))).Format(time.RFC3339Nano) + "\n"
		}
		hls += fmt.Sprintf("#EXTINF:%.3f,\n%s\n", secPerSeg, p.SegmentFiles[s])
		wall += secPerSeg
	}
	hls += "#EXT-X-ENDLIST\n"
	p.HLSPath = filepath.Join(dir, "prog.m3u8")
	if err := os.WriteFile(p.HLSPath, []byte(hls), 0o644); err != nil {
		return nil, err
	}
	p.Packets = p.buildPackets()
	return p, nil
}

// buildPackets 从分段载荷构建 RTP 流并施加确定性损伤：
// 重复、乱序、突发丢失（由种子与 LossBurst 决定）。
func (p *Program) buildPackets() []Packet {
	o := p.Opts
	fs := o.FrameSamples()
	frameBytes := fs * o.Channels * 2
	var pkts []Packet
	var seq uint16
	var ts uint32
	for s := 0; s < o.Segments; s++ {
		pcm := sinePCM(o.Rate, o.Channels, o.SegFrames, s, o.Amp)
		for off := 0; off+frameBytes <= len(pcm); off += frameBytes {
			pl := make([]byte, frameBytes)
			copy(pl, pcm[off:off+frameBytes])
			pkts = append(pkts, Packet{Seq: seq, TS: ts, Payload: pl})
			seq++
			ts += uint32(fs)
		}
	}
	rng := rand.New(rand.NewSource(o.Seed))
	var out []Packet
	dropped := map[int]bool{}
	// 突发丢失：在 60% 处连续丢 LossBurst 个
	if o.LossBurst > 0 && len(pkts) > 10 {
		start := len(pkts)*3/5 + rng.Intn(5)
		for i := start; i < start+o.LossBurst && i < len(pkts); i++ {
			dropped[i] = true
		}
	}
	consumed := map[int]bool{}
	for i, pkt := range pkts {
		if dropped[i] || consumed[i] {
			continue
		}
		// 每第 13 个包与后一个交换（乱序）
		if i%13 == 12 && i+1 < len(pkts) && !dropped[i+1] && !consumed[i+1] {
			out = append(out, pkts[i+1], pkt)
			consumed[i+1] = true
		} else {
			out = append(out, pkt)
		}
		// 每第 17 个包重复一次
		if i%17 == 16 {
			out = append(out, pkt)
		}
	}
	return out
}

// TicksPerSegment 每分段时长。
func (o Options) TicksPerSegment() clock.Ticks {
	return clock.SamplesToTicks(int64(o.SegFrames*o.FrameSamples()), o.Rate)
}
