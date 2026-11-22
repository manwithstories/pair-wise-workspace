// Package manifest 生成确定性时间清单并做区间对齐校验。
// 清单只在完整处理成功后写出；任何对齐越界都返回结构化错误且不落成功清单。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"

	"mediacheck/internal/adapter"
	"mediacheck/internal/model"
)

// Entry 是清单中的单个分片条目，时间统一为秒的分子/分母有理数。
type Entry struct {
	Sequence uint32 `json:"sequence"`
	File     string `json:"file"`
	StartNum int64  `json:"start_num"`
	StartDen int64  `json:"start_den"`
	EndNum   int64  `json:"end_num"`
	EndDen   int64  `json:"end_den"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// Manifest 是跨模块对齐的时间清单。
type Manifest struct {
	Timescale       uint32  `json:"timescale"`
	TargetRate      uint32  `json:"target_rate"`
	TargetLayout    string  `json:"target_layout"`
	Entries         []Entry `json:"entries"`
	TotalDuration   Rational `json:"total_duration"`
	AlignmentMaxMS  float64 `json:"alignment_max_ms"`
}

// Rational 是清单的 JSON 友好有理数（独立于内部 model，保证序列化稳定）。
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Build 根据时钟时间线与再封装文件构造清单条目，并校验相邻区间连续性。
// maxAlignMS 为允许的最大对齐误差（规格要求 ≤1ms）。
func Build(timeline []model.Interval, files map[uint32]string,
	timescale, targetRate uint32, layout string, maxAlignMS float64) (*Manifest, error) {
	if len(timeline) == 0 {
		return nil, &model.MediaError{Category: model.ErrShortDuration, Message: "empty timeline for manifest"}
	}
	seqs := make([]uint32, 0, len(timeline))
	for _, iv := range timeline {
		seqs = append(seqs, iv.FragmentSeq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var entries []Entry
	var maxGap model.Rational
	for idx, seq := range seqs {
		var iv model.Interval
		for _, cand := range timeline {
			if cand.FragmentSeq == seq {
				iv = cand
				break
			}
		}
		file, ok := files[seq]
		if !ok {
			return nil, &model.MediaError{Category: model.ErrManifestAlign, Message: "missing packaged file for seq"}
		}
		entries = append(entries, Entry{
			Sequence: seq, File: file,
			StartNum: iv.Start.Num, StartDen: iv.Start.Den,
			EndNum: iv.End.Num, EndDen: iv.End.Den,
			Bytes: iv.ByteLength, SHA256: hashFile(file),
		})
		if idx > 0 {
			prev := entries[idx-1]
			prevEnd := model.NewRational(prev.EndNum, prev.EndDen)
			gap := iv.Start.Sub(prevEnd)
			if gap.Cmp(model.NewRational(0, 1)) < 0 {
				gap = model.NewRational(-gap.Num, gap.Den)
			}
			if maxGap.Den == 0 || gap.Cmp(maxGap) > 0 {
				maxGap = gap
			}
		}
	}
	alignMS := maxGap.Milli()
	if alignMS > maxAlignMS {
		return nil, &model.MediaError{
			Category: model.ErrManifestAlign,
			Message:  "manifest alignment exceeds limit",
		}
	}
	last := entries[len(entries)-1]
	return &Manifest{
		Timescale: timescale, TargetRate: targetRate, TargetLayout: layout,
		Entries: entries,
		TotalDuration: Rational{
			Num: model.NewRational(last.EndNum, last.EndDen).Num,
			Den: model.NewRational(last.EndNum, last.EndDen).Den,
		},
		AlignmentMaxMS: alignMS,
	}, nil
}

// Write 以规范化（键排序、无多余空白差异）JSON 原子写出清单。
func (m *Manifest) Write(outDir, name string) (string, error) {
	path, err := adapter.SafeOutputPath(outDir, name)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
	}
	if err := atomicWrite(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

// SummarySHA 对清单规范化字节求 sha256，供跨运行稳定性比对。
func (m *Manifest) SummarySHA() string {
	data, _ := json.Marshal(m)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
	}
	return nil
}
