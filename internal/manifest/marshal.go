// MPD 模型与 XML 的双向转换，以及分片模板展开。
package manifest

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"dashpack/internal/errs"
)

type xmlS struct {
	D int64 `xml:"d,attr"`
	T int64 `xml:"t,attr,omitempty"`
}

type xmlTimeline struct {
	S []xmlS `xml:"S"`
}

type xmlSegmentTemplate struct {
	Timescale      int64       `xml:"timescale,attr"`
	Duration       int64       `xml:"duration,attr"`
	StartNumber    int64       `xml:"startNumber,attr"`
	Media          string      `xml:"media,attr"`
	Initialization string      `xml:"initialization,attr"`
	Timeline       *xmlTimeline `xml:"SegmentTimeline,omitempty"`
}

type xmlRepresentation struct {
	ID        string `xml:"id,attr"`
	Bandwidth int64  `xml:"bandwidth,attr"`
	Codecs    string `xml:"codecs,attr"`
	Template  xmlSegmentTemplate `xml:"SegmentTemplate"`
}

type xmlAdaptationSet struct {
	ContentType     string `xml:"contentType,attr"`
	MimeType        string `xml:"mimeType,attr"`
	Representations []xmlRepresentation `xml:"Representation"`
}

type xmlPeriod struct {
	Duration string `xml:"duration,attr"`
	Sets     []xmlAdaptationSet `xml:"AdaptationSet"`
}

type xmlMPD struct {
	XMLName  xml.Name `xml:"MPD"`
	Xmlns    string   `xml:"xmlns,attr"`
	Type     string   `xml:"type,attr"`
	Duration string   `xml:"mediaPresentationDuration,attr"`
	MinBuf   string   `xml:"minBufferTime,attr"`
	Periods  []xmlPeriod `xml:"Period"`
}

// Marshal 把模型序列化为 MPD XML。
func Marshal(m *MPD) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	var totalNs int64
	x := xmlMPD{Xmlns: "urn:mpeg:dash:schema:mpd:2011", Type: m.Type,
		MinBuf: FormatDuration(m.MinBufferTimeNs)}
	for _, p := range m.Periods {
		totalNs += p.DurationNs
		xp := xmlPeriod{Duration: FormatDuration(p.DurationNs)}
		for _, as := range p.AdaptationSets {
			xs := xmlAdaptationSet{ContentType: as.ContentType,
				MimeType: as.ContentType + "/mp4"}
			for _, r := range as.Representations {
				xt := xmlSegmentTemplate{
					Timescale:      r.Template.Timescale,
					Duration:       r.Template.Duration,
					StartNumber:    r.Template.StartNumber,
					Media:          r.Template.Media,
					Initialization: r.Template.Initialization,
				}
				if len(r.Segments) > 0 {
					tl := &xmlTimeline{}
					var t int64
					for _, d := range r.Segments {
						tl.S = append(tl.S, xmlS{D: d, T: t})
						t += d
					}
					xt.Timeline = tl
				}
				xs.Representations = append(xs.Representations, xmlRepresentation{
					ID: r.ID, Bandwidth: r.Bandwidth, Codecs: r.Codecs, Template: xt,
				})
			}
			xp.Sets = append(xp.Sets, xs)
		}
		x.Periods = append(x.Periods, xp)
	}
	x.Duration = FormatDuration(totalNs)
	out, err := xml.MarshalIndent(x, "", "  ")
	if err != nil {
		return nil, errs.New(errs.EManifest, errs.ExitUsage, "marshal: %v", err)
	}
	return append([]byte(xml.Header), out...), nil
}

// Unmarshal 解析 MPD XML 回模型。
func Unmarshal(data []byte) (*MPD, error) {
	var x xmlMPD
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, errs.New(errs.EManifest, errs.ExitUsage, "parse mpd: %v", err)
	}
	m := &MPD{Type: x.Type}
	if v, err := ParseDuration(x.MinBuf); err == nil {
		m.MinBufferTimeNs = v
	}
	for _, xp := range x.Periods {
		d, err := ParseDuration(xp.Duration)
		if err != nil {
			return nil, err
		}
		p := Period{DurationNs: d}
		for _, xs := range xp.Sets {
			as := AdaptationSet{ContentType: xs.ContentType}
			for _, xr := range xs.Representations {
				r := Representation{
					ID: xr.ID, Bandwidth: xr.Bandwidth, Codecs: xr.Codecs,
					ContentType: xs.ContentType,
					Template: SegmentTemplate{
						Timescale:      xr.Template.Timescale,
						Duration:       xr.Template.Duration,
						StartNumber:    xr.Template.StartNumber,
						Media:          xr.Template.Media,
						Initialization: xr.Template.Initialization,
					},
				}
				if xr.Template.Timeline != nil {
					for _, s := range xr.Template.Timeline.S {
						r.Segments = append(r.Segments, s.D)
					}
				}
				as.Representations = append(as.Representations, r)
			}
			p.AdaptationSets = append(p.AdaptationSets, as)
		}
		m.Periods = append(m.Periods, p)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}


// ExpandTemplate 展开分片命名模板。
// 支持 $RepresentationID$、$Number%05d$（宽度可省）、$Time$、$$ 转义。
// 宽度非法或变量含路径分隔符报 E_TEMPLATE。
func ExpandTemplate(tmpl string, vars map[string]any) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(tmpl) {
		if tmpl[i] != '$' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		if i+1 < len(tmpl) && tmpl[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '$')
		if end < 0 {
			return "", errs.New(errs.ETemplate, errs.ExitUsage,
				"unterminated variable in %q", tmpl)
		}
		expr := tmpl[i+1 : i+1+end]
		i += end + 2
		name, width := expr, -1
		if j := strings.IndexByte(expr, '%'); j >= 0 {
			name = expr[:j]
			w := expr[j+1:]
			if !strings.HasSuffix(w, "d") || len(w) < 2 {
				return "", errs.New(errs.ETemplate, errs.ExitUsage,
					"invalid width %q in %q", expr, tmpl)
			}
			n, err := strconv.Atoi(strings.TrimSuffix(w, "d"))
			if err != nil || n < 1 || n > 18 {
				return "", errs.New(errs.ETemplate, errs.ExitUsage,
					"invalid width %q in %q", expr, tmpl)
			}
			width = n
		}
		v, ok := vars[name]
		if !ok {
			return "", errs.New(errs.ETemplate, errs.ExitUsage,
				"unknown variable %q", name)
		}
		var s string
		switch name {
		case "Number", "Time":
			n, ok := v.(int64)
			if !ok {
				return "", errs.New(errs.ETemplate, errs.ExitUsage,
					"variable %q requires int64", name)
			}
			if width > 0 {
				s = fmt.Sprintf("%0*d", width, n)
			} else {
				s = strconv.FormatInt(n, 10)
			}
		default:
			if width > 0 {
				return "", errs.New(errs.ETemplate, errs.ExitUsage,
					"width not allowed on %q", name)
			}
			s = fmt.Sprint(v)
		}
		if strings.ContainsAny(s, "/\\") {
			return "", errs.New(errs.ETemplate, errs.ExitUsage,
				"variable %q expands to path separator", name)
		}
		b.WriteString(s)
	}
	return b.String(), nil
}
