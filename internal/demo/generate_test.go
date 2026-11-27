package demo

import (
	"os"
	"strings"
	"testing"

	"avcore/internal/clock"
)

func TestParseTimecodeCSV(t *testing.T) {
	csvData := strings.Join([]string{
		"track,rate,mode,value,offset_ms",
		"A,30,normal,00:00:01:00,0",
		"A,30,offset,00:00:02:00,-1500",
		"A,30000/1001,dropframe,00:00:59;29,0",
		"A,30000/1001,wrap,23:59:59;29,40",
	}, "\n") + "\n"
	rows, err := ParseTimecodeCSV(strings.NewReader(csvData))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	// Non-drop 30 fps one second is exactly 90000 ticks.
	if rows[0].BaseTicks != clock.TicksPerSecond {
		t.Errorf("normal row ticks = %d, want %d", rows[0].BaseTicks, clock.TicksPerSecond)
	}
	// Negative offset of 1500 ms moves 45 frames (at 30 fps) earlier.
	wantShifted := rows[1].BaseTicks - clock.Millis(1500)
	if rows[1].ShiftedTicks != wantShifted {
		t.Errorf("negative offset: shifted=%d want=%d", rows[1].ShiftedTicks, wantShifted)
	}
	// The drop-frame row parses to the ST 12-1 real frame 1799 at t=0.
	if rows[2].BaseTicks != 1799*3003 {
		t.Errorf("dropframe ticks = %d, want %d", rows[2].BaseTicks, 1799*3003)
	}
	// The wrap row crosses midnight and lands shortly after 00:00:00.
	if rows[3].ShiftedTicks < 0 || rows[3].ShiftedTicks > clock.TicksPerSecond {
		t.Errorf("day wrap did not land just after midnight: %d", rows[3].ShiftedTicks)
	}
	for i, r := range rows {
		if r.SourceLine != i+2 {
			t.Errorf("row %d source line = %d", i, r.SourceLine)
		}
	}
}

func TestParseTimecodeCSVRejectsBadHeader(t *testing.T) {
	if _, err := ParseTimecodeCSV(strings.NewReader("a,b,c,d,e\n")); err == nil {
		t.Error("bad header must be rejected")
	}
}

func TestParseTimecodeCSVBadRate(t *testing.T) {
	csvData := "track,rate,mode,value,offset_ms\nA,29.97,normal,00:00:01:00,0\n"
	if _, err := ParseTimecodeCSV(strings.NewReader(csvData)); err == nil {
		t.Error("float rate must be rejected")
	}
}

func TestGenerateFixtures(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{
		Dir: dir, Seed: 41, DurationS: 4, SegmentS: 1,
		Rate: clock.Rate30,
	}
	paths, err := Generate(spec)
	if err != nil {
		if strings.Contains(err.Error(), "ffmpeg not found") {
			t.Skip("ffmpeg unavailable")
		}
		t.Fatalf("generate: %v", err)
	}
	for _, p := range []string{paths.Source, paths.SRT, paths.BadSRT, paths.VTT, paths.BadVTT, paths.TimecodeCSV} {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("fixture missing %s: %v", p, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("empty fixture %s", p)
		}
	}
	// Generated CSV must round-trip through the parser.
	f, err := os.Open(paths.TimecodeCSV)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := ParseTimecodeCSV(f)
	if err != nil {
		t.Fatalf("generated csv unparseable: %v", err)
	}
	if len(rows) < 3 {
		t.Errorf("expected wrap/offset rows, got %d", len(rows))
	}
}
