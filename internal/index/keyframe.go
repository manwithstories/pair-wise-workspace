// Package index 构建并校验关键帧随机访问索引。
package index

import (
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediatool/internal/clock"
	"mediatool/internal/fanout"
)

// Index 是关键帧随机访问索引。
type Index struct {
	Entries []fanout.KeyEntry
}

func New(entries []fanout.KeyEntry) *Index { return &Index{Entries: entries} }

// Query 查找指定层、指定时刻所在的片段内不晚于该时刻的最近关键帧。
func (ix *Index) Query(repID int, t clock.Ticks) (fanout.KeyEntry, bool) {
	best := -1
	for i, e := range ix.Entries {
		if e.RepID != repID || e.Time > t {
			continue
		}
		if best < 0 || e.Time > ix.Entries[best].Time {
			best = i
		}
	}
	if best < 0 {
		return fanout.KeyEntry{}, false
	}
	return ix.Entries[best], true
}

// nonSyncSample 是 sample_flags 中的非同步样本位。
const nonSyncSample = 0x00010000

// VerifyEntry 独立重解析片段文件，确认索引项命中真实的同步采样：
// 偏移必须与按 trun 表累加出的采样位置一致，且该采样未置非同步位。
func VerifyEntry(e fanout.KeyEntry) (bool, error) {
	f, err := os.Open(e.Path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	boxes, err := mp4.ExtractBoxesWithPayload(f, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTrun()},
	})
	if err != nil {
		return false, fmt.Errorf("extract trun: %w", err)
	}
	mdats, err := mp4.ExtractBoxes(f, nil, []mp4.BoxPath{{mp4.BoxTypeMdat()}})
	if err != nil || len(mdats) == 0 {
		return false, fmt.Errorf("extract mdat: %v", err)
	}
	if len(boxes) == 0 {
		return false, fmt.Errorf("no trun box in %s", e.Path)
	}
	trun, ok := boxes[0].Payload.(*mp4.Trun)
	if !ok {
		return false, fmt.Errorf("unexpected payload type %T", boxes[0].Payload)
	}
	mdatDataStart := int64(mdats[0].Offset + mdats[0].HeaderSize)
	off := mdatDataStart
	for i := uint32(0); i < trun.SampleCount; i++ {
		ent := trun.Entries[i]
		if off == e.Offset {
			if ent.SampleSize != e.Size {
				return false, nil
			}
			return ent.SampleFlags&nonSyncSample == 0, nil
		}
		off += int64(ent.SampleSize)
	}
	return false, nil
}

// HitRate 对全部索引项执行独立核验，返回命中数与总数。
func (ix *Index) HitRate() (hits, total int, err error) {
	for _, e := range ix.Entries {
		ok, verr := VerifyEntry(e)
		if verr != nil {
			return 0, 0, verr
		}
		total++
		if ok {
			hits++
		}
	}
	return hits, total, nil
}
