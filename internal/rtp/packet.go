// Package rtpx 基于 github.com/pion/rtp 完成确定性的 RTP 打包与解包。
package rtpx

import (
	"github.com/pion/rtp"

	"bcastqc/internal/media"
)

// MTU 为本模拟链路固定的 RTP 数据报大小上限。
const MTU uint16 = 1200

// 载荷类型（私有动态 PT，仅在本项目内自洽）。
const (
	PayloadTypeVideo uint8 = 96
	PayloadTypeAudio uint8 = 97
)

// Frame 是送入打包器的一帧/一个音频块。
type Frame struct {
	Track   uint32
	TimeNS  int64
	Key     bool
	Payload []byte
}

// Packet 是穿过模拟链路的最小数据单元，携带管线记账字段。
type Packet struct {
	// Seq 为轨道内单调展开后的 64 位序列号（回绕已展开）。
	Seq int64
	// SendNS 为发送侧虚拟时钟，用于端到端时延统计。
	SendNS    int64
	TimeNS    int64
	Track     uint32
	Key       bool
	ClockRate uint32
	Raw       []byte // pion/rtp 编解码后的完整报文
}

// fixedPayloader 按 MTU 朴素切片。
type fixedPayloader struct{}

func (fixedPayloader) Payload(mtu uint16, payload []byte) [][]byte {
	if mtu == 0 {
		return nil
	}
	var out [][]byte
	for len(payload) > 0 {
		n := int(mtu)
		if n > len(payload) {
			n = len(payload)
		}
		chunk := make([]byte, n)
		copy(chunk, payload[:n])
		out = append(out, chunk)
		payload = payload[n:]
	}
	return out
}

// Packetizer 维护单条轨道的 RTP 序号/时间戳状态。
type Packetizer struct {
	track     uint32
	clockRate uint32
	inner     rtp.Packetizer
	nextSeq   int64
	prevNS    int64
	havePrev  bool
}

// NewPacketizer 创建确定性打包器：固定起始序号与起始 RTP 时间戳 0。
func NewPacketizer(track uint32, clockRate, ssrc uint32, pt uint8) *Packetizer {
	const startSeq uint16 = 1
	inner := rtp.NewPacketizerWithOptions(
		MTU,
		fixedPayloader{},
		rtp.NewFixedSequencer(startSeq),
		clockRate,
		rtp.WithSSRC(ssrc),
		rtp.WithPayloadType(pt),
		rtp.WithTimestamp(0),
	)
	return &Packetizer{
		track:     track,
		clockRate: clockRate,
		inner:     inner,
		nextSeq:   int64(startSeq),
	}
}

// Packetize 把一帧拆为若干 RTP 报文并封装为链路 Packet。
// pion 内部按帧间采样差累加 RTP 时间戳；同帧报文共享时间戳，末包 Marker 置位。
func (p *Packetizer) Packetize(f Frame, sendNS int64) ([]Packet, error) {
	deltaNS := f.TimeNS
	if p.havePrev {
		deltaNS = f.TimeNS - p.prevNS
	}
	p.havePrev = true
	p.prevNS = f.TimeNS
	samples := uint32(media.NSToSamples(p.clockRate, deltaNS))
	if int64(samples) < 0 {
		samples = 0 // 负回退不静默改写时间线，仍以 0 增量发包并由同步阶段上报
	}
	rtpPkts := p.inner.Packetize(f.Payload, samples)
	out := make([]Packet, 0, len(rtpPkts))
	for _, rp := range rtpPkts {
		raw, err := rp.Marshal()
		if err != nil {
			return nil, media.NewError(media.StageTransport, media.KindIO,
				"RTP 序列化失败").Wrap(err)
		}
		out = append(out, Packet{
			Seq:       p.nextSeq,
			SendNS:    sendNS,
			TimeNS:    f.TimeNS,
			Track:     p.track,
			Key:       f.Key,
			ClockRate: p.clockRate,
			Raw:       raw,
		})
		p.nextSeq++
	}
	return out, nil
}

// Decoder 用 pion/rtp 反解报文，并展开 16 位序列号。
type Decoder struct {
	track   uint32
	started bool
	lastSeq int64
}

// NewDecoder 为一条轨道创建解包器。
func NewDecoder(track uint32) *Decoder { return &Decoder{track: track} }

// Decoded 是反解后的报文视图。
type Decoded struct {
	Seq     int64
	RTPSN   uint16
	RTPTS   uint32
	Marker  bool
	Payload []byte
	RawLen  int
}

// Decode 反序列化并展开序列号；重复/乱序由上层抖动缓存处理。
func (d *Decoder) Decode(raw []byte) (*Decoded, error) {
	var rp rtp.Packet
	if err := rp.Unmarshal(raw); err != nil {
		return nil, media.NewError(media.StageTransport, media.KindIO,
			"RTP 反序列化失败").Wrap(err)
	}
	if !d.started {
		d.started = true
		d.lastSeq = int64(rp.SequenceNumber)
	} else {
		d.lastSeq = media.Extend16(d.lastSeq, rp.SequenceNumber)
	}
	return &Decoded{
		Seq:     d.lastSeq,
		RTPSN:   rp.SequenceNumber,
		RTPTS:   rp.Timestamp,
		Marker:  rp.Marker,
		Payload: rp.Payload,
		RawLen:  len(raw),
	}, nil
}

// Reassemble 按报文顺序把同一帧的分片载荷拼回；以 Marker 判定帧尾。
// 输入必须已按序列号去重排序。
func Reassemble(decoded []*Decoded) [][]byte {
	var frames [][]byte
	var cur []byte
	for _, d := range decoded {
		cur = append(cur, d.Payload...)
		if d.Marker {
			frames = append(frames, cur)
			cur = nil
		}
	}
	return frames
}
