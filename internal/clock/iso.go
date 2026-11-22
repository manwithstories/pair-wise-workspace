package clock

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseISODurationTicks parses an ISO-8601 duration such as PT20S,
// PT1M2.5S or P1DT0.5S into exact ticks.
func ParseISODurationTicks(s string, timescale uint32) (int64, error) {
	orig := s
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("clock: invalid ISO duration %q", orig)
	}
	s = s[1:]
	if s == "" {
		return 0, fmt.Errorf("clock: invalid ISO duration %q", orig)
	}
	var total int64
	datePart, timePart := s, ""
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		datePart, timePart = s[:i], s[i+1:]
	}
	parseUnits := func(part string, units []string) error {
		if part == "" {
			return nil
		}
		if !strings.HasSuffix(part, units[len(units)-1]) {
			return errors.New("clock: missing duration unit")
		}
		for _, unit := range units {
			idx := strings.IndexByte(part, unit[0])
			if idx < 0 {
				continue
			}
			num := part[:idx]
			part = part[idx+1:]
			if num == "" || !allDigits(num) {
				return fmt.Errorf("clock: invalid duration component %q", orig)
			}
			n, err := strconv.ParseInt(num, 10, 64)
			if err != nil {
				return fmt.Errorf("clock: duration overflow: %w", err)
			}
			mult := int64(timescale)
			switch unit {
			case "D":
				mult *= 86400
			case "H":
				mult *= 3600
			case "M":
				mult *= 60
			}
			total += n * mult
		}
		if part != "" {
			return fmt.Errorf("clock: invalid duration component %q", orig)
		}
		return nil
	}
	if err := parseUnits(datePart, []string{"D"}); err != nil {
		return 0, err
	}
	if timePart != "" {
		if !strings.HasSuffix(timePart, "S") {
			return 0, fmt.Errorf("clock: invalid duration %q", orig)
		}
		idx := strings.IndexByte(timePart, 'S')
		before := timePart[:idx]
		// Split off the decimal seconds first; H/M components are integers.
		secIdx := strings.LastIndexAny(before, "HMS")
		if secIdx >= 0 {
			prefix, sec := before[:secIdx+1], before[secIdx+1:]
			if err := parseUnits(prefix, []string{"H", "M"}); err != nil {
				return 0, err
			}
			ticks, err := ParseDecimalTicks(sec, timescale)
			if err != nil {
				return 0, fmt.Errorf("clock: invalid duration %q: %w", orig, err)
			}
			total += ticks
		} else {
			ticks, err := ParseDecimalTicks(before, timescale)
			if err != nil {
				return 0, fmt.Errorf("clock: invalid duration %q: %w", orig, err)
			}
			total += ticks
		}
	}
	return total, nil
}

// FormatISODurationTicks renders ticks as the shortest PT#H#M#S form with
// up to three fractional digits. Whole seconds are rendered without a
// fraction (PT2S), matching common DASH manifests.
func FormatISODurationTicks(ticks int64, timescale uint32) string {
	secs := FormatDecimalTicks(ticks, timescale, 3)
	secs = strings.TrimRight(strings.TrimRight(secs, "0"), ".")
	if secs == "" || secs == "-" {
		secs = "0"
	}
	return "PT" + secs + "S"
}
