package manifest

import (
	"encoding/xml"
	"fmt"
	"io/fs"
	"strings"

	"mediad/internal/model"
)

type mpdRoot struct {
	XMLName xml.Name    `xml:"MPD"`
	Periods []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	Sets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	ID       string         `xml:"id,attr"`
	Template mpdSegmentTmpl `xml:"SegmentTemplate"`
}

type mpdSegmentTmpl struct {
	Timescale   int64          `xml:"timescale,attr"`
	Init        string         `xml:"initialization,attr"`
	Media       string         `xml:"media,attr"`
	StartNumber int64          `xml:"startNumber,attr"`
	Timeline    mpdSegmentLine `xml:"SegmentTimeline"`
}

type mpdSegmentLine struct {
	Entries []mpdS `xml:"S"`
}

type mpdS struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R *int64 `xml:"r,attr"`
}

// ParseDASH expands the SegmentTimeline of every AdaptationSet whose id is a
// known TrackRole. Missing media fragments stop expansion at the first gap and
// mark a partial timeline; negative durations and int64 overflow are rejected.
func ParseDASH(fsys fs.FS, mpdPath string) (*model.Timeline, error) {
	data, err := fs.ReadFile(fsys, mpdPath)
	if err != nil {
		return nil, err
	}
	var root mpdRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("dash: invalid MPD: %w", err)
	}
	if len(root.Periods) == 0 {
		return nil, fmt.Errorf("dash: MPD has no Period")
	}
	tl := model.NewTimeline()
	for _, period := range root.Periods {
		for _, set := range period.Sets {
			role := model.TrackRole(set.ID)
			if role != model.TrackStudio && role != model.TrackField && role != model.TrackVoice {
				return nil, fmt.Errorf("dash: AdaptationSet id %q is not a known track role", set.ID)
			}
			if err := expandSet(fsys, tl, role, set.Template); err != nil {
				return nil, err
			}
		}
	}
	return tl, nil
}

func expandSet(fsys fs.FS, tl *model.Timeline, role model.TrackRole, tmpl mpdSegmentTmpl) error {
	if len(tmpl.Timeline.Entries) == 0 {
		return fmt.Errorf("dash: track %s has no SegmentTimeline", role)
	}
	ts := tmpl.Timescale
	if ts <= 0 {
		ts = 1
	}
	startNo := tmpl.StartNumber
	if startNo == 0 {
		startNo = 1
	}
	var (
		cursor     = model.MustRat(0, 1)
		unitCursor int64
		number     = startNo
		idx        int
	)
	for _, s := range tmpl.Timeline.Entries {
		if s.D <= 0 {
			return fmt.Errorf("%w: dash S d=%d", model.ErrNegativeDuration, s.D)
		}
		if s.T != nil {
			if *s.T < 0 {
				return fmt.Errorf("%w: dash S t=%d", model.ErrNegativeDuration, *s.T)
			}
			unitCursor = *s.T
			r, err := model.Seconds(*s.T, ts)
			if err != nil {
				return err
			}
			cursor = r
		}
		repeats := int64(0)
		if s.R != nil {
			if *s.R < 0 {
				return fmt.Errorf("%w: dash S r=%d", model.ErrNegativeDuration, *s.R)
			}
			repeats = *s.R
		}
		dur, err := model.Seconds(s.D, ts)
		if err != nil {
			return err
		}
		for k := int64(0); k <= repeats; k++ {
			uri := expandTemplate(tmpl.Media, number, unitCursor)
			ok, err := commitDASHEntry(fsys, tl, role, idx, uri, cursor, dur, ts)
			if err != nil {
				return err
			}
			if !ok {
				tl.MarkPartial(model.MissingDependency{
					Track: role, Kind: model.KindDASH, Index: idx, URI: uri,
					Reason: "segment file not found",
				})
				return nil
			}
			next, err := cursor.Add(dur)
			if err != nil {
				return err
			}
			cursor = next
			unitCursor, err = addCheck(unitCursor, s.D)
			if err != nil {
				return err
			}
			number++
			idx++
		}
	}
	return nil
}

func commitDASHEntry(fsys fs.FS, tl *model.Timeline, role model.TrackRole, idx int,
	uri string, start, dur model.Rat, ts int64) (bool, error) {
	info, err := fs.Stat(fsys, uri)
	if err != nil {
		return false, nil
	}
	seg := model.SegmentRef{
		Track: role, Kind: model.KindDASH, Index: idx, URI: uri,
		Start: start, Duration: dur, ByteSize: info.Size(),
		Timescale: ts, Sync: true,
	}
	if err := tl.Add(seg); err != nil {
		return false, err
	}
	return true, nil
}

// expandTemplate substitutes $Number$ and $Time$; other $$ style tokens are
// left untouched as they do not occur in the generated fixtures.
func expandTemplate(pattern string, number, time int64) string {
	r := strings.NewReplacer("$Number$", fmt.Sprintf("%d", number), "$Time$", fmt.Sprintf("%d", time))
	return r.Replace(pattern)
}
