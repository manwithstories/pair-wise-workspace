package selftest

import (
	"bytes"
	"strings"
	"testing"
)

func TestThresholdChecks(t *testing.T) {
	good := []Row{{
		Case: "normal", TargetBPS: 450000, ActualBPS: 460000,
		BitrateDeltaPct: 2.2, PSNRMeanDB: 35, SSIMMean: 0.95, MaxAlignmentMS: 10,
	}}
	checks := thresholdChecks("normal", good)
	for _, c := range checks {
		if !c.OK {
			t.Errorf("good row failed %s: %s", c.Name, c.Detail)
		}
	}
	bad := []Row{{
		Case: "normal", TargetBPS: 450000, ActualBPS: 600000,
		BitrateDeltaPct: 33.3, PSNRMeanDB: 20, SSIMMean: 0.80, MaxAlignmentMS: 40,
	}}
	failed := thresholdChecks("normal", bad)
	failCount := 0
	for _, c := range failed {
		if !c.OK {
			failCount++
		}
	}
	if failCount != 4 {
		t.Errorf("bad row should fail all 4 gates, got %d", failCount)
	}
}

func TestReportPrintContainsColumns(t *testing.T) {
	rep := &Report{
		Rows: []Row{{Case: "normal", TargetBPS: 450000, ActualBPS: 449000,
			BitrateDeltaPct: -0.2, PSNRMeanDB: 46, SSIMMean: 0.99, MaxAlignmentMS: 0,
			ManifestSHAEqual: true}},
		Checks: []Check{{Name: "timecode_wrap_offset_dropframe", OK: true, Detail: "rows=4"}},
	}
	var buf bytes.Buffer
	Print(&buf, rep, true)
	out := buf.String()
	for _, col := range []string{
		"case", "target_bps", "actual_bps", "bitrate_delta_pct", "psnr_mean_db",
		"ssim_mean", "max_alignment_ms", "bad_records", "reused_segments",
		"manifest_sha256_equal",
	} {
		if !strings.Contains(out, col) {
			t.Errorf("report missing column %s", col)
		}
	}
	if !strings.Contains(out, "RESULT: PASS") || !strings.Contains(out, "[PASS]") {
		t.Errorf("pass verdict missing:\n%s", out)
	}
}

func TestRowsForAndAppend(t *testing.T) {
	rep := &Report{}
	rep.Rows = append(rep.Rows, Row{Case: "normal"}, Row{Case: "other"})
	if got := rep.rowsFor("normal"); len(got) != 1 {
		t.Errorf("rowsFor filtered %d, want 1", len(got))
	}
	if got := rep.rowsFor("missing"); len(got) != 0 {
		t.Errorf("missing case should be empty, got %d", len(got))
	}
}

func TestJoinDetails(t *testing.T) {
	if got := joinDetails([]string{"a", "b", "c"}); got != "a; b; c" {
		t.Errorf("joinDetails = %q", got)
	}
}

func TestAbs(t *testing.T) {
	if abs(-3.5) != 3.5 || abs(2.0) != 2.0 {
		t.Error("abs helper wrong")
	}
}
