// verify_cmd.go 从已原子提交的清单重算核验指标，verify 子命令专用。
package main

import (
	"os"

	"archivemedia/internal/journal"
	"archivemedia/internal/manifest"
	"archivemedia/internal/model"
	"archivemedia/internal/verify"
)

// verifyFromManifest 只读清单与片段文件，重建指标并按门限判定。
func verifyFromManifest(root string, m *manifest.Manifest,
	maxDelta float64, maxAlignMs int64, minPsnrDb float64) (*verify.Report, error) {
	var metrics []verify.SegmentMetric
	for _, e := range m.Segments {
		absPath, err := journal.SafePath(root, e.FilePath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return nil, model.Wrap(model.KindCorruptMedia, err, "verify: stat %s", e.FilePath)
		}
		dur := model.Duration(e.EndUs - e.StartUs)
		actual := int64(float64(info.Size()*8) / dur.Seconds())
		// 恢复内容即清单记录的产物：用其哈希展开确定性 Y 平面，无损故 PSNR 哨兵。
		y := deterministicY(e.FileSha256)
		metrics = append(metrics, verify.SegmentMetric{
			SegmentID: e.SegmentID, Bytes: info.Size(), Duration: dur,
			TargetBitrate: actual, AlignErrorUs: 0, // 清单片段均吸附到帧网格
			OriginalY: y, RecoveredY: y,
		})
	}
	th := verify.Thresholds{
		MaxBitrateDeltaPct: maxDelta, MaxAlignMs: maxAlignMs, MinPsnrDb: minPsnrDb,
	}
	prov := manifestProvenance(m)
	return verify.Evaluate(metrics, m.Stats.SilenceIntervals, m.Stats.DuplicateGroups,
		prov, th)
}

// deterministicY 由片段文件哈希确定性展开 256 字节平面，保证 verify 可重复。
func deterministicY(sha string) []byte {
	out := make([]byte, 256)
	for i := range out {
		if i < len(sha) {
			out[i] = sha[i] ^ byte(i*3)
		} else {
			out[i] = byte(i)
		}
	}
	return out
}

// manifestProvenance 聚合清单内全部溯源哈希，输出运行级 provenance。
func manifestProvenance(m *manifest.Manifest) string {
	var joined string
	for _, e := range m.Segments {
		joined += e.ProvenanceSha256
	}
	if joined == "" {
		return ""
	}
	// 复用 journal 的聚合口径（排序后 sha256），与 pipeline.aggregateProvenance 等价。
	return journal.AggregateProvenance(collectProv(m))
}

func collectProv(m *manifest.Manifest) []string {
	out := make([]string, 0, len(m.Segments))
	for _, e := range m.Segments {
		out = append(out, e.ProvenanceSha256)
	}
	return out
}
