package core

import (
	"fmt"
	"sort"

	"mediacheck/internal/model"
)

// ColorPriority 配置色彩元数据冲突时的优先级顺序（高优先级在前）。
// 键为 "primaries/transfer/matrix" 组合字符串；未命中时回退到 Default。
type ColorPriority struct {
	Order   []string // 如 ["9/16/9", "1/1/1"]
	Default model.ColorMeta
}

// ColorDecision 记录一次色彩仲裁的可复核信息：源值、选择值与冲突数。
type ColorDecision struct {
	Sources   []model.ColorMeta
	Selected  model.ColorMeta
	Conflicts int
}

func colorKey(c model.ColorMeta) string {
	// 用稳定的定长编码，避免字符串拼接歧义。
	return keyString([3]uint16{c.ColorPrimaries, c.Transfer, c.Matrix})
}

func keyString(k [3]uint16) string {
	return fmt.Sprintf("%d/%d/%d", k[0], k[1], k[2])
}

// ResolveColor 对一组来源色彩元数据按配置优先级仲裁。
// 所有来源完全一致时零冲突；任一不同即累计冲突并按优先级选择。
func ResolveColor(sources []*model.ColorMeta, prio ColorPriority) ColorDecision {
	dec := ColorDecision{}
	var distinct []model.ColorMeta
	seen := map[[3]uint16]bool{}
	for _, s := range sources {
		if s == nil {
			continue
		}
		dec.Sources = append(dec.Sources, *s)
		key := [3]uint16{s.ColorPrimaries, s.Transfer, s.Matrix}
		if !seen[key] {
			seen[key] = true
			distinct = append(distinct, *s)
		}
	}
	if len(distinct) == 0 {
		dec.Selected = prio.Default
		return dec
	}
	if len(distinct) == 1 {
		dec.Selected = distinct[0]
		return dec
	}
	dec.Conflicts = len(distinct) - 1
	// 在优先级表中选择排名最靠前的来源组合。
	rank := map[string]int{}
	for i, k := range prio.Order {
		rank[k] = i
	}
	candidates := append([]model.ColorMeta{}, distinct...)
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, iok := rank[colorKey(candidates[i])]
		rj, jok := rank[colorKey(candidates[j])]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		default:
			return false // 都不在表中，保持稳定顺序
		}
	})
	if _, known := rank[colorKey(candidates[0])]; known {
		dec.Selected = candidates[0]
	} else {
		dec.Selected = prio.Default // 表中无匹配，回退默认并保留冲突计数
	}
	return dec
}
