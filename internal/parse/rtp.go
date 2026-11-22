package parse

import (
	"fmt"

	"github.com/pion/rtp"

	"fieldcut/internal/timeline"
)

// RTPPacket 是 pion/rtp 解包后的时间戳视图，保留包边界与原始时间戳。
type RTPPacket struct {
	Sequence  uint16
	RawTS     uint32 // 原始 RTP 时间戳（回绕前）
	Marker    bool
	Payload   []byte
	Ns        int64 // 映射到共同时间线的纳秒
	WrapCount uint64
}

// RTPTrack 处理单条 RTP 流的时间戳扩展、回绕与丢包检测。
type RTPTrack struct {
	SSRC      uint32
	ClockRate uint32
	TrackID   uint32
	Kind      string
	mapper    *timeline.Mapper
	graph     *timeline.Graph
	lastSeq   uint16
	haveSeq   bool
	Lost      int
	// MaxBackStep 判定回绕的最大允许回退 tick，默认 1 秒的时钟数。
	MaxBackStep uint64
}

// NewRTPTrack 在给定时间线图中注册 RTP 轨道并返回处理器。
func NewRTPTrack(g *timeline.Graph, trackID uint32, kind string,
	ssrc uint32, clockRate uint32) (*RTPTrack, error) {
	clock, err := timeline.NewClock(trackID, kind, clockRate, 0)
	if err != nil {
		return nil, err
	}
	if err := g.Add(&timeline.Track{
		ID: trackID, Kind: kind, Label: fmt.Sprintf("rtp-%d", ssrc),
		Codec: "rtp", Clock: clock,
	}); err != nil {
		return nil, err
	}
	backstep := uint64(clockRate) // 1 秒
	return &RTPTrack{
		SSRC: ssrc, ClockRate: clockRate, TrackID: trackID, Kind: kind,
		mapper: timeline.NewMapper(g), graph: g, MaxBackStep: backstep,
	}, nil
}

// Ingest 解析一个 RTP 数据报，完成时间戳映射与序列号连续性检查。
// 解析失败返回错误且不改变内部状态，保证重跑可核验。
func (t *RTPTrack) Ingest(datagram []byte) (RTPPacket, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(datagram); err != nil {
		return RTPPacket{}, &Error{Level: LevelRecoverable, BoxType: "rtp",
			TrackID: t.TrackID, Message: err.Error()}
	}
	if t.haveSeq {
		gap := pkt.SequenceNumber - (t.lastSeq + 1)
		if gap != 0 {
			// uint16 回绕天然成立；gap 为大无符号值时表示重复/乱序。
			if gap < 0x8000 {
				t.Lost += int(gap)
			}
		}
	}
	t.lastSeq = pkt.SequenceNumber
	t.haveSeq = true

	pt, err := t.mapper.Map(t.TrackID, uint64(pkt.Timestamp), t.MaxBackStep)
	if err != nil {
		// 保留原始时间戳与轨道标识，不做猜测。
		return RTPPacket{Sequence: pkt.SequenceNumber, RawTS: pkt.Timestamp,
			Marker: pkt.Marker, Payload: pkt.Payload}, err
	}
	return RTPPacket{
		Sequence: pkt.SequenceNumber, RawTS: pkt.Timestamp,
		Marker: pkt.Marker, Payload: pkt.Payload,
		Ns: pt.Ns, WrapCount: pt.Wrap,
	}, nil
}

// Wraps 返回该流累计时间戳回绕次数。
func (t *RTPTrack) Wraps() uint64 { return t.mapper.Wraps(t.TrackID) }

// MarshalPacket 用 pion 重新序列化一个包，供写入/往返一致性测试。
func MarshalPacket(sequence uint16, ts uint32, ssrc uint32,
	marker bool, payloadType byte, payload []byte) ([]byte, error) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: sequence,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         marker,
		},
		Payload: payload,
	}
	return pkt.Marshal()
}
