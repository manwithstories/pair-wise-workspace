package index

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"medpipe/internal/model"
	"medpipe/internal/scan"
)

// fixture builds a small container, scans it and returns a valid index.
func fixture(t *testing.T, frames int) (raw []byte, idx *scan.ScanIndex, digest string, size int64) {
	t.Helper()
	// Recreate the deterministic container layout locally to avoid an import
	// cycle: scan tests own their builder, so the store test mirrors it.
	var buf bytes.Buffer
	if err := model.WriteHeader(&buf, uint32(frames), 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < frames; i++ {
		meta := []byte(`{"width":8,"height":8,"color":"yuv420","keyframe":true,"frame_num":0}`)
		payload := []byte{byte(i), byte(i >> 8), 0, 0}
		if _, err := model.WriteRecord(&buf, model.KindVideo, model.FlagKeyframe,
			uint32(i), int64(i)*40_000_000, meta, payload); err != nil {
			t.Fatal(err)
		}
	}
	raw = buf.Bytes()
	rd, err := scan.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rd.Scan()
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err = scan.DigestSource(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return raw, scan.BuildIndex(res, digest, size), digest, size
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	_, idx, digest, size := fixture(t, 5)

	if err := st.Save("clipA", idx); err != nil {
		t.Fatal(err)
	}
	rebuildCalls := 0
	got, reason, err := st.LoadOrRebuild("clipA", digest, size, func() (*scan.ScanIndex, error) {
		rebuildCalls++
		return idx, nil
	})
	if err != nil || reason != "" || rebuildCalls != 0 {
		t.Fatalf("stored index should be reused: reason=%q err=%v calls=%d", reason, err, rebuildCalls)
	}
	if got.SourceDigest != digest {
		t.Fatal("digest lost on reload")
	}
	// No temp file remains after an atomic save.
	if _, err := os.Stat(filepath.Join(dir, "clipA.idx.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file leaked")
	}
}

func TestRebuildWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	_, idx, digest, size := fixture(t, 3)
	got, reason, err := st.LoadOrRebuild("missing", digest, size, func() (*scan.ScanIndex, error) {
		return idx, nil
	})
	if err != nil || reason != ReasonAbsent || got == nil {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if _, err := os.Stat(st.Path("missing")); err != nil {
		t.Fatalf("rebuilt index not persisted: %v", err)
	}
}

func TestRebuildWhenDigestChanges(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	_, idx, _, size := fixture(t, 3)
	if err := st.Save("clip", idx); err != nil {
		t.Fatal(err)
	}
	altDigest := strings.Repeat("a", 64)
	got, reason, err := st.LoadOrRebuild("clip", altDigest, size, func() (*scan.ScanIndex, error) {
		idx2 := *idx
		idx2.SourceDigest = altDigest
		return &idx2, nil
	})
	if err != nil || reason != ReasonDigestChanged {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if got.SourceDigest != altDigest {
		t.Fatal("rebuilt index not saved with new digest")
	}
}

func TestRebuildWhenFieldsMissing(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	_, idx, digest, size := fixture(t, 3)
	if err := st.Save("clip", idx); err != nil {
		t.Fatal(err)
	}
	// Truncate the canonical file to produce invalid JSON.
	p := st.Path("clip")
	good, _ := os.ReadFile(p)
	if err := os.WriteFile(p, good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, reason, err := st.LoadOrRebuild("clip", digest, size, func() (*scan.ScanIndex, error) {
		return idx, nil
	})
	if err != nil {
		t.Fatalf("rebuild should recover: %v", err)
	}
	if reason != ReasonIncomplete && reason != ReasonFieldMissing {
		t.Fatalf("reason=%q", reason)
	}
}

func TestInterruptedTempNeverLoaded(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	_, idx, digest, size := fixture(t, 2)
	if err := st.MarkIncomplete("clip", []byte(`{"schema_version":`)); err != nil {
		t.Fatal(err)
	}
	got, reason, err := st.LoadOrRebuild("clip", digest, size, func() (*scan.ScanIndex, error) {
		return idx, nil
	})
	if err != nil || got == nil {
		t.Fatalf("rebuild failed: %v", err)
	}
	if reason != ReasonTempLeft && reason != ReasonAbsent {
		t.Fatalf("stranded temp must force rebuild, got %q", reason)
	}
}

func TestSaveRejectsInvalidIndex(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	bad := &scan.ScanIndex{SchemaVersion: 1}
	if err := st.Save("bad", bad); err == nil {
		t.Fatal("invalid index must not be persisted")
	}
}
