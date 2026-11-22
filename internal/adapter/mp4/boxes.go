// Package mp4 基于 go-mp4 的 box 头读取实现 fMP4 解析：
// go-mp4 负责可靠的 box 边界识别，本包按 ISO/IEC 14496-12 手动解析
// mfhd/tfdt/trun/mdhd/tkhd/colr 等 payload，未知 box 保留并计入摘要。
package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"

	go_mp4 "github.com/abema/go-mp4"

	"mediacheck/internal/model"
)

// 容器 box 白名单：只有这些 box 才递归进入，其余按叶子 box 处理。
var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"moof": true, "traf": true, "udta": true, "edts": true, "mvex": true,
}

type rawBox struct {
	info    *go_mp4.BoxInfo
	payload []byte // 叶子 box 的完整 payload（已做上限校验）
}

// walkBoxes 从 r 当前位置遍历到 end，对每个叶子/容器 box 调用 fn。
// 截断（声明 size 超出数据尾部）与超大 box 在此统一转为结构化错误。
func walkBoxes(r *bytes.Reader, end int64, depth int, fn func(rawBox, int) error) error {
	for r.Size() != 0 {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if pos >= end {
			return nil
		}
		info, err := go_mp4.ReadBoxInfo(r)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: pos, Message: "box header truncated"}
			}
			return &model.MediaError{Category: model.ErrTruncatedBox, Offset: pos, Message: err.Error()}
		}
		boxEnd := int64(info.Offset + info.Size)
		if boxEnd > end || boxEnd <= pos {
			return &model.MediaError{
				Category: model.ErrTruncatedBox,
				Offset:   int64(info.Offset),
				Message:  "box size exceeds enclosing box or data end",
			}
		}
		typ := info.Type.String()
		rb := rawBox{info: info}
		if containers[typ] {
			if err := fn(rb, depth); err != nil {
				return err
			}
			if _, err := info.SeekToPayload(r); err != nil {
				return err
			}
			if err := walkBoxes(r, boxEnd, depth+1, fn); err != nil {
				return err
			}
			if _, err := r.Seek(boxEnd, io.SeekStart); err != nil {
				return err
			}
			continue
		}
		payloadLen := boxEnd - int64(info.Offset+info.HeaderSize)
		if payloadLen < 0 {
			return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(info.Offset)}
		}
		buf := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, buf); err != nil {
			return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(info.Offset), Message: err.Error()}
		}
		rb.payload = buf
		if err := fn(rb, depth); err != nil {
			return err
		}
		if _, err := r.Seek(boxEnd, io.SeekStart); err != nil {
			return err
		}
	}
	return nil
}

// summarize 收集 box 摘要，未知 box（go-mp4 不识别类型）显式标记。
func summarize(list *[]model.BoxSummary) func(rawBox, int) error {
	return func(rb rawBox, depth int) error {
		typ := rb.info.Type.String()
		*list = append(*list, model.BoxSummary{
			Type: typ, Offset: int64(rb.info.Offset),
			Size: int64(rb.info.Size), Depth: depth,
			Unknown: !rb.info.IsSupportedType(),
		})
		return nil
	}
}

func contentHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readU24 读取三字节大端无符号整数（colr/stsd 中使用）。
func readU24(b []byte, off int) (uint32, bool) {
	if off+3 > len(b) {
		return 0, false
	}
	return uint32(b[off])<<16 | uint32(b[off+1])<<8 | uint32(b[off+2]), true
}
