// Package validate 合并容器 TLV 与视频轨 SEI 两路 HDR 字段，执行缺省补全、
// 逐轨校验、跨轨全局一致性检查与冲突分级，产出 reporter 直接渲染的报告模型。
package validate

import (
	"fmt"
	"sort"

	"hdrcheck/internal/garc"
	"hdrcheck/internal/hdr"
)

// 字段状态：ok 两路一致或单源 / inherited 容器缺省由 SEI 继承 /
// conflict 两路均存在但不等 / missing 两路均无。
const (
	StatusOK        = "ok"
	StatusInherited = "inherited"
	StatusConflict  = "conflict"
	StatusMissing   = "missing"
)

// 业务问题代码（结构类代码复用 hdr.Issue 中解析器产出的代码）。
const (
	CodeFieldConflict   = "FIELD_CONFLICT"
	CodeFieldMissing    = "FIELD_MISSING"
	CodeHDRFlagMissing  = "HDR_FLAG_MISSING"
	CodeGlobalCLL       = "GLOBAL_MAXCLL_CONFLICT"
	CodeGlobalFALL      = "GLOBAL_MAXFALL_CONFLICT"
	CodeGlobalMastering = "GLOBAL_MASTERING_CONFLICT"
)

// ScalarField 是合并后的单个标量亮度字段（MaxCLL / MaxFALL）。
type ScalarField struct {
	Present        bool            `json:"present"`
	Nits           uint16          `json:"nits,omitempty"`
	Source         hdr.Source      `json:"source"`
	Status         string          `json:"status"`
	ContainerValue *uint16         `json:"container_value,omitempty"`
	SEIValues      []SEIScalar     `json:"sei_values,omitempty"`
	Provenance     *hdr.Provenance `json:"provenance,omitempty"`
}

// SEIScalar 保留一条 SEI 记录中的标量值及其出处。
type SEIScalar struct {
	Nits       uint16         `json:"nits"`
	Provenance hdr.Provenance `json:"provenance"`
}

// MasteringField 是合并后的母版显示信息视图，冲突时双值均保留。
type MasteringField struct {
	Present        bool                   `json:"present"`
	Source         hdr.Source             `json:"source"`
	Status         string                 `json:"status"`
	Value          *hdr.MasteringDisplay  `json:"value,omitempty"`
	ContainerValue *hdr.MasteringDisplay  `json:"container_value,omitempty"`
	SEIValues      []hdr.MasteringDisplay `json:"sei_values,omitempty"`
}

// TrackReport 是单条视频轨的合并与校验结果。
type TrackReport struct {
	TrackID   uint32         `json:"track_id"`
	HDRFlag   bool           `json:"hdr_flag"`
	Mastering MasteringField `json:"mastering_display"`
	MaxCLL    ScalarField    `json:"max_content_light_level"`
	MaxFALL   ScalarField    `json:"max_frame_average_light_level"`
	Issues    []hdr.Issue    `json:"issues"`
}

// Summary 是终端摘要与 JSON 头部共用的计数与输入指纹。
type Summary struct {
	VideoTrackCount int    `json:"video_track_count"`
	ConflictCount   int    `json:"conflict_count"`
	MissingCount    int    `json:"missing_count"`
	ErrorCount      int    `json:"error_count"`
	WarningCount    int    `json:"warning_count"`
	InputSHA256     string `json:"input_sha256"`
	InputBytes      int64  `json:"input_bytes"`
}

// Report 是 validate 的完整产物。
type Report struct {
	Summary Summary       `json:"summary"`
	Tracks  []TrackReport `json:"tracks"`
	// 容器级与跨轨问题（track_id=0）。
	GlobalIssues []hdr.Issue            `json:"global_issues"`
	Container    *hdr.ContainerMetadata `json:"container_metadata,omitempty"`
	SEI          *hdr.SEIResult         `json:"sei,omitempty"`
	// Fatal 为 true 时命令层退出码按 3 处理。
	Fatal bool `json:"-"`
}

