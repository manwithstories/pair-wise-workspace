package rtp

import (
	"bytes"
	"fmt"

	mp4 "github.com/abema/go-mp4"
)

// MP4Issue 为 MP4 结构问题，Offset 为最早可定位的盒子字节偏移。
type MP4Issue struct {
	Kind   string // missing_index / sample_out_of_range / count_mismatch
	Offset int64
	Detail string
}

// MP4Info 为自生成 MP4 样例的轨道信息。
type MP4Info struct {
	Timescale        uint32
	SampleCount      uint64
	FirstErrorOffset int64
	Issues           []MP4Issue
}

// containerTypes 为需要递归展开的容器盒。
var containerTypes = []mp4.BoxType{
	mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
	mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeEdts(), mp4.BoxTypeDinf(),
}

func isContainer(t mp4.BoxType) bool {
	for _, c := range containerTypes {
		if t == c {
			return true
		}
	}
	return false
}

// ReadMP4 用 go-mp4 遍历盒子树，提取轨道时基并校验样本表一致性。
// 发现缺失索引或越界样本时报告最早可定位位置。
func ReadMP4(data []byte) MP4Info {
	info := MP4Info{FirstErrorOffset: -1}
	r := bytes.NewReader(data)
	var sttsTotal uint64
	var sttsOff, stszOff, stcoOff int64 = -1, -1, -1
	var stszCount, stszBytes, maxChunkEnd uint64

	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		if !h.BoxInfo.IsSupportedType() {
			return nil, nil
		}
		payload, _, perr := h.ReadPayload()
		if perr != nil {
			return nil, perr
		}
		off := int64(h.BoxInfo.Offset)
		switch box := payload.(type) {
		case *mp4.Mdhd:
			if info.Timescale == 0 {
				info.Timescale = box.Timescale
			}
		case *mp4.Stts:
			sttsOff = off
			for _, e := range box.Entries {
				sttsTotal += uint64(e.SampleCount)
			}
		case *mp4.Stsz:
			stszOff = off
			stszCount = uint64(box.SampleCount)
			if box.SampleSize == 0 {
				for _, s := range box.EntrySize {
					stszBytes += uint64(s)
				}
			} else {
				stszBytes = uint64(box.SampleSize) * stszCount
			}
		case *mp4.Stco:
			stcoOff = off
			for _, co := range box.ChunkOffset {
				if end := uint64(co) + stszBytes; end > maxChunkEnd {
					maxChunkEnd = end
				}
			}
		}
		if isContainer(h.BoxInfo.Type) {
			_, err := h.Expand()
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		info.add(MP4Issue{Kind: "missing_index", Offset: 0, Detail: err.Error()})
		return info
	}
	info.SampleCount = sttsTotal

	if info.Timescale == 0 {
		info.add(MP4Issue{Kind: "missing_index", Offset: 0, Detail: "no mdhd timescale"})
	}
	if sttsOff < 0 {
		info.add(MP4Issue{Kind: "missing_index", Offset: 0, Detail: "missing stts (sample index)"})
	}
	if stszOff < 0 {
		info.add(MP4Issue{Kind: "missing_index", Offset: 0, Detail: "missing stsz (sample sizes)"})
	}
	if sttsOff >= 0 && stszOff >= 0 && sttsTotal != stszCount {
		off := sttsOff
		if stszOff < off {
			off = stszOff
		}
		info.add(MP4Issue{Kind: "count_mismatch", Offset: off,
			Detail: fmt.Sprintf("stts total %d != stsz count %d", sttsTotal, stszCount)})
	}
	if stcoOff >= 0 && maxChunkEnd > uint64(len(data)) {
		info.add(MP4Issue{Kind: "sample_out_of_range", Offset: stcoOff,
			Detail: fmt.Sprintf("chunk end %d beyond file size %d", maxChunkEnd, len(data))})
	}
	return info
}

func (m *MP4Info) add(i MP4Issue) {
	m.Issues = append(m.Issues, i)
	if m.FirstErrorOffset < 0 || i.Offset < m.FirstErrorOffset {
		m.FirstErrorOffset = i.Offset
	}
}
