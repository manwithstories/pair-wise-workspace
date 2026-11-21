package manifest

import (
	"encoding/xml"
	"fmt"
	"strings"

	"mediadeliver/internal/clock"
	"mediadeliver/internal/fault"
)

// ParseDASH 解析 DASH MPD 中的 SegmentTemplate/SegmentTimeline，
// 产出与 HLS 一致的分片引用。截断 XML 保留已解析的 S 条目。
func ParseDASH(source string, data []byte) Playlist {
	p := Playlist{Source: source}
	var timescale, segDur, startNum int64 = 1, 0, 1
	var media string
	var inTimeline bool
	var curTime int64
	var haveTime bool
	idx := int64(0)

	dec := xml.NewDecoder(strings.NewReader(string(data)))
	addSeg := func(d int64, off int64) {
		if len(p.Segments) >= MaxSegments {
			p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
				Offset: off, Message: "segment limit exceeded", Advice: "拆分 MPD"})
			p.Truncated = true
			return
		}
		uri := strings.ReplaceAll(media, "$Number$", fmt.Sprintf("%d", startNum+idx))
		p.Segments = append(p.Segments, Segment{
			Index:    startNum + idx,
			Duration: clock.New(d, timescale),
			URI:      uri,
			Offset:   off,
		})
		p.Duration = p.Duration.Add(clock.New(d, timescale))
		idx++
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			// XML 语法错误（含截断）：保留已解析前缀
			p.Truncated = true
			p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
				Offset: dec.InputOffset(), Recoverable: true,
				Message: "MPD truncated or malformed: " + err.Error(),
				Advice:  "保留已解析条目，等待完整 MPD 重传"})
			break
		}
		off := dec.InputOffset()
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "SegmentTemplate":
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "timescale":
						fmt.Sscanf(a.Value, "%d", &timescale)
					case "duration":
						fmt.Sscanf(a.Value, "%d", &segDur)
					case "startNumber":
						fmt.Sscanf(a.Value, "%d", &startNum)
					case "media":
						media = a.Value
					}
				}
				if timescale <= 0 {
					p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
						Offset: off, Message: "invalid timescale", Advice: "修正 timescale"})
					timescale = 1
				}
			case "SegmentTimeline":
				inTimeline = true
			case "S":
				if !inTimeline {
					continue
				}
				var t, d, r int64
				var hasT bool
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "t":
						fmt.Sscanf(a.Value, "%d", &t)
						hasT = true
					case "d":
						fmt.Sscanf(a.Value, "%d", &d)
					case "r":
						fmt.Sscanf(a.Value, "%d", &r)
					}
				}
				if d <= 0 {
					p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
						Offset: off, Message: "S with non-positive duration", Advice: "跳过该条目"})
					continue
				}
				if hasT {
					curTime, haveTime = t, true
				}
				if !haveTime {
					curTime, haveTime = 0, true
				}
				for i := int64(0); i <= r; i++ {
					addSeg(d, off)
					curTime += d
				}
			}
		case xml.EndElement:
			if el.Name.Local == "SegmentTimeline" {
				inTimeline = false
			}
		}
	}
	// 无 SegmentTimeline 时按固定 duration 展开为单条目序列无法确定数量，
	// 仅记录模板存在；调用方应优先使用 SegmentTimeline。
	if len(p.Segments) == 0 && segDur > 0 && !p.Truncated {
		p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
			Offset: -1, Recoverable: true,
			Message: "SegmentTemplate without SegmentTimeline", Advice: "补充 SegmentTimeline 以定位分片"})
	}
	return p
}
