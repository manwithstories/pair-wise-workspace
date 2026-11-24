package manifest

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"mediatime/internal/clock"
)

// dashMPD 是 MPD 中本工具关心的最小结构子集。
type dashMPD struct {
	XMLName xml.Name      `xml:"MPD"`
	Periods []dashPeriod  `xml:"Period"`
}

type dashPeriod struct {
	AdaptationSets []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	SegmentTemplates []dashSegmentTemplate `xml:"SegmentTemplate"`
	Representations  []dashRepresentation  `xml:"Representation"`
}

type dashRepresentation struct {
	ID               string                `xml:"id,attr"`
	SegmentTemplates []dashSegmentTemplate `xml:"SegmentTemplate"`
}

type dashSegmentTemplate struct {
	Timescale      uint64             `xml:"timescale,attr"`
	StartNumber    int64              `xml:"startNumber,attr"`
	Media          string             `xml:"media,attr"`
	Initialization string             `xml:"initialization,attr"`
	Timeline       []dashSegmentTL    `xml:"SegmentTimeline"`
}

type dashSegmentTL struct {
	Entries []dashS `xml:"S"`
}

type dashS struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int64 `xml:"r,attr"`
}

// ParseDASH 解析 DASH MPD 中的 SegmentTimeline，展开 r 重复项，
// 按 90 kHz 统一时间基准输出片段描述。
func ParseDASH(data []byte) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, ErrManifestTooLarge
	}
	var mpd dashMPD
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	if err := dec.Decode(&mpd); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if len(mpd.Periods) == 0 || len(mpd.Periods[0].AdaptationSets) == 0 {
		return nil, fmt.Errorf("%w: MPD has no Period/AdaptationSet", ErrInvalidManifest)
	}
	as := mpd.Periods[0].AdaptationSets[0]
	tpl, repID, err := pickTemplate(as)
	if err != nil {
		return nil, err
	}
	timescale := tpl.Timescale
	if timescale == 0 {
		timescale = 1
	}
	if tpl.Media == "" || len(tpl.Timeline) == 0 || len(tpl.Timeline[0].Entries) == 0 {
		return nil, fmt.Errorf("%w: SegmentTemplate lacks media or SegmentTimeline", ErrInvalidManifest)
	}
	startNumber := tpl.StartNumber
	if startNumber == 0 {
		startNumber = 1
	}

	var frags []Fragment
	index := 0
	var cursor int64 // 清单时间轴累计 Tick
	for _, e := range tpl.Timeline[0].Entries {
		repeat := e.R
		if repeat < 0 {
			return nil, fmt.Errorf("%w: negative r attribute", ErrInvalidManifest)
		}
		for k := int64(0); k <= repeat; k++ {
			if e.D <= 0 {
				return nil, fmt.Errorf("%w: non-positive segment duration", ErrInvalidManifest)
			}
			uri := expandTemplate(tpl.Media, repID, startNumber+int64(index), e.T)
			if err := validatePath(uri); err != nil {
				return nil, fmt.Errorf("%w: sequence %d", err, index)
			}
			dur := scaleTo90k(e.D, timescale)
			frags = append(frags, Fragment{
				Sequence:      index,
				URI:           uri,
				DurationTicks: dur,
				StartTicks:    cursor,
			})
			cursor += dur
			index++
			if err := checkFragmentCount(index); err != nil {
				return nil, err
			}
		}
	}
	if len(frags) == 0 {
		return nil, fmt.Errorf("%w: empty SegmentTimeline", ErrInvalidManifest)
	}
	return &Manifest{
		Protocol:       ProtoDASH,
		TargetDuration: frags[0].DurationTicks,
		Fragments:      frags,
		MediaSequence:  int(startNumber),
	}, nil
}

func pickTemplate(as dashAdaptationSet) (dashSegmentTemplate, string, error) {
	if len(as.SegmentTemplates) > 0 {
		return as.SegmentTemplates[0], "", nil
	}
	for _, rep := range as.Representations {
		if len(rep.SegmentTemplates) > 0 {
			return rep.SegmentTemplates[0], rep.ID, nil
		}
	}
	return dashSegmentTemplate{}, "", fmt.Errorf("%w: no SegmentTemplate", ErrInvalidManifest)
}

func scaleTo90k(v int64, timescale uint64) int64 {
	// v * 90000 / timescale，先做乘法并在溢出风险下退化为先除。
	num := v * int64(clock.TickRate)
	if v != 0 && num/v != int64(clock.TickRate) {
		return v / int64(timescale) * int64(clock.TickRate)
	}
	return num / int64(timescale)
}

func expandTemplate(tmpl string, repID string, number int64, t int64) string {
	out := tmpl
	out = strings.ReplaceAll(out, "$RepresentationID$", repID)
	out = strings.ReplaceAll(out, "$Number$", strconv.FormatInt(number, 10))
	out = strings.ReplaceAll(out, "$Time$", strconv.FormatInt(t, 10))
	return out
}
