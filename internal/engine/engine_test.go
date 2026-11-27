package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"avrepair/internal/fixture"
)

func runCase(t *testing.T, name string) *Report {
	t.Helper()
	sc, err := fixture.Build(name)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(sc, Options{OutDir: t.TempDir(), Rerun: true})
	if err != nil {
		t.Fatalf("case %s pipeline error: %v", name, err)
	}
	return rep
}

func TestCleanCaseMeetsThresholds(t *testing.T) {
	rep := runCase(t, fixture.CaseClean)
	if rep.ExitCode != ExitOK {
		t.Fatalf("clean exit=%d errors=%v", rep.ExitCode, rep.Errors)
	}
	if absF(rep.DriftPct) > MaxDriftPct {
		t.Errorf("drift %.4f%% exceeds %.1f%%", rep.DriftPct, MaxDriftPct)
	}
	if rep.ManifestMaxSkewMS > MaxSkewMS {
		t.Errorf("skew %.4fms exceeds %.1fms", rep.ManifestMaxSkewMS, MaxSkewMS)
	}
	if rep.SeekMiss != 0 {
		t.Errorf("seek miss = %d, must be 0", rep.SeekMiss)
	}
	if rep.OutputSHA256 == "" {
		t.Error("output digest missing")
	}
	if rep.RepairCount != 0 {
		t.Errorf("clean case should need no repairs, got %d", rep.RepairCount)
	}
}

func TestAllCasesRunAndShape(t *testing.T) {
	for _, name := range fixture.KnownCases {
		rep := runCase(t, name)
		// Interrupted runs stop before statistics; all others must report rates.
		if name != fixture.CaseInterrupted {
			if rep.TargetBPS <= 0 || rep.ActualBPS <= 0 {
				t.Errorf("case %s non-positive bitrate", name)
			}
		}
		// Every non-interrupted case must index packets and produce a digest.
		if name != fixture.CaseInterrupted {
			if rep.Indexed == 0 || rep.OutputSHA256 == "" {
				t.Errorf("case %s missing index/digest", name)
			}
			if rep.SeekMiss != 0 {
				t.Errorf("case %s seek miss %d", name, rep.SeekMiss)
			}
		}
	}
}

func TestInterruptedIsUnrecoverable(t *testing.T) {
	rep := runCase(t, fixture.CaseInterrupted)
	if rep.ExitCode != ExitUnrecoverable {
		t.Fatalf("interrupted exit=%d want %d", rep.ExitCode, ExitUnrecoverable)
	}
}

func TestRepairCasesStayWithinQuality(t *testing.T) {
	for _, name := range []string{fixture.CaseRollback, fixture.CaseOverlap,
		fixture.CaseMetaConflict, fixture.CaseTruncated} {
		rep := runCase(t, name)
		if rep.ExitCode != ExitOK {
			t.Errorf("case %s exit=%d, should recover cleanly", name, rep.ExitCode)
		}
		if absF(rep.DriftPct) > MaxDriftPct {
			t.Errorf("case %s drift %.4f%% over threshold", name, rep.DriftPct)
		}
		if rep.ManifestMaxSkewMS > MaxSkewMS {
			t.Errorf("case %s skew %.4fms over threshold", name, rep.ManifestMaxSkewMS)
		}
	}
}

func TestRerunDigestStable(t *testing.T) {
	dir := t.TempDir()
	sc, _ := fixture.Build(fixture.CaseRollback)
	first, err := Run(sc, Options{OutDir: dir, Rerun: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(sc, Options{OutDir: dir, Rerun: true})
	if err != nil {
		t.Fatalf("rerun failed: %v", err)
	}
	if first.OutputSHA256 != second.OutputSHA256 {
		t.Fatal("output digest changed across reruns")
	}
	// Summary file must be valid JSON carrying the contract fields.
	data, err := os.ReadFile(filepath.Join(dir, sc.Name, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"case", "target_bps", "actual_bps", "drift_pct",
		"audio_rms_delta_db", "video_luma_mae", "manifest_max_skew_ms",
		"repair_count", "seek_miss", "output_sha256"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("summary missing contract field %q", key)
		}
	}
}

func TestTruncationRecoversPrefix(t *testing.T) {
	rep := runCase(t, fixture.CaseTruncated)
	if rep.RecoveredNS <= 0 {
		t.Fatal("recovered interval not recorded")
	}
	if rep.Indexed == 0 {
		t.Fatal("verified prefix must still be indexed")
	}
	found := false
	for _, e := range rep.Errors {
		if e.Code == "INPUT_TRUNCATED" {
			found = true
		}
	}
	if !found {
		t.Fatal("truncation defect must propagate as a structured error")
	}
}

func TestRejectsUnsafeOutputDir(t *testing.T) {
	sc, _ := fixture.Build(fixture.CaseClean)
	if _, err := Run(sc, Options{OutDir: ""}); err == nil {
		t.Fatal("empty output dir must be rejected")
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
