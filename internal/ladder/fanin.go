package ladder

// BoundaryMismatch 描述一次跨表示层边界冲突。
type BoundaryMismatch struct {
	Index       int
	RepA        string
	RepB        string
	StartA      int64
	StartB      int64
	DeltaNS     int64
	ToleranceNS int64
}

// MergeReport 是汇合核验的结论。
type MergeReport struct {
	Accepted   bool
	Mismatches []BoundaryMismatch
	Reps       []string
}

// Fanin 以共享时钟产出的清单为准，逐边界交叉核验所有表示层。
// 规则：
//  1. 任一层携带首错（输入/恢复失败）直接拒绝发布；
//  2. 各层分片数必须一致，否则按缺失边界拒绝；
//  3. 同序号边界的起始媒体时间差不得超过容差；
//  4. 落在隔离时钟段的边界不具备发布资格，直接拒绝。
func Fanin(results []RepResult, toleranceNS int64, isolated func(rep string, seq uint32) bool) MergeReport {
	report := MergeReport{Accepted: true}
	if len(results) < 2 {
		report.Accepted = false
		return report
	}
	for _, r := range results {
		report.Reps = append(report.Reps, r.Rep)
		if r.FirstErr != nil || len(r.Manifest.Segments) == 0 {
			report.Accepted = false
			return report
		}
		if last := r.Manifest.Segments[len(r.Manifest.Segments)-1]; last.TailIncomplete {
			report.Accepted = false
			report.Mismatches = append(report.Mismatches, BoundaryMismatch{
				RepA: r.Rep, RepB: r.Rep,
				DeltaNS: -1, ToleranceNS: toleranceNS,
			})
			return report
		}
	}
	base := results[0]
	n := len(base.Manifest.Segments)
	for _, r := range results[1:] {
		if len(r.Manifest.Segments) != n {
			report.Accepted = false
			report.Mismatches = append(report.Mismatches, BoundaryMismatch{
				RepA: base.Rep, RepB: r.Rep,
				DeltaNS: -1, ToleranceNS: toleranceNS,
			})
			continue
		}
		for i := 0; i < n; i++ {
			sa, sb := base.Manifest.Segments[i], r.Manifest.Segments[i]
			if isolated(base.Rep, sa.FirstSeq) || isolated(r.Rep, sb.FirstSeq) {
				report.Accepted = false
				report.Mismatches = append(report.Mismatches, BoundaryMismatch{
					Index: i, RepA: base.Rep, RepB: r.Rep,
					StartA: sa.StartNS, StartB: sb.StartNS,
					DeltaNS: -1, ToleranceNS: toleranceNS,
				})
				continue
			}
			d := sa.StartNS - sb.StartNS
			if d < 0 {
				d = -d
			}
			if d > toleranceNS {
				report.Accepted = false
				report.Mismatches = append(report.Mismatches, BoundaryMismatch{
					Index: i, RepA: base.Rep, RepB: r.Rep,
					StartA: sa.StartNS, StartB: sb.StartNS,
					DeltaNS: d, ToleranceNS: toleranceNS,
				})
			}
		}
	}
	return report
}
