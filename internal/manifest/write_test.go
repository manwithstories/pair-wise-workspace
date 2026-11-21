package manifest

import (
	"path/filepath"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/model"
	"mediad/internal/scheduler"
)

func sampleManifest() *Manifest {
	m := New(42, "demo")
	m.Timebase = clock.Video90k.String()
	m.Tracks = []TrackRecord{{Key: "v0", Kind: "video", Timebase: "1/90000", Duration: 9000}}
	m.Color = ColorRecord{Primaries: 1, Transfer: 1, Matrix: 1, Source: "colr"}
	m.Frames = []FrameRecord{
		{Index: 0, Start: 0, End: 3000, PlanHash: "p0", FrameHash: "h0"},
		{Index: 1, Start: 3000, End: 6000, PlanHash: "p1", FrameHash: "h1"},
	}
	m.Metrics = Metrics{FrameCount: 2, ManifestDriftMs: 0, BlockedNode: "none"}
	return m
}

func TestWriteAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m := sampleManifest()
	if err := m.Write(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version || got.Seed != 42 || len(got.Frames) != 2 {
		t.Fatalf("loaded %+v", got)
	}
	if got.Frames[1].FrameHash != "h1" || got.Color.Key() != "p1/t1/m1/full=false/src=colr" {
		t.Fatalf("fields lost: %+v", got)
	}
}

func TestCompareStableAndUnstable(t *testing.T) {
	a := sampleManifest()
	b := sampleManifest()
	rec := Compare(a, b, 1)
	if !rec.FrameHashesStable || !rec.ManifestStable || rec.DriftMs != 0 {
		t.Fatalf("identical runs unstable: %+v", rec)
	}

	// Changed frame hash.
	b.Frames[1].FrameHash = "different"
	rec = Compare(a, b, 1)
	if rec.FrameHashesStable || rec.ManifestStable || len(rec.MismatchedFrames) != 1 || rec.MismatchedFrames[0] != 1 {
		t.Fatalf("changed hash not detected: %+v", rec)
	}

	// Alignment drift beyond tolerance.
	c := sampleManifest()
	c.Frames[1].Start = 3002
	rec = Compare(a, c, 1)
	if rec.ManifestStable || rec.DriftMs != 2 {
		t.Fatalf("drift not detected: %+v", rec)
	}

	// Changed frame count.
	d := sampleManifest()
	d.Frames = d.Frames[:1]
	d.Metrics.FrameCount = 1
	rec = Compare(a, d, 1)
	if rec.FrameHashesStable {
		t.Fatal("frame count change not detected")
	}

	// Changed color decision.
	e := sampleManifest()
	e.Color.Source = "default"
	if rec := Compare(a, e, 1); rec.ManifestStable {
		t.Fatal("color change not detected")
	}
}

func TestNodesFromReport(t *testing.T) {
	s, err := scheduler.New([]scheduler.Node{
		{Name: "metadata", Run: func() (scheduler.Result, error) {
			return scheduler.Result{}, model.New(model.CategoryMetadata, "parse", "bad")
		}},
		{Name: "render", Deps: []string{"metadata"}, Run: func() (scheduler.Result, error) {
			return scheduler.Result{Hash: "r"}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := New(1, "test")
	m.NodesFromReport(s.Run())
	if len(m.Nodes) != 2 {
		t.Fatalf("nodes = %d", len(m.Nodes))
	}
	if m.Nodes[0].Name != "metadata" || m.Nodes[0].Status != "failed" {
		t.Fatalf("node 0 = %+v", m.Nodes[0])
	}
	if m.Nodes[1].Status != "blocked" || m.Nodes[1].RootCause != "metadata" {
		t.Fatalf("node 1 = %+v", m.Nodes[1])
	}
}

func TestAddArtifactHashes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frame.png")
	if err := writeFile(path, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	m := New(1, "test")
	if err := m.AddArtifact(path); err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 1 || m.Artifacts[0].SHA256 == "" || m.Artifacts[0].Bytes != 3 {
		t.Fatalf("artifact = %+v", m.Artifacts)
	}
	// The hash is the SHA-256 of "abc".
	if m.Artifacts[0].SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash = %s", m.Artifacts[0].SHA256)
	}
	if err := m.AddArtifact(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing artifact accepted")
	}
}

func TestSelfHashIsReproducibleFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	m := sampleManifest()
	if err := m.Write(path); err != nil {
		t.Fatal(err)
	}
	sum, err := m.ContentSHA256()
	if err != nil {
		t.Fatal(err)
	}
	m.Artifacts = append(m.Artifacts, ArtifactRecord{Path: "manifest.json", SHA256: sum})
	if err := m.Write(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.VerifySelfHash(); err != nil {
		t.Fatalf("freshly written manifest fails its own hash: %v", err)
	}
	// Tampering with the body must be detected.
	loaded.Frames[0].FrameHash = "tampered"
	if err := loaded.VerifySelfHash(); err == nil {
		t.Fatal("tampered manifest passed self-hash verification")
	}
	// A missing entry is an error, not a silent pass.
	loaded2 := sampleManifest()
	if err := loaded2.VerifySelfHash(); err == nil {
		t.Fatal("manifest without a self hash passed verification")
	}
}
