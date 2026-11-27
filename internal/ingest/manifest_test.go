package ingest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fieldcut/internal/media"
)

func sample() *Manifest {
	return &Manifest{
		Version: 1, OutputFPS: 25, Frames: 4, Width: 64, Height: 36,
		WrapAt: 4294967296, Chunk: 32,
		Tracks: []Track{{Name: "A", Frames: 4, RateNum: 1, RateDen: 1}},
		Entries: []Entry{
			{Track: "A", Seq: 1, Tick: 100, Nanos: 1, Width: 64, Height: 36, Path: "frames/a1.png", SHA256: repeat("a", 64)},
			{Track: "A", Seq: 0, Tick: 0, Nanos: 0, Width: 64, Height: 36, Path: "frames/a0.png", SHA256: repeat("b", 64)},
			{Track: "A", Seq: 1, Tick: 999, Nanos: 9, Width: 64, Height: 36, Path: "frames/a1b.png", SHA256: repeat("c", 64)},
			{Track: "A", Seq: 3, Tick: 300, Nanos: 3, Width: 64, Height: 36, Path: "frames/a3.png", SHA256: repeat("d", 64)},
		},
	}
}

func repeat(s string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = s[0]
	}
	return string(out)
}

func TestValidateOrdersAndDeduplicates(t *testing.T) {
	sets, err := Validate(sample())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := sets["A"]
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 after dedupe", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Seq >= got[i].Seq {
			t.Fatalf("entries not ordered: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
	if got[1].Path != "frames/a1.png" {
		t.Fatalf("duplicate seq 1 kept %q, want first arrival", got[1].Path)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		kind   media.Kind
	}{
		{"oversize", func(m *Manifest) { m.Entries[0].Width = media.MaxWidth + 1 }, media.KindInput},
		{"zero-size", func(m *Manifest) { m.Entries[0].Height = 0 }, media.KindInput},
		{"traversal", func(m *Manifest) { m.Entries[0].Path = "../secret.png" }, media.KindInput},
		{"absolute", func(m *Manifest) { m.Entries[0].Path = "/tmp/x.png" }, media.KindInput},
		{"bad-hash", func(m *Manifest) { m.Entries[0].SHA256 = repeat("z", 64) }, media.KindInput},
		{"short-hash", func(m *Manifest) { m.Entries[0].SHA256 = "abc" }, media.KindInput},
		{"unknown-track", func(m *Manifest) { m.Entries[0].Track = "Z" }, media.KindInput},
		{"too-many-tracks", func(m *Manifest) {
			m.Tracks = append(m.Tracks, Track{Name: "B"}, Track{Name: "C"}, Track{Name: "D"})
		}, media.KindLimit},
		{"bad-fps", func(m *Manifest) { m.OutputFPS = 30 }, media.KindInput},
		{"bad-wrap", func(m *Manifest) { m.WrapAt = 0 }, media.KindClock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sample()
			tc.mutate(m)
			_, err := Validate(m)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			var me *media.MediaError
			if !errors.As(err, &me) {
				t.Fatalf("not a MediaError: %v", err)
			}
			if me.Kind != tc.kind {
				t.Fatalf("kind=%v, want %v (%v)", me.Kind, tc.kind, err)
			}
		})
	}
}

func TestSafePath(t *testing.T) {
	ok := []string{"a.png", "frames/A/0001.png", "x/y/z.png"}
	for _, p := range ok {
		if err := SafePath(p); err != nil {
			t.Errorf("SafePath(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{"", ".", "..", "../a.png", "a/../b.png", "/abs.png", "a\\b.png", "C:/x.png", "a\x00.png"}
	for _, p := range bad {
		if err := SafePath(p); err == nil {
			t.Errorf("SafePath(%q) accepted", p)
		}
	}
}

func TestRoundTripAndHashMismatch(t *testing.T) {
	dir := t.TempDir()
	m := sample()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frames", "a1.png"), []byte("frame"), 0o644); err != nil {
		if os.MkdirAll(filepath.Join(dir, "frames"), 0o755) != nil {
			t.Fatal(err)
		}
		if os.WriteFile(filepath.Join(dir, "frames", "a1.png"), []byte("frame"), 0o644) != nil {
			t.Fatal(err)
		}
	}
	_, sets, err := Load(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := sets["A"][1]
	if err := VerifyEntry(dir, e); err == nil {
		t.Fatalf("hash mismatch accepted")
	}
	sum, err := HashFile(filepath.Join(dir, e.Path))
	if err != nil {
		t.Fatal(err)
	}
	e.SHA256 = sum
	if err := VerifyEntry(dir, e); err != nil {
		t.Fatalf("correct hash rejected: %v", err)
	}
}
