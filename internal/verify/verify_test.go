package verify

import (
	"testing"

	"relaycheck/internal/media"
	"relaycheck/internal/segment"
	"relaycheck/internal/txn"
)

func segBytes(i int32, samples []int16) []byte {
	s := &segment.Segment{
		H: segment.Header{
			Index: i, TrackID: 1, StartNum: 0, StartDen: 1,
			EndNum: 1, EndDen: 50, SampleRate: 48000, Frames: 2, Channels: 2,
		},
		Samples: samples,
	}
	return s.Encode()
}

func commitFixture(t *testing.T, n int) (string, [][]byte) {
	t.Helper()
	dir := t.TempDir()
	c, f := txn.NewCommitter(dir)
	if f != nil {
		t.Fatal(f)
	}
	bodies := make([][]byte, n)
	entries := make([]txn.ManifestEntry, n)
	for i := 0; i < n; i++ {
		b := segBytes(int32(i), []int16{int16(i + 1), 2, 3, 4})
		sha, f := c.CommitSegment(i, b)
		if f != nil {
			t.Fatal(f)
		}
		bodies[i] = b
		entries[i] = txn.ManifestEntry{
			Index: i, File: txn.SegmentFileName(i), Size: int64(len(b)),
			SHA256: sha, KeyframeID: i,
		}
	}
	if f := c.WriteManifest(&txn.Manifest{
		Seed: 25, TargetRate: 48000, Layout: "stereo", SegmentCount: n, Entries: entries,
	}); f != nil {
		t.Fatal(f)
	}
	c.Close()
	return dir, bodies
}

func TestVerifyAllHashesRoundtripAndKeyframes(t *testing.T) {
	dir, _ := commitFixture(t, 6)
	rep, f := VerifyDirectory(dir, 25)
	if f != nil {
		t.Fatal(f)
	}
	if rep.HashMatched != 6 || rep.RoundtripMatched != 6 {
		t.Fatalf("hash=%d/%d roundtrip=%d/%d", rep.HashMatched, rep.HashTotal, rep.RoundtripMatched, rep.RoundtripTotal)
	}
	if rep.KeyHits != 6 || rep.KeyTotal != 6 {
		t.Fatalf("keyframe %d/%d", rep.KeyHits, rep.KeyTotal)
	}
	if len(rep.Hashes) != 6 {
		t.Fatalf("hashes=%d", len(rep.Hashes))
	}
	if got := Percent(rep.HashMatched, rep.HashTotal); got != "100.00%" {
		t.Fatalf("percent=%q", got)
	}
}

func TestVerifyDetectsTamperedFragment(t *testing.T) {
	dir, _ := commitFixture(t, 4)
	c, f := txn.NewCommitter(dir)
	if f != nil {
		t.Fatal(f)
	}
	other := segBytes(1, []int16{9, 9, 9, 9}) // valid canonical bytes, different content
	if _, f := c.CommitSegment(1, other); f != nil {
		t.Fatal(f) // overwrites file but not the already-written manifest
	}
	c.Close()

	rep, f := VerifyDirectory(dir, 25)
	if f != nil {
		t.Fatal(f)
	}
	if rep.HashMatched != 3 {
		t.Fatalf("expected 3 manifest hashes to match after tamper, got %d/4", rep.HashMatched)
	}
}

func TestVerifyMissingManifestIsIntegrityFault(t *testing.T) {
	_, f := VerifyDirectory(t.TempDir(), 25)
	if f == nil || f.Code != media.CodeIntegrity || f.ExitCode() != 3 {
		t.Fatalf("want integrity_unrecoverable exit3, got %v", f)
	}
}

func TestCompareHashesAndPercentZero(t *testing.T) {
	m, total := CompareHashes([]string{"a", "b", "c"}, []string{"a", "x", "c"})
	if m != 2 || total != 3 {
		t.Fatalf("compare=%d/%d", m, total)
	}
	if Percent(0, 0) != "0.00%" {
		t.Fatal("zero total must be 0.00%")
	}
}
