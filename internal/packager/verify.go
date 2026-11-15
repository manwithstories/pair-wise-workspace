// MPD 反向校验：解析清单、展开模板、回查源目录文件。
package packager

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dashpack/internal/align"
	"dashpack/internal/errs"
	"dashpack/internal/manifest"
	"dashpack/internal/source"
)

// VerifyResult 是 verify 的结构化输出。
type VerifyResult struct {
	Representations  int     `json:"representations"`
	AdaptationSets   int     `json:"adaptation_sets"`
	SegmentsExpanded int     `json:"segments_expanded"`
	PeriodDurationMs int64   `json:"period_duration_ms"`
	MaxDeviationTicks int64  `json:"max_deviation_ticks"`
	MaxDeviationMs   Fixed3  `json:"max_deviation_ms"`
	RepairedItems    int     `json:"repaired_items"`
	FilesMissing     int     `json:"files_missing"`
}

// Fixed3 以三位小数序列化浮点。
type Fixed3 float64

// MarshalJSON 实现三位小数输出。
func (f Fixed3) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%.3f", float64(f))), nil
}

// Verify 反向校验 MPD 与源目录的一致性；不一致返回退出码 3 的错误。
func Verify(mpdPath, root string) (*VerifyResult, error) {
	data, err := os.ReadFile(mpdPath)
	if err != nil {
		return nil, errs.New(errs.ESource, errs.ExitUsage, "read mpd: %v", err)
	}
	m, err := manifest.Unmarshal(data)
	if err != nil {
		return nil, err
	}
	reps, err := source.Scan(root)
	if err != nil {
		return nil, err
	}
	aligned, err := align.Align(reps, align.PolicyTolerant)
	if err != nil {
		return nil, err
	}
	expected, info, err := Assemble(reps, aligned)
	if err != nil {
		return nil, err
	}

	res := &VerifyResult{PeriodDurationMs: info.PeriodDurationMs,
		RepairedItems: info.RepairedItems}
	for _, rr := range aligned.Reps {
		if rr.MaxDeviationTicks > res.MaxDeviationTicks {
			res.MaxDeviationTicks = rr.MaxDeviationTicks
			res.MaxDeviationMs = Fixed3(rr.MaxDeviationMs)
		}
	}

	var problems []string
	// 1. 模型对比：声明时长须与源记录（经修补）一致。
	problems = append(problems, diffModels(m, expected)...)
	// 2. 模板展开回查文件。
	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			res.AdaptationSets++
			for _, r := range as.Representations {
				res.Representations++
				missing, n, prob := checkFiles(root, r)
				res.FilesMissing += missing
				res.SegmentsExpanded += n
				problems = append(problems, prob...)
			}
		}
	}
	// 3. 累计时长与 Period@duration 的差值。
	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			for _, r := range as.Representations {
				var sum int64
				for _, d := range r.Segments {
					sum += d
				}
				sumMs := sum * 1000 / r.Template.Timescale
				if diff := sumMs*1e6 - p.DurationNs; diff != 0 {
					problems = append(problems, fmt.Sprintf(
						"%s: cumulative %dms differs from period duration by %dns",
						r.ID, sumMs, diff))
				}
			}
		}
	}
	if res.FilesMissing > 0 {
		problems = append(problems, fmt.Sprintf("%d expanded files missing", res.FilesMissing))
	}
	if len(problems) > 0 {
		return res, errs.New(errs.EVerify, errs.ExitAlignment,
			"verify failed:\n  %s", strings.Join(problems, "\n  "))
	}
	return res, nil
}

// diffModels 对比解析所得模型与按源重建的期望模型。
func diffModels(got, want *manifest.MPD) []string {
	var probs []string
	if len(got.Periods) != len(want.Periods) {
		return []string{"period count mismatch"}
	}
	for i := range got.Periods {
		gp, wp := &got.Periods[i], &want.Periods[i]
		if gp.DurationNs != wp.DurationNs {
			probs = append(probs, fmt.Sprintf("period %d duration %d != expected %d",
				i, gp.DurationNs, wp.DurationNs))
		}
		if len(gp.AdaptationSets) != len(wp.AdaptationSets) {
			probs = append(probs, fmt.Sprintf("period %d adaptationSet count mismatch", i))
			continue
		}
		for j := range gp.AdaptationSets {
			ga, wa := &gp.AdaptationSets[j], &wp.AdaptationSets[j]
			if ga.ContentType != wa.ContentType {
				probs = append(probs, fmt.Sprintf("adaptationSet %d contentType %q != %q",
					j, ga.ContentType, wa.ContentType))
			}
			if len(ga.Representations) != len(wa.Representations) {
				probs = append(probs, fmt.Sprintf("adaptationSet %d representation count mismatch", j))
				continue
			}
			for k := range ga.Representations {
				probs = append(probs, diffRep(&ga.Representations[k], &wa.Representations[k])...)
			}
		}
	}
	return probs
}

func diffRep(g, w *manifest.Representation) []string {
	var probs []string
	bad := func(format string, args ...any) {
		probs = append(probs, fmt.Sprintf("representation %s: "+format,
			append([]any{g.ID}, args...)...))
	}
	if g.ID != w.ID || g.Bandwidth != w.Bandwidth || g.Codecs != w.Codecs {
		bad("id/bandwidth/codecs mismatch")
	}
	if g.Template != w.Template {
		bad("segment template mismatch")
	}
	if len(g.Segments) != len(w.Segments) {
		bad("segment count %d != expected %d", len(g.Segments), len(w.Segments))
		return probs
	}
	for i := range g.Segments {
		if g.Segments[i] != w.Segments[i] {
			bad("segment %d duration %d != expected %d",
				i+1, g.Segments[i], w.Segments[i])
		}
	}
	return probs
}

// checkFiles 展开模板并回查文件存在性与目录一致性。
func checkFiles(root string, r manifest.Representation) (int, int, []string) {
	var probs []string
	expanded := map[string]bool{}
	var time int64
	missing := 0
	for i, d := range r.Segments {
		name, err := manifest.ExpandTemplate(r.Template.Media, map[string]any{
			"RepresentationID": r.ID,
			"Number":           r.Template.StartNumber + int64(i),
			"Time":             time,
		})
		if err != nil {
			probs = append(probs, err.Error())
			continue
		}
		time += d
		expanded[name] = true
		p, err := source.SafeJoinRel(root, name)
		if err != nil {
			probs = append(probs, err.Error())
			continue
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			missing++
			probs = append(probs, fmt.Sprintf("%s: missing file %s", r.ID, name))
		}
	}
	initName, err := manifest.ExpandTemplate(r.Template.Initialization,
		map[string]any{"RepresentationID": r.ID})
	if err != nil {
		probs = append(probs, err.Error())
	} else {
		expanded[initName] = true
		if p, err := source.SafeJoinRel(root, initName); err != nil {
			probs = append(probs, err.Error())
		} else if st, err := os.Stat(p); err != nil || st.IsDir() {
			missing++
			probs = append(probs, fmt.Sprintf("%s: missing file %s", r.ID, initName))
		}
	}
	// 展开集合须与目录文件一致。
	dir := filepath.Join(root, r.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		probs = append(probs, fmt.Sprintf("%s: read dir: %v", r.ID, err))
		return missing, len(r.Segments), probs
	}
	var extra []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !expanded[r.ID+"/"+e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		probs = append(probs, fmt.Sprintf("%s: files not in template expansion: %s",
			r.ID, strings.Join(extra, ",")))
	}
	return missing, len(r.Segments), probs
}