// Build 执行全量合并与校验。container / sei 为解析器产物，sha 为输入摘要。
func Build(f *garc.File, container *hdr.ContainerMetadata, sei *hdr.SEIResult, sha string) *Report {
	rep := &Report{Container: container, SEI: sei}
	rep.Summary = Summary{
		VideoTrackCount: len(f.VideoTracks()),
		InputSHA256:     sha,
		InputBytes:      int64(f.Reader.Len()),
	}
	rep.Fatal = container.Fatal || sei.Fatal

	// 解析期问题先按 track_id 分流。
	trackIssues := map[uint32][]hdr.Issue{}
	for _, is := range append(append([]hdr.Issue{}, container.Issues...), sei.Issues...) {
		if is.TrackID != 0 {
			trackIssues[is.TrackID] = append(trackIssues[is.TrackID], is)
		} else {
			rep.GlobalIssues = append(rep.GlobalIssues, is)
		}
	}

	for _, t := range f.VideoTracks() {
		tr := TrackReport{TrackID: t.ID, HDRFlag: t.HDRFlag == 1}
		recs := sei.RecordsOf(t.ID)
		if !tr.HDRFlag {
			tr.Issues = append(tr.Issues, hdr.Issue{
				Code: CodeHDRFlagMissing, Severity: hdr.SeverityWarning,
				TrackID: t.ID,
				Message: "video track does not declare the HDR marker and carries no SEI",
				Hint:    "re-export with the HDR flag set, or route the track as SDR",
			})
			tr.Mastering = missingMastering()
			tr.MaxCLL = missingScalar()
			tr.MaxFALL = missingScalar()
		} else {
			var issues []hdr.Issue
			tr.Mastering, issues = mergeMastering(container, recs, t.ID)
			tr.Issues = append(tr.Issues, issues...)
			tr.MaxCLL, issues = mergeScalar(container, recs, t.ID, true)
			tr.Issues = append(tr.Issues, issues...)
			tr.MaxFALL, issues = mergeScalar(container, recs, t.ID, false)
			tr.Issues = append(tr.Issues, issues...)
			rangeCheckTrack(&tr, recs)
		}
		tr.Issues = append(tr.Issues, trackIssues[t.ID]...)
		sortIssues(tr.Issues)
		rep.Tracks = append(rep.Tracks, tr)
	}
	rep.globalCrossTrackChecks()
	rep.collectCounts()
	sortIssues(rep.GlobalIssues)
	return rep
}

func mergeMastering(c *hdr.ContainerMetadata, recs []hdr.SEIRecord, trackID uint32) (MasteringField, []hdr.Issue) {
	mf := MasteringField{}
	var issues []hdr.Issue
	var cVal *hdr.MasteringDisplay
	if c.Mastering != nil {
		v := *c.Mastering
		cVal = &v
	}
	for _, rec := range recs {
		if rec.Mastering != nil {
			mf.SEIValues = append(mf.SEIValues, *rec.Mastering)
		}
	}
	switch {
	case cVal != nil && len(mf.SEIValues) > 0:
		mf.Present, mf.Source, mf.ContainerValue = true, hdr.SourceContainer, cVal
		mf.Value, mf.Status = cVal, StatusOK
		for i := range mf.SEIValues {
			if !hdr.EqualMastering(*cVal, mf.SEIValues[i]) {
				mf.Status = StatusConflict
				issues = append(issues, fieldIssue(trackID, mf.SEIValues[i].Provenance.Offset, CodeFieldConflict, fmt.Sprintf(
					"mastering display differs: container vs SEI seq %d at offset %d",
					mf.SEIValues[i].Provenance.SeqNum, mf.SEIValues[i].Provenance.Offset)))
			}
		}
	case cVal != nil:
		mf.Present, mf.Source, mf.Value, mf.Status = true, hdr.SourceContainer, cVal, StatusOK
	case len(mf.SEIValues) > 0:
		v := mf.SEIValues[0]
		mf.Present, mf.Source, mf.Value, mf.Status = true, hdr.SourceSEI, &v, StatusInherited
	default:
		mf.Status = StatusMissing
		issues = append(issues, fieldIssue(trackID, 0, CodeFieldMissing,
			"mastering display missing from both container and SEI"))
	}
	return mf, issues
}

