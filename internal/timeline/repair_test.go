package timeline

import (
	"testing"

	"archive/internal/manifest"
)

func track(samples ...manifest.RawSample) *manifest.RawTrack {
	return &manifest.RawTrack{ID: "v1", Type: "video", Timescale: 90000, Samples: samples}
}

func s(dts, dur int64) manifest.RawSample {
	return manifest.RawSample{DTS: dts, Duration: dur, Size: 10}
}

func TestNonZeroStartShifted(t *testing.T) {
	rt, err := Repair(track(s(9000, 3000), s(12000, 3000), s(15000, 3000)), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Shift != 9000 {
		t.Fatalf("Shift=%d", rt.Shift)
	}
	for i, smp := range rt.Samples {
		if want := int64(i * 3000); smp.DTS != want {
			t.Fatalf("sample %d DTS=%d want %d", i, smp.DTS, want)
		}
	}
	if rt.Decisions[0].Kind != "nonzero-start" || rt.Decisions[0].Original != 9000 {
		t.Fatalf("decision not recorded: %+v", rt.Decisions[0])
	}
	if rt.Originals[0].DTS != 9000 {
		t.Fatal("original values not preserved")
	}
}

func TestBackwardClamped(t *testing.T) {
	// Small backward jump (<= 10*median) is clamped to prev+1.
	rt, _ := Repair(track(s(0, 100), s(100, 100), s(80, 100), s(300, 100)), 1_000_000)
	if rt.Samples[2].DTS != 101 {
		t.Fatalf("backward DTS=%d want 101", rt.Samples[2].DTS)
	}
	d := rt.Decisions[0]
	if d.Kind != "backward" || d.Original != 80 || d.Repaired != 101 {
		t.Fatalf("bad decision %+v", d)
	}
}

func TestHugeBackwardMarkedUnlocatable(t *testing.T) {
	// Jump backward by far more than 10*median -> unlocatable, no keyframe
	// fabrication: sample stays flagged but its interval is unlocatable.
	rt, _ := Repair(track(s(0, 100), s(100, 100), s(5000, 100), s(50, 100), s(5200, 100)), 1_000_000)
	if len(rt.Unlocatable) != 1 {
		t.Fatalf("unlocatable ranges: %v", rt.Unlocatable)
	}
	kind := ""
	for _, d := range rt.Decisions {
		if d.Index == 3 {
			kind = d.Kind
		}
	}
	if kind != "unlocatable" {
		t.Fatalf("kind=%s decisions=%+v", kind, rt.Decisions)
	}
	if !rt.InUnlocatable(60) || rt.InUnlocatable(6000) {
		t.Fatal("InUnlocatable broken")
	}
}

func TestGapRecorded(t *testing.T) {
	rt, _ := Repair(track(s(0, 100), s(100, 100), s(1000, 100)), 1_000_000)
	found := false
	for _, d := range rt.Decisions {
		if d.Kind == "gap" && d.Index == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("gap not recorded: %+v", rt.Decisions)
	}
	if rt.Samples[2].DTS != 1000 {
		t.Fatal("gap sample value must be preserved")
	}
}

func TestProgramMicrosWithDwell(t *testing.T) {
	tr := track(s(9000, 3000), s(12000, 3000))
	tr.EditList = []manifest.RawEdit{
		{MediaTime: -1, SegmentDuration: 100792}, // 100.792ms dwell @1e6
		{MediaTime: 9000, SegmentDuration: 900000},
	}
	rt, _ := Repair(tr, 1_000_000)
	us, err := rt.ProgramMicros(0)
	if err != nil {
		t.Fatal(err)
	}
	if us != 100792 {
		t.Fatalf("program=%d want 100792", us)
	}
}

func TestSyncError(t *testing.T) {
	va := track(s(9000, 3000))
	va.EditList = []manifest.RawEdit{{MediaTime: -1, SegmentDuration: 100000},
		{MediaTime: 9000, SegmentDuration: 900000}}
	au := &manifest.RawTrack{ID: "a1", Type: "audio", Timescale: 48000,
		Samples: []manifest.RawSample{s(4800, 1024)},
		EditList: []manifest.RawEdit{{MediaTime: -1, SegmentDuration: 100792},
			{MediaTime: 4800, SegmentDuration: 480000}}}
	rtv, _ := Repair(va, 1_000_000)
	rta, _ := Repair(au, 1_000_000)
	d, err := SyncErrorMicros(rtv, rta)
	if err != nil {
		t.Fatal(err)
	}
	if d != 792 {
		t.Fatalf("sync error=%dµs want 792", d)
	}
}
