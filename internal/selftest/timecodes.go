package selftest

import (
	"fmt"
	"os"

	"avcore/internal/clock"
	"avcore/internal/demo"
)

// checkTimecodes parses the generated CSV and asserts every documented
// conversion path: a normal address, a negative offset moving earlier in
// time, a drop-frame address and a day-boundary wrap landing just after
// midnight. Each becomes a named Check in the report.
func checkTimecodes(csvPath string) Check {
	f, err := os.Open(csvPath)
	if err != nil {
		return Check{Name: "timecode_csv", OK: false, Detail: err.Error()}
	}
	defer f.Close()
	rows, err := demo.ParseTimecodeCSV(f)
	if err != nil {
		return Check{Name: "timecode_csv", OK: false, Detail: err.Error()}
	}

	var details []string
	ok := true
	sawOffset, sawDrop, sawWrap := false, false, false
	for _, row := range rows {
		switch row.Mode {
		case "normal":
			if row.BaseTicks <= 0 {
				ok = false
				details = append(details, "normal row not in future")
			}
		case "offset":
			sawOffset = true
			if row.OffsetMS >= 0 || row.ShiftedTicks >= row.BaseTicks {
				ok = false
				details = append(details, "negative offset did not move earlier")
			}
		case "dropframe":
			sawDrop = true
			// ST 12-1 frame 1799 at 29.97 must match the integer conversion.
			want := int64(1799) * clock.Rate2997.FrameDuration(1)
			if row.Rate != clock.Rate2997 || row.BaseTicks != want {
				ok = false
				details = append(details, fmt.Sprintf("dropframe ticks=%d want=%d", row.BaseTicks, want))
			}
		case "wrap":
			sawWrap = true
			// After wrapping across midnight the result is inside the first
			// second of the new day, never a negative or multi-second value.
			if row.ShiftedTicks < 0 || row.ShiftedTicks > clock.TicksPerSecond {
				ok = false
				details = append(details, fmt.Sprintf("wrap landed at %d", row.ShiftedTicks))
			}
		}
	}
	if !(sawOffset && sawDrop && sawWrap) {
		ok = false
		details = append(details, fmt.Sprintf(
			"missing mode coverage offset=%v drop=%v wrap=%v", sawOffset, sawDrop, sawWrap))
	}
	detail := fmt.Sprintf("rows=%d", len(rows))
	if len(details) > 0 {
		detail = joinDetails(details)
	}
	return Check{Name: "timecode_wrap_offset_dropframe", OK: ok, Detail: detail}
}

func joinDetails(d []string) string {
	out := ""
	for i, s := range d {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
