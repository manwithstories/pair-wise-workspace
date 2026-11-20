package segment

import (
	"testing"

	"mediamap/internal/testkit"
)

func goodSegment() []byte {
	return testkit.MediaSegment(7, 123456, true, make([]byte, 400))
}

func TestParseGoodSegment(t *testing.T) {
	info, faults := Parse("t", goodSegment())
	if len(faults) != 0 {
		t.Fatalf("faults: %v", faults)
	}
	if !info.Keyframe || info.DecodeTime != 123456 || info.Sequence != 7 {
		t.Fatalf("info: %+v", info)
	}
	if info.DataBytes != 400 {
		t.Fatalf("mdat bytes: got %d", info.DataBytes)
	}
	// Every box must carry its source offset; styp is first at 0.
	if info.Boxes[0].Type != "styp" || info.Boxes[0].Offset != 0 {
		t.Fatalf("first box: %+v", info.Boxes[0])
	}
}

func TestParseTruncatedTailRecoverable(t *testing.T) {
	full := goodSegment()
	cut := full[:len(full)*3/5] // inside mdat
	info, faults := Parse("t", cut)
	if faults.HasFatal() {
		t.Fatalf("truncation must be recoverable: %v", faults)
	}
	if len(faults) == 0 {
		t.Fatal("truncation must be reported")
	}
	f := faults[0]
	if f.Offset < 0 || f.Offset >= int64(len(cut)) {
		t.Fatalf("offset outside buffer: %d", f.Offset)
	}
	// tfdt sits before mdat, so the decode time survives.
	if info.DecodeTime != 123456 {
		t.Fatalf("decode time lost: %+v", info)
	}
	if info.DataBytes >= 400 {
		t.Fatalf("partial mdat must be smaller: %d", info.DataBytes)
	}
}

func TestParseCorruptLengthReportsOffset(t *testing.T) {
	full := goodSegment()
	bad := testkit.CorruptLength(full, "moof", 0x10000)
	_, faults := Parse("t", bad)
	if len(faults) == 0 || faults.HasFatal() {
		t.Fatalf("want one recoverable fault: %v", faults)
	}
	// moof follows styp; the fault offset must equal moof's start.
	var stypSize int64
	for _, b := range ParseBoxes(t, full) {
		if b.Type == "styp" {
			stypSize = b.Size
		}
	}
	if faults[0].Offset != stypSize {
		t.Fatalf("fault offset %d, moof starts at %d", faults[0].Offset, stypSize)
	}
}

func TestParseBadLengthFatal(t *testing.T) {
	bad := testkit.BadLength(goodSegment())
	_, faults := Parse("t", bad)
	if !faults.HasFatal() {
		t.Fatalf("size below header must be fatal: %v", faults)
	}
	if faults[0].Offset != 0 {
		t.Fatalf("fatal offset: %d", faults[0].Offset)
	}
}

func TestParseNeverOverrunsBuffer(t *testing.T) {
	full := goodSegment()
	for cut := 0; cut <= len(full); cut += 7 {
		_, _ = Parse("t", full[:cut]) // must not panic
	}
}

func TestParseInitSegment(t *testing.T) {
	info, faults := Parse("t", testkit.InitSegment())
	if len(faults) != 0 || !info.Init {
		t.Fatalf("init: %+v faults=%v", info, faults)
	}
}

// ParseBoxes is a small test helper returning just the box list.
func ParseBoxes(t *testing.T, buf []byte) []Box {
	t.Helper()
	info, _ := Parse("t", buf)
	return info.Boxes
}
