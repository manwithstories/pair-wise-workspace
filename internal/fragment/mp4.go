// Package fragment 解析 fMP4 初始化段与媒体段的 box 结构，
// 报告截断边界与损坏 box 的原始偏移，且不把后续字节误认成新 box。
package fragment

import (
	"bytes"
	"encoding/binary"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediadeliver/internal/fault"
)

// 解析上限，保证内存有界。
const (
	MaxBoxSize = 64 << 20
	MaxBoxes   = 10000
)

// Box 是一个已识别的顶层 box。
type Box struct {
	Type   string `json:"type"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

// SidxRef 是 sidx 中的一条分片索引引用。
type SidxRef struct {
	Size     uint32 `json:"size"`
	Duration uint32 `json:"duration"`
}

// Result 是 fMP4 解析结果。
type Result struct {
	Source    string     `json:"source"`
	Boxes     []Box      `json:"boxes"`
	SidxRefs  []SidxRef  `json:"sidx_refs,omitempty"`
	Timescale uint32     `json:"timescale,omitempty"`
	HasInit   bool       `json:"has_init"` // 见到 ftyp 与 moov
	Truncated bool       `json:"truncated"`
	Faults    fault.List `json:"faults,omitempty"`
}

// Parse 解析 data 中的顶层 box 序列。
func Parse(source string, data []byte) Result {
	res := Result{Source: source}
	r := bytes.NewReader(data)
	var sawFtyp, sawMoov bool
	for {
		if r.Len() == 0 {
			break
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				res.Truncated = true
				res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: source,
					Offset: int64(len(data) - r.Len()), Recoverable: true,
					Message: "truncated box header", Advice: "保留已解析 box，等待重传"})
				break
			}
			res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: source,
				Offset: int64(len(data) - r.Len()), Message: "box read error: " + err.Error(),
				Advice: "检查采集链路"})
			break
		}
		off := int64(bi.Offset)
		// 长度校验：越界或超限时停止，绝不在残余字节中重新同步。
		if bi.Size < bi.HeaderSize || bi.Size > MaxBoxSize {
			res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: source,
				Offset: off, Message: "invalid box size", Advice: "拒绝该分片，从下一分片恢复"})
			break
		}
		if int64(bi.Size)-int64(bi.HeaderSize) > int64(r.Len()) {
			res.Truncated = true
			res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: source,
				Offset: off, Recoverable: true,
				Message: "box extends past end of data", Advice: "保留前段，等待剩余字节"})
			break
		}
		if len(res.Boxes) >= MaxBoxes {
			res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: source,
				Offset: off, Message: "box count limit exceeded", Advice: "拒绝该分片"})
			break
		}
		res.Boxes = append(res.Boxes, Box{Type: bi.Type.String(), Offset: off, Size: int64(bi.Size)})
		payloadLen := int64(bi.Size) - int64(bi.HeaderSize)
		switch bi.Type {
		case mp4.BoxTypeFtyp():
			sawFtyp = true
			skip(r, payloadLen)
		case mp4.BoxTypeMoov():
			sawMoov = true
			skip(r, payloadLen)
		case mp4.BoxTypeSidx():
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(r, payload); err == nil {
				res.parseSidx(payload, off)
			}
		default:
			skip(r, payloadLen)
		}
	}
	res.HasInit = sawFtyp && sawMoov
	return res
}

func skip(r *bytes.Reader, n int64) { _, _ = r.Seek(n, io.SeekCurrent) }

// parseSidx 手动解析 sidx 负载；引用条目逐条解析，
// 不完整时保留已解析条目并标记截断。
func (res *Result) parseSidx(p []byte, boxOff int64) {
	if len(p) < 4 {
		res.Truncated = true
		return
	}
	version := p[0]
	hdr := 4 + 4 + 4 // flags+ver, refID, timescale
	if len(p) < hdr {
		res.Truncated = true
		return
	}
	res.Timescale = binary.BigEndian.Uint32(p[8:12])
	if version == 1 {
		hdr += 16
	} else {
		hdr += 8
	}
	hdr += 2 // reserved
	if len(p) < hdr+2 {
		res.Truncated = true
		res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: res.Source,
			Offset: boxOff + int64(len(p)), Recoverable: true,
			Message: "sidx header truncated", Advice: "保留已解析引用"})
		return
	}
	count := int(binary.BigEndian.Uint16(p[hdr : hdr+2]))
	pos := hdr + 2
	for i := 0; i < count; i++ {
		if pos+12 > len(p) {
			res.Truncated = true
			res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageFragment, Source: res.Source,
				Offset: boxOff + int64(pos), Recoverable: true,
				Message: "sidx reference truncated", Advice: "使用已解析引用定位前段"})
			return
		}
		size := binary.BigEndian.Uint32(p[pos:pos+4]) & 0x7fffffff
		dur := binary.BigEndian.Uint32(p[pos+4 : pos+8])
		res.SidxRefs = append(res.SidxRefs, SidxRef{Size: size, Duration: dur})
		pos += 12
	}
}
