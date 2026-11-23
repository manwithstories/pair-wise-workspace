package manifest

import (
	"encoding/xml"
	"fmt"
	"io"

	"medrelay/internal/timeline"
)

// DASHTimeline is the parsed representation layer of one Representation:
// its timescale plus the expanded segment list.
type DASHTimeline struct {
	RepresentationID string
	Timescale        int64
	Segments         []Segment
}

type mpdXML struct {
	Periods []struct {
		Sets []struct {
			Reps []struct {
				ID      string `xml:"id,attr"`
				Template struct {
					Timescale  int64  `xml:"timescale,attr"`
					Media      string `xml:"media,attr"`
					StartNum   uint64 `xml:"startNumber,attr"`
					Timeline   struct {
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

// ParseDASH parses a simplified MPD and expands each Representation's
// SegmentTimeline. It rejects missing media templates, non-positive
// durations, overlapping/duplicate timeline entries and out-of-range
// timestamps, reporting the first anomaly located by representation.
func ParseDASH(r io.Reader) ([]DASHTimeline, error) {
	var doc mpdXML
	dec := xml.NewDecoder(io.LimitReader(r, 4<<20))
	if err := dec.Decode(&doc); err != nil {
		return nil, timeline.New(timeline.ErrMissingRef, "", 0, 0, "MPD parse: "+err.Error())
	}
	var out []DASHTimeline
	for _, p := range doc.Periods {
		for _, as := range p.Sets {
			for _, rep := range as.Reps {
				tl, err := expandRep(rep.ID, &rep.Template)
				if err != nil {
					return nil, err
				}
				out = append(out, tl)
			}
		}
	}
	if len(out) == 0 {
		return nil, timeline.New(timeline.ErrMissingRef, "", 0, 0, "MPD contains no Representation")
	}
	return out, nil
}

type repTemplate = struct {
	Timescale int64  `xml:"timescale,attr"`
	Media     string `xml:"media,attr"`
	StartNum  uint64 `xml:"startNumber,attr"`
	Timeline  struct {
		S []struct {
			T *int64 `xml:"t,attr"`
			D int64  `xml:"d,attr"`
			R int    `xml:"r,attr"`
		} `xml:"S"`
	} `xml:"SegmentTimeline"`
}

func expandRep(id string, tpl *repTemplate) (DASHTimeline, error) {
	tl := DASHTimeline{RepresentationID: id, Timescale: tpl.Timescale}
	if tpl.Media == "" {
		return tl, timeline.New(timeline.ErrMissingRef, id, 0, 0, "Representation missing SegmentTemplate@media")
	}
	if tpl.Timescale <= 0 {
		return tl, timeline.New(timeline.ErrInvalidDur, id, 0, 0, "SegmentTemplate timescale must be positive")
	}
	if len(tpl.Timeline.S) == 0 {
		return tl, timeline.New(timeline.ErrMissingRef, id, 0, 0, "Representation has empty SegmentTimeline")
	}
	seq := tpl.StartNum
	var nextT int64
	first := true
	for i, s := range tpl.Timeline.S {
		if s.D <= 0 {
			return tl, timeline.New(timeline.ErrInvalidDur, id, 0, 0,
				fmt.Sprintf("S[%d]: non-positive duration %d", i, s.D))
		}
		t := nextT
		if s.T != nil {
			t = *s.T
		}
		if first {
			nextT = t
		}
		if t != nextT {
			return tl, timeline.NewSeq(timeline.ErrDuplicateSeq, id, seq, 0, 0,
				fmt.Sprintf("S[%d]: timeline overlap/gap, t=%d want %d", i, t, nextT))
		}
		first = false
		count := 1
		if s.R > 0 {
			count += s.R
		}
		for k := 0; k < count; k++ {
			startMs := timeline.ToMillis(t, timeline.Timescale(tpl.Timescale))
			endMs := timeline.ToMillis(t+s.D, timeline.Timescale(tpl.Timescale))
			if endMs > 24*3600*1000 {
				return tl, timeline.NewSeq(timeline.ErrTimestampRange, id, seq, startMs, endMs,
					"segment timestamp exceeds 24h")
			}
			uri := replaceNumber(tpl.Media, seq)
			tl.Segments = append(tl.Segments, Segment{
				URI:      uri,
				Seq:      seq,
				Duration: float64(s.D) / float64(tpl.Timescale),
				StartMs:  startMs,
			})
			seq++
			t += s.D
			if len(tl.Segments) > MaxSegments {
				return tl, timeline.New(timeline.ErrLimit, id, 0, 0, "too many segments")
			}
		}
		nextT = t
	}
	return tl, nil
}

func replaceNumber(media string, n uint64) string {
	out := []byte(media)
	for i := 0; i+7 < len(out); i++ {
		if string(out[i:i+8]) == "$Number$" {
			return string(out[:i]) + fmt.Sprintf("%d", n) + string(out[i+8:])
		}
	}
	return media
}
