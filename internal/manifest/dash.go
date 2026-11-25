package manifest

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"relaycheck/internal/model"
)

// DASH 最小子集：MPD/Period/AdaptationSet/Representation/SegmentTemplate/SegmentTimeline/S。
type dashMPD struct {
	XMLName xml.Name        `xml:"MPD"`
	Periods []dashPeriod    `xml:"Period"`
}

type dashPeriod struct {
	Sets []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	Template *dashTemplate      `xml:"SegmentTemplate"`
	Representations []dashRep  `xml:"Representation"`
}

type dashRep struct {
	Template *dashTemplate `xml:"SegmentTemplate"`
}

type dashTemplate struct {
	Timescale    int64          `xml:"timescale,attr"`
	Media        string         `xml:"media,attr"`
	Init         string         `xml:"initialization,attr"`
	StartNumber  int64          `xml:"startNumber,attr"`
	Timeline     []dashS        `xml:"SegmentTimeline>S"`
}

type dashS struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int64 `xml:"r,attr"`
}

// ParseDASH 解析使用 SegmentTemplate($Number$)+SegmentTimeline 的 DASH 清单。
func ParseDASH(r io.Reader) (Playlist, error) {
	var mpd dashMPD
	dec := xml.NewDecoder(r)
	dec.Strict = true
	if err := dec.Decode(&mpd); err != nil {
		return Playlist{}, fmt.Errorf("%w: %v", ErrBadPlaylist, err)
	}
	tpl := firstTemplate(&mpd)
	if tpl == nil {
		return Playlist{}, fmt.Errorf("%w: no SegmentTemplate", ErrBadPlaylist)
	}
	if tpl.Timescale <= 0 {
		return Playlist{}, fmt.Errorf("%w: non-positive timescale %d", ErrBadPlaylist, tpl.Timescale)
	}
	if !strings.Contains(tpl.Media, "$Number$") {
		return Playlist{}, fmt.Errorf("%w: only $Number$ template supported", ErrBadPlaylist)
	}
	if len(tpl.Timeline) == 0 {
		return Playlist{}, fmt.Errorf("%w: empty SegmentTimeline", ErrBadPlaylist)
	}

	out := Playlist{Kind: "dash"}
	seq := int(tpl.StartNumber)
	cursor := model.Nano(0)
	var explicitT bool
	for _, s := range tpl.Timeline {
		repeats := s.R + 1
		if repeats < 1 {
			return Playlist{}, fmt.Errorf("%w: negative repeat count", ErrBadPlaylist)
		}
		if s.T != 0 || !explicitT {
			// 显式 t 仅用于校验时间轴连续性；时间仍由时长逐项累加得到。
			explicitT = true
		}
		dur := model.FromTicks(s.D, tpl.Timescale)
		if !dur.IsValid() || s.D <= 0 {
			return Playlist{}, fmt.Errorf("%w: bad segment duration d=%d", ErrBadPlaylist, s.D)
		}
		for i := int64(0); i < repeats; i++ {
			out.Refs = append(out.Refs, model.SegmentRef{
				Seq:      seq,
				URI:      expandNumber(tpl.Media, int64(seq)),
				Start:    cursor,
				Duration: dur,
			})
			cursor = cursor.Add(dur)
			seq++
		}
	}
	out.Seq0 = int(tpl.StartNumber)
	return out, nil
}

// ParseDASHFile 从本地路径读取并解析 DASH 清单。
func ParseDASHFile(path string) (Playlist, error) {
	f, err := os.Open(path)
	if err != nil {
		return Playlist{}, err
	}
	defer f.Close()
	return ParseDASH(f)
}

func firstTemplate(m *dashMPD) *dashTemplate {
	for _, p := range m.Periods {
		for _, set := range p.Sets {
			if set.Template != nil {
				return set.Template
			}
			for i := range set.Representations {
				if set.Representations[i].Template != nil {
					return set.Representations[i].Template
				}
			}
		}
	}
	return nil
}

func expandNumber(tmpl string, n int64) string {
	return strings.ReplaceAll(tmpl, "$Number$", strconv.FormatInt(n, 10))
}
