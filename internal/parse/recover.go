package parse

import (
	"io"

	"github.com/abema/go-mp4"
)

// Recovery 描述一次恢复的量化结果。
type Recovery struct {
	Reclaimed      int64  `json:"reclaimed_bytes"`  // 跳过坏片段后仍可用的字节
	LostTail       int64  `json:"lost_tail_bytes"`  // 尾部确认丢失的字节
	LastGoodOffset int64  `json:"last_good_offset"` // 最后一个完整盒的结束偏移
	LastGoodBox    string `json:"last_good_box"`
	Truncated      bool   `json:"truncated"`
}

// Recover 在容错遍历结果上计算可保留范围。
// 规则：尾部截断保留最后一个完整样本所在盒；坏盒之前的完整内容全部保留，
// 之后的内容按盒逐个继续尝试（由 Walk 的边界记录描述）。
func Recover(r io.ReadSeeker, roots []*RawBox, fileSize int64, rep *Report) Recovery {
	rc := Recovery{LastGoodOffset: 0, LastGoodBox: "-"}
	var walk func(nodes []*RawBox)
	walk = func(nodes []*RawBox) {
		for _, n := range nodes {
			end := int64(n.Info.Offset + n.Info.Size)
			if end <= fileSize && end > rc.LastGoodOffset {
				rc.LastGoodOffset = end
				rc.LastGoodBox = n.Type()
			}
			if len(n.Children) > 0 {
				walk(n.Children)
			}
		}
	}
	walk(roots)
	if rep.Truncated {
		rc.Truncated = true
		rc.LostTail = fileSize - rc.LastGoodOffset
	}
	// 可恢复字节 = 最后完整位置之前全部可用内容。
	rc.Reclaimed = rc.LastGoodOffset
	rep.RecoveredBytes = rc.Reclaimed
	return rc
}

// SafePrefix 返回可以整体信任的字节前缀长度：取第一个坏盒/截断边界偏移。
func SafePrefix(rep *Report, fileSize int64) int64 {
	safe := fileSize
	for _, b := range rep.Boundaries {
		if b.Offset >= 0 && b.Offset < safe {
			safe = b.Offset
		}
	}
	return safe
}

// SkipPayload 将读指针移动到盒净荷之后，用于显式跳过不支持的盒。
func SkipPayload(r io.ReadSeeker, b *RawBox) error {
	_, err := r.Seek(int64(b.Info.Offset+b.Info.Size), io.SeekStart)
	return err
}

// IsKnownBox 报告盒类型是否被 go-mp4 注册（可语义解析）。
func IsKnownBox(t string) bool {
	return mp4.StrToBoxType(t).IsSupported(mp4.Context{})
}
