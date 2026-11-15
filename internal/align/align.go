// Package align 把各路表示层对齐到公共时间轴，并计算逐片偏差。
package align

import (
	"fmt"
	"strings"

	"dashpack/internal/errs"
	"dashpack/internal/source"
)

// 对齐策略。
const (
	PolicyStrict   = "strict"
	PolicyTolerant = "tolerant"
)

// Segment 是对齐后的单片。
type Segment struct {
	Index      int64
	File       string
	StartMs    int64 // 公共时间轴上的起点
	DurationMs int64
	FilledFrom string // 缺片修补时复制来源文件，空表示原生分片
}

// Repair 描述一次修补动作。
type Repair struct {
	RepID  string `json:"rep_id"`
	Kind   string `json:"kind"` // shift / fill / truncate
	Index  int64  `json:"index"`
	Detail string `json:"detail"`
}

// RepResult 是一路表示层的对齐结果。
type RepResult struct {
	RepID              string
	ContentType        string
	Timescale          int64
	NominalMs          int64
	Segments           []Segment
	Repairs            []Repair
	MaxDeviationTicks  int64
	MaxDeviationMs     float64
	MeanDeviationMs    float64
	MisalignedSegments []int64
	TotalMs            int64
}

// Result 是整体对齐结果。
type Result struct {
	Policy        string
	CommonStartMs int64
	Reps          []RepResult
}

// tickMs 返回 1 tick 对应的毫秒（浮点，用于阈值比较）。
func tickMs(timescale int64) float64 {
	return 1000.0 / float64(timescale)
}

// Align 按策略对齐各路表示层。
// strict：任何超 1 tick 偏移、缺片、片长与声明不符直接报错（退出 3）。
// tolerant：取公共最小起点、丢前导残片、复制前片补缺片。
func Align(reps []source.Rep, policy string) (*Result, error) {
	if policy != PolicyStrict && policy != PolicyTolerant {
		return nil, errs.New(errs.EAlign, errs.ExitUsage, "unknown policy %q", policy)
	}
	res := &Result{Policy: policy}

	// 公共起点：所有路首片起点的最小值。
	first := true
	for _, r := range reps {
		if len(r.Segments) == 0 {
			continue
		}
		start := r.Segments[0].OffsetMs
		if first || start < res.CommonStartMs {
			res.CommonStartMs = start
			first = false
		}
	}

	var problems []string
	for _, r := range reps {
		rr := RepResult{RepID: r.ID, ContentType: r.ContentType,
			Timescale: r.Timescale, NominalMs: r.NominalMs}
		if len(r.Segments) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no segments", r.ID))
			continue
		}
		offset := r.Segments[0].OffsetMs
		if offset != 0 {
			prob := fmt.Sprintf("%s: first segment offset %+dms exceeds 1 tick",
				r.ID, offset)
			exceeds := offset > int64(tickMs(r.Timescale)) ||
				offset < -int64(tickMs(r.Timescale))
			switch {
			case policy == PolicyStrict && exceeds:
				problems = append(problems, prob)
			case policy == PolicyTolerant:
				rr.Repairs = append(rr.Repairs, Repair{RepID: r.ID, Kind: "shift",
					Index: 1, Detail: fmt.Sprintf("drop leading partial of %dms", offset)})
			}
		}
		// tolerant：丢前导残片，所有路首片对齐到公共起点（重定基为 0）。
		cursor := offset
		if policy == PolicyTolerant {
			cursor = 0
		}
		var prev *source.LadderSegment
		for _, s := range r.Segments {
			if policy == PolicyStrict && s.FilledFrom != "" {
				problems = append(problems, fmt.Sprintf(
					"%s: segment %d was filled from %s", r.ID, s.Index, s.FilledFrom))
			}
			if !s.Present {
				prob := fmt.Sprintf("%s: segment %d missing", r.ID, s.Index)
				if policy == PolicyStrict {
					problems = append(problems, prob)
					cursor += s.DurationMs
					prev = &s.LadderSegment
					continue
				}
				fill := Segment{Index: s.Index, File: s.File, StartMs: cursor,
					DurationMs: r.NominalMs}
				if prev != nil {
					fill.FilledFrom = prev.File
				}
				rr.Segments = append(rr.Segments, fill)
				rr.Repairs = append(rr.Repairs, Repair{RepID: r.ID, Kind: "fill",
					Index: s.Index, Detail: "copy previous segment " + fill.FilledFrom})
				cursor += fill.DurationMs
				prev = &s.LadderSegment
				continue
			}
			if s.DurationMs != r.NominalMs {
				prob := fmt.Sprintf("%s: segment %d duration %dms != declared %dms",
					r.ID, s.Index, s.DurationMs, r.NominalMs)
				if policy == PolicyStrict {
					problems = append(problems, prob)
				}
				// tolerant：保留实际片长，MPD 中改写 duration。
			}
			rr.Segments = append(rr.Segments, Segment{
				Index: s.Index, File: s.File, StartMs: cursor, DurationMs: s.DurationMs,
			})
			cursor += s.DurationMs
			prev = &s.LadderSegment
		}
		rr.TotalMs = cursor
		computeDeviations(&rr)
		res.Reps = append(res.Reps, rr)
	}
	if len(problems) > 0 {
		return res, errs.New(errs.EAlign, errs.ExitAlignment,
			"alignment failed:\n  %s", strings.Join(problems, "\n  "))
	}
	return res, nil
}

// computeDeviations 计算逐片相对标称网格的偏差。
func computeDeviations(rr *RepResult) {
	var expected int64
	var sumAbs float64
	for _, s := range rr.Segments {
		devMs := float64(s.StartMs) - float64(expected)
		expected += rr.NominalMs
		abs := devMs
		if abs < 0 {
			abs = -abs
		}
		sumAbs += abs
		ticks := int64(abs / tickMs(rr.Timescale))
		if ticks > rr.MaxDeviationTicks {
			rr.MaxDeviationTicks = ticks
			rr.MaxDeviationMs = abs
		}
		if ticks > 1 {
			rr.MisalignedSegments = append(rr.MisalignedSegments, s.Index)
		}
	}
	if len(rr.Segments) > 0 {
		rr.MeanDeviationMs = sumAbs / float64(len(rr.Segments))
	}
}
