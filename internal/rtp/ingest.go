// Package rtp 校验跨节点 RTP 包序与时间戳，并读取自生成 MP4 样例的轨道时基。
// 本地文件格式：每条记录为 [uint16 大端长度][RTP 包字节]。
package rtp

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pion/rtp"
)

// Issue 为可恢复的传输问题。
type Issue struct {
	Kind   string // gap / duplicate / reorder / timestamp_jump / malformed
	Offset int64  // 问题记录在文件中的字节偏移
	Detail string
}

func (i Issue) Error() string {
	return fmt.Sprintf("%s at byte %d: %s", i.Kind, i.Offset, i.Detail)
}

// IngestResult 汇总 RTP 校验结果。
type IngestResult struct {
	Packets          int
	SeqGaps          int // 缺包数（按序号差累计）
	Duplicates       int
	Reorders         int
	TimestampWraps   int // 合法时间戳回绕次数
	FirstErrorOffset int64
	Issues           []Issue
}

// Ingest 解析并校验 RTP 记录流。单包损坏不中断后续校验。
func Ingest(data []byte) IngestResult {
	res := IngestResult{FirstErrorOffset: -1}
	var prevSeq uint16
	var prevTS uint32
	havePrev := false
	off := int64(0)
	for off+2 <= int64(len(data)) {
		n := int64(binary.BigEndian.Uint16(data[off:]))
		if n <= 0 || off+2+n > int64(len(data)) {
			res.addIssue(Issue{Kind: "malformed", Offset: off,
				Detail: fmt.Sprintf("bad record length %d", n)})
			break // 长度字段损坏，无法定位下一条，停止
		}
		raw := data[off+2 : off+2+n]
		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			res.addIssue(Issue{Kind: "malformed", Offset: off, Detail: err.Error()})
			off += 2 + n
			continue
		}
		res.Packets++
		if havePrev {
			res.checkOrder(prevSeq, pkt.SequenceNumber, off)
			res.checkTimestamp(prevTS, pkt.Timestamp, off)
		}
		prevSeq = pkt.SequenceNumber
		prevTS = pkt.Timestamp
		havePrev = true
		off += 2 + n
	}
	if off != int64(len(data)) && len(res.Issues) == 0 {
		res.addIssue(Issue{Kind: "malformed", Offset: off, Detail: "trailing bytes"})
	}
	return res
}

func (r *IngestResult) addIssue(i Issue) {
	r.Issues = append(r.Issues, i)
	if r.FirstErrorOffset < 0 || i.Offset < r.FirstErrorOffset {
		r.FirstErrorOffset = i.Offset
	}
}

// checkOrder 校验序号连续性，处理 16 位回绕。
func (r *IngestResult) checkOrder(prev, cur uint16, off int64) {
	diff := cur - prev // uint16 回绕算术
	switch {
	case diff == 0:
		r.Duplicates++
		r.addIssue(Issue{Kind: "duplicate", Offset: off, Detail: fmt.Sprintf("seq %d repeated", cur)})
	case diff == 1:
		// 正常顺序
	case diff < 0x8000:
		// 前向跳号：缺包
		gap := int(diff) - 1
		r.SeqGaps += gap
		r.addIssue(Issue{Kind: "gap", Offset: off,
			Detail: fmt.Sprintf("seq %d -> %d, missing %d", prev, cur, gap)})
	default:
		// 序号倒退：乱序
		r.Reorders++
		r.addIssue(Issue{Kind: "reorder", Offset: off,
			Detail: fmt.Sprintf("seq %d -> %d (out of order)", prev, cur)})
	}
}

// checkTimestamp 校验时间戳单调性；32 位回绕为合法事件。
func (r *IngestResult) checkTimestamp(prev, cur uint32, off int64) {
	if cur >= prev {
		if prev > 0xFFFFF000 && cur < 0x00001000 {
			r.TimestampWraps++
		}
		return
	}
	// cur < prev：仅当属于回绕（prev 接近上限且 cur 接近 0）时合法
	if prev > 0xFFFFF000 && cur < 0x00010000 {
		r.TimestampWraps++
		return
	}
	r.addIssue(Issue{Kind: "timestamp_jump", Offset: off,
		Detail: fmt.Sprintf("timestamp %d -> %d goes backward", prev, cur)})
}

// Encode 将 RTP 包编码为本地记录格式（供夹具生成使用）。
func Encode(w io.Writer, pkt *rtp.Packet) error {
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	if len(raw) > 0xFFFF {
		return fmt.Errorf("packet too large: %d", len(raw))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(raw)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}
