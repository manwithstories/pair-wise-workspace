package index

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestDigestTreeInvalidatesOnMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !shaRE.MatchString(d1) {
		t.Fatalf("digest not 64 hex: %q", d1)
	}

	// Modification changes the digest.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello-world"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, _ := DigestTree(root)
	if d2 == d1 {
		t.Fatal("content modification must invalidate digest")
	}

	// Adding a file changes it.
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3, _ := DigestTree(root)
	if d3 == d2 {
		t.Fatal("added file must invalidate digest")
	}

	// Renaming changes it even with identical bytes.
	if err := os.Rename(filepath.Join(root, "b.txt"), filepath.Join(root, "c.txt")); err != nil {
		t.Fatal(err)
	}
	d4, _ := DigestTree(root)
	if d4 == d3 {
		t.Fatal("rename must invalidate digest")
	}

	// Deletion changes it and returns toward the prior state only if the
	// remaining tree matches exactly (here a.txt was modified, so differ).
	_ = os.Remove(filepath.Join(root, "c.txt"))
	d5, _ := DigestTree(root)
	if d5 == d4 {
		t.Fatal("delete must invalidate digest")
	}

	// Temp files are excluded so atomic writes do not perturb the digest.
	if err := os.WriteFile(filepath.Join(root, "x.json.tmp-123"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	d6, _ := DigestTree(root)
	if d6 != d5 {
		t.Fatal("temporary files must be excluded from input digest")
	}
}

func sampleIndex() *Index {
	return &Index{
		Version: IndexVersion, InputSHA256: "abc", CommonTimescale: 720000,
		Chunks: []Chunk{{ID: "v-seg000", TrackID: "v", SegmentIndex: 0, StartCommon: 0, EndCommon: 720000}},
		Breakpoints: []Breakpoint{{TrackID: "v", SegmentIndex: 2, Kind: "declared_discontinuity"}},
		RTP: []RTPSummary{{Stream: "audio", TotalLost: 32, Recovered: 30, UnrecoveredGaps: 1, LossRecovery: 0.9375}},
	}
}

func TestIndexDigestDeterministicAndAtomicRoundTrip(t *testing.T) {
	idx := sampleIndex()
	d1, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// An independently built but identical index must hash the same.
	d2, _ := sampleIndex().Digest()
	if d1 != d2 {
		t.Fatal("identical indexes must have equal digests")
	}
	path := filepath.Join(t.TempDir(), "nested", "index.json")
	saved, err := idx.SaveAtomic(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved != d1 || !shaRE.MatchString(saved) {
		t.Fatalf("saved digest mismatch %q vs %q", saved, d1)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	d3, _ := loaded.Digest()
	if d3 != d1 {
		t.Fatal("round-trip changed index digest")
	}
	// No temp leftovers.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestBreakpointAndPacketLossPropagateToIndexDigest(t *testing.T) {
	base, _ := sampleIndex().Digest()

	withShift := sampleIndex()
	withShift.Chunks[0].StartCommon = 1_440_000 // breakpoint inserted a gap
	dShift, _ := withShift.Digest()
	if dShift == base {
		t.Fatal("timeline breakpoint shift must propagate to index digest")
	}

	withBreakpoint := sampleIndex()
	withBreakpoint.Breakpoints = append(withBreakpoint.Breakpoints,
		Breakpoint{TrackID: "v", SegmentIndex: 5, Kind: "wrap"})
	dBP, _ := withBreakpoint.Digest()
	if dBP == base {
		t.Fatal("breakpoint record must propagate to index digest")
	}

	withLoss := sampleIndex()
	withLoss.RTP[0].Recovered = 29
	withLoss.RTP[0].LossRecovery = 0.90625
	withLoss.UnrecoveredGaps = 2
	dLoss, _ := withLoss.Digest()
	if dLoss == base {
		t.Fatal("packet loss recovery must propagate to index digest")
	}
}
