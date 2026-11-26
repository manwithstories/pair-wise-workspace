package verify_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
	"mediaroute/internal/model"
	"mediaroute/internal/mp4"
	"mediaroute/internal/output"
	"mediaroute/internal/repair"
	"mediaroute/internal/verify"
)

// buildChunk scans one fixture representation, cuts its first 60 s
// segment and writes it as a chunk, returning everything verify needs.
func buildChunk(t *testing.T) (chunkPath, sha, digest string, ix *index.Index, start int64) {
	t.Helper()
	dir := t.TempDir()
	g, err := fixture.Generate(fixture.Spec{
		Seed: 3, Minutes: 4, Cameras: 4, Bitrates: []uint32{100, 200, 400}, Dir: dir,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	src := filepath.Join(g.BaseDir, "media/cam0_100.mp4")
	rt, err := mp4.ScanFile(src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	tr, err := repair.RepairTrack(rt, model.RepresentationID{Camera: "cam0", Bitrate: 100})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	ix = index.Build(tr)
	segEnd := int64(60 * 90000)
	var seg []model.Sample
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var payload []byte
	for _, s := range tr.Samples {
		if s.Truncated || s.Clock >= segEnd {
			continue
		}
		buf := make([]byte, s.Size)
		if _, err := f.ReadAt(buf, int64(s.Offset)); err != nil {
			t.Fatalf("read sample: %v", err)
		}
		seg = append(seg, s)
		payload = append(payload, buf...)
	}
	if len(seg) == 0 {
		t.Fatal("empty segment")
	}
	start = seg[0].Clock
	digest = verify.DigestSamples(seg)
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chunkPath, sha, _, err = output.WriteChunk(outDir, "cam0_100_seg0000.mp4",
		output.Chunk{Timescale: tr.Timescale, Samples: seg, Payload: payload})
	if err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	return chunkPath, sha, digest, ix, start
}

func TestVerifyChunkOK(t *testing.T) {
	path, sha, digest, ix, start := buildChunk(t)
	r := verify.VerifyChunk(path, sha, digest, ix, start)
	if !r.OK() {
		t.Fatalf("report=%+v", r)
	}
	var s verify.Summary
	s.Add(r)
	if s.Chunks != 1 || s.HashMatch != 1 || s.Roundtrip != 1 || s.KeyHit != 1 {
		t.Fatalf("summary=%+v", s)
	}
}

func TestVerifyChunkDetectsCorruption(t *testing.T) {
	path, sha, digest, ix, start := buildChunk(t)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xFF, 0xFF, 0xFF}, 40); err != nil { // inside mdat
		t.Fatal(err)
	}
	f.Close()
	r := verify.VerifyChunk(path, sha, digest, ix, start)
	if r.SHA256Match {
		t.Fatal("corrupted chunk still matches SHA-256")
	}
	if r.OK() {
		t.Fatal("corrupted chunk passes verification")
	}
}

func TestVerifyChunkDetectsDigestMismatch(t *testing.T) {
	path, sha, _, ix, start := buildChunk(t)
	r := verify.VerifyChunk(path, sha, "0000dead", ix, start)
	if r.RoundtripEqual {
		t.Fatal("digest mismatch not detected")
	}
	if !r.SHA256Match || !r.KeyframeHit {
		t.Fatalf("unrelated checks failed: %+v", r)
	}
}
