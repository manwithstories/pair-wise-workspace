// Package demo 编排端到端核验流程：生成 → 损坏注入 → 解析 → 时间线 →
// 合成 → 渲染 → 分片哈希 → 检查点恢复，并汇总验收指标。
package demo

import (
	"fmt"
	"os"
	"path/filepath"

	"tsproof/internal/analyzer"
	"tsproof/internal/clock"
	"tsproof/internal/fixture"
	"tsproof/internal/mixer"
	"tsproof/internal/pipeline"
	"tsproof/internal/render"
	"tsproof/internal/segment"
)

// 验收固定参数：24 个片段，每片段 32 个 TS 包。
const (
	SegmentsTarget = 24
	PacketsPerSeg  = 32
	totalPackets   = SegmentsTarget * PacketsPerSeg // 768，尾部空包填充
)

// Metrics 是演示命令打印与写盘的完整核验结果。
type Metrics struct {
	Seed                  uint64 `json:"seed"`
	RoundtripConsistency  string `json:"roundtrip_consistency"`
	RoundtripPct          float64 `json:"roundtrip_pct"`
	KeyframeHit           string `json:"keyframe_hit"`
	Segments              int    `json:"segments"`
	HashMatch             string `json:"hash_match"`
	ReplayIdentical       string `json:"replay_identical"`
	ResumeSegments        int    `json:"resume_segments"`
	// 损坏路径计数。
	CCGaps      int `json:"cc_gaps"`
	PSISwitches int `json:"psi_switches"`
	CRCReject   int `json:"crc_reject"`
	Truncated   int `json:"truncated"`
	Duplicates  int `json:"duplicates"`
	Reorders    int `json:"reorders"`
	PCRWraps    int `json:"pcr_wraps"`
}

// Run 执行完整核验并把产物写到 outDir，返回指标与最终退出码（0/1/2）。
func Run(seed uint64, outDir string) (Metrics, int) {
	var m Metrics
	m.Seed = seed

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create output dir: %v\n", err)
		return m, 2
	}

	// ---- 1. 干净流生成与空包填充到固定总包数 ----
	clean := fixture.GenerateClean(seed)
	padded := padNullPackets(clean.Packets, totalPackets)

	// ---- 2. 干净路径解析（往返一致性与关键帧命中） ----
	res, err := analyzer.New().Parse(packetsToBytes(padded))
	if err != nil {
		fmt.Fprintf(os.Stderr, "clean parse failed: %v\n", err)
		return m, 2
	}
	m.RoundtripPct = roundtripConsistency(clean, res)
	m.RoundtripConsistency = fmt.Sprintf("%.2f%%", m.RoundtripPct)

	// 关键帧命中：与生成器已知关键帧 PTS 位置逐一比对。
	videoTrack := res.Timeline.Track(fixture.PIDVideo)
	locator := segment.BuildKeyframeLocator(videoTrack)
	knownKeys := make([]clock.Tick, 0, len(clean.KeyIndexes))
	for _, ki := range clean.KeyIndexes {
		knownKeys = append(knownKeys, clock.Tick(clean.FramePTS[ki]))
	}
	seekRep := locator.VerifySeeds(knownKeys)
	m.KeyframeHit = fmt.Sprintf("%.2f%% (%d/%d)", seekRep.HitRate(), seekRep.Hits, seekRep.Requests)

	// ---- 3. 多轨合成与渲染（仅干净流产出 PNG 帧序列） ----
	plan := mixer.Compose(res.Blocks, fixture.PIDVideo, fixture.PIDAudio)
	frameDir := filepath.Join(outDir, "frames")
	renderer, err := render.New(frameDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "renderer init: %v\n", err)
		return m, 2
	}
	var manifest []pipeline.ManifestLine
	for seq, slot := range plan.Slots {
		png, entry, rerr := renderer.RenderSlot(slot, seq)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "render slot %d: %v\n", seq, rerr)
			return m, 2
		}
		if werr := renderer.WriteFrame(seq, png); werr != nil {
			fmt.Fprintf(os.Stderr, "write frame %d: %v\n", seq, werr)
			return m, 2
		}
		manifest = append(manifest, pipeline.ManifestLine{
			Seq: entry.Seq, File: entry.File, PTS27: entry.PTS27,
			PTSSecs: entry.PTSSecs, Key: entry.Key, Untrusted: entry.Untrusted,
			AudioN: entry.AudioN, SHA256: entry.SHA256,
		})
	}
	writeManifest(outDir, manifest)

	// ---- 4. 分片哈希：固定 32 包/段，独立核验 ----
	hasher, err := segment.NewHasher(PacketsPerSeg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hasher: %v\n", err)
		return m, 2
	}
	wantHashes, err := hasher.HashPackets(padded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash packets: %v\n", err)
		return m, 2
	}
	m.Segments = len(wantHashes)
	// 复算一遍模拟独立核验。
	reHashes, _ := hasher.HashPackets(padded)
	mismatch := segment.Verify(reHashes, wantHashes)
	m.HashMatch = fmt.Sprintf("%d/%d", m.Segments-len(mismatch), m.Segments)

	// ---- 5. 检查点提交与恢复（独立子目录，先提交前半，再恢复续跑）----
	// 子目录与帧产物隔离，保证每次 demo 都从空检查点开始，可重复演示恢复。
	cpDir := filepath.Join(outDir, "checkpoint")
	_ = os.RemoveAll(cpDir)
	half := m.Segments / 2
	first, ferr := pipeline.NewStore(cpDir, seed, PacketsPerSeg)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "checkpoint init: %v\n", ferr)
		return m, 2
	}
	for i := 0; i < half; i++ {
		if cerr := first.CommitSegment(wantHashes[i], wantHashes[i]); cerr != nil {
			fmt.Fprintf(os.Stderr, "commit %d: %v\n", i, cerr)
			return m, 1
		}
	}
	// 模拟中断后重启：重新打开同一检查点，只续跑未提交片段。
	resume, rerr := pipeline.NewStore(cpDir, seed, PacketsPerSeg)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "checkpoint resume: %v\n", rerr)
		return m, 2
	}
	resumeFrom := resume.CommittedCount()
	for i := resumeFrom; i < m.Segments; i++ {
		if cerr := resume.CommitSegment(wantHashes[i], wantHashes[i]); cerr != nil {
			fmt.Fprintf(os.Stderr, "resume commit %d: %v\n", i, cerr)
			return m, 1
		}
	}
	m.ResumeSegments = m.Segments - resumeFrom
	_ = resume.SetTotalPackets(totalPackets)

	// ---- 6. 损坏路径：在干净流副本上分别注入并解析，汇总计数 ----
	m = collectDamage(seed, m)

	// ---- 7. 重放一致性：相同种子重新生成并哈希，必须完全相同 ----
	replay := fixture.GenerateClean(seed)
	replayPadded := padNullPackets(replay.Packets, totalPackets)
	replayHashes, _ := hasher.HashPackets(replayPadded)
	m.ReplayIdentical = "no"
	if hashesEqual(replayHashes, wantHashes) {
		m.ReplayIdentical = "yes"
	}

	writeMetrics(outDir, m)
	if !passes(m, seekRep) {
		return m, 1
	}
	return m, 0
}
