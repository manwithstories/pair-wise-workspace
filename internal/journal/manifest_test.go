package journal

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/media"
)

func TestManifestWriteLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := NewManifest()
	m.InputDigest = DigestHex([]byte("in"))
	m.InputPackets = 100
	m.Duplicates = 3
	m.Conflicts = 1
	m.Missing = 0
	m.Corrupt = 2
	m.OutputFile = "out.wav"
	m.OutputOffset = 4096
	m.OutputDigest = DigestHex([]byte("out"))
	m.IntegratedLUFS = -18.0
	m.TruePeakDBTP = -1.5
	path := filepath.Join(dir, "manifest.json")
	if err := m.Write(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputPackets != 100 || got.Conflicts != 1 ||
		got.OutputDigest != m.OutputDigest || got.IntegratedLUFS != -18.0 {
		t.Fatalf("清单往返字段丢失: %+v", got)
	}
}

func TestManifestDeterministicBytes(t *testing.T) {
	dir := t.TempDir()
	build := func() []byte {
		m := NewManifest()
		m.InputDigest = "deadbeef"
		m.InputPackets = 7
		m.OutputDigest = "cafebabe"
		path := filepath.Join(dir, "m.json")
		if err := m.Write(path); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := build(), build()
	if string(a) != string(b) {
		t.Fatal("清单序列化不确定")
	}
}

func TestManifestVerifyMatchAndMismatch(t *testing.T) {
	base := &Manifest{
		Version: 1, InputDigest: "x", InputPackets: 10,
		OutputDigest: "y", OutputOffset: 100,
	}
	same := &Manifest{
		Version: 1, InputDigest: "x", InputPackets: 10,
		OutputDigest: "y", OutputOffset: 100,
	}
	if err := base.Verify(same); err != nil {
		t.Fatalf("一致清单应通过: %v", err)
	}
	cases := []func() *Manifest{
		func() *Manifest { c := *same; c.InputDigest = "z"; return &c },
		func() *Manifest { c := *same; c.InputPackets = 11; return &c },
		func() *Manifest { c := *same; c.OutputDigest = "q"; return &c },
		func() *Manifest { c := *same; c.OutputOffset = 99; return &c },
	}
	for i, fn := range cases {
		if err := base.Verify(fn()); !media.IsKind(err, media.KindCheckpoint) {
			t.Fatalf("用例 %d 应报 KindCheckpoint: %v", i, err)
		}
	}
	if err := base.Verify(nil); !media.IsKind(err, media.KindCheckpoint) {
		t.Fatalf("nil 对照应报错: %v", err)
	}
}

func TestLoadManifestBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); !media.IsKind(err, media.KindCheckpoint) {
		t.Fatalf("坏 JSON 应报 KindCheckpoint: %v", err)
	}
}
