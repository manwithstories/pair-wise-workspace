package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"aircheck/internal/media"
)

// ParseISODuration parses an xs:duration into an exact Duration at 1/1000 s
// resolution, e.g. "PT10S" or "PT10.000S".
func ParseISODuration(s string) (media.Duration, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "P") {
		return media.Duration{}, fmt.Errorf("bad duration %q", s)
	}
	clock := media.MustClock(1000)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	var total int64
	inTime := false
	seenComponent := false
	num := ""
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 'T':
			inTime = true
			continue
		case c == 'P':
			continue
		}
		if c == '.' {
			return media.Duration{}, fmt.Errorf("fractional xs:duration %q unsupported", s)
		}
		if c >= '0' && c <= '9' {
			num += string(c)
			continue
		}
		if num == "" {
			return media.Duration{}, fmt.Errorf("bad duration %q", s)
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return media.Duration{}, err
		}
		seenComponent = true
		switch c {
		case 'D':
			total += v * 86400
		case 'H':
			if !inTime {
				return media.Duration{}, fmt.Errorf("bad duration %q", s)
			}
			total += v * 3600
		case 'M':
			if !inTime {
				return media.Duration{}, fmt.Errorf("bad duration %q", s)
			}
			total += v * 60
		case 'S':
			if !inTime {
				return media.Duration{}, fmt.Errorf("bad duration %q", s)
			}
			total += v
		case 'Y', 'W':
			return media.Duration{}, fmt.Errorf("calendar duration %q unsupported", s)
		default:
			return media.Duration{}, fmt.Errorf("bad duration %q", s)
		}
		num = ""
	}
	if num != "" {
		return media.Duration{}, fmt.Errorf("trailing digits in %q", s)
	}
	if !seenComponent {
		return media.Duration{}, fmt.Errorf("duration %q has no component", s)
	}
	if neg {
		total = -total
	}
	return clock.FromSeconds(total), nil
}

// FormatISODuration renders a span as the xs:duration form used in fixtures.
func FormatISODuration(d media.Duration) string {
	ms := int64(d.Milliseconds())
	sec := ms / 1000
	if ms%1000 == 0 {
		return fmt.Sprintf("PT%dS", sec)
	}
	return fmt.Sprintf("PT%d.%03dS", sec, ms%1000)
}

// TotalDuration sums all tracks' durations.
func (d *Dash) TotalDuration() media.Duration {
	if len(d.Tracks) == 0 {
		return media.Duration{}
	}
	return d.Tracks[0].Duration
}

// SelectTracks chooses the highest-bandwidth video and audio for the target.
func (d *Dash) SelectTracks(targetBps int64) (video, audio *DashTrack) {
	for i := range d.Tracks {
		t := &d.Tracks[i]
		if t.Kind == media.KindVideo {
			if video == nil || closer(t.Bandwidth, targetBps) < closer(video.Bandwidth, targetBps) {
				video = t
			}
		} else if t.Kind == media.KindAudio {
			if audio == nil || t.Bandwidth > audio.Bandwidth {
				audio = t
			}
		}
	}
	return
}

func closer(a, target int64) int64 {
	d := a - target
	if d < 0 {
		return -d
	}
	return d
}
