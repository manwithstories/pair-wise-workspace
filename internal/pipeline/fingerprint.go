package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
)

// Segment 是对齐共同时间线切出的一个可剪辑片段。
type Segment struct {
	ID         string `json:"id"`
	Camera     string `json:"camera"`      // 来源机位标签
	TrackID    uint32 `json:"track_id"`
	StartNs    int64  `json:"start_ns"`
	EndNs      int64  `json:"end_ns"`
	ErrNs      int64  `json:"err_ns"`       // 边界时钟误差范围
	SafeCut    bool   `json:"safe_cut"`     // 是否对齐确认的随机访问点
	StartSample uint32 `json:"start_sample"`
	EndSample  uint32 `json:"end_sample"`
	ByteRanges []ByteRange `json:"byte_ranges"`
	SHA256     string `json:"sha256"`
	Degraded   bool   `json:"degraded"`     // 含部分损坏/不安全边界
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// ByteRange 是片段在源文件中占用的对齐字节区间。
type ByteRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// Fingerprinter 对片段的对齐字节区间计算确定性 SHA-256。
// 读取器由调用方提供，相同输入在任意重跑中产生相同哈希。
type Fingerprinter struct {
	readAt func(offset int64, p []byte) (int, error)
}

// NewFingerprinter 接收一个按偏移读取的函数（通常包装 *os.File/bytes.Reader）。
func NewFingerprinter(readAt func(offset int64, p []byte) (int, error)) *Fingerprinter {
	return &Fingerprinter{readAt: readAt}
}

// HashRanges 对一组字节区间按顺序哈希，区间边界也参与摘要，
// 保证不同切分布局即使内容重叠也不会碰撞。
func (fp *Fingerprinter) HashRanges(ranges []ByteRange) (string, int64, error) {
	h := sha256.New()
	var total int64
	lenBuf := make([]byte, 8)
	for _, r := range ranges {
		// 写入区间定界，防止 [A][B] 与 [AB] 产生相同摘要。
		putBE(lenBuf, uint64(r.Offset))
		h.Write(lenBuf)
		putBE(lenBuf, uint64(r.Length))
		h.Write(lenBuf)
		buf := make([]byte, r.Length)
		if r.Length > 0 {
			n, err := fp.readAt(r.Offset, buf)
			if err != nil {
				return "", total, err
			}
			h.Write(buf[:n])
			total += int64(n)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

// ApplyFingerprint 计算并填充片段哈希。
func (fp *Fingerprinter) ApplyFingerprint(seg *Segment) error {
	sum, n, err := fp.HashRanges(seg.ByteRanges)
	if err != nil {
		return err
	}
	seg.SHA256 = sum
	_ = n
	return nil
}

// Dedup 按哈希分组，重复片段指向首个出现者（保留首个，其余标记 DuplicateOf）。
// 输入顺序决定保留对象，因此重跑结果稳定。
func Dedup(segs []*Segment) int {
	first := make(map[string]string)
	dupes := 0
	for _, s := range segs {
		if s.SHA256 == "" {
			continue
		}
		if id, ok := first[s.SHA256]; ok {
			s.DuplicateOf = id
			dupes++
			continue
		}
		first[s.SHA256] = s.ID
	}
	return dupes
}

func putBE(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}
