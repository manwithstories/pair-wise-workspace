package pipeline

import (
	"errors"
	"testing"
)

func TestCleanReportMeetsDeliveryMetrics(t *testing.T) {
	rep, err := Run(Config{Case: "clean", Seed: 17})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RoundtripPercent != 100 || rep.CuesOK != 48 || rep.CuesTotal != 48 {
		t.Fatalf("subtitle metrics wrong: %+v", rep)
	}
	if rep.KeyframesFound != 12 || rep.KeyframesExpected != 12 {
		t.Fatalf("keyframe metrics wrong: %d/%d", rep.KeyframesFound, rep.KeyframesExpected)
	}
	if rep.Duplicates != 0 || rep.Reordered != 0 || rep.BurstLost != 0 || rep.DamagedSegments != 0 {
		t.Fatalf("clean case has impairments: %+v", rep)
	}
	if len(rep.SegmentSHA256) != 64 {
		t.Fatalf("bad segment hash %q", rep.SegmentSHA256)
	}
}

func TestDamagedReportHasExactFaultCountsAndPropagation(t *testing.T) {
	rep, err := Run(Config{Case: "damaged", Seed: 17})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Duplicates != 6 || rep.Reordered != 9 || rep.BurstLost != 4 {
		t.Fatalf("transport counts wrong: dup=%d reorder=%d burst=%d", rep.Duplicates, rep.Reordered, rep.BurstLost)
	}
	if rep.BadCRC != 1 || rep.DamagedSegments != 1 {
		t.Fatalf("CRC/damage wrong: crc=%d damaged=%d", rep.BadCRC, rep.DamagedSegments)
	}
	if rep.ConflictCount == 0 || rep.HashOK {
		t.Fatalf("fault did not propagate to boundary: conflicts=%d hashOK=%v", rep.ConflictCount, rep.HashOK)
	}
	if !rep.Monotonic {
		t.Fatal("delivered prefix should remain monotonic")
	}
	strict := *rep
	strict.Strict = true
	if strict.ExitCode() != 3 {
		t.Fatal("strict mode should map bad segment to exit 3")
	}
}

func TestRecoverReportAndStableRerunHashes(t *testing.T) {
	first, err := Run(Config{Case: "recover", Seed: 17})
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicates != 6 || first.Reordered != 9 || first.BurstLost != 4 {
		t.Fatalf("impairment counts changed: %+v", first)
	}
	if first.Recovered != 4 || first.DamagedSegments != 0 || !first.Monotonic {
		t.Fatalf("recovery wrong: recovered=%d damaged=%d monotonic=%v",
			first.Recovered, first.DamagedSegments, first.Monotonic)
	}
	second, err := Run(Config{Case: "recover", Seed: 17})
	if err != nil {
		t.Fatal(err)
	}
	if first.SegmentSHA256 != second.SegmentSHA256 {
		t.Fatal("primary segment hash differs across reruns")
	}
	if len(first.SegmentHashes) != 12 {
		t.Fatalf("expected 12 per-segment hashes, got %d", len(first.SegmentHashes))
	}
	for i := range first.SegmentHashes {
		if first.SegmentHashes[i].SegmentSHA256 != second.SegmentHashes[i].SegmentSHA256 {
			t.Fatalf("segment %d hash differs across reruns", i)
		}
	}
}

func TestAtomicWorkdirResumeAndIOError(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Case: "recover", Seed: 17, WorkDir: dir}
	first, err := Run(cfg)
	if err != nil || first.IndexResumed {
		t.Fatalf("first run should build index: resumed=%v err=%v", first.IndexResumed, err)
	}
	second, err := Run(cfg)
	if err != nil || !second.IndexResumed {
		t.Fatalf("second run should resume valid index: resumed=%v err=%v", second.IndexResumed, err)
	}

	bad := Config{Case: "clean", Seed: 17, WorkDir: "/dev/null/medcheck-impossible"}
	if _, err := Run(bad); err == nil {
		t.Fatal("expected local I/O error")
	} else {
		var ioErr *IOError
		if !errors.As(err, &ioErr) {
			t.Fatalf("expected IOError, got %T: %v", err, err)
		}
	}
}
