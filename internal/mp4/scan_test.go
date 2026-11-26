package mp4_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
	"mediaroute/internal/model"
	"mediaroute/internal/mp4"
	"mediaroute/internal/repair"
)

func generateSmall(t *testing.T) *fixture.Generated {
	t.Helper()
	g, err := fixture.Generate(fixture.Spec{
		Seed: 7, Minutes: 6, Cameras: 4,
		Bitrates: []uint32{100, 200, 400},
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return g
}

func hasFault(track *model.Track, code model.FaultCode) bool {
	for _, f := range track.Faults {
		if f.Code == code {
			return true
		}
	}
	return false
}

func repairOne(t *testing.T, path string) *model.Track {
	t.Helper()
	rt, err := mp4.ScanFile(path)
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	tr, err := repair.RepairTrack(rt, model.RepresentationID{Camera: "c", Bitrate: 1})
	if err != nil {
		t.Fatalf("repair %s: %v", path, err)
	}
	return tr
}

func TestScanNormalTrack(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam0_100.mp4"))
	if tr.Timescale != 90000 {
		t.Fatalf("timescale=%d want 90000", tr.Timescale)
	}
	if len(tr.Samples) != 90 { // 6 min / 4 s
		t.Fatalf("samples=%d want 90", len(tr.Samples))
	}
	for i := 1; i < len(tr.Samples); i++ {
		if tr.Samples[i].DTS <= tr.Samples[i-1].DTS {
			t.Fatalf("DTS not monotonic at %d", i)
		}
	}
	// keyframes every 60 s on the shared clock
	ix := index.Build(tr)
	if len(ix.Entries) != 6 {
		t.Fatalf("index entries=%d want 6", len(ix.Entries))
	}
	e, ok := ix.Lookup(120 * 90000)
	if !ok || e.Clock != 120*90000 {
		t.Fatalf("lookup(120s)=%+v,%v", e, ok)
	}
	if len(tr.Faults) != 0 {
		t.Fatalf("unexpected faults: %+v", tr.Faults)
	}
}

func TestScanBoxOverflow(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam1_100.mp4"))
	if !hasFault(tr, model.FaultBoxOverflow) {
		t.Fatal("box overflow fault not detected")
	}
	if len(tr.Samples) != 90 { // sample data intact
		t.Fatalf("samples=%d want 90", len(tr.Samples))
	}
}

func TestScanTruncatedMdat(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam1_200.mp4"))
	if !hasFault(tr, model.FaultTruncatedMdat) {
		t.Fatal("truncated mdat fault not detected")
	}
	if len(tr.Gaps) != 1 || tr.Gaps[0].Bytes == 0 {
		t.Fatalf("gaps=%+v want one non-empty gap", tr.Gaps)
	}
	st, _ := os.Stat(filepath.Join(g.BaseDir, "media/cam1_200.mp4"))
	if tr.Gaps[0].Offset != uint64(st.Size()) {
		t.Fatalf("gap offset=%d want file size %d", tr.Gaps[0].Offset, st.Size())
	}
	trunc := 0
	for _, s := range tr.Samples {
		if s.Truncated {
			trunc++
		}
	}
	if trunc == 0 || trunc >= len(tr.Samples) {
		t.Fatalf("truncated=%d of %d", trunc, len(tr.Samples))
	}
	// index must not point at truncated samples
	for _, e := range index.Build(tr).Entries {
		if tr.Samples[e.Sample].Truncated {
			t.Fatal("index points at truncated sample")
		}
	}
}

func TestRepairDTSRegression(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam2_100.mp4"))
	if !hasFault(tr, model.FaultDTSRegression) {
		t.Fatal("dts regression fault not detected")
	}
	for i := 1; i < len(tr.Samples); i++ {
		if tr.Samples[i].DTS <= tr.Samples[i-1].DTS {
			t.Fatalf("DTS still not monotonic at %d", i)
		}
	}
}

func TestRepairElstOutOfRange(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam2_200.mp4"))
	if !hasFault(tr, model.FaultElstRange) {
		t.Fatal("elst out-of-range fault not detected")
	}
	if tr.Samples[0].Clock != 0 { // edit ignored: presentation starts at 0
		t.Fatalf("first clock=%d want 0", tr.Samples[0].Clock)
	}
}

func TestRepairMissingKeyframes(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam3_100.mp4"))
	if !hasFault(tr, model.FaultNoSync) {
		t.Fatal("missing keyframes fault not detected")
	}
	if len(index.Build(tr).Entries) == 0 {
		t.Fatal("index empty after sync fallback")
	}
}

func TestRepairNonZeroStart(t *testing.T) {
	g := generateSmall(t)
	tr := repairOne(t, filepath.Join(g.BaseDir, "media/cam3_200.mp4"))
	if hasFault(tr, model.FaultElstRange) {
		t.Fatal("valid non-zero start must not be a fault")
	}
	if tr.Samples[0].Clock != 0 {
		t.Fatalf("first clock=%d want 0 after rebase", tr.Samples[0].Clock)
	}
	if len(tr.Samples) != 88 { // 90 - 2 dropped before the edit window
		t.Fatalf("samples=%d want 88", len(tr.Samples))
	}
}

func TestScanUnrecoverable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "junk.mp4")
	if err := os.WriteFile(p, []byte("not an mp4 at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := mp4.ScanFile(p)
	if !model.HasCode(err, model.CodeUnrecoverable) {
		t.Fatalf("err=%v want unrecoverable", err)
	}
}
