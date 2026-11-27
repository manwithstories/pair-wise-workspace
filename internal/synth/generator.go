// Package synth 用 Go 与 go-astiav 在本地生成确定性的 Opus/AAC 样例包，
// 不引用任何外部文件。同一 seed/参数组合逐位复现，支撑固定输入重复运行校验。
package synth

import (
	"math"
	"math/rand"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/packet"
)

// Fault 是可注入的故障类型。
type Fault string

const (
	FaultDuplicate Fault = "duplicate"
	FaultReorder   Fault = "reorder"
	FaultBurst     Fault = "burst"
	FaultLate      Fault = "late"
	FaultCRC       Fault = "crc"
)

// Config 描述一次样例生成。
type Config struct {
	Seed       int64
	Seconds    int
	Codec      packet.Codec
	SampleRate int
	Faults     []Fault
	// StreamID/Epoch 写入事件键。
	StreamID uint32
	Epoch    uint32
}

// Generated 持有生成结果与故障统计。
type Generated struct {
	// Events 是按"到达顺序"排列的事件（已注入乱序/重复/迟到）。
	Events []*packet.Event
	// GroundTruth 是按序号排列的完整事件（无故障），用于最终一致性比对。
	GroundTruth []*packet.Event
	// ExtraData 是 AAC 解码所需的 AudioSpecificConfig；Opus 为 nil。
	ExtraData []byte
	Stats      FaultStats
}

// FaultStats 记录注入的各类故障数量。
type FaultStats struct {
	Duplicates int
	Reorders   int
	Bursts     int
	Late       int
	CRC        int
	// BurstMissing 是突发丢失的序号集合（展开前 uint16）。
	BurstMissing []uint16
}

// encoder 包装 astiav 音频编码器。
type encoder struct {
	ctx   *astiav.CodecContext
	frame *astiav.Frame
	pkt   *astiav.Packet
	codec packet.Codec
	rate  int
}

func newEncoder(codec packet.Codec, rate int) (*encoder, []byte, error) {
	astiav.SetLogLevel(astiav.LogLevelQuiet)
	var id astiav.CodecID
	if codec == packet.CodecOpus {
		id = astiav.CodecIDOpus
	} else {
		id = astiav.CodecIDAac
	}
	c := astiav.FindEncoder(id)
	if c == nil {
		return nil, nil, errUnsupported(codec)
	}
	ctx := astiav.AllocCodecContext(c)
	ctx.SetSampleRate(rate)
	ctx.SetChannelLayout(astiav.ChannelLayoutStereo)
	ctx.SetBitRate(64000)
	if codec == packet.CodecOpus {
		ctx.SetSampleFormat(astiav.SampleFormatFlt)
	} else {
		ctx.SetSampleFormat(astiav.SampleFormatFltp)
	}
	if err := ctx.Open(c, nil); err != nil {
		ctx.Free()
		return nil, nil, err
	}
	return &encoder{
		ctx: ctx, frame: astiav.AllocFrame(), pkt: astiav.AllocPacket(),
		codec: codec, rate: rate,
	}, append([]byte(nil), ctx.ExtraData()...), nil
}

func (e *encoder) close() {
	e.frame.Free()
	e.pkt.Free()
	e.ctx.Free()
}

