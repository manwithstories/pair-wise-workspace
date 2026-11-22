// Package manifest 解析 DASH/HLS 清单，把分段映射到统一纳秒时钟。
package manifest

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

// Segment 是一个媒体分段在统一时钟上的位置。
type Segment struct {
	Index     int
	URI       string
	Start     clock.Ticks // 统一时钟上的起点
	Duration  clock.Ticks
	RepID     string // 所属表示层
	Bandwidth int
}

// Representation 表示层及其分段。
type Representation struct {
	ID        string
	Bandwidth int
	Segments  []Segment
	InitURI   string
}

// Program 是解析后的节目：保留表示层、分段边界与时间映射。
type Program struct {
	Representations []Representation
	Timeline        *clock.Timeline
}

// mpd XML 结构（仅解析所需字段）。
type mpdXML struct {
	Periods []struct {
		Adaptations []struct {
			Reps []struct {
				ID        string `xml:"id,attr"`
				Bandwidth int    `xml:"bandwidth,attr"`
				Template  *struct {
					Media          string `xml:"media,attr"`
					Initialization string `xml:"initialization,attr"`
					Timescale      int    `xml:"timescale,attr"`
					Timeline       *struct {
						S []struct {
							T *int64 `xml:"t,attr"`
							D int64  `xml:"d,attr"`
							R int    `xml:"r,attr"`
						} `xml:"S"`
					} `xml:"SegmentTimeline"`
				} `xml:"SegmentTemplate"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// ParseDASH 解析 MPD，把每个表示层的分段映射到统一时钟。
// 非单调时间戳、缺失初始化段、重叠边界均以带文件/行位置的错误报告。
func ParseDASH(name string, r io.Reader) (*Program, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1, "read: %v", err)
	}
	// 记录每个 <S> 元素结束位置的行号，用于可定位错误。
	sLines := scanTagLines(string(data), "<S ")
	var doc mpdXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		line := 1 + strings.Count(string(data), "\n")
		return nil, fault.New(fault.KindManifest, fault.Fail, name, line, -1, "XML parse: %v", err)
	}
	prog := &Program{Timeline: clock.NewTimeline()}
	sIdx := 0
	for _, p := range doc.Periods {
		for _, ad := range p.Adaptations {
			for _, rep := range ad.Reps {
				if rep.Template == nil {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1,
						"representation %q missing SegmentTemplate", rep.ID)
				}
				tp := rep.Template
				if tp.Initialization == "" {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1,
						"representation %q missing initialization segment", rep.ID)
				}
				if tp.Timescale <= 0 {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1,
						"representation %q invalid timescale %d", rep.ID, tp.Timescale)
				}
				out := Representation{ID: rep.ID, Bandwidth: rep.Bandwidth, InitURI: tp.Initialization}
				if tp.Timeline == nil || len(tp.Timeline.S) == 0 {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1,
						"representation %q has empty SegmentTimeline", rep.ID)
				}
				var cur int64 // 当前本地时间（timescale 单位）
				segIdx := 0
				for _, s := range tp.Timeline.S {
					line := 0
					if sIdx < len(sLines) {
						line = sLines[sIdx]
					}
					sIdx++
					if s.D <= 0 {
						return nil, fault.New(fault.KindManifest, fault.Fail, name, line, segIdx,
							"non-positive segment duration %d", s.D)
					}
					if s.T != nil {
						if *s.T < cur {
							return nil, fault.New(fault.KindManifest, fault.Fail, name, line, segIdx,
								"non-monotonic timestamp t=%d after %d", *s.T, cur)
						}
						cur = *s.T
					}
					repeat := s.R
					if repeat < 0 {
						repeat = 0
					}
					for k := 0; k <= repeat; k++ {
						start := clock.Ticks(cur * int64(clock.Second) / int64(tp.Timescale))
						dur := clock.Ticks(s.D * int64(clock.Second) / int64(tp.Timescale))
						uri := strings.ReplaceAll(tp.Media, "$RepresentationID$", rep.ID)
						uri = strings.ReplaceAll(uri, "$Number$", fmt.Sprintf("%d", segIdx+1))
						out.Segments = append(out.Segments, Segment{
							Index: segIdx, URI: uri, Start: start, Duration: dur,
							RepID: rep.ID, Bandwidth: rep.Bandwidth,
						})
						cur += s.D
						segIdx++
					}
				}
				// 重叠边界检查：后一段起点不得早于前一段终点。
				for i := 1; i < len(out.Segments); i++ {
					prev, seg := out.Segments[i-1], out.Segments[i]
					if seg.Start < prev.Start+prev.Duration {
						return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, seg.Index,
							"overlapping boundary: start=%d prev_end=%d", seg.Start, prev.Start+prev.Duration)
					}
				}
				prog.Representations = append(prog.Representations, out)
			}
		}
	}
	if len(prog.Representations) == 0 {
		return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1, "no representations")
	}
	return prog, nil
}

// scanTagLines 返回每个以 prefix 开头的标签所在行号（1 起始）。
func scanTagLines(data, prefix string) []int {
	var lines []int
	for i, ln := range strings.Split(data, "\n") {
		if strings.Contains(ln, prefix) {
			lines = append(lines, i+1)
		}
	}
	return lines
}
