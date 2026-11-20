// Package report 生成逐片段 SHA-256 哈希以及机器可读的 JSON 结果。
// 所有输出排序稳定，保证相同输入重复运行得到字节一致的报告。
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"sort"

	"mediarepair/internal/model"
)

// SegmentHash 是一个片段的样本流哈希。
type SegmentHash struct {
	ID         string `json:"id"`
	TrackID    uint32 `json:"track_id"`
	Transition bool   `json:"transition"`
	Algorithm  string `json:"algorithm"`
	Digest     string `json:"digest"`
	Samples    uint64 `json:"samples"`
	Bytes      uint64 `json:"bytes"`
}

// Hasher 按片段累积 SHA-256。样本按到达顺序喂入，并以
// trackID、seq、repairedDTS、长度、负载的固定编码进入摘要，保证可复现。
type Hasher struct {
	segments map[string]*segmentHasher
	order    []string
}

type segmentHasher struct {
	h    hash.Hash
	info SegmentHash
}

// NewHasher 创建空哈希累加器。
func NewHasher() *Hasher {
	return &Hasher{segments: make(map[string]*segmentHasher)}
}

// Add 把一个修复后样本计入其所属片段的摘要；未归属片段的样本被忽略。
func (h *Hasher) Add(rs model.RepairedSample) error {
	if rs.SegmentID == "" {
		return nil
	}
	sh := h.segments[rs.SegmentID]
	if sh == nil {
		sh = &segmentHasher{h: sha256.New(), info: SegmentHash{
			ID: rs.SegmentID, TrackID: rs.TrackID, Transition: rs.IsTransition, Algorithm: "sha256",
		}}
		h.segments[rs.SegmentID] = sh
		h.order = append(h.order, rs.SegmentID)
	}
	var hdr [24]byte
	putU64(hdr[0:], uint64(rs.TrackID))
	putU64(hdr[8:], rs.Seq)
	putU64(hdr[16:], rs.RepairedDTS)
	if _, err := sh.h.Write(hdr[:]); err != nil {
		return err
	}
	var lenBuf [4]byte
	putU32(lenBuf[:], uint32(len(rs.Data)))
	if _, err := sh.h.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := sh.h.Write(rs.Data); err != nil {
		return err
	}
	sh.info.Samples++
	sh.info.Bytes += uint64(len(rs.Data))
	return nil
}

// Finalize 固化各片段摘要并返回排序稳定的列表。
func (h *Hasher) Finalize() []SegmentHash {
	out := make([]SegmentHash, 0, len(h.order))
	for _, id := range h.order {
		sh := h.segments[id]
		sh.info.Digest = hex.EncodeToString(sh.h.Sum(nil))
		out = append(out, sh.info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TrackID != out[j].TrackID {
			return out[i].TrackID < out[j].TrackID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// WriteSamples 把样本负载按稳定编码写入 w，用于流式落盘的往返比对。
func WriteSamples(w io.Writer, rs model.RepairedSample) error {
	var hdr [24]byte
	putU64(hdr[0:], uint64(rs.TrackID))
	putU64(hdr[8:], rs.Seq)
	putU64(hdr[16:], rs.RepairedDTS)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var lenBuf [4]byte
	putU32(lenBuf[:], uint32(len(rs.Data)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(rs.Data)
	return err
}

func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (56 - 8*i))
	}
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// TrackReport 是单轨修复与索引统计。
type TrackReport struct {
	TrackID     uint32  `json:"track_id"`
	Timescale   uint32  `json:"timescale"`
	Kind        string  `json:"kind"`
	Samples     uint64  `json:"samples"`
	Wraps       uint64  `json:"wraps"`
	Duplicates  uint64  `json:"duplicates"`
	Regressions uint64  `json:"regressions"`
	ClipJumps   uint64  `json:"clip_jumps"`
	Keyframes   int     `json:"keyframes"`
	Seeks       uint64  `json:"seeks"`
	Hits        uint64  `json:"hits"`
	HitRate     float64 `json:"keyframe_hit_rate"`
}

// Result 是完整的机器可读报告。
type Result struct {
	Schema            string          `json:"schema"`
	Input             string          `json:"input"`
	RepairedInput     string          `json:"repaired_input,omitempty"`
	RoundtripMatch    float64         `json:"roundtrip_match"`
	KeyframeHitRate   float64         `json:"keyframe_hit_rate"`
	SegmentHashCount  int             `json:"segment_hash_count"`
	PeakBufferSamples int             `json:"peak_buffer_samples"`
	BackpressureWait  int64           `json:"backpressure_wait_ms"`
	BufferCapacity    int             `json:"buffer_capacity"`
	Tracks            []TrackReport   `json:"tracks"`
	SegmentHashes     []SegmentHash   `json:"segment_hashes"`
	Segments          []SegmentReport `json:"segments"`
	FailedOffset      int64           `json:"failed_offset"`
	FailedStage       string          `json:"failed_stage"`
	Recoverable       bool            `json:"recoverable"`
	Error             string          `json:"error,omitempty"`
}

// SegmentReport 是逐片段的修复统计。
type SegmentReport struct {
	ID         string `json:"id"`
	TrackID    uint32 `json:"track_id"`
	Transition bool   `json:"transition"`
	StartTick  uint64 `json:"start_tick"`
	EndTick    uint64 `json:"end_tick"`
	Samples    uint64 `json:"samples"`
	Repairs    uint64 `json:"repairs"`
}

// Summary 渲染验收所需的单行摘要字段。
func (r *Result) Summary() string {
	return fmt.Sprintf(
		"roundtrip_match=%.4f keyframe_hit_rate=%.4f segment_hashes=%d peak_buffer_samples=%d backpressure_wait_ms=%d",
		r.RoundtripMatch, r.KeyframeHitRate, len(r.SegmentHashes), r.PeakBufferSamples, r.BackpressureWait)
}

// MarshalIndent 以两空格缩进输出 JSON 报告。
func (r Result) MarshalIndent() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
