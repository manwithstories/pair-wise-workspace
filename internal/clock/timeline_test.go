package clock

import "testing"

func cfg(trusted ...uint64) RepairConfig {
	set := map[uint64]bool{}
	for _, s := range trusted {
		set[s] = true
	}
	return RepairConfig{
		NominalDelta: 3600, // 25 fps @ 90 kHz
		MaxGap:       4 * Timescale,
		Trusted:      func(seq uint64) bool { return set[seq] },
	}
}

func TestRepairMonotonic(t *testing.T) {
	var ss []Sample
	for i := 0; i < 10; i++ {
		ss = append(ss, Sample{Seq: uint64(i), TS: uint32(1000 + i*3600), Keyframe: i%5 == 0})
	}
	fixed, faults, segs := Repair(ss, cfg())
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %v", faults)
	}
	for i := 1; i < len(fixed); i++ {
		if got := fixed[i].Time - fixed[i-1].Time; got != 3600 {
			t.Fatalf("delta[%d]=%d, want 3600", i, got)
		}
	}
	if len(segs) != 1 || segs[0].Samples != 10 {
		t.Fatalf("segments=%+v", segs)
	}
}

func TestRepairWrap(t *testing.T) {
	base := uint32(WrapModulus - 3*3600)
	var ss []Sample
	for i := 0; i < 8; i++ {
		ss = append(ss, Sample{Seq: uint64(i), TS: base + uint32(i*3600)})
	}
	fixed, faults, _ := Repair(ss, cfg())
	if len(faults) != 1 || faults[0].Kind != FaultWrap {
		t.Fatalf("faults=%v", faults)
	}
	for i := 1; i < len(fixed); i++ {
		if got := fixed[i].Time - fixed[i-1].Time; got != 3600 {
			t.Fatalf("non-monotonic across wrap at %d: delta=%d", i, got)
		}
	}
}

func TestRepairRestartTrusted(t *testing.T) {
	var ss []Sample
	for i := 0; i < 6; i++ {
		ts := uint32(1000 + i*3600)
		if i >= 3 {
			ts = uint32(100 + (i-3)*3600) // encoder restart, clock reset
		}
		ss = append(ss, Sample{Seq: uint64(i), TS: ts})
	}
	fixed, faults, segs := Repair(ss, cfg(3))
	if len(segs) != 2 || !segs[1].Recovered {
		t.Fatalf("segments=%+v", segs)
	}
	if len(faults) != 1 || faults[0].Kind != FaultRestart {
		t.Fatalf("faults=%v", faults)
	}
	for i := 1; i < len(fixed); i++ {
		if fixed[i].Time <= fixed[i-1].Time {
			t.Fatalf("time not monotonic at %d: %d <= %d", i, fixed[i].Time, fixed[i-1].Time)
		}
	}
}

func TestRepairIsolatedWithoutAnchor(t *testing.T) {
	var ss []Sample
	for i := 0; i < 6; i++ {
		ts := uint32(1000 + i*3600)
		if i >= 3 {
			ts = uint32(100 + (i-3)*3600)
		}
		ss = append(ss, Sample{Seq: uint64(i), TS: ts})
	}
	fixed, faults, segs := Repair(ss, cfg()) // no trusted anchors
	if len(segs) != 2 || !segs[1].Isolated {
		t.Fatalf("segments=%+v", segs)
	}
	for _, f := range fixed[3:] {
		if !f.Isolated || f.Time != -1 {
			t.Fatalf("isolated sample has fabricated time: %+v", f)
		}
	}
	if faults[len(faults)-1].Kind != FaultIsolated {
		t.Fatalf("faults=%v", faults)
	}
}

func TestDriftModel(t *testing.T) {
	var d DriftModel
	if d.PPM() != 0 {
		t.Fatal("no anchors: want 0 ppm")
	}
	d.AddAnchor(0, 0)
	d.AddAnchor(90000, 90000+45) // +500 ppm
	if got := d.PPM(); got < 499 || got > 501 {
		t.Fatalf("ppm=%v", got)
	}
	if got := d.Correct(180000); got != 180090 {
		t.Fatalf("correct=%d", got)
	}
	var none DriftModel
	if none.Correct(42) != 42 {
		t.Fatal("no anchors: Correct must be identity")
	}
}
