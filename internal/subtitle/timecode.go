package subtitle

import (
	"fmt"
	"strconv"
	"strings"
)

func ParseSRTTime(value string) (int64, error) {
	return parseArtifactClock(value, 3)
}

func ParseASSTime(value string) (int64, error) {
	return parseArtifactClock(value, 2)
}

func ParseVTTTime(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if strings.Count(value, ":") == 1 {
		value = "00:" + value
	}
	return parseArtifactClock(value, 3)
}

func parseArtifactClock(value string, expectedDigits int) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	i := strings.LastIndexAny(value, ".,")
	fraction := ""
	if i >= 0 {
		fraction = value[i+1:]
		value = value[:i]
		if len(fraction) > expectedDigits {
			return 0, fmt.Errorf("too many fractional digits")
		}
		for len(fraction) < expectedDigits {
			fraction += "0"
		}
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("expected HH:MM:SS")
	}
	hour, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || hour < 0 {
		return 0, fmt.Errorf("invalid hours")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("invalid minutes")
	}
	second, err := strconv.Atoi(parts[2])
	if err != nil || second < 0 || second > 59 {
		return 0, fmt.Errorf("invalid seconds")
	}
	frac := int64(0)
	if fraction != "" {
		frac, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid fractional seconds")
		}
	}
	if expectedDigits == 2 {
		frac *= 10
	}
	ms := (hour*3600+int64(minute)*60+int64(second))*1000 + frac
	if ms > MaxTimelineMS {
		return 0, ErrTimelineOverflow
	}
	return ms, nil
}

var ErrTimelineOverflow = fmt.Errorf("timeline exceeds 99 hours")

func FormatSRTTime(ms int64) string {
	return formatClock(ms, ",", 3)
}

func FormatASSTime(ms int64) string {
	return formatClock(ms, ".", 2)
}

func FormatVTTTime(ms int64) string {
	return formatClock(ms, ".", 3)
}

func formatClock(ms int64, sep string, digits int) string {
	if ms < 0 {
		ms = 0
	}
	if digits == 2 {
		total := (ms + 5) / 10
		h := total / 360000
		m := (total % 360000) / 6000
		s := (total % 6000) / 100
		f := total % 100
		return fmt.Sprintf("%d:%02d:%02d%s%02d", h, m, s, sep, f)
	}
	h := ms / 3600000
	m := (ms % 3600000) / 60000
	s := (ms % 60000) / 1000
	f := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", h, m, s, sep, f)
}
