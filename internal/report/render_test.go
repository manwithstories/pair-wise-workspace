package report_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"loudness/internal/loudness"
	"loudness/internal/report"
)

func TestMeasureJSONFieldOrderAndPrecision(t *testing.T) {
	m := loudness.Metrics{
		IntegratedLUFS: -23.02, ShortTermMaxLUFS: -22.90,
		MomentaryMaxLUFS: -22.80, TruePeakDBTP: -20.01, GatedBlocks: 117,
	}
	row := report.Judge("x.wav", m, -23, -1, report.DefaultToleranceLU)
	var buf bytes.Buffer
	if err := report.RenderMeasure(&buf, row, true); err != nil {
		t.Fatal(err)
	}
	want := `{"integrated_lufs":-23.02,"short_term_max_lufs":-22.90,"momentary_max_lufs":-22.80,"true_peak_dbtp":-20.01,"gated_blocks":117,"verdict":"PASS"}` + "\n"
	if buf.String() != want {
		t.Fatalf("json = %q, want %q", buf.String(), want)
	}
}

func TestMeasureHumanFailure(t *testing.T) {
	m := loudness.Metrics{IntegratedLUFS: -35, TruePeakDBTP: -30, GatedBlocks: 100}
	row := report.Judge("quiet.wav", m, -23, -1, report.DefaultToleranceLU)
	var buf bytes.Buffer
	if err := report.RenderMeasure(&buf, row, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "verdict=FAIL\n") || !strings.Contains(buf.String(), "reason=below_target\n") {
		t.Fatalf("output = %s", buf.String())
	}
}

func TestInfiniteIntegratedBecomesNullInJSON(t *testing.T) {
	m := loudness.Metrics{
		IntegratedLUFS: math.Inf(-1), ShortTermMaxLUFS: math.Inf(-1),
		MomentaryMaxLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1), Reason: loudness.ReasonSilent,
	}
	row := report.Judge("silent.wav", m, -23, -1, report.DefaultToleranceLU)
	var buf bytes.Buffer
	if err := report.RenderMeasure(&buf, row, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"integrated_lufs":null`) || !strings.Contains(buf.String(), `"reason":"silent"`) {
		t.Fatalf("output = %s", buf.String())
	}
}

func TestJudgeReasons(t *testing.T) {
	base := loudness.Metrics{IntegratedLUFS: -23, TruePeakDBTP: -5}
	if row := report.Judge("x", base, -23, -1, 0.5); row.Verdict != report.VerdictPass {
		t.Fatalf("base verdict = %+v", row)
	}
	peak := base
	peak.TruePeakDBTP = -0.5
	if row := report.Judge("x", peak, -23, -1, 0.5); row.Reason != report.ReasonTruePeakExceeded {
		t.Fatalf("peak reason = %+v", row)
	}
	loud := base
	loud.IntegratedLUFS = -22
	if row := report.Judge("x", loud, -23, -1, 0.5); row.Reason != report.ReasonAboveTarget {
		t.Fatalf("loud reason = %+v", row)
	}
	withNaN := base
	withNaN.NaNs = 1
	if row := report.Judge("x", withNaN, -23, -1, 0.5); row.Reason != report.ReasonNonFiniteSamples {
		t.Fatalf("nan reason = %+v", row)
	}
}

func TestBatchJSON(t *testing.T) {
	m := loudness.Metrics{IntegratedLUFS: -23, ShortTermMaxLUFS: -23, MomentaryMaxLUFS: -23, TruePeakDBTP: -2}
	rows := []report.Row{report.Judge("a.wav", m, -23, -1, 0.5)}
	summary := report.Summary{Total: 2, Pass: 1, Error: 1}
	var buf bytes.Buffer
	if err := report.RenderBatch(&buf, rows, summary, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"summary":{"total":2,"pass":1,"fail":0,"error":1}`) {
		t.Fatalf("batch json = %s", buf.String())
	}
}
