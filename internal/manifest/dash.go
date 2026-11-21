package manifest

import (
	"encoding/xml"
	"fmt"
	"io"

	"mediaroute/internal/model"
)

// DASH MPD structures (SegmentList form).
type mpd struct {
	XMLName xml.Name `xml:"MPD"`
	Period  struct {
		AdaptationSet struct {
			SegmentList *segmentList `xml:"SegmentList"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

type segmentList struct {
	Timescale   int64 `xml:"timescale,attr"`
	Duration    int64 `xml:"duration,attr"`
	SegmentURLs []struct {
		Media string `xml:"media,attr"`
	} `xml:"SegmentURL"`
}

const dashPath = "/MPD/Period/AdaptationSet/SegmentList"

// ParseDASH parses a DASH MPD using a SegmentList. A missing or
// non-positive segment duration is rejected with the XML path.
func ParseDASH(r io.Reader, track string) ([]model.Segment, error) {
	var m mpd
	if err := xml.NewDecoder(r).Decode(&m); err != nil {
		return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Location: "/MPD", Msg: "XML parse", Cause: err}
	}
	sl := m.Period.AdaptationSet.SegmentList
	if sl == nil {
		return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Location: dashPath, Msg: "missing SegmentList"}
	}
	if sl.Duration <= 0 {
		return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Location: dashPath + "/@duration",
			Msg:    "missing or non-positive segment duration"}
	}
	ts := sl.Timescale
	if ts <= 0 {
		ts = 90000
	}
	if len(sl.SegmentURLs) == 0 {
		return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
			Track: track, Location: dashPath, Msg: "no SegmentURL entries"}
	}
	segs := make([]model.Segment, 0, len(sl.SegmentURLs))
	seen := map[string]int{}
	var start int64
	for i, su := range sl.SegmentURLs {
		if su.Media == "" {
			return nil, &model.MediaError{Code: model.ErrManifest, Module: "manifest",
				Track: track, Location: fmt.Sprintf("%s/SegmentURL[%d]/@media", dashPath, i),
				Msg:    "empty media URI"}
		}
		if prev, dup := seen[su.Media]; dup {
			return nil, &model.MediaError{Code: model.ErrDuplicate, Module: "manifest",
				Track: track, Seq: int64(i),
				Location: fmt.Sprintf("%s/SegmentURL[%d]", dashPath, i),
				Msg:      fmt.Sprintf("segment %d duplicates URI of segment %d", i, prev)}
		}
		seen[su.Media] = i
		segs = append(segs, model.Segment{Index: i, Track: track,
			Start: start, Duration: sl.Duration, URI: su.Media})
		start += sl.Duration
	}
	_ = ts
	return segs, nil
}

// Dependencies returns the linear dependency chain of segment indexes:
// every segment depends on its predecessor on the same track.
func Dependencies(segs []model.Segment) [][2]int {
	var deps [][2]int
	for i := 1; i < len(segs); i++ {
		deps = append(deps, [2]int{segs[i].Index, segs[i-1].Index})
	}
	return deps
}
