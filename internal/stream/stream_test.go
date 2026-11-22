package stream

import (
	"bytes"
	"strings"
	"testing"

	"mediascan/internal/fault"
)

func TestScanValidContainer(t *testing.T) {
	data, err := BuildMinimalMP4([]byte("frame-payload-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Scan(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", a.Faults)
	}
	var types []string
	for _, b := range a.Boxes {
		types = append(types, b.Type)
	}
	want := map[string]bool{"ftyp": true, "moov": true, "mdat": true, "mvhd": true}
	for w := range want {
		if !contains(types, w) {
			t.Fatalf("box %s missing, got %v", w, types)
		}
	}
	if len(a.MdatOffsets) != 1 {
		t.Fatalf("mdat offsets = %v", a.MdatOffsets)
	}
}

func TestScanTruncatedBox(t *testing.T) {
	data, _ := BuildMinimalMP4(make([]byte, 64))
	// truncate the file mid-mdat: declared size still claims full length
	truncated := data[:len(data)-20]
	a, err := Scan(bytes.NewReader(truncated))
	if err != nil {
		t.Fatal(err)
	}
	if !hasFault(a.Faults, fault.CatTruncatedBox) {
		t.Fatalf("truncation not located: %v", a.Faults)
	}
	f := firstFault(a.Faults, fault.CatTruncatedBox)
	if f.Byte < 0 || !strings.Contains(f.Msg, "exceeds file size") {
		t.Fatalf("fault must carry byte offset and reason: %v", f)
	}
}

func TestScanIllegalSize(t *testing.T) {
	data, _ := BuildMinimalMP4(make([]byte, 32))
	// corrupt the first box size to claim only 4 bytes (smaller than 8-byte header)
	corrupt := append([]byte(nil), data...)
	corrupt[0] = 0
	corrupt[1] = 0
	corrupt[2] = 0
	corrupt[3] = 4
	a, err := Scan(bytes.NewReader(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	if !hasFault(a.Faults, fault.CatIllegalSize) {
		t.Fatalf("illegal size not detected: %v", a.Faults)
	}
}

func TestScanMissingRequiredBox(t *testing.T) {
	data, _ := BuildMinimalMP4(make([]byte, 16))
	// drop the trailing mdat box entirely (8-byte header + 16-byte payload)
	noMdat := data[:len(data)-24]
	a, err := Scan(bytes.NewReader(noMdat))
	if err != nil {
		t.Fatal(err)
	}
	if !hasFault(a.Faults, fault.CatIllegalSize) {
		t.Fatalf("missing box not detected: %v", a.Faults)
	}
}

func TestAnalyseGOPRegular(t *testing.T) {
	idx := []int{0, 60, 120, 180}
	s := AnalyseGOP(idx, 240, 60, 0.25)
	if len(s.Faults) != 0 {
		t.Fatalf("regular GOP flagged: %v", s.Faults)
	}
	if s.P50 != 60 || s.Min != 60 || s.Max != 60 {
		t.Fatalf("gop stats wrong: %+v", s)
	}
}

func TestAnalyseGOPAnomalyAndUnreachable(t *testing.T) {
	idx := []int{30, 90, 200} // 30 unreachable frames, last interval 110
	s := AnalyseGOP(idx, 240, 60, 0.25)
	if len(s.Unreachable) != 30 {
		t.Fatalf("unreachable = %v", s.Unreachable)
	}
	if !hasFault(s.Faults, fault.CatGOPAnomaly) {
		t.Fatalf("anomaly not detected: %v", s.Faults)
	}
	if s.P50 == 0 || s.Max != 110 {
		t.Fatalf("interval stats wrong: %+v", s)
	}
}

func TestAnalyseGOPNoKeyframes(t *testing.T) {
	s := AnalyseGOP(nil, 12, 60, 0.25)
	if len(s.Unreachable) != 12 || !hasFault(s.Faults, fault.CatGOPAnomaly) {
		t.Fatalf("no-keyframe case wrong: %+v", s)
	}
}

func hasFault(fs []fault.Fault, cat fault.Category) bool {
	return firstFault(fs, cat).Cat != ""
}

func firstFault(fs []fault.Fault, cat fault.Category) fault.Fault {
	for _, f := range fs {
		if f.Cat == cat {
			return f
		}
	}
	return fault.Fault{}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
