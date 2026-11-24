package segment

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"mediatime/internal/clock"
)

// canonicalEntries 把索引条目编码为定长、大端、无映射迭代的字节序列，
// 从而让 SHA256 仅依赖条目内容与顺序。
func canonicalEntries(entries []IndexEntry) []byte {
	buf := make([]byte, 0, len(entries)*48)
	tmp := make([]byte, 8)
	for _, e := range entries {
		binary.BigEndian.PutUint32(tmp, uint32(e.Sequence))
		buf = append(buf, tmp[:4]...)
		binary.BigEndian.PutUint32(tmp, e.TrackID)
		buf = append(buf, tmp[:4]...)
		binary.BigEndian.PutUint32(tmp, e.SampleNumber)
		buf = append(buf, tmp[:4]...)
		binary.BigEndian.PutUint64(tmp, uint64(e.Time))
		buf = append(buf, tmp...)
		binary.BigEndian.PutUint64(tmp, uint64(e.Offset))
		buf = append(buf, tmp...)
		binary.BigEndian.PutUint32(tmp, e.Size)
		buf = append(buf, tmp[:4]...)
		binary.BigEndian.PutUint64(tmp, uint64(e.FileSize))
		buf = append(buf, tmp...)
	}
	return buf
}

// IndexEntry 是随机访问索引中的一个关键帧条目。
//
// 三个可核验不变量：
//  1. Time（90 kHz）随条目严格单调；
//  2. SampleNumber 在同片段内严格单调；
//  3. Offset 位于对应分片文件 [0, FileSize) 内且随条目单调。
type IndexEntry struct {
	Sequence     int        `json:"sequence"`
	TrackID      uint32     `json:"track_id"`
	SampleNumber uint32     `json:"sample_number"`
	Time         clock.Tick `json:"time_90k"`
	Offset       int64      `json:"file_offset"`
	Size         uint32     `json:"size"`
	FileSize     int64      `json:"file_size"`
}

// Index 是全部片段汇总后的随机访问索引。
type Index struct {
	Entries []IndexEntry `json:"entries"`
	// Keys 是索引条目总数（验收字段 index_keys）。
	Keys int `json:"keys"`
	// SHA256 是规范化条目序列的摘要，相同输入/参数/种子下逐字节稳定。
	SHA256 string `json:"sha256"`
}

// BuildIndex 从已提取片段与修复后时间线构建随机访问索引。
// 任一不变量被违反即返回错误，绝不返回部分有效索引。
func BuildIndex(frags []*ExtractedFragment, tl *clock.Timeline) (*Index, error) {
	// 片段序号 → 文件大小，供偏移边界核验。
	fileSize := map[int]int64{}
	for _, f := range frags {
		fileSize[f.Sequence] = f.FileSize
	}
	// 时间线样本键 → 修复后 DTS。
	type key struct {
		seq    int
		track  uint32
		sample uint32
	}
	dtsByKey := make(map[key]clock.Tick, len(tl.Samples))
	for _, s := range tl.Samples {
		dtsByKey[key{s.Sequence, s.TrackID, s.SampleNumber}] = s.DTS
	}

	var entries []IndexEntry
	for _, f := range frags {
		fs, ok := fileSize[f.Sequence]
		if !ok {
			return nil, &FragmentError{Sequence: f.Sequence, Op: "index", Err: ErrBoxOverflow}
		}
		for _, s := range f.Samples {
			if !s.IsSync {
				continue
			}
			dts, ok := dtsByKey[key{f.Sequence, s.TrackID, s.SampleNumber}]
			if !ok {
				return nil, &FragmentError{
					Sequence: f.Sequence, Op: "index lookup", Err: ErrBoxOverflow,
				}
			}
			if s.Offset < 0 || s.Offset+int64(s.Size) > fs {
				return nil, &FragmentError{
					Sequence: f.Sequence, Op: "index offset", Err: ErrBoxOverflow,
				}
			}
			entries = append(entries, IndexEntry{
				Sequence:     f.Sequence,
				TrackID:      s.TrackID,
				SampleNumber: s.SampleNumber,
				Time:         dts,
				Offset:       s.Offset,
				Size:         s.Size,
				FileSize:     fs,
			})
		}
	}

	// 确定性排序：时间 → 片段序号 → 轨道 → 样本号。
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Time != b.Time {
			return a.Time < b.Time
		}
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.SampleNumber < b.SampleNumber
	})

	if err := verifyMonotonic(entries); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonicalEntries(entries))
	return &Index{
		Entries: entries,
		Keys:    len(entries),
		SHA256:  hex.EncodeToString(sum[:]),
	}, nil
}

// verifyMonotonic 独立核验三条单调性/边界不变量。
func verifyMonotonic(entries []IndexEntry) error {
	for i, e := range entries {
		if e.Offset < 0 || e.Offset >= e.FileSize || e.Offset+int64(e.Size) > e.FileSize {
			return &FragmentError{Sequence: e.Sequence, Op: "bounds", Err: ErrBoxOverflow}
		}
		if i == 0 {
			continue
		}
		prev := entries[i-1]
		if e.Time <= prev.Time {
			return &FragmentError{Sequence: e.Sequence, Op: "time monotonic", Err: ErrBoxOverflow}
		}
		// 文件偏移在同一分片内严格单调；跨分片的 mdat 各自从低偏移开始，不能全局比较。
		if e.Sequence == prev.Sequence && e.Offset <= prev.Offset {
			return &FragmentError{Sequence: e.Sequence, Op: "offset monotonic", Err: ErrBoxOverflow}
		}
		if e.Sequence == prev.Sequence && e.SampleNumber <= prev.SampleNumber {
			return &FragmentError{Sequence: e.Sequence, Op: "sample monotonic", Err: ErrBoxOverflow}
		}
	}
	return nil
}