// encodeFrames 编码 tone 样本，返回每帧的裸包载荷与每帧采样数。
func (e *encoder) encodeFrames(rng *rand.Rand, totalFrames int) ([][]byte, int, error) {
	frameSize := e.ctx.FrameSize()
	if frameSize <= 0 {
		frameSize = 960
	}
	// 多音叠加，模拟持续变化带宽下的非平稳信号。
	freqs := []float64{220, 440, 660, 880}
	var out [][]byte
	sampleIdx := 0
	interleaved := e.codec == packet.CodecOpus
	for f := 0; f < totalFrames; f++ {
		e.frame.Unref()
		e.frame.SetNbSamples(frameSize)
		sf := astiav.SampleFormatFltp
		if interleaved {
			sf = astiav.SampleFormatFlt
		}
		e.frame.SetSampleFormat(sf)
		e.frame.SetSampleRate(e.rate)
		e.frame.SetChannelLayout(astiav.ChannelLayoutStereo)
		if err := e.frame.AllocBuffer(0); err != nil {
			return nil, 0, err
		}
		buf := make([]float32, frameSize*2)
		for i := 0; i < frameSize; i++ {
			// 缓慢幅度调制，保证短片/尾包能量非零且确定。
			mod := 0.15 + 0.1*math.Sin(2*math.Pi*float64(sampleIdx)/float64(e.rate*2))
			var v float32
			for k, fr := range freqs {
				v += float32(mod * 0.25 * math.Sin(2*math.Pi*fr*float64(sampleIdx)/float64(e.rate)+float64(k)))
			}
			if interleaved {
				buf[i*2] = v
				buf[i*2+1] = v
			} else {
				buf[i] = v
				buf[frameSize+i] = v
			}
			sampleIdx++
		}
		if err := e.frame.Data().SetBytes(float32Bytes(buf), 0); err != nil {
			return nil, 0, err
		}
		if err := e.ctx.SendFrame(e.frame); err != nil {
			return nil, 0, err
		}
		for {
			e.pkt.Unref()
			err := e.ctx.ReceivePacket(e.pkt)
			if err == astiav.ErrEagain || err == astiav.ErrEof {
				break
			}
			if err != nil {
				return nil, 0, err
			}
			out = append(out, append([]byte(nil), e.pkt.Data()...))
		}
	}
	return out, frameSize, nil
}

// Generate 按配置生成完整样例并注入故障。
func Generate(cfg Config) (*Generated, error) {
	if cfg.Codec == 0 {
		cfg.Codec = packet.CodecOpus
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 48000
	}
	if cfg.StreamID == 0 {
		cfg.StreamID = 1
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	enc, extra, err := newEncoder(cfg.Codec, cfg.SampleRate)
	if err != nil {
		return nil, err
	}
	defer enc.close()

	frameSize := enc.ctx.FrameSize()
	if frameSize <= 0 {
		frameSize = 960
	}
	totalFrames := cfg.Seconds * cfg.SampleRate / frameSize
	payloads, _, err := enc.encodeFrames(rng, totalFrames)
	if err != nil {
		return nil, err
	}

	// 构造基准事件流（ground truth），序号与 RTP 时间戳严格递增。
	truth := make([]*packet.Event, 0, len(payloads))
	ts := uint64(0)
	for i, p := range payloads {
		e := &packet.Event{
			StreamID:      cfg.StreamID,
			Epoch:         cfg.Epoch,
			Seq:           uint16(i),
			Codec:         cfg.Codec,
			RTPTimestamp:  ts,
			ClockRate:     uint32(cfg.SampleRate),
			DurationTicks: uint32(frameSize),
			ArrivalMicros: int64(i) * 20_000,
			ArrivalIndex:  i,
			Payload:       p,
		}
		truth = append(truth, e)
		ts += uint64(frameSize)
	}

	gen := &Generated{GroundTruth: truth, ExtraData: extra}
	gen.Events = injectFaults(truth, rng, cfg.Faults, &gen.Stats)
	return gen, nil
}

func errUnsupported(c packet.Codec) error {
	return &simpleErr{msg: "不支持的编码: " + c.String()}
}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

func float32Bytes(s []float32) []byte {
	b := make([]byte, len(s)*4)
	for i, v := range s {
		bits := math.Float32bits(v)
		b[i*4] = byte(bits)
		b[i*4+1] = byte(bits >> 8)
		b[i*4+2] = byte(bits >> 16)
		b[i*4+3] = byte(bits >> 24)
	}
	return b
}
