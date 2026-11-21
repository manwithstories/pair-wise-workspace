package manifest

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"mediascan/internal/model"
)

func firstTemplate(ts []mpdTemplate) *mpdTemplate {
	if len(ts) == 0 {
		return nil
	}
	t := ts[0]
	return &t
}

func parseTimescale(own, parent string) (uint32, error) {
	for _, s := range []string{own, parent} {
		if s == "" {
			continue
		}
		v, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("%w: timescale %q", ErrBadManifest, s)
		}
		if err := model.CheckTimescale(uint32(v)); err != nil {
			return 0, err
		}
		return uint32(v), nil
	}
	return DASHTimescale, nil
}

func expandTemplate(r *Resolver, tpl *mpdTemplate, ts uint32, repID string) ([]Segment, string, model.Rational, error) {
	if tpl.Media == "" {
		return nil, "", model.Rational{}, fmt.Errorf("%w: empty media template", ErrUnsupported)
	}
	var initURI string
	if tpl.Initialization != "" {
		uri := strings.ReplaceAll(tpl.Initialization, "$RepresentationID$", repID)
		if _, err := r.Resolve(uri); err != nil {
			return nil, "", model.Rational{}, err
		}
		initURI = uri
	}
	type tickSeg struct {
		startTicks uint64
		durTicks   uint64
		number     uint64
		timeMode   bool
	}
	var plan []tickSeg
	startNumber := uint64(1)
	if tpl.StartNumber != "" {
		n, err := strconv.ParseUint(tpl.StartNumber, 10, 64)
		if err != nil {
			return nil, "", model.Rational{}, fmt.Errorf("%w: startNumber %q", ErrBadManifest, tpl.StartNumber)
		}
		startNumber = n
	}
	number := startNumber
	timeMode := strings.Contains(tpl.Media, "$Time$")
	if tpl.Timeline != nil {
		var cursor uint64
		for _, s := range tpl.Timeline.S {
			if s.T != "" {
				t, err := strconv.ParseUint(s.T, 10, 64)
				if err != nil {
					return nil, "", model.Rational{}, fmt.Errorf("%w: timeline t=%q", ErrBadManifest, s.T)
				}
				if t > 0 && t < cursor {
					return nil, "", model.Rational{}, fmt.Errorf("%w: timeline goes backwards at t=%d", ErrBadManifest, t)
				}
				cursor = t
			}
			if s.D == "" {
				return nil, "", model.Rational{}, fmt.Errorf("%w: timeline entry without d", ErrBadManifest)
			}
			d, err := strconv.ParseUint(s.D, 10, 64)
			if err != nil || d == 0 {
				return nil, "", model.Rational{}, fmt.Errorf("%w: timeline d=%q", ErrBadManifest, s.D)
			}
			repeat := int64(0)
			if s.R != "" {
				rr, err := strconv.ParseInt(s.R, 10, 64)
				if err != nil || rr < -1 {
					return nil, "", model.Rational{}, fmt.Errorf("%w: timeline r=%q", ErrBadManifest, s.R)
				}
				repeat = rr
			}
			if repeat == -1 {
				return nil, "", model.Rational{}, fmt.Errorf("%w: unbounded repeat", ErrUnsupported)
			}
			if repeat+1 > int64(MaxSegments) {
				return nil, "", model.Rational{}, ErrTooManyItems
			}
			for i := int64(0); i <= repeat; i++ {
				plan = append(plan, tickSeg{startTicks: cursor, durTicks: d, number: number, timeMode: timeMode})
				if cursor > math.MaxUint64-d {
					return nil, "", model.Rational{}, model.ErrTimeOverflow
				}
				cursor += d
				number++
			}
		}
	} else {
		return nil, "", model.Rational{}, fmt.Errorf("%w: template without SegmentTimeline", ErrUnsupported)
	}
	if len(plan) > MaxSegments {
		return nil, "", model.Rational{}, ErrTooManyItems
	}
	segs := make([]Segment, 0, len(plan))
	end := model.Zero
	for _, p := range plan {
		media := strings.ReplaceAll(tpl.Media, "$RepresentationID$", repID)
		media = strings.ReplaceAll(media, "$Number$", strconv.FormatUint(p.number, 10))
		media = strings.ReplaceAll(media, "$Time$", strconv.FormatUint(p.startTicks, 10))
		if _, err := r.Resolve(media); err != nil {
			return nil, "", model.Rational{}, err
		}
		start, err := model.FromTicks(int64(p.startTicks), ts)
		if err != nil {
			return nil, "", model.Rational{}, err
		}
		dur, err := model.FromTicks(int64(p.durTicks), ts)
		if err != nil {
			return nil, "", model.Rational{}, err
		}
		seg, err := r.ValidateSegment(Segment{Name: baseName(media), URI: media, Start: start, Duration: dur})
		if err != nil {
			return nil, "", model.Rational{}, err
		}
		segs = append(segs, seg)
		end, err = start.Add(dur)
		if err != nil {
			return nil, "", model.Rational{}, err
		}
	}
	return segs, initURI, end, nil
}

// parseISO8601Duration converts the MPD duration subset PnDTnHnMnS.
func parseISO8601Duration(s string) (model.Rational, error) {
	if !strings.HasPrefix(s, "P") {
		return model.Rational{}, fmt.Errorf("%w: duration %q", ErrBadManifest, s)
	}
	body := strings.TrimPrefix(s, "P")
	whole, frac, _ := strings.Cut(body, ".")
	number := ""
	scale := int64(1)
	total := model.Zero
	for _, ch := range whole {
		switch {
		case ch >= '0' && ch <= '9':
			number += string(ch)
		case ch == 'T':
			// hours/minutes/seconds come next
		case ch == 'D' || ch == 'H' || ch == 'M' || ch == 'S':
			if number == "" {
				return model.Rational{}, fmt.Errorf("%w: duration %q", ErrBadManifest, s)
			}
			v, err := strconv.ParseInt(number, 10, 64)
			if err != nil {
				return model.Rational{}, fmt.Errorf("%w: duration %q", ErrBadManifest, s)
			}
			switch ch {
			case 'D':
				scale = 86400
			case 'H':
				scale = 3600
			case 'M':
				scale = 60
			case 'S':
				scale = 1
			default:
				return model.Rational{}, fmt.Errorf("%w: duration %q", ErrBadManifest, s)
			}
			part, err := model.FromTicks(v, uint32(scale))
			if err != nil {
				return model.Rational{}, err
			}
			total, err = total.Add(part)
			if err != nil {
				return model.Rational{}, err
			}
			number = ""
		default:
			return model.Rational{}, fmt.Errorf("%w: duration %q", ErrBadManifest, s)
		}
	}
	if frac != "" {
		f, err := model.FromString("0." + frac)
		if err != nil {
			return model.Rational{}, err
		}
		total, err = total.Add(f)
		if err != nil {
			return model.Rational{}, err
		}
	}
	if total.IsZero() {
		return model.Rational{}, fmt.Errorf("%w: empty duration", ErrBadManifest)
	}
	return total, nil
}

func classifyKind(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "video"):
		return "video"
	case strings.Contains(s, "audio"):
		return "audio"
	default:
		return "unknown"
	}
}