func mergeScalar(c *hdr.ContainerMetadata, recs []hdr.SEIRecord, trackID uint32, isCLL bool) (ScalarField, []hdr.Issue) {
	sf := ScalarField{}
	var issues []hdr.Issue
	var cVal *uint16
	if c.CLL != nil {
		var v uint16
		if isCLL {
			v = c.CLL.MaxContentLightLevel.Nits
		} else {
			v = c.CLL.MaxFrameAverageLightLevel.Nits
		}
		cVal = &v
	}
	field := "MaxFALL"
	if isCLL {
		field = "MaxCLL"
	}
	for _, rec := range recs {
		if rec.CLL == nil {
			continue
		}
		ll := rec.CLL.MaxFrameAverageLightLevel
		if isCLL {
			ll = rec.CLL.MaxContentLightLevel
		}
		sf.SEIValues = append(sf.SEIValues, SEIScalar{Nits: ll.Nits, Provenance: ll.Provenance})
	}
	switch {
	case cVal != nil && len(sf.SEIValues) > 0:
		sf.Present, sf.Source, sf.ContainerValue = true, hdr.SourceContainer, cVal
		sf.Nits, sf.Status = *cVal, StatusOK
		for _, sv := range sf.SEIValues {
			if sv.Nits != *cVal {
				sf.Status = StatusConflict
				issues = append(issues, fieldIssue(trackID, sv.Provenance.Offset, CodeFieldConflict, fmt.Sprintf(
					"%s differs: container=%d nits vs SEI=%d nits (seq %d)",
					field, *cVal, sv.Nits, sv.Provenance.SeqNum)))
			}
		}
	case cVal != nil:
		sf.Present, sf.Source, sf.Nits, sf.Status = true, hdr.SourceContainer, *cVal, StatusOK
	case len(sf.SEIValues) > 0:
		sf.Present, sf.Source = true, hdr.SourceSEI
		sf.Nits, sf.Status = sf.SEIValues[0].Nits, StatusInherited
		p := sf.SEIValues[0].Provenance
		sf.Provenance = &p
	default:
		sf.Status = StatusMissing
		issues = append(issues, fieldIssue(trackID, 0, CodeFieldMissing,
			fmt.Sprintf("%s missing from both container and SEI", field)))
	}
	return sf, issues
}

// rangeCheckTrack 对 SEI 侧数值补做范围检查（容器侧在 TLV 解析时已检查）。
func rangeCheckTrack(tr *TrackReport, recs []hdr.SEIRecord) {
	for _, rec := range recs {
		if rec.Mastering != nil {
			m := rec.Mastering
			if m.MaxDisplayMasteringLuminance < m.MinDisplayMasteringLuminance ||
				!hdr.ValidateMasteringLuminance(m.MaxDisplayMasteringLuminance) ||
				!hdr.ValidateMasteringLuminance(m.MinDisplayMasteringLuminance) {
				tr.Issues = append(tr.Issues, hdr.Issue{
					Code: "VALUE_OUT_OF_RANGE", Severity: hdr.SeverityError,
					TrackID: tr.TrackID, Offset: rec.Offset,
					Message: fmt.Sprintf("SEI mastering luminance illegal: min=%d max=%d",
						m.MinDisplayMasteringLuminance, m.MaxDisplayMasteringLuminance),
					Hint: "correct the luminance at source and re-export",
				})
			}
		}
		if rec.CLL != nil {
			for name, ll := range map[string]hdr.LightLevel{
				"MaxCLL":  rec.CLL.MaxContentLightLevel,
				"MaxFALL": rec.CLL.MaxFrameAverageLightLevel,
			} {
				if !hdr.ValidateCLL(ll.Nits) {
					tr.Issues = append(tr.Issues, hdr.Issue{
						Code: "VALUE_OUT_OF_RANGE", Severity: hdr.SeverityError,
						TrackID: tr.TrackID, Offset: ll.Provenance.Offset,
						Message: fmt.Sprintf("SEI %s=%d nits outside [%d,%d]",
							name, ll.Nits, hdr.MinCLL, hdr.MaxCLL),
						Hint: "correct the value at source and re-export",
					})
				}
			}
		}
	}
}

