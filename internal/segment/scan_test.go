package segment

import "testing"

func TestParseName(t *testing.T) {
	seg, err := ParseName("r2-s017-t1699900000400-d4000.rt3.ts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seg.Rendition != 2 || seg.Seq != 17 || seg.SourceTS != 1699900000400 ||
		seg.DeclaredMS != 4000 || seg.Retransmit != 3 {
		t.Fatalf("unexpected segment: %+v", seg)
	}

	plain, err := ParseName("r0-s0-t1000-d4000.ts")
	if err != nil || plain.Retransmit != 0 {
		t.Fatalf("plain name parse failed: %+v err=%v", plain, err)
	}

	bad := []string{
		"r9-s0-t1-d4000.ts",  // unknown rendition
		"r0-s0-t1.ts",        // too few fields
		"r0-s0x-t1-d4000.ts", // non-numeric seq
		"r0-s0-t1-d0.ts",     // zero duration
		"r0-s0-t1-d4000.rt.ts",
		"r0-s0-t1-d4000.txt",
	}
	for _, name := range bad {
		if _, err := ParseName(name); err == nil {
			t.Errorf("expected error for %q", name)
		}
	}
}

func TestMeasuredDuration(t *testing.T) {
	if got := MeasuredDurationMS(200000, 400000); got != 4000 {
		t.Fatalf("measured duration = %d, want 4000", got)
	}
	if got := MeasuredDurationMS(1, 0); got != 0 {
		t.Fatalf("zero bitrate case = %d, want 0", got)
	}
}
