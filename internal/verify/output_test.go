package verify

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/index"
	"archivecheck/internal/parse"
	"archivecheck/internal/repair"
)

func sample(dur, size, flags uint32, offset int64) parse.Sample {
	return parse.Sample{TrackID: 1, Duration: dur, Size: size, Flags: flags, Offset: offset}
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func writeFixture(t *testing.T) (string, []byte, repair.FragmentTimeline) {
	t.Helper()
	src := append(payload(4, 1), payload(4, 9)...)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	srcFile, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srcFile.Close() })
	ft := repair.FragmentTimeline{
		Segment: "frag-001.m4s", TrackID: 1, Sequence: 1, Timescale: 90000,
		Samples: []repair.SampleStamp{
			{Sample: sample(3000, 4, 0x02000000, 0), DTS: 0, EndDTS: 3000},
			{Sample: sample(3000, 4, 0x01010000, 4), DTS: 3000, EndDTS: 6000},
		},
	}
	outPath := filepath.Join(dir, "out.acx")
	w, err := NewWriter(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteFragment(ft, srcFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	return outPath, src, ft
}

func TestWriteThenScanDeterministic(t *testing.T) {
	path, src, _ := writeFixture(t)
	scan, err := ScanFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Records) != 1 || len(scan.Entries) != 2 {
		t.Fatalf("scan = %+v", scan)
	}
	if scan.Entries[0].Offset >= scan.Entries[1].Offset {
		t.Fatal("entry offsets not ordered")
	}
	for i, e := range scan.Entries {
		got := make([]byte, e.Size)
		f, _ := os.Open(path)
		f.ReadAt(got, e.Offset)
		f.Close()
		want := src[i*4 : i*4+4]
		if !bytes.Equal(got, want) {
			t.Fatalf("entry %d payload = %x, want %x", i, got, want)
		}
	}
	// Rebuild the same input twice; output SHA-256 must be identical.
	path2, _, _ := writeFixture(t)
	a, _ := os.ReadFile(path)
	b, _ := os.ReadFile(path2)
	if !bytes.Equal(a, b) {
		t.Fatal("repeated writes are not byte-identical")
	}
	sum := sha256.Sum256(a)
	if scan.OutputSHA256 != hexOf(sum[:]) {
		t.Fatalf("sha mismatch: %s", scan.OutputSHA256)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0xf]
	}
	return string(out)
}

func TestCheckDetectsRegressionAndBadOffset(t *testing.T) {
	path, _, _ := writeFixture(t)
	cp := &index.Checkpoint{Version: index.CheckpointVersion, CommittedDigest: "x", OutputBytes: 1}
	rep, err := Check(path, cp)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verified || len(rep.Reasons) == 0 {
		t.Fatalf("expected mismatch report, got %+v", rep)
	}

	scan, err := ScanFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	good := &index.Checkpoint{
		Version: index.CheckpointVersion, CommittedDigest: scan.ContentDigest,
		OutputBytes: scan.ContentBytes, Index: &index.Index{Entries: scan.Entries},
	}
	rep, err = Check(path, good)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Verified {
		t.Fatalf("reasons = %v", rep.Reasons)
	}

	bad := &index.Checkpoint{
		Version: index.CheckpointVersion, CommittedDigest: scan.ContentDigest,
		OutputBytes: scan.ContentBytes,
		Index:       &index.Index{Entries: []index.Entry{{Offset: 1 << 30, Size: 4, DTS: 0, EndDTS: 10}}},
	}
	rep, err = Check(path, bad)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verified || rep.InvalidIndexOffsets == 0 {
		t.Fatalf("expected invalid offset report, got %+v", rep)
	}
}

func TestScanLimitedCompletedOutput(t *testing.T) {
	path, _, _ := writeFixture(t)
	full, err := ScanFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A checkpoint of a completed run stores the content byte length, which
	// ends at the end marker and excludes the trailing digest.
	limited, err := ScanFile(path, full.ContentBytes)
	if err != nil {
		t.Fatalf("limited scan of completed output failed: %v", err)
	}
	if limited.ContentDigest != full.ContentDigest {
		t.Fatalf("limited digest %s != full %s", limited.ContentDigest, full.ContentDigest)
	}
	if len(limited.Entries) != len(full.Entries) {
		t.Fatalf("limited entries %d != %d", len(limited.Entries), len(full.Entries))
	}
}
