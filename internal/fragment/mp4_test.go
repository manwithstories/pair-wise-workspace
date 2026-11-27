package fragment

import (
	"os"
	"path/filepath"
	"testing"

	"review/internal/fault"
)

func writeTestSeg(t *testing.T, dir string, payloads [][]byte, dropIndex int) string {
	t.Helper()
	p := filepath.Join(dir, "seg.m4s")
	if _, err := WriteSegment(p, 90000, payloads, 3000, dropIndex); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	init := &InitInfo{Codec: "jpeg", Timescale: 90000, Width: 160, Height: 90,
		FrameDuration: 3000}
	ip := filepath.Join(dir, "init.mp4")
	if err := WriteInit(ip, init); err != nil {
		t.Fatal(err)
	}
	got, ferr := ParseInit(ip)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if got.Codec != "jpeg" || got.Timescale != 90000 || got.Width != 160 || got.FrameDuration != 3000 {
		t.Fatalf("init=%v", got)
	}

	payloads := [][]byte{[]byte("frame-zero"), []byte("frame-one!"), []byte("frame-two??")}
	p := writeTestSeg(t, dir, payloads, 0)
	seg, errs := ParseSegment(p)
	if len(errs) != 0 {
		t.Fatalf("faults: %v", errs)
	}
	if seg.BaseTime != 90000 || len(seg.Samples) != 3 || seg.IndexCount != 3 {
		t.Fatalf("seg=%+v", seg)
	}
	// 时间戳累进
	if seg.Samples[1].Time != 93000 || seg.Samples[2].Time != 96000 {
		t.Fatalf("times: %v %v", seg.Samples[1].Time, seg.Samples[2].Time)
	}
	for i, want := range payloads {
		data, ferr := seg.ReadData(i)
		if ferr != nil {
			t.Fatal(ferr)
		}
		if string(data) != string(want) {
			t.Fatalf("sample %d=%q", i, data)
		}
	}
}

func TestTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	p := writeTestSeg(t, dir, [][]byte{[]byte("aaaa"), []byte("bbbb"), []byte("cccc")}, 0)
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, raw[:len(raw)-3], 0o644); err != nil { // 截尾
		t.Fatal(err)
	}
	seg, errs := ParseSegment(p)
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindTruncated {
		t.Fatalf("want truncated, got %v", errs)
	}
	if !f.Recoverable {
		t.Fatal("truncation should be recoverable")
	}
	if len(seg.Samples) >= 3 {
		t.Fatal("truncated sample must be dropped")
	}
}

func TestCRCMismatch(t *testing.T) {
	dir := t.TempDir()
	p := writeTestSeg(t, dir, [][]byte{[]byte("payload-0"), []byte("payload-1")}, 0)
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 0xFF // 翻转末字节破坏 CRC
	os.WriteFile(p, raw, 0o644)
	seg, errs := ParseSegment(p)
	if len(errs) != 0 {
		t.Fatalf("parse faults: %v", errs)
	}
	if _, ferr := seg.ReadData(0); ferr != nil {
		t.Fatalf("sample 0 should be intact: %v", ferr)
	}
	if _, ferr := seg.ReadData(1); ferr == nil || ferr.Kind != fault.KindCRC {
		t.Fatalf("want crc fault, got %v", ferr)
	}
}

func TestMissingIndex(t *testing.T) {
	dir := t.TempDir()
	p := writeTestSeg(t, dir, [][]byte{[]byte("s0"), []byte("s1"), []byte("s2"), []byte("s3")}, 2)
	seg, errs := ParseSegment(p)
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindMissingIndex {
		t.Fatalf("want missing_index, got %v", errs)
	}
	if seg.IndexCount != 2 || len(seg.Samples) != 4 {
		t.Fatalf("index=%d samples=%d", seg.IndexCount, len(seg.Samples))
	}
	// 索引缺项可恢复：样本仍可从 trun 读取
	if _, ferr := seg.ReadData(3); ferr != nil {
		t.Fatal(ferr)
	}
}

func TestSegmentTooLarge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.m4s")
	if err := os.WriteFile(p, make([]byte, MaxSegmentBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errs := ParseSegment(p); len(errs) == 0 || errs[0].Kind != fault.KindLimit {
		t.Fatalf("want limit fault, got %v", errs)
	}
}
