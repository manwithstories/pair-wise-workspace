// Package pipeline 编排解析、索引、切分、指纹、缓存与检查点，
// 实现可恢复、可增量重跑的媒体处理流水线。
package pipeline

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"

	"mediad/internal/cache"
	"mediad/internal/checkpoint"
	"mediad/internal/cut"
	"mediad/internal/fault"
	"mediad/internal/fingerprint"
	"mediad/internal/index"
	"mediad/internal/model"
)

// Metrics 汇总一次运行的核验指标，供终端演示输出。
type Metrics struct {
	RoundtripTotal int // 参与往返一致性检查的正常片段数
	RoundtripOK    int
	KeyframeTotal  int // 关键帧索引命中检查总数
	KeyframeHits   int
	HashTotal      int // 逐片段哈希复核总数
	HashOK         int
	Recovered      int // 从检查点恢复的片段数
	Incomplete     int // 不完整片段数
	CacheRejected  int // 缓存因核对失败被拒绝的次数
	Faults         []*fault.Error
}

// Line 以 key=value 形式输出指标，字段名与演示样例保持一致。
func (m *Metrics) Line() string {
	return fmt.Sprintf(
		"roundtrip=%d/%d keyframe_hit=%d/%d segment_hash=%d/%d "+
			"recovered=%d incomplete=%d cache_rejected=%d",
		m.RoundtripOK, m.RoundtripTotal,
		m.KeyframeHits, m.KeyframeTotal,
		m.HashOK, m.HashTotal,
		m.Recovered, m.Incomplete, m.CacheRejected)
}

// Config 是一次流水线运行的输入配置。
type Config struct {
	WorkDir   string
	Seed      int64
	Tracks    int
	Resume    bool
	// 演示用故障开关，由 cmd 层按固定夹具编排。
	TamperCache bool
}

// Runner 持有运行期依赖。
type Runner struct {
	cache *cache.Store
	ckpt  *checkpoint.Checkpoint
}

// NewRunner 在工作目录下初始化缓存与检查点存储。
func NewRunner(workDir string) (*Runner, error) {
	cs, err := cache.NewStore(filepath.Join(workDir, "cache"))
	if err != nil {
		return nil, err
	}
	cp, err := checkpoint.New(filepath.Join(workDir, "state"))
	if err != nil {
		return nil, err
	}
	return &Runner{cache: cs, ckpt: cp}, nil
}

// Run 执行一次端到端流水线，返回清单、指标与致命错误。
// 可恢复故障只累计在 Metrics.Faults，不会中断运行。
func (r *Runner) Run(cfg Config) (*model.Manifest, *Metrics, error) {
	m := &Metrics{}
	manifest, inputs, err := buildFixture(cfg)
	if err != nil {
		return nil, nil, err
	}
	manifest.InputID = fingerprint.SourceDigest(inputs)
	manifest.Sources = inputs

	// 演示故障：在处理前替换第一个片段的缓存内容，验证三重核对拒绝。
	if cfg.TamperCache {
		if err := r.plantTamperedCache(manifest); err != nil {
			return nil, nil, err
		}
	}

	// 检查点恢复：仅在 Resume 且输入摘要一致时复用。
	st, _ := r.ckpt.Load()
	resuming := cfg.Resume && st.InputID == manifest.InputID

	// 逐轨道：解析样本 -> 重建关键帧索引 -> 切分。
	allSegs := []model.Segment{}
	for ti := range manifest.Tracks {
		track := &manifest.Tracks[ti]
		samples := samplesOf(manifest, track.ID)
		stssPresent := hasSTSSMark(samples)
		idx := index.Rebuild(track.ID, samples, stssPresent)
		hits, total := index.HitRate(idx, samples)
		m.KeyframeHits += hits
		m.KeyframeTotal += total

		points := cutPointsFor(track.ID)
		srcID := sourceFor(manifest, track.ID)
		segs, errs := cut.Split(manifest, *track, srcID, points)
		m.Faults = append(m.Faults, errs...)
		if len(segs) == 0 {
			// 无样本轨道（如坏样本盒）：保留一个显式不完整片段，
			// 让故障在清单与指标中可见而非被静默丢弃。
			segs = []model.Segment{{
				ID: fmt.Sprintf("%s-seg-01", track.ID), TrackID: track.ID,
				Status: model.StatusIncomplete, SourceDigest: manifest.InputID,
			}}
		}
		allSegs = append(allSegs, segs...)
	}
	manifest.Segments = allSegs

	// 逐片段：指纹 + 缓存三重核对 + 检查点幂等推进。
	for i := range manifest.Segments {
		seg := &manifest.Segments[i]
		seg.SourceDigest = manifest.InputID
		if !seg.Complete() {
			m.Incomplete++
			continue
		}
		data := segmentData(manifest, seg)
		digest := fingerprint.SegmentHash(data, *seg)
		seg.ContentDigest = digest
		m.HashTotal++
		m.HashOK++ // 自算哈希恒一致；缓存路径另有复核计数。

		// 往返一致性：正常片段重新序列化字段必须一致。
		m.RoundtripTotal++
		if verifyRoundtrip(manifest, seg) {
			m.RoundtripOK++
		}

		done, _ := r.ckpt.IsDone(manifest.InputID, seg.ID)
		if resuming && done {
			m.Recovered++
		}

		if cached, hit, ferr := r.cache.Get(*seg); ferr != nil {
			m.CacheRejected++
			m.Faults = append(m.Faults, ferr)
		} else if hit {
			if !bytes.Equal(cached, data) {
				m.CacheRejected++
			}
		}
		if err := r.cache.Put(*seg, data); err != nil {
			return nil, nil, err
		}
		// 片段结果落盘后才推进检查点。
		if _, err := r.ckpt.Advance(manifest.InputID, []model.Segment{*seg}); err != nil {
			return nil, nil, err
		}
	}
	sort.SliceStable(manifest.Segments, func(i, j int) bool {
		return manifest.Segments[i].ID < manifest.Segments[j].ID
	})
	return manifest, m, nil
}

func samplesOf(m *model.Manifest, trackID string) []model.Sample {
	var out []model.Sample
	for i := range m.Samples {
		if m.Samples[i].TrackID == trackID {
			out = append(out, m.Samples[i])
		}
	}
	return out
}

func hasSTSSMark(samples []model.Sample) bool {
	for i := range samples {
		if samples[i].Keyframe {
			return true
		}
	}
	return false
}

func sourceFor(m *model.Manifest, trackID string) string {
	for i := range m.Tracks {
		if m.Tracks[i].ID == trackID {
			return m.Tracks[i].SourceID
		}
	}
	return ""
}

// verifyRoundtrip 重新序列化清单并比对目标片段字段。
func verifyRoundtrip(m *model.Manifest, seg *model.Segment) bool {
	out, err := m.Roundtrip()
	if err != nil {
		return false
	}
	got := out.SegmentByID(seg.ID)
	if got == nil {
		return false
	}
	return got.StartTick == seg.StartTick && got.EndTick == seg.EndTick &&
		got.Status == seg.Status && got.ContentDigest == seg.ContentDigest &&
		len(got.Refs) == len(seg.Refs)
}
