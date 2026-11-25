package repair

import (
	"testing"

	"mediaqa/internal/media"
	mp4scan "mediaqa/internal/mp4"
)

func mk(idx int, dts, pts, dur int64) mp4scan.Sample {
	return mp4scan.Sample{Index: idx, DTS: dts, PTS: pts, Duration: dur}
}

func TestCleanSequenceUnchanged(t *testing.T) {
	in := []mp4scan.Sample{mk(0, 0, 0, 100), mk(1, 100, 100, 100), mk(2, 200, 200, 100)}
	var errs media.List
	out := Stamps(in, 90000, "v1", &errs)
	if len(out) != 3 || errs.Len() != 0 {
		t.Fatalf("out=%d errs=%v", len(out), errs.All())
	}
	for i, s := range out {
		if s.DTS != int64(i*100) {
			t.Fatalf("sample %d dts=%d", i, s.DTS)
		}
	}
}

func TestRegressiveDTSRepaired(t *testing.T) {
	// B-frame style PTS offset must survive the repair.
	in := []mp4scan.Sample{
		mk(0, 0, 200, 100),
		mk(1, 50, 0, 100), // regressive: 50 <= 0? no — 50 > 0, fine
		mk(2, 60, 100, 100),
	}
	in[2].DTS = 50 // regressive: equals previous
	var errs media.List
	out := Stamps(in, 90000, "v1", &errs)
	if len(out) != 3 {
		t.Fatalf("out=%d", len(out))
	}
	if out[2].DTS != 150 {
		t.Fatalf("repaired dts=%d", out[2].DTS)
	}
	if out[2].PTS-out[2].DTS != 50 { // offset 100-50 preserved
		t.Fatalf("composition offset lost: pts=%d dts=%d", out[2].PTS, out[2].DTS)
	}
	if errs.Count(media.SeverityRecoverable) != 1 {
		t.Fatalf("errs=%v", errs.All())
	}
}

func TestMissingDTSInferred(t *testing.T) {
	in := []mp4scan.Sample{mk(0, 0, 0, 100), mk(1, MissingDTS, MissingDTS+100, 100)}
	var errs media.List
	out := Stamps(in, 90000, "v1", &errs)
	if len(out) != 2 || out[1].DTS != 100 {
		t.Fatalf("out=%+v", out)
	}
}

func TestUninferrableGapStopsTrack(t *testing.T) {
	// First sample missing: nothing to infer from -> fatal, empty prefix.
	in := []mp4scan.Sample{mk(0, MissingDTS, 0, 100), mk(1, 100, 100, 100)}
	var errs media.List
	out := Stamps(in, 90000, "v1", &errs)
	if len(out) != 0 || !errs.HasFatal() {
		t.Fatalf("out=%d fatal=%v", len(out), errs.HasFatal())
	}
}

func TestCorruptSamplesSkipped(t *testing.T) {
	in := []mp4scan.Sample{mk(0, 0, 0, 100), {Index: 1, Corrupt: true}, mk(2, 100, 100, 100)}
	var errs media.List
	out := Stamps(in, 90000, "v1", &errs)
	if len(out) != 2 || out[1].DTS != 100 {
		t.Fatalf("out=%+v", out)
	}
}
