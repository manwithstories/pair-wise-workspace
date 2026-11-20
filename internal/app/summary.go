// Package app 编排 mediagraph 命令行：demo/resume/verify 的流水线组装、
// 摘要计算与确定性清单输出。清单不含任何计时字段，
// 保证固定种子重复执行得到相同校验和。
package app

import (
	"crypto/sha256"
	"encoding/hex"

	"mediagraph/internal/checkpoint"
	"mediagraph/internal/process"
	"mediagraph/internal/provenance"
)

// 步骤 ID（同时作为轨道依赖图节点）。
const (
	stepSRTGood    = "parse:srt_good"
	stepSRTCorrupt = "parse:srt_corrupt"
	stepVTT        = "parse:vtt"
	stepRTPGood    = "ingest:rtp_good"
	stepRTPCorrupt = "ingest:rtp_corrupt"
	stepMP4Good    = "mp4:good"
	stepMP4Corrupt = "mp4:corrupt"
	stepTimecode   = "timecode:normalize"
	stepLevels     = "audio:levels"
	stepSplit      = "split:main"
	stepSilence    = "silence:detect"
	stepDedup      = "fingerprint:dedup"
)

// Summary 为终端输出的核心指标。
type Summary struct {
	FirstErrorOffset int64  `json:"first_error_offset"` // 首个可恢复错误的源字节偏移，无错误为 -1
	FirstErrorFile   string `json:"first_error_file,omitempty"`
	RecoveredCues    int    `json:"recovered_cues"` // 损坏字幕中恢复出的可用条数
	Segments         int    `json:"segments"`
	SilenceMs        int64  `json:"silence_ms"`
	Deduplicated     int    `json:"deduplicated"`
	OutputSHA256     string `json:"output_sha256"` // 全部步骤结果的有序联合摘要
}

// Manifest 为写入工作目录的确定性清单（不含计时字段）。
type Manifest struct {
	Version        string            `json:"version"`
	Seed           int64             `json:"seed"`
	Params         map[string]string `json:"params"`
	InputDigest    string            `json:"input_digest"`
	StepsCompleted []string          `json:"steps_completed"`
	Interrupted    bool              `json:"interrupted"`
	Summary
}

// --- 各步骤存入检查点的结果 ---

type parseResult struct {
	Format           string `json:"format"`
	Cues             int    `json:"cues"`
	Errors           int    `json:"errors"`
	FirstErrorOffset int64  `json:"first_error_offset"`
}

type rtpResult struct {
	Packets          int   `json:"packets"`
	Gaps             int   `json:"gaps"`
	Duplicates       int   `json:"duplicates"`
	Reorders         int   `json:"reorders"`
	Wraps            int   `json:"wraps"`
	Issues           int   `json:"issues"`
	FirstErrorOffset int64 `json:"first_error_offset"`
}

type mp4Result struct {
	Timescale        uint32 `json:"timescale"`
	Samples          uint64 `json:"samples"`
	Issues           int    `json:"issues"`
	FirstErrorOffset int64  `json:"first_error_offset"`
}

type timecodeResult struct {
	Converted        int   `json:"converted"`
	Errors           int   `json:"errors"`
	FirstErrorOffset int64 `json:"first_error_offset"`
}

type levelsResult struct {
	Levels []float64 `json:"levels"`
}

type splitResult struct {
	Segments []process.Segment `json:"segments"`
}

type silenceResult struct {
	Intervals int   `json:"intervals"`
	SilenceMs int64 `json:"silence_ms"`
}

type dedupResult struct {
	Kept         int `json:"kept"`
	Deduplicated int `json:"deduplicated"`
	Collisions   int `json:"collisions"`
}

// summarize 从检查点结果与溯源记录计算摘要；首错误按记录顺序取最早。
func summarize(state *checkpoint.State, log *provenance.Log) Summary {
	s := Summary{FirstErrorOffset: -1}
	for i := range log.Records {
		r := &log.Records[i]
		if len(r.Errors) > 0 {
			s.FirstErrorOffset = r.ByteStart
			s.FirstErrorFile = r.SourceFile
			break
		}
	}
	var pr parseResult
	if state.Get(stepSRTCorrupt, &pr) {
		s.RecoveredCues = pr.Cues
	}
	var sp splitResult
	if state.Get(stepSplit, &sp) {
		s.Segments = len(sp.Segments)
	}
	var si silenceResult
	if state.Get(stepSilence, &si) {
		s.SilenceMs = si.SilenceMs
	}
	var dd dedupResult
	if state.Get(stepDedup, &dd) {
		s.Deduplicated = dd.Deduplicated
	}
	s.OutputSHA256 = outputHash(state)
	return s
}

// outputHash 按完成顺序对全部步骤结果做联合 SHA-256，与执行计时无关。
func outputHash(state *checkpoint.State) string {
	h := sha256.New()
	for _, id := range state.Completed {
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write(state.Results[id])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
