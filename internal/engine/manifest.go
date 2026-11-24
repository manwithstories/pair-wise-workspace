package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"media-pipeline/internal/demo"
	"media-pipeline/internal/resume"
)

// Manifest 是机器可读的处理清单，字段顺序固定、不含墙钟时间，保证可复现。
type Manifest struct {
	Seed       uint64             `json:"seed"`
	Format     string             `json:"format"`
	Scenarios  []ScenarioManifest `json:"scenarios"`
	ResumeMS   int64              `json:"resume_ms"`
	ContentSha string             `json:"content_sha256,omitempty"`
}

// ScenarioManifest 是单个场景的可复核条目。
type ScenarioManifest struct {
	Scenario    string        `json:"scenario"`
	Segments    int           `json:"segments"`
	SilenceMS   uint64        `json:"silence_ms"`
	Duplicates  int           `json:"duplicates"`   // RTP 传输层重复包
	ContentDups int           `json:"content_dups"` // 内容指纹重复窗口
	Gaps        int           `json:"gaps"`
	BadByte     int64         `json:"bad_byte"`
	BadSample   int64         `json:"bad_sample"`
	GapRanges   []GapManifest `json:"gap_ranges,omitempty"`
	Segments256 []string      `json:"segment_sha256,omitempty"`
	Source      resume.SourceRef `json:"source"`
}

// GapManifest 是缺口区间的机器可读形式。
type GapManifest struct {
	FirstSample uint64 `json:"first_sample"`
	Frames      uint64 `json:"frames"`
	Reason      string `json:"reason"`
}

// BuildManifest 汇总各场景 Stats 与源引用，生成不含自哈希的清单。
func BuildManifest(seed uint64, format string, rows []ScenarioManifest) Manifest {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Scenario < rows[j].Scenario })
	return Manifest{Seed: seed, Format: format, Scenarios: rows}
}

// Digest 对清单的规范化 JSON（不含 ContentSha）计算 SHA-256，
// 作为整次运行产物可信性的最终校验和。
func (m Manifest) Digest() string {
	cp := m
	cp.ContentSha = ""
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ToScenarioRow 把引擎 Stats 转为清单项。
func ToScenarioRow(st *Stats, src []byte) ScenarioManifest {
	row := ScenarioManifest{
		Scenario:    st.Scenario,
		Segments:    st.Segments,
		SilenceMS:   st.SilenceMS,
		Duplicates:  st.Duplicates,
		ContentDups: st.ContentDups,
		Gaps:        st.Gaps,
		BadByte:     st.BadByte,
		BadSample:   st.BadSample,
		Segments256: append([]string(nil), st.SegmentSums...),
	}
	for _, g := range st.GapRanges {
		row.GapRanges = append(row.GapRanges, GapManifest{
			FirstSample: g.FirstSample, Frames: g.Frames, Reason: g.Reason,
		})
	}
	sum := demo.HashBytes(src)
	row.Source = resume.SourceRef{BlockID: st.Scenario, Sha256: hex.EncodeToString(sum[:])}
	return row
}

// clockNow 以变量封装便于测试，生产使用真实墙钟（仅用于 resume_ms，不进哈希）。
var clockNow = time.Now
