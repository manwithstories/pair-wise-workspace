// Package rtp 解析本地生成的 RTP 样本并展开时间戳。
//
// 仅处理结构与时间戳，不做任何网络收发或媒体解码。32 位 RTP 时间戳
// 通过 clock.UnwrapRTP 展开为 int64 单调序列，回绕与负偏移都可表达。
package rtp

import (
	"fmt"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Track 是一串 RTP 包构成的本地样本轨道。
type Track struct {
	Source    model.Source
	ClockRate clock.Rational
	// Packets 为已序列化的 RTP 包字节（测试夹具生成）。
	Packets [][]byte
}

// Sample 是展开后的 RTP 样本。
type Sample struct {
	Sequence  uint16
	Timestamp int64 // 展开后的单调时间戳，允许负偏移
	Marker    bool
	Payload   []byte
}

// Decode 解析 RTP 包并展开时间戳，返回按序号排列的样本与首个包错误。
// 单个包解析失败只产生可恢复错误并跳过该包，不中断整条轨道。
func Decode(t Track) ([]Sample, []error) {
	out := make([]Sample, 0, len(t.Packets))
	var errs []error
	var prev int64
	first := true
	for i, raw := range t.Packets {
		var p rtp.Packet
		if err := p.Unmarshal(raw); err != nil {
			errs = append(errs, fmt.Errorf("rtp: packet %d decode: %w", i, err))
			continue
		}
		if first {
			prev = int64(p.Timestamp)
			first = false
		}
		ts := clock.UnwrapRTP(prev, p.Timestamp)
		prev = ts
		out = append(out, Sample{
			Sequence:  p.SequenceNumber,
			Timestamp: ts,
			Marker:    p.Marker,
			Payload:   append([]byte(nil), p.Payload...),
		})
	}
	return out, errs
}

// ToManifestSamples 把展开后的 RTP 样本映射为模型样本。
// keyframe 由 RTP marker 位决定（夹具约定 marker 标记关键帧）。
func ToManifestSamples(trackID string, ss []Sample) []model.Sample {
	out := make([]model.Sample, len(ss))
	for i, s := range ss {
		out[i] = model.Sample{
			TrackID:  trackID,
			Index:    uint32(i + 1),
			PTS:      s.Timestamp,
			Duration: 1,
			Size:     uint32(len(s.Payload)),
			Keyframe: s.Marker,
			Complete: true,
		}
	}
	return out
}

// Encode 生成一个 RTP 包的序列化字节，供测试夹具使用。
func Encode(seq uint16, ts uint32, marker bool, payload []byte) ([]byte, error) {
	p := &rtp.Packet{
		Payload: payload,
	}
	p.Version = 2
	p.PayloadType = 96
	p.SequenceNumber = seq
	p.Timestamp = ts
	p.Marker = marker
	p.SSRC = 4141
	return p.Marshal()
}
