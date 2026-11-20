// Package index 由样本依赖关系重建关键帧随机访问索引。
// 指向未注册（缺失）样本的条目会被拒绝。
package index

import (
	"sort"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
)

// Entry 是一个关键帧索引条目。
type Entry struct {
	TrackID      uint32
	Seq          uint64 // 轨道内样本序号
	UnifiedDTS   uint64 // 统一时间基准
	SourceOffset int64
	SegmentID    string
}

// Index 是按轨道组织的关键帧索引。
type Index struct {
	entries map[uint32][]Entry
	known   map[uint32]map[uint64]bool // 已注册样本（trackID -> seq）
	seeks   uint64
	hits    uint64
}

// New 创建空索引。
func New() *Index {
	return &Index{
		entries: make(map[uint32][]Entry),
		known:   make(map[uint32]map[uint64]bool),
	}
}

// RegisterSample 登记一个已存在的样本，供关键帧条目校验。
func (ix *Index) RegisterSample(trackID uint32, seq uint64) {
	m := ix.known[trackID]
	if m == nil {
		m = make(map[uint64]bool)
		ix.known[trackID] = m
	}
	m[seq] = true
}

// AddSample 处理一个修复后样本：登记并按需生成关键帧条目。
// 关键帧条目指向缺失样本时返回错误（拒绝静默跳过）。
func (ix *Index) AddSample(rs model.RepairedSample) error {
	ix.RegisterSample(rs.TrackID, rs.Seq)
	if !rs.Keyframe {
		return nil
	}
	if !ix.known[rs.TrackID][rs.Seq] {
		return fault.New(fault.StageIndex, rs.TrackID, rs.SourceOffset, false,
			"关键帧条目指向缺失样本")
	}
	ix.entries[rs.TrackID] = append(ix.entries[rs.TrackID], Entry{
		TrackID: rs.TrackID, Seq: rs.Seq, UnifiedDTS: rs.UnifiedDTS,
		SourceOffset: rs.SourceOffset, SegmentID: rs.SegmentID,
	})
	return nil
}

// AddEntry 直接插入一个条目；指向未注册样本时拒绝。
func (ix *Index) AddEntry(e Entry) error {
	if !ix.known[e.TrackID][e.Seq] {
		return fault.New(fault.StageIndex, e.TrackID, e.SourceOffset, false,
			"关键帧条目指向缺失样本")
	}
	ix.entries[e.TrackID] = append(ix.entries[e.TrackID], e)
	return nil
}

// Finalize 排序各轨道条目。
func (ix *Index) Finalize() {
	for _, es := range ix.entries {
		sort.Slice(es, func(i, j int) bool { return es[i].UnifiedDTS < es[j].UnifiedDTS })
	}
}

// Seek 查找轨道上不大于 unifiedTime 的最近关键帧。
func (ix *Index) Seek(trackID uint32, unifiedTime uint64) (Entry, bool) {
	ix.seeks++
	es := ix.entries[trackID]
	pos := sort.Search(len(es), func(i int) bool { return es[i].UnifiedDTS > unifiedTime }) - 1
	if pos < 0 {
		return Entry{}, false
	}
	ix.hits++
	return es[pos], true
}

// EntryCount 返回条目总数。
func (ix *Index) EntryCount() int {
	n := 0
	for _, es := range ix.entries {
		n += len(es)
	}
	return n
}

// Stats 返回查找统计。
func (ix *Index) Stats() (seeks, hits uint64) { return ix.seeks, ix.hits }

// HitRate 返回关键帧命中率；无查找时返回 1。
func (ix *Index) HitRate() float64 {
	if ix.seeks == 0 {
		return 1
	}
	return float64(ix.hits) / float64(ix.seeks)
}

// Entries 返回某轨道的全部条目。
func (ix *Index) Entries(trackID uint32) []Entry { return ix.entries[trackID] }
