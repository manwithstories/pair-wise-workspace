// Package manifest 生成稳定排序的分片清单与边界摘要，采用临时文件 +
// 原子替换写出，并对内容计算 SHA-256 供重复运行一致性校验。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"media-audit/internal/stream"
)

// Segment 是清单中的一个分片。
type Segment struct {
	Index           int                  `json:"index"`
	StartMicros     int64                `json:"start_micros"`
	EndMicros       int64                `json:"end_micros"`
	DurationUS      int64                `json:"duration_micros"`
	Representations map[string]ByteRange `json:"representations"`
	Errors          []stream.Issue       `json:"error_summary"`
	SHA256          string               `json:"sha256"`
}

// ByteRange 是某表示层在该分片中的源字节区间。
type ByteRange struct {
	Start int64 `json:"start_bytes"`
	End   int64 `json:"end_bytes"`
}

// Manifest 是完整分片清单。
type Manifest struct {
	Version      int            `json:"version"`
	Codec        string         `json:"codec"`
	SegmentCount int            `json:"segment_count"`
	Segments     []Segment      `json:"segments"`
	ErrorKinds   map[string]int `json:"error_kinds"`
	SHA256       string         `json:"sha256"`
}

// BuildInput 是清单生成输入。CutPoints 为对齐切点（微秒+每层帧字节范围），
// Issues 为各轨道诊断，按字节区间归入相应分片。
type BuildInput struct {
	Codec     string
	CutPoints []CutPoint
	Issues    []stream.Issue
	RepIDs    []string
}

// CutPoint 是一个对齐切点的边界信息。
type CutPoint struct {
	AnchorMicros int64
	EndMicros    int64
	Ranges       map[string]ByteRange
}

// Build 构造稳定排序、自带分片与整体校验和的清单。
func Build(in BuildInput) *Manifest {
	m := &Manifest{
		Version:    1,
		Codec:      in.Codec,
		ErrorKinds: map[string]int{},
	}
	for i, cp := range in.CutPoints {
		seg := Segment{
			Index:           i,
			StartMicros:     cp.AnchorMicros,
			EndMicros:       cp.EndMicros,
			DurationUS:      cp.EndMicros - cp.AnchorMicros,
			Representations: normalizeRanges(cp.Ranges),
			Errors:          issuesInRange(in.Issues, cp.Ranges),
		}
		seg.SHA256 = segmentDigest(seg)
		m.Segments = append(m.Segments, seg)
		for _, is := range seg.Errors {
			m.ErrorKinds[string(is.Kind)]++
		}
	}
	m.SegmentCount = len(m.Segments)
	m.SHA256 = manifestDigest(m)
	return m
}

// normalizeRanges 按表示层 ID 稳定化 map（JSON 编码前排序由编码器保证，
// 这里返回普通 map 但确保非 nil）。
func normalizeRanges(r map[string]ByteRange) map[string]ByteRange {
	out := make(map[string]ByteRange, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

// issuesInRange 返回字节落在本分片任一表示层区间内的诊断。
func issuesInRange(all []stream.Issue, ranges map[string]ByteRange) []stream.Issue {
	var out []stream.Issue
	for _, is := range all {
		for _, br := range ranges {
			if is.Offset >= br.Start && is.Offset < br.End {
				out = append(out, is)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Offset != out[j].Offset {
			return out[i].Offset < out[j].Offset
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// canonicalJSON 产生确定字节的 JSON（无多余空白、map 键有序）。
func canonicalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// segmentDigest 对分片的稳定字段计算 SHA-256（不含其自身 SHA256 字段）。
func segmentDigest(s Segment) string {
	clone := s
	clone.SHA256 = ""
	raw, err := canonicalJSON(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// manifestDigest 对除整体 SHA256 外的全部内容计算摘要。
func manifestDigest(m *Manifest) string {
	clone := *m
	clone.SHA256 = ""
	raw, err := canonicalJSON(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// WriteAtomic 把清单以临时文件 + 原子替换写到 path，写完回读校验内容。
func WriteAtomic(path string, m *Manifest) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建清单目录失败: %w", err)
	}
	payload, err := canonicalJSON(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时清单失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("写清单失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	// 回读校验临时文件内容与预期一致后再原子替换。
	if err := verifyFile(tmpName, payload); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("原子替换清单失败: %w", err)
	}
	return nil
}

func verifyFile(path string, want []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	sumWant := sha256.Sum256(want)
	sumGot := sha256.Sum256(got)
	if sumWant != sumGot {
		return fmt.Errorf("清单回读校验不一致")
	}
	return nil
}

// ReadAtomic 读取并解析清单。
func ReadAtomic(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
