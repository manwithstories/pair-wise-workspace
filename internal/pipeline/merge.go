package pipeline

import (
	"errors"
	"sort"

	"mediatime/internal/segment"
)

// Merged 是按清单序号归并后的确定性视图，与任务完成顺序无关。
type Merged struct {
	// Ordered 严格按 Sequence 升序，仅包含成功提取的片段。
	Ordered []*segment.ExtractedFragment
	// Failed 记录每个失败序号及其首个确定性错误。
	Failed map[int]error
	// Missing 是既无成功结果也无失败记录的序号（缺片）。
	Missing []int
	// Completed 是成功片段数。
	Completed int
}

// Merge 把乱序到达的工作池结果按清单序号 [0, total) 归并。
//
// 相同输入无论完成顺序如何，输出顺序与内容完全一致；同序号多个结果
// 只保留首个成功结果，冲突显式报错而不是静默覆盖。
func Merge(results []Result, total int) (*Merged, error) {
	bySeq := map[int]*segment.ExtractedFragment{}
	failed := map[int]error{}
	var conflict error
	for _, r := range results {
		if r.Sequence < 0 || r.Sequence >= total {
			return nil, errors.New("pipeline: result sequence out of manifest range")
		}
		if r.Err != nil {
			if _, ok := failed[r.Sequence]; !ok {
				failed[r.Sequence] = r.Err
			}
			continue
		}
		if existing, ok := bySeq[r.Sequence]; ok {
			if existing.URI != r.Fragment.URI {
				conflict = errors.New("pipeline: conflicting results for same sequence")
			}
			continue
		}
		bySeq[r.Sequence] = r.Fragment
	}

	seqs := make([]int, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	ordered := make([]*segment.ExtractedFragment, 0, len(seqs))
	for _, seq := range seqs {
		ordered = append(ordered, bySeq[seq])
	}
	var missing []int
	for seq := 0; seq < total; seq++ {
		if _, ok := bySeq[seq]; !ok {
			if _, failedOK := failed[seq]; !failedOK {
				missing = append(missing, seq)
			}
		}
	}
	if conflict != nil {
		return nil, conflict
	}
	return &Merged{
		Ordered:   ordered,
		Failed:    failed,
		Missing:   missing,
		Completed: len(ordered),
	}, nil
}
