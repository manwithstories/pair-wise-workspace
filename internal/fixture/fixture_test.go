package fixture_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fixture"
)

func TestGenerateDeterministic(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	i1, err := fixture.Generate(fixture.Options{Seed: 41, Profile: fixture.ProfileClean, Cams: 4, Segments: 10, Root: dir1})
	if err != nil {
		t.Fatal(err)
	}
	i2, err := fixture.Generate(fixture.Options{Seed: 41, Profile: fixture.ProfileClean, Cams: 4, Segments: 10, Root: dir2})
	if err != nil {
		t.Fatal(err)
	}
	if i1.Expected.PayloadSHA != i2.Expected.PayloadSHA {
		t.Fatalf("same seed produced different payload digests")
	}
	if i1.Expected.TotalSamples != i2.Expected.TotalSamples {
		t.Fatalf("sample counts differ")
	}
	// Sample payloads are scene-stable per camera/frame (so recovery can be
	// scored byte-exactly), but the seed must still drive the VFR frame grid.
	dir3 := t.TempDir()
	if _, err := fixture.Generate(fixture.Options{Seed: 42, Profile: fixture.ProfileClean, Cams: 4, Segments: 10, Root: dir3, VFR: true}); err != nil {
		t.Fatal(err)
	}
	b3, err := os.ReadFile(filepath.Join(dir3, "media", "cam1", "seg1.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	dir4 := t.TempDir()
	fixture.Generate(fixture.Options{Seed: 43, Profile: fixture.ProfileClean, Cams: 4, Segments: 10, Root: dir4, VFR: true})
	b4, _ := os.ReadFile(filepath.Join(dir4, "media", "cam1", "seg1.m4s"))
	if string(b3) == string(b4) {
		t.Fatal("seed does not influence the VFR frame grid")
	}
}

func TestGenerateCounts(t *testing.T) {
	root := t.TempDir()
	info, err := fixture.Generate(fixture.Options{Seed: 7, Profile: fixture.ProfileClean, Cams: 4, Segments: 180, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if info.Expected.TotalSamples != 4*180*4 {
		t.Fatalf("total samples=%d want %d", info.Expected.TotalSamples, 4*180*4)
	}
	if len(info.CameraList) != 4 {
		t.Fatalf("cameras=%d", len(info.CameraList))
	}
	// Each camera's segments and init must exist on disk.
	for _, cam := range info.CameraList {
		if _, err := os.Stat(filepath.Join(root, "media", cam.Name, "init.mp4")); err != nil {
			t.Fatalf("%s init missing: %v", cam.Name, err)
		}
	}
}

func TestCorruptProfileTruncatesOneSegment(t *testing.T) {
	root := t.TempDir()
	info, err := fixture.Generate(fixture.Options{
		Seed: 41, Profile: fixture.ProfileCorrupt, Cams: 4, Segments: 180, Root: root, TruncateMoof: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Expected.TruncatedSegs != 1 {
		t.Fatalf("truncated=%d want 1", info.Expected.TruncatedSegs)
	}
}

func TestRecoverProfileReportsRedundancy(t *testing.T) {
	root := t.TempDir()
	info, err := fixture.Generate(fixture.Options{
		Seed: 41, Profile: fixture.ProfileRecover, Cams: 4, Segments: 60, Root: root,
		Loss: 0.03, DriftPPM: 120, WrapPTS: 8589900000, VFR: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Expected.LostPackets == 0 {
		t.Fatal("recover profile dropped no packets")
	}
	if info.Expected.RecoveredPackets == 0 {
		t.Fatal("recover profile marked no packets recoverable")
	}
	if info.Expected.RecoveredPackets > info.Expected.LostPackets {
		t.Fatalf("recoverable %d exceeds lost %d", info.Expected.RecoveredPackets, info.Expected.LostPackets)
	}
	// Trace files must exist for every camera.
	for _, cam := range info.CameraList {
		if _, err := os.Stat(filepath.Join(root, "media", "rtp", cam.Name+".json")); err != nil {
			t.Fatalf("%s trace missing: %v", cam.Name, err)
		}
	}
}
