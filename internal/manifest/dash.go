// Package manifest 解析 DASH MPD 与含 discontinuity 的 HLS 播放列表。
package manifest

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"review/internal/fault"
)

// MaxTracks 单清单允许的最大轨道（表示层）数。
const MaxTracks = 16

// 允许编码白名单：fMP4 JPEG 视频与 PCM 音频，其他编码明确拒绝。
var allowedCodecs = map[string]bool{"jpeg": true, "lpcm": true}

// Representation 一档 DASH 表示层。
type Representation struct {
	ID              string
	Codecs          string
	Bandwidth       int64
	Timescale       int64
	SegmentDuration int64 // timescale 单位
	InitPath        string
	MediaPaths      []string
}

// DASH 解析结果。
type DASH struct {
	Representations []Representation
}

type mpdXML struct {
	Periods []struct {
		Adaptations []struct {
			Reps []struct {
				ID        string `xml:"id,attr"`
				Codecs    string `xml:"codecs,attr"`
				Bandwidth int64  `xml:"bandwidth,attr"`
				Template  struct {
					Timescale    int64  `xml:"timescale,attr"`
					Init         string `xml:"initialization,attr"`
					Media        string `xml:"media,attr"`
					StartNumber  int64  `xml:"startNumber,attr"`
					Duration     int64  `xml:"duration,attr"`
					SegmentCount int64  `xml:"segmentCount,attr"`
				} `xml:"SegmentTemplate"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// Resolve 在 baseDir 内解析模板路径，拒绝路径逃逸与越界。
func Resolve(baseDir, template, repID string, number int64) (string, *fault.Error) {
	s := strings.ReplaceAll(template, "$RepresentationID$", repID)
	if idx := strings.Index(s, "$Number"); idx >= 0 {
		end := strings.Index(s[idx:], "$")
		if end <= 0 {
			return "", fault.New(fault.KindLimit, template, 0, false, "unterminated $Number")
		}
		spec := s[idx : idx+end+1]
		width := 1
		if strings.HasPrefix(spec, "$Number%0") && strings.HasSuffix(spec, "d$") {
			fmt.Sscanf(spec, "$Number%%0%dd$", &width)
		}
		s = strings.Replace(s, spec, fmt.Sprintf("%0*d", width, number), 1)
	}
	if strings.Contains(s, "$") {
		return "", fault.Newf(fault.KindLimit, template, 0, false, "unsupported template %q", s)
	}
	full := filepath.Join(baseDir, filepath.FromSlash(s))
	base, err1 := filepath.Abs(baseDir)
	abs, err2 := filepath.Abs(full)
	if err1 != nil || err2 != nil || abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", fault.Newf(fault.KindPathEscape, s, 0, false, "path escapes base dir")
	}
	return full, nil
}

// ParseDASH 解析 MPD，校验编码白名单、初始化段存在性与轨道数上限。
func ParseDASH(path string) (*DASH, []*fault.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []*fault.Error{fault.Newf(fault.KindMissingInit, path, 0, false, "read mpd: %v", err)}
	}
	var doc mpdXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, []*fault.Error{fault.Newf(fault.KindLimit, path, 0, false, "xml: %v", err)}
	}
	baseDir := filepath.Dir(path)
	out := &DASH{}
	var errs []*fault.Error
	for _, p := range doc.Periods {
		for _, as := range p.Adaptations {
			for _, r := range as.Reps {
				src := "rep:" + r.ID
				if len(out.Representations) >= MaxTracks {
					errs = append(errs, fault.Newf(fault.KindLimit, src, 0, false,
						"track count exceeds %d", MaxTracks))
					continue
				}
				codecOff := int64(strings.Index(string(data), `codecs="`+r.Codecs+`"`))
				if !allowedCodecs[r.Codecs] {
					errs = append(errs, fault.Newf(fault.KindUnknownCodec, src, codecOff, false,
						"codec %q not supported (jpeg/lpcm only)", r.Codecs))
					continue
				}
				rep := Representation{ID: r.ID, Codecs: r.Codecs, Bandwidth: r.Bandwidth,
					Timescale: r.Template.Timescale, SegmentDuration: r.Template.Duration}
				initPath, ferr := Resolve(baseDir, r.Template.Init, r.ID, 0)
				if ferr != nil {
					errs = append(errs, ferr)
					continue
				}
				rep.InitPath = initPath
				if _, err := os.Stat(initPath); err != nil {
					errs = append(errs, fault.Newf(fault.KindMissingInit, src, codecOff, true,
						"init segment %s missing", initPath))
					continue
				}
				for n := r.Template.StartNumber; n < r.Template.StartNumber+r.Template.SegmentCount; n++ {
					mp, ferr := Resolve(baseDir, r.Template.Media, r.ID, n)
					if ferr != nil {
						errs = append(errs, ferr)
						break
					}
					rep.MediaPaths = append(rep.MediaPaths, mp)
				}
				out.Representations = append(out.Representations, rep)
			}
		}
	}
	return out, errs
}
