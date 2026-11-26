// Package fingerprint 计算分段内容指纹并做去重分组。
//
// 设计约束：
//   - 指纹同时纳入解码内容与时间跨度：同内容不同封装可归并，
//     但出现在不同时间位置的相同内容默认区分；
//   - 转场复合段使用带类型的指纹，绝不与普通段误并；
//   - 同内容指纹但来源元数据冲突时，保留全部候选并标记 Conflict，
//     不静默丢弃任何来源。
package fingerprint

import (
	"hash/fnv"
	"sort"
	"strconv"

	"archivemedia/internal/model"
)

// Window 是参与指纹的一个内容窗：哈希 + 窗内样本/帧计数。
type Window struct {
	ContentHash uint64 // 解码内容的确定性哈希（适配器提供或本地重算）
	Count       int
}

// Input 是一个待指纹化的分段。
type Input struct {
	SegmentID  string
	SourceID   string
	IsTransition bool
	// 时间跨度（统一轴微秒），纳入指纹。
	Start model.MediaTime
	End   model.MediaTime
	// 轨道布局纳入指纹：转场段含多轨，普通段单轨。
	Tracks  []int
	Windows []Window
}

// Fingerprint 是计算结果。
type Fingerprint struct {
	SegmentID string
	// ContentKey 只取决于解码内容与窗序列：同内容不同封装相同。
	ContentKey uint64
	// FullKey 额外纳入时间跨度、轨道布局与段类型：用于最终归并。
	FullKey uint64
	Input   Input
}

// ContentKeyHex 返回内容键的十六进制形式，供幂等摘要使用。
func (f Fingerprint) ContentKeyHex() string {
	return strconv.FormatUint(f.ContentKey, 16)
}

// Compute 计算单段指纹，纯函数、确定性、无分配副作用。
func Compute(in Input) Fingerprint {
	c := fnv.New64a()
	writeUint64 := func(v uint64) {
		var b [8]byte
		for i := 0; i < 8; i++ {
			b[i] = byte(v >> (8 * i))
		}
		c.Write(b[:])
	}
	writeInt := func(v int) { writeUint64(uint64(v)) }

	// 1) 内容部分：窗内容哈希与计数，顺序敏感。
	for _, w := range in.Windows {
		writeUint64(w.ContentHash)
		writeInt(w.Count)
	}
	contentKey := c.Sum64()

	// 2) 完整指纹：内容 + 时间跨度 + 轨道布局 + 段类型。
	writeUint64(contentKey)
	writeUint64(uint64(in.Start.Microseconds()))
	writeUint64(uint64(in.End.Microseconds()))
	tracks := append([]int(nil), in.Tracks...)
	sort.Ints(tracks) // 轨道顺序归一，保证确定性
	for _, tr := range tracks {
		writeInt(tr)
	}
	if in.IsTransition {
		c.Write([]byte{'T'})
	} else {
		c.Write([]byte{'P'})
	}
	return Fingerprint{SegmentID: in.SegmentID, ContentKey: contentKey, FullKey: c.Sum64(), Input: in}
}

// Group 是去重分组。
type Group struct {
	ContentKey uint64
	Members    []Fingerprint
	// Conflict 表示内容相同但来源元数据（容器/编码信息）不一致，
	// 此时保留全部候选供人工或后续流程裁决。
	Conflict bool
}

// Dedup 按 ContentKey 归并同内容分段。
//
// 转场段（IsTransition）单独成组：即使内容哈希碰撞也不与普通段合并。
// 同组内若出现不同来源容器/编码签名则标记 Conflict，候选全部保留。
func Dedup(fps []Fingerprint) []Group {
	groups := map[uint64]*Group{}
	order := make([]uint64, 0)
	// 转场使用独立键空间，加高位标记避免与普通段 contentKey 碰撞。
	const transitionSalt uint64 = 1 << 63
	for _, fp := range fps {
		key := fp.ContentKey
		if fp.Input.IsTransition {
			key ^= transitionSalt
		}
		g, ok := groups[key]
		if !ok {
			g = &Group{ContentKey: key}
			groups[key] = g
			order = append(order, key)
		}
		// 冲突检测：同内容但段类型或来源不同。
		for _, m := range g.Members {
			if m.Input.IsTransition != fp.Input.IsTransition || m.Input.SourceID != fp.Input.SourceID {
				g.Conflict = true
			}
		}
		g.Members = append(g.Members, fp)
	}
	out := make([]Group, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ContentKey < out[j].ContentKey })
	return out
}

// DuplicateGroups 只返回含 2 个及以上成员的分组，供终端报告 duplicate_groups。
func DuplicateGroups(groups []Group) []Group {
	var out []Group
	for _, g := range groups {
		if len(g.Members) >= 2 {
			out = append(out, g)
		}
	}
	return out
}
