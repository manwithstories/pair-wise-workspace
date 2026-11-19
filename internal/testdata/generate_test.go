package testdata

import (
	"os"
	"path/filepath"
	"testing"

	"archive/internal/manifest"
	"archive/internal/mp4"
)

func testConfig() Config {
	return Config{Seed: 41, BurstLoss: 7, Reorder: 12, Bandwidth: []float64{1, 8, 2}}
}

func TestGenerate(t *testing.T) {
	root := t.TempDir()
	f, err := Generate(testConfig(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Good) != 5 || len(f.Corrupt) != 4 {
		t.Fatalf("good=%d corrupt=%d", len(f.Good), len(f.Corrupt))
	}
	if len(f.RTP) == 0 {
		t.Fatal("no RTP packets")
	}
	// every good asset manifest parses and its segments demux
	for _, a := range f.Good {
		entry := filepath.Join(a.Dir, a.ManifestFile)
		data, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		m, err := manifest.Parse(a.Name, data)
		if err != nil {
			t.Fatalf("%s: %v", a.Name, err)
		}
		if m.Type == manifest.TypeHLSMaster {
			data, _ = os.ReadFile(filepath.Join(a.Dir, m.Variants[0].URI))
			m, err = manifest.Parse(a.Name, data)
			if err != nil {
				t.Fatalf("%s media: %v", a.Name, err)
			}
		}
		if len(m.Segments) == 0 {
			t.Fatalf("%s: no segments", a.Name)
		}
		seg, err := os.ReadFile(filepath.Join(a.Dir, m.Segments[0].URI))
		if err != nil {
			t.Fatalf("%s: %v", a.Name, err)
		}
		tracks, err := mp4.Demux(a.Name, m.Segments[0].URI, seg)
		if err != nil {
			t.Fatalf("%s demux: %v", a.Name, err)
		}
		if len(tracks) < 2 {
			t.Fatalf("%s: tracks %d", a.Name, len(tracks))
		}
	}
}

func TestGenerateDeterministic(t *testing.T) {
	r1, r2 := t.TempDir(), t.TempDir()
	f1, err := Generate(testConfig(), r1)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Generate(testConfig(), r2)
	if err != nil {
		t.Fatal(err)
	}
	if len(f1.RTP) != len(f2.RTP) {
		t.Fatal("RTP stream not deterministic")
	}
	for i := range f1.RTP {
		if string(f1.RTP[i]) != string(f2.RTP[i]) {
			t.Fatalf("RTP packet %d differs", i)
		}
	}
}