func (r *Report) globalCrossTrackChecks() {
	eff := map[uint32]TrackReport{}
	for _, tr := range r.Tracks {
		eff[tr.TrackID] = tr
	}
	cll := map[uint16]uint32{}
	fall := map[uint16]uint32{}
	mast := map[string]uint32{}
	for _, tr := range r.Tracks {
		if tr.MaxCLL.Present && tr.MaxCLL.Status != StatusConflict {
			cll[tr.MaxCLL.Nits] = tr.TrackID
		}
		if tr.MaxFALL.Present && tr.MaxFALL.Status != StatusConflict {
			fall[tr.MaxFALL.Nits] = tr.TrackID
		}
		if tr.Mastering.Present && tr.Mastering.Status != StatusConflict && tr.Mastering.Value != nil {
			mast[masteringKey(*tr.Mastering.Value)] = tr.TrackID
		}
	}
	if len(cll) > 1 {
		r.GlobalIssues = append(r.GlobalIssues, globalIssue(CodeGlobalCLL,
			fmt.Sprintf("tracks disagree on effective MaxCLL: %v", keys(cll))))
	}
	if len(fall) > 1 {
		r.GlobalIssues = append(r.GlobalIssues, globalIssue(CodeGlobalFALL,
			fmt.Sprintf("tracks disagree on effective MaxFALL: %v", keys(fall))))
	}
	if len(mast) > 1 {
		r.GlobalIssues = append(r.GlobalIssues, globalIssue(CodeGlobalMastering,
			"tracks disagree on effective mastering display parameters"))
	}
}

func (r *Report) collectCounts() {
	count := func(list []hdr.Issue) {
		for _, is := range list {
			switch is.Severity {
			case hdr.SeverityError:
				r.Summary.ErrorCount++
			case hdr.SeverityWarning:
				r.Summary.WarningCount++
			}
			switch is.Code {
			case CodeFieldConflict, CodeGlobalCLL, CodeGlobalFALL, CodeGlobalMastering:
				r.Summary.ConflictCount++
			case CodeFieldMissing, CodeHDRFlagMissing:
				r.Summary.MissingCount++
			}
		}
	}
	count(r.GlobalIssues)
	for _, tr := range r.Tracks {
		count(tr.Issues)
	}
}

// --- 小工具 ---

func fieldIssue(trackID uint32, off int64, code, msg string) hdr.Issue {
	return hdr.Issue{Code: code, Severity: hdr.SeverityWarning, TrackID: trackID, Offset: off, Message: msg}
}

func missingMastering() MasteringField {
	return MasteringField{Status: StatusMissing, Source: hdr.SourceMissing}
}
func missingScalar() ScalarField {
	return ScalarField{Status: StatusMissing, Source: hdr.SourceMissing}
}

func globalIssue(code, msg string) hdr.Issue {
	return hdr.Issue{Code: code, Severity: hdr.SeverityWarning, Message: msg,
		Hint: "reconcile the tracks at source; one package must declare one HDR grade"}
}

func masteringKey(m hdr.MasteringDisplay) string {
	return fmt.Sprintf("%v|%v|%d|%d", m.DisplayPrimaries, m.WhitePoint,
		m.MinDisplayMasteringLuminance, m.MaxDisplayMasteringLuminance)
}

func keys(m map[uint16]uint32) []uint16 {
	out := make([]uint16, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortIssues(list []hdr.Issue) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].TrackID != list[j].TrackID {
			return list[i].TrackID < list[j].TrackID
		}
		if list[i].Offset != list[j].Offset {
			return list[i].Offset < list[j].Offset
		}
		return list[i].Code < list[j].Code
	})
}
