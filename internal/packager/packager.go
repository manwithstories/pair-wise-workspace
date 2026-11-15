// Package packager 按策略修补分片并组装 MPD 模型。
package packager

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dashpack/internal/align"
	"dashpack/internal/errs"
	"dashpack/internal/manifest"
	"dashpack/internal/source"
)

// BuildInfo 记录组装过程中的修补统计。
type BuildInfo struct {
	PeriodDurationMs int64
	Repairs          []align.Repair
	RepairedItems    int // 至少经过一次修补的表示层数量
}

// Build 把对齐结果组装为 MPD 模型，并在 tolerant 下把补缺片复制落盘。
func Build(root string, reps []source.Rep, aligned *align.Result) (*manifest.MPD, *BuildInfo, error) {
	m, info, err := Assemble(reps, aligned)
	if err != nil {
		return nil, nil, err
	}
	filled := map[string]map[int64]string{}
	for _, rr := range aligned.Reps {
		for _, s := range rr.Segments {
			if s.FilledFrom == "" {
				continue
			}
			if err := copyFile(filepath.Join(root, rr.RepID, s.FilledFrom),
				filepath.Join(root, rr.RepID, s.File)); err != nil {
				return nil, nil, err
			}
			if filled[rr.RepID] == nil {
				filled[rr.RepID] = map[int64]string{}
			}
			filled[rr.RepID][s.Index] = s.FilledFrom
		}
	}
	if len(filled) > 0 {
		if err := markFilled(root, filled); err != nil {
			return nil, nil, err
		}
	}
	return m, info, nil
}

// markFilled 把补缺来源回写进 ladder.json，供后续 strict 审计。
func markFilled(root string, filled map[string]map[int64]string) error {
	l, err := source.LoadLadder(root)
	if err != nil {
		return err
	}
	for i := range l.Reps {
		marks := filled[l.Reps[i].ID]
		if len(marks) == 0 {
			continue
		}
		for j := range l.Reps[i].Segments {
			if from, ok := marks[l.Reps[i].Segments[j].Index]; ok {
				l.Reps[i].Segments[j].FilledFrom = from
			}
		}
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "encode ladder: %v", err)
	}
	tmp := filepath.Join(root, "ladder.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "write ladder: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "ladder.json")); err != nil {
		os.Remove(tmp)
		return errs.New(errs.EWrite, errs.ExitWrite, "rename ladder: %v", err)
	}
	return nil
}

// Assemble 把对齐结果组装为 MPD 模型（纯函数，不触碰文件系统）。
// Period 时长取各档总时长最小值；跨边界末片截断并改写 duration；
// startNumber 固定从 1 起。
func Assemble(reps []source.Rep, aligned *align.Result) (*manifest.MPD, *BuildInfo, error) {
	info := &BuildInfo{}
	if len(aligned.Reps) == 0 {
		return nil, nil, errs.New(errs.EEmpty, errs.ExitUsage, "no aligned representations")
	}
	info.PeriodDurationMs = aligned.Reps[0].TotalMs
	for _, rr := range aligned.Reps {
		if rr.TotalMs < info.PeriodDurationMs {
			info.PeriodDurationMs = rr.TotalMs
		}
	}

	byID := map[string]source.Rep{}
	for _, r := range reps {
		byID[r.ID] = r
	}

	period := manifest.Period{DurationNs: info.PeriodDurationMs * 1e6}
	setIdx := map[string]int{}
	for i := range aligned.Reps {
		rr := &aligned.Reps[i]
		src := byID[rr.RepID]
		// 末片跨 Period 边界：截断并改写 duration。
		if rr.TotalMs > info.PeriodDurationMs {
			excess := rr.TotalMs - info.PeriodDurationMs
			last := &rr.Segments[len(rr.Segments)-1]
			if last.DurationMs-excess <= 0 {
				return nil, nil, errs.New(errs.EAlign, errs.ExitAlignment,
					"%s: cannot truncate last segment by %dms", rr.RepID, excess)
			}
			last.DurationMs -= excess
			rr.TotalMs = info.PeriodDurationMs
			rr.Repairs = append(rr.Repairs, align.Repair{RepID: rr.RepID,
				Kind: "truncate", Index: last.Index,
				Detail: fmt.Sprintf("trim last segment by %dms at period boundary", excess)})
		}
		if len(rr.Repairs) > 0 {
			info.RepairedItems++
			info.Repairs = append(info.Repairs, rr.Repairs...)
		}

		rep := manifest.Representation{
			ID: rr.RepID, Bandwidth: src.Bandwidth, Codecs: src.Codecs,
			ContentType: rr.ContentType,
			Template: manifest.SegmentTemplate{
				Timescale:      rr.Timescale,
				Duration:       rr.NominalMs * rr.Timescale / 1000,
				StartNumber:    1,
				Media:          src.MediaTemplate,
				Initialization: src.InitTemplate,
			},
		}
		for _, s := range rr.Segments {
			rep.Segments = append(rep.Segments, s.DurationMs*rr.Timescale/1000)
		}
		idx, ok := setIdx[rr.ContentType]
		if !ok {
			period.AdaptationSets = append(period.AdaptationSets,
				manifest.AdaptationSet{ContentType: rr.ContentType})
			idx = len(period.AdaptationSets) - 1
			setIdx[rr.ContentType] = idx
		}
		as := &period.AdaptationSets[idx]
		as.Representations = append(as.Representations, rep)
	}

	m := &manifest.MPD{Type: "static", MinBufferTimeNs: 2 * 1e9,
		Periods: []manifest.Period{period}}
	if err := m.Validate(); err != nil {
		return nil, nil, err
	}
	return m, info, nil
}

// copyFile 复制文件；失败报 E_WRITE。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "copy: open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "copy: create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return errs.New(errs.EWrite, errs.ExitWrite, "copy: write %s: %v", dst, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return errs.New(errs.EWrite, errs.ExitWrite, "copy: close %s: %v", dst, err)
	}
	return nil
}
