package demo

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"avcore/internal/clock"
)

// TimecodeRow is one parsed CSV time-code event on the shared 90 kHz base.
type TimecodeRow struct {
	Track       string
	Rate        clock.Rate
	Mode        string
	RawValue    string
	OffsetMS    int64
	BaseTicks   int64 // time code converted to ticks
	ShiftedTicks int64 // base plus the signed offset, wrap applied
	SourceLine  int
}

// ParseTimecodeCSV reads the documented schema
// (track,rate,mode,value,offset_ms). Rates use the exact spellings accepted
// by clock.ParseRate; drop-frame values use ';' markers. Negative offsets and
// day-boundary wrap are applied through the integer time-code machinery.
func ParseTimecodeCSV(r io.Reader) ([]TimecodeRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 5
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("demo: cannot read csv header: %w", err)
	}
	want := []string{"track", "rate", "mode", "value", "offset_ms"}
	for i, h := range header {
		if strings.TrimSpace(strings.ToLower(h)) != want[i] {
			return nil, fmt.Errorf("demo: unexpected csv header %v, want %v", header, want)
		}
	}
	var rows []TimecodeRow
	line := 1
	for {
		rec, rerr := cr.Read()
		line++
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("demo: csv line %d: %w", line, rerr)
		}
		rate, perr := clock.ParseRate(strings.TrimSpace(rec[1]))
		if perr != nil {
			return nil, fmt.Errorf("demo: csv line %d: %w", line, perr)
		}
		tc, terr := clock.ParseTimecode(strings.TrimSpace(rec[3]), rate)
		if terr != nil {
			return nil, fmt.Errorf("demo: csv line %d: %w", line, terr)
		}
		offset, oerr := strconv.ParseInt(strings.TrimSpace(rec[4]), 10, 64)
		if oerr != nil {
			return nil, fmt.Errorf("demo: csv line %d bad offset_ms: %w", line, oerr)
		}
		baseTicks, _ := tc.Ticks()
		offsetFrames := clock.Rate(rate).TicksToFrame(clock.Millis(offset))
		shifted, serr := tc.Shift(offsetFrames)
		if serr != nil {
			return nil, fmt.Errorf("demo: csv line %d shift: %w", line, serr)
		}
		shiftedTicks, _ := shifted.Ticks()
		rows = append(rows, TimecodeRow{
			Track: rec[0], Rate: rate, Mode: rec[2], RawValue: rec[3],
			OffsetMS: offset, BaseTicks: baseTicks, ShiftedTicks: shiftedTicks,
			SourceLine: line,
		})
	}
	return rows, nil
}
