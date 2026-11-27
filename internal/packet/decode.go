package packet

import (
	"errors"
	"math"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/media"
)

// PCMFrame 是解码输出的一帧交错 float32 PCM。
type PCMFrame struct {
	// Samples 为交错立体声/单声道浮点样本，取值 [-1,1]。
	Samples []float32
	// Channels 为帧实际声道数（重采样前）。
	Channels int
	// SampleRate 为帧实际采样率（重采样前）。
	SampleRate int
	// NbSamples 为每声道采样点数。
	NbSamples int
}

// Decoder 封装 go-astiav 的 Opus/AAC 解码上下文。
// 一个 Decoder 实例只服务一种编码；损坏包返回带原始位置的媒体错误，
// 绝不产出伪装成有效音频的静音。
type Decoder struct {
	codec Codec
	ctx   *astiav.CodecContext
	pkt   *astiav.Packet
	frame *astiav.Frame
}

// NewDecoder 按编码初始化解码器。
// extraData 为 AAC 等自描述编码所需的 AudioSpecificConfig；Opus 传 nil。
func NewDecoder(codec Codec, extraData []byte) (*Decoder, error) {
	var id astiav.CodecID
	switch codec {
	case CodecOpus:
		id = astiav.CodecIDOpus
	case CodecAAC:
		id = astiav.CodecIDAac
	default:
		return nil, media.E(media.KindArgument, "packet.new-decoder",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"不支持的音频编码", nil)
	}
	c := astiav.FindDecoder(id)
	if c == nil {
		return nil, media.E(media.KindDecode, "packet.new-decoder",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"找不到解码器", nil)
	}
	ctx := astiav.AllocCodecContext(c)
	if ctx == nil {
		return nil, media.E(media.KindDecode, "packet.new-decoder",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"解码上下文分配失败", nil)
	}
	if len(extraData) > 0 {
		if err := ctx.SetExtraData(extraData); err != nil {
			ctx.Free()
			return nil, media.E(media.KindDecode, "packet.new-decoder",
				media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
				"写入 extradata 失败", err)
		}
	}
	// 原生包为裸编码数据，无容器 extradata。
	if err := ctx.Open(c, nil); err != nil {
		ctx.Free()
		return nil, media.E(media.KindDecode, "packet.new-decoder",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"解码器打开失败", err)
	}
	return &Decoder{
		codec: codec,
		ctx:   ctx,
		pkt:   astiav.AllocPacket(),
		frame: astiav.AllocFrame(),
	}, nil
}

// Codec 返回解码器绑定的编码。
func (d *Decoder) Codec() Codec { return d.codec }

// Close 释放全部 C 资源，可安全多次调用。
func (d *Decoder) Close() {
	if d.frame != nil {
		d.frame.Free()
		d.frame = nil
	}
	if d.pkt != nil {
		d.pkt.Free()
		d.pkt = nil
	}
	if d.ctx != nil {
		d.ctx.Free()
		d.ctx = nil
	}
}

// Decode 解码单个事件。占位事件直接返回错误，由时间线静音填充处理；
// 损坏载荷返回 KindDecode 错误并携带原始包位置。
func (d *Decoder) Decode(e *Event) (*PCMFrame, error) {
	if e.Missing() {
		return nil, media.E(media.KindDecode, "packet.decode", e.Location(),
			"占位事件无有效载荷，禁止解码为音频", nil)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	d.pkt.Unref()
	if err := d.pkt.FromData(e.Payload); err != nil {
		return nil, media.E(media.KindDecode, "packet.decode", e.Location(),
			"载荷装入 AVPacket 失败", err)
	}
	if err := d.ctx.SendPacket(d.pkt); err != nil {
		return nil, media.E(media.KindDecode, "packet.decode", e.Location(),
			"解码器拒绝包", err)
	}
	d.frame.Unref()
	if err := d.ctx.ReceiveFrame(d.frame); err != nil {
		// EAGAIN 表示该包未产出帧（编码延迟），不算损坏。
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil, nil
		}
		return nil, media.E(media.KindDecode, "packet.decode", e.Location(),
			"解码输出失败", err)
	}
	return d.extract(e)
}

// extract 把 AVFrame 平面/交错样本统一拷贝为 Go 侧 float32 切片。
func (d *Decoder) extract(e *Event) (*PCMFrame, error) {
	n := d.frame.NbSamples()
	channels := d.frame.ChannelLayout().Channels()
	if n <= 0 || channels <= 0 {
		return nil, media.E(media.KindDecode, "packet.extract", e.Location(),
			"解码帧采样点或声道数非法", nil)
	}
	// 直接走 FrameData.Bytes，拿到按原生 sample_fmt 排列的字节，
	// 再由音频层做格式/布局归一，解码层不假设平面性。
	raw, err := d.frame.Data().Bytes(1)
	if err != nil {
		return nil, media.E(media.KindDecode, "packet.extract", e.Location(),
			"读取帧字节失败", err)
	}
	switch d.frame.SampleFormat() {
	case astiav.SampleFormatFltp, astiav.SampleFormatFlt:
	default:
		return nil, media.E(media.KindDecode, "packet.extract", e.Location(),
			"仅支持浮点采样格式，得到非浮点帧", nil)
	}
	samples := bytesToFloat32(raw)
	// planar 帧需要按声道交错化，供音频层统一处理。
	if d.frame.SampleFormat() == astiav.SampleFormatFltp {
		samples = interleavePlanar(samples, n, channels)
	}
	return &PCMFrame{
		Samples:    samples,
		Channels:   channels,
		SampleRate: d.frame.SampleRate(),
		NbSamples:  n,
	}, nil
}

// Flush 冲刷解码器内部延迟，返回残余帧；通常在流结束时调用。
func (d *Decoder) Flush() ([]*PCMFrame, error) {
	if err := d.ctx.SendPacket(nil); err != nil &&
		!errors.Is(err, astiav.ErrEof) && !errors.Is(err, astiav.ErrEagain) {
		return nil, media.E(media.KindDecode, "packet.flush",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"冲刷解码器失败", err)
	}
	var out []*PCMFrame
	for {
		d.frame.Unref()
		err := d.ctx.ReceiveFrame(d.frame)
		if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
			break
		}
		if err != nil {
			return out, media.E(media.KindDecode, "packet.flush",
				media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
				"冲刷取帧失败", err)
		}
		f, err := d.extract(&Event{ArrivalIndex: -1})
		if err != nil {
			return out, err
		}
		if f != nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func bytesToFloat32(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := uint32(b[i*4]) | uint32(b[i*4+1])<<8 |
			uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out
}

func interleavePlanar(planar []float32, nbSamples, channels int) []float32 {
	if channels == 1 {
		return planar
	}
	out := make([]float32, nbSamples*channels)
	for ch := 0; ch < channels; ch++ {
		base := ch * nbSamples
		for s := 0; s < nbSamples; s++ {
			out[s*channels+ch] = planar[base+s]
		}
	}
	return out
}
