package store

import (
	"os"
	"path/filepath"
	"testing"

	"mediatool/internal/clock"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "seg0.m4s", "hello")
	m := &Manifest{Version: 1, InputSHA256: "in", ConfigSHA256: "cfg"}
	h, err := HashFile(f)
	if err != nil {
		t.Fatal(err)
	}
	m.AddSegment(SegmentRecord{RepID: 0, SegID: 0, File: f, SHA256: h, Complete: true})
	m.Faults = clock.Faults{{Kind: clock.FaultCCGap, Offset: 188, Recovered: true}}
	p := ManifestPath(dir)
	if err := m.Save(p); err != nil {
		t.Fatal(err)
	}
	m2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Segments) != 1 || m2.Segments[0].SHA256 != h {
		t.Fatalf("roundtrip mismatch: %+v", m2.Segments)
	}
	if len(m2.Faults) != 1 || m2.Faults[0].Kind != clock.FaultCCGap {
		t.Fatalf("faults not preserved: %+v", m2.Faults)
	}
}

func TestResume(t *testing.T) {
	dir := t.TempDir()
	f0 := writeFile(t, dir, "seg0.m4s", "a")
	f1 := writeFile(t, dir, "seg1.m4s", "b")
	h0, _ := HashFile(f0)
	m := &Manifest{InputSHA256: "in", ConfigSHA256: "cfg"}
	m.AddSegment(SegmentRecord{RepID: 0, SegID: 0, File: f0, SHA256: h0, Complete: true})
	m.AddSegment(SegmentRecord{RepID: 0, SegID: 1, File: f1, SHA256: "deadbeef", Complete: true}) // 哈希不符
	m.AddSegment(SegmentRecord{RepID: 0, SegID: 2, File: f1, SHA256: h0, Complete: false})        // 未完成
	done, err := m.Resume("in", "cfg")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || !done[[2]int{0, 0}] {
		t.Fatalf("done = %v, want only seg 0", done)
	}
	// 输入或配置变化 → 不可续跑
	if _, err := m.Resume("other", "cfg"); err == nil {
		t.Fatal("input change must block resume")
	}
	if _, err := m.Resume("in", "other"); err == nil {
		t.Fatal("config change must block resume")
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "seg0.m4s", "good")
	h, _ := HashFile(f)
	m := &Manifest{}
	m.AddSegment(SegmentRecord{File: f, SHA256: h, Complete: true})
	if bad, err := m.Verify(); err != nil || len(bad) != 0 {
		t.Fatalf("clean verify: bad=%v err=%v", bad, err)
	}
	if err := os.WriteFile(f, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := m.Verify()
	if err != nil || len(bad) != 1 {
		t.Fatalf("tamper not detected: bad=%v err=%v", bad, err)
	}
}
