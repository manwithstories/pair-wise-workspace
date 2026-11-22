package parse

import (
	"io"

	"github.com/abema/go-mp4"
)

// CopyContainer 用 go-mp4 Writer 把遍历得到的顶层盒原样回写到 w。
// 回写后重新解析应得到相同的样本时间戳与事件，用于往返一致性核验。
// 遇到截断/坏盒时仅复制完整的顶层盒，损坏部分自然排除在外。
func CopyContainer(r io.ReadSeeker, roots []*RawBox, w io.WriteSeeker) error {
	mw := mp4.NewWriter(w)
	for _, n := range roots {
		if err := mw.CopyBox(r, n.Info); err != nil {
			return &Error{Level: LevelRecoverable, BoxType: n.Type(),
				Offset: int64(n.Info.Offset), Message: err.Error()}
		}
	}
	return nil
}
