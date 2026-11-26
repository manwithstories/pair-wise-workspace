package demo

import (
	"encoding/json"
	"os"
	"path/filepath"

	"tsproof/internal/analyzer"
	"tsproof/internal/fixture"
	"tsproof/internal/pipeline"
	"tsproof/internal/segment"
	"tsproof/internal/ts"
)

// padNullPackets 在尾部追加 PID 0x1FFF 空包，使包数恰为 target。
// 输入已超过 target 时报错由调用方通过长度检查规避。
func padNullPackets(pkts [][]byte, target int) [][]byte {
	out := make([][]byte, 0, target)
	out = append(out, pkts...)
	for len(out) < target {
		raw := make([]byte, ts.PacketSize188)
		raw[0] = ts.SyncByte
		raw[1] = 0x1F
		raw[2] = 0xFF // PID 0x1FFF
		out = append(out, raw)
	}
	return out[:target]
}

func packetsToBytes(pkts [][]byte) []byte {
	out := make([]byte, 0, len(pkts)*ts.PacketSize188)
	for _, p := range pkts {
		out = append(out, p...)
	}
	return out
}

// roundtripConsistency 比对往返后规范化包数与轨道摘要：
// 解析得到的视频帧数、音频块数与生成器真值一致即记 100%。
func roundtripConsistency(clean *fixture.CleanStream, res *analyzer.Result) float64 {
	total := fixture.FrameCount + fixture.AudioBlocks
	got := len(res.Blocks[fixture.PIDVideo]) + len(res.Blocks[fixture.PIDAudio])
	if total == 0 {
		return 0
	}
	return float64(got) * 100 / float64(total)
}

// collectDamage 在独立副本上注入每类损坏并解析，累计指标到 m。
func collectDamage(seed uint64, m Metrics) Metrics {
	base := fixture.GenerateClean(seed)

	parse := func(pkts [][]byte) *analyzer.Result {
		padded := padNullPackets(pkts, totalPackets)
		res, err := analyzer.New().Parse(packetsToBytes(padded))
		if err != nil {
			return nil
		}
		return res
	}

	if r := parse(mustDamage(fixture.InjectCCJump(base.Packets))); r != nil {
		m.CCGaps += r.Metrics.CCGaps
	}
	if r := parse(mustDamage(fixture.InjectDuplicate(base.Packets))); r != nil {
		m.Duplicates += r.Metrics.Duplicates
	}
	if r := parse(mustDamage(fixture.InjectReorder(base.Packets))); r != nil {
		m.Reorders += r.Metrics.Reorders
	}
	if r := parse(mustDamage(fixture.InjectTruncate(base.Packets))); r != nil {
		m.Truncated += r.Metrics.TruncatedPES
	}
	if r := parse(mustDamage(fixture.InjectBadCRCPI(base.Packets))); r != nil {
		m.CRCReject += r.Metrics.CRCReject
	}
	if r := parse(mustDamage(fixture.InjectPCRWrap(base.Packets))); r != nil {
		m.PCRWraps += r.Metrics.PCRWraps
	}
	updated, _ := fixture.InjectPSIUpdate(base.Packets, len(base.Packets)/2)
	if r := parse(updated); r != nil {
		m.PSISwitches += r.Metrics.PSISwitches
	}
	return m
}

// mustDamage 解包损坏注入的 (packets, hits)，忽略计数（计数由解析侧汇总）。
func mustDamage(pkts [][]byte, _ int) [][]byte { return pkts }

func hashesEqual(a, b []segment.Hash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SHA256 != b[i].SHA256 ||
			a[i].PacketFrom != b[i].PacketFrom || a[i].PacketTo != b[i].PacketTo {
			return false
		}
	}
	return true
}

// passes 汇总验收门槛：全部关键指标达标才返回 true。
func passes(m Metrics, seek segment.SeekReport) bool {
	return m.RoundtripPct == 100.0 &&
		seek.HitRate() == 100.0 &&
		m.Segments == SegmentsTarget &&
		m.HashMatch == "24/24" &&
		m.ReplayIdentical == "yes" &&
		m.CRCReject > 0 && m.CCGaps > 0 && m.Truncated > 0 &&
		m.PSISwitches > 0 && m.ResumeSegments > 0
}

func writeMetrics(outDir string, m Metrics) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(outDir, "metrics.json"), raw, 0o644)
}

// writeManifest 把帧清单以确定性 JSON 落盘，供人工复核与重放比对。
func writeManifest(outDir string, lines []pipeline.ManifestLine) {
	raw, err := json.MarshalIndent(lines, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(outDir, "manifest.json"), raw, 0o644)
}
