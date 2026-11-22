package frame

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"mediascan/internal/fault"
)

func goodFrame(i int, key bool, size int) Frame {
	sum := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
	return Frame{
		Index: i, ID: "f" + string(rune('a'+i%26)), Keyframe: key,
		Timestamp: int64(i) * 1000 / 30, Size: size,
		SHA256: hex.EncodeToString(sum[:]),
	}
}

func TestValidateHappyPath(t *testing.T) {
	m := &Manifest{FPS: 30, ClockRate: 90000, GOP: 2, TargetKbps: 900}
	for i := 0; i < 6; i++ {
		m.Frames = append(m.Frames, goodFrame(i, i%2 == 0, 1000))
	}
	res, err := m.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults)
	}
	if res.TotalBytes != 6000 {
		t.Fatalf("total bytes = %d", res.TotalBytes)
	}
	if len(res.KeyframeIdx) != 3 {
		t.Fatalf("keyframes = %v", res.KeyframeIdx)
	}
	if res.FirstTick != 0 {
		t.Fatalf("first tick = %d", res.FirstTick)
	}
}

func TestValidateDuplicateAndNonMonotonic(t *testing.T) {
	m := &Manifest{FPS: 30, ClockRate: 90000, GOP: 2}
	m.Frames = []Frame{goodFrame(0, true, 100), goodFrame(1, false, 100), goodFrame(2, false, 100)}
	m.Frames[2].Timestamp = m.Frames[1].Timestamp // duplicate
	res, err := m.Validate()
	if err != nil {
		t.Fatal(err)
	}
	var cats []fault.Category
	for _, f := range res.Faults {
		cats = append(cats, f.Cat)
	}
	found := map[fault.Category]bool{}
	for _, c := range cats {
		found[c] = true
	}
	if !found[fault.CatDupTimestamp] {
		t.Fatalf("duplicate not detected: %v", cats)
	}
	m.Frames[2].Timestamp = m.Frames[1].Timestamp - 5
	res, _ = m.Validate()
	found = map[fault.Category]bool{}
	for _, f := range res.Faults {
		found[f.Cat] = true
	}
	if !found[fault.CatNonMonotonic] && !found[fault.CatDupTimestamp] {
		t.Fatalf("non-monotonic not detected: %v", res.Faults)
	}
}

func TestValidateSizeAndHashShape(t *testing.T) {
	m := &Manifest{FPS: 30, ClockRate: 90000, GOP: 2}
	bad := goodFrame(0, true, MaxFrameSize+1)
	bad.SHA256 = "abcd"
	m.Frames = []Frame{bad}
	res, err := m.Validate()
	if err != nil {
		t.Fatal(err)
	}
	found := map[fault.Category]bool{}
	for _, f := range res.Faults {
		found[f.Cat] = true
	}
	if !found[fault.CatSizeViolation] || !found[fault.CatBadChecksum] {
		t.Fatalf("size/hash faults missing: %v", res.Faults)
	}
}

func TestTotalBytesLimit(t *testing.T) {
	m := &Manifest{FPS: 30, ClockRate: 90000, GOP: 2}
	for i := 0; i < 65; i++ { // 65 * 16MiB > 1GiB
		m.Frames = append(m.Frames, goodFrame(i, i == 0, MaxFrameSize))
	}
	if _, err := m.Validate(); err == nil {
		t.Fatal("total byte cap must be enforced")
	}
}

func TestVerifyHash(t *testing.T) {
	fr := goodFrame(0, true, 2)
	if f := fr.Verify([]byte{0, 0}); !IsZero(f) {
		t.Fatalf("valid payload flagged: %v", f)
	}
	if f := fr.Verify([]byte{9, 9}); f.Cat != fault.CatBadChecksum {
		t.Fatalf("tampered payload must fail checksum, got %v", f)
	}
}

func TestAlignmentMeasured(t *testing.T) {
	m := &Manifest{FPS: 30, ClockRate: 90000, GOP: 2}
	fr := goodFrame(0, true, 100)
	fr.Timestamp = 33 // frame 0 expected at 0 ms
	m.Frames = []Frame{fr}
	res, err := m.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if res.AlignmentMS != 33 {
		t.Fatalf("alignment = %d, want 33", res.AlignmentMS)
	}
}
