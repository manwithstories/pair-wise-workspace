package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"mediaarchive/internal/clock"
)

// SourceRef 指向源归档文件中的精确位置。
type SourceRef struct {
	Path         string `json:"path"`
	Container    string `json:"container"` // rtp-records 或 mp4
	Offset       int64  `json:"offset"`    // RTP 头或 MP4 sample 数据偏移
	RecordOffset int64  `json:"record_offset,omitempty"`
	Length       int    `json:"length"`
	Box          string `json:"box,omitempty"`
	SampleIndex  int    `json:"sample_index,omitempty"`
}

// Packet 是 RTP/MP4 归一后的媒体包。
type Packet struct {
	No           int         `json:"no"` // 生成目录中的全局包序号
	TrackID      string      `json:"track_id"`
	Codec        string      `json:"codec"`
	Seq          uint16      `json:"seq"`
	ExtSeq       int64       `json:"ext_seq"`
	Timestamp    uint32      `json:"timestamp"`
	ExtTimestamp int64       `json:"ext_timestamp"`
	Start        clock.Ticks `json:"start_ticks"`
	Duration     clock.Ticks `json:"duration_ticks"`
	Hash         string      `json:"sha256"`
	Source       SourceRef   `json:"source"`
	Payload      []byte      `json:"-"`
}

// HashBytes 返回稳定的内容哈希。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Identity 返回“轨道 + 扩展序号”身份键。
func (p Packet) Identity() string {
	return p.TrackID + "#" + strconv.FormatInt(p.ExtSeq, 10)
}
