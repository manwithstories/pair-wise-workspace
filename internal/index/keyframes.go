// Package index 校验外部关键帧索引并与裸流扫描结果交叉核对。
// 索引可能缺失、过期、重复或越界；无法确认时报告最小可疑字节范围，
// 并支持在隔离的重建结果上安全重建。
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"media-audit/internal/stream"
)

// Entry 是一条关键帧索引记录：帧序号与其在裸流中的字节偏移。
type Entry struct {
	Frame  int   `json:"frame"`
	Offset int64 `json:"offset_bytes"`
}

// Index 是一条轨道的关键帧索引文件内容。
type Index struct {
	TrackID    int     `json:"track_id"`
	StreamHash string  `json:"stream_sha256"` // 索引对应的裸流内容哈希
	Entries    []Entry `json:"entries"`
}

// Status 描述索引校验结论。
type Status string

// 索引校验状态。
const (
	StatusValid      Status = "valid"      // 与扫描结果完全一致
	StatusMissing    Status = "rebuilt"    // 索引缺失，已安全重建
	StatusRebuilt    Status = "rebuilt"    // 别名：过期/损坏后重建
	StatusSuspicious Status = "suspicious" // 无法确认，给出最小可疑范围
)

// Problem 是索引层面的结构化问题。
type Problem struct {
	Kind    string `json:"kind"`
	Frame   int    `json:"frame"`
	Offset  int64  `json:"offset_bytes"`
	RangeLo int64  `json:"suspect_lo"`
	RangeHi int64  `json:"suspect_hi"`
	Message string `json:"message"`
}

// 索引问题类别。
const (
	KindDuplicate  = "duplicate_entry"
	KindOutOfRange = "offset_out_of_range"
	KindStale      = "stale_index"
	KindMismatch   = "keyframe_mismatch"
)

// Result 是校验与（可能的）重建结果。
type Result struct {
	Status   Status
	Rebuilt  *Index
	Problems []Problem
}

// Verify 把外部索引与扫描结果交叉校验。streamHash 为当前裸流的实际哈希。
// external 为 nil 表示索引缺失，将直接基于扫描结果重建。
func Verify(external *Index, scan *stream.ScanResult, streamHash string) (*Result, error) {
	if scan == nil {
		return nil, errors.New("扫描结果为空，无法校验索引")
	}
	truth := groundTruth(scan, streamHash)
	if external == nil {
		return &Result{Status: StatusMissing, Rebuilt: truth}, nil
	}

	// 哈希不匹配 => 索引对应的输入版本已过期，整体不可信，安全重建。
	if external.StreamHash != "" && external.StreamHash != streamHash {
		return &Result{
			Status:  StatusRebuilt,
			Rebuilt: truth,
			Problems: []Problem{{
				Kind:    KindStale,
				Message: "索引流哈希与当前输入不匹配，已按当前扫描重建",
			}},
		}, nil
	}

	probs := checkEntries(external, scan)
	if len(probs) > 0 {
		// 条目级损坏：能定位最小可疑范围时标记 suspicious，否则整体重建。
		return &Result{
			Status:   StatusSuspicious,
			Rebuilt:  truth,
			Problems: probs,
		}, nil
	}
	return &Result{Status: StatusValid, Rebuilt: external}, nil
}

// groundTruth 从扫描结果构造权威关键帧索引。
func groundTruth(scan *stream.ScanResult, streamHash string) *Index {
	idx := &Index{TrackID: scan.TrackID, StreamHash: streamHash}
	for _, au := range scan.AUs {
		if au.Keyframe {
			idx.Entries = append(idx.Entries, Entry{
				Frame:  au.Index,
				Offset: au.Offset,
			})
		}
	}
	return idx
}

// checkEntries 检测重复、越界与关键帧不匹配，返回最小可疑字节范围。
func checkEntries(ext *Index, scan *stream.ScanResult) []Problem {
	var probs []Problem
	seen := map[int]bool{}
	// 扫描结果的真实关键帧偏移集合，用于不匹配判定。
	truthOff := map[int64]bool{}
	for _, au := range scan.AUs {
		if au.Keyframe {
			truthOff[au.Offset] = true
		}
	}
	for _, e := range ext.Entries {
		if seen[e.Frame] {
			probs = append(probs, Problem{
				Kind: KindDuplicate, Frame: e.Frame, Offset: e.Offset,
				RangeLo: e.Offset, RangeHi: e.Offset + 1,
				Message: "关键帧索引存在重复帧序号",
			})
			continue
		}
		seen[e.Frame] = true
		if e.Offset < 0 || e.Offset >= scan.Size {
			probs = append(probs, Problem{
				Kind: KindOutOfRange, Frame: e.Frame, Offset: e.Offset,
				RangeLo: clampRange(e.Offset, scan.Size),
				RangeHi: clampRange(e.Offset+1, scan.Size),
				Message: "关键帧偏移越界",
			})
			continue
		}
		if !truthOff[e.Offset] {
			// 偏移落在真实流内但不是关键帧：缩小到包含该偏移的 AU 区间。
			lo, hi := containingAURange(scan, e.Offset)
			probs = append(probs, Problem{
				Kind: KindMismatch, Frame: e.Frame, Offset: e.Offset,
				RangeLo: lo, RangeHi: hi,
				Message: "索引偏移不指向随机访问帧",
			})
		}
	}
	sort.Slice(probs, func(i, j int) bool { return probs[i].Offset < probs[j].Offset })
	return probs
}

// containingAURange 返回包含 offset 的 AU 字节区间，即最小可疑范围。
func containingAURange(scan *stream.ScanResult, offset int64) (lo, hi int64) {
	for _, au := range scan.AUs {
		if offset >= au.Offset && offset < au.End {
			return au.Offset, au.End
		}
	}
	// 落不到任何 AU 时退化为单字节范围。
	return clampRange(offset, scan.Size), clampRange(offset+1, scan.Size)
}

func clampRange(v int64, size int64) int64 {
	if v < 0 {
		return 0
	}
	if v > size {
		return size
	}
	return v
}

// HashBytes 返回内容的 SHA-256 十六进制摘要。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
