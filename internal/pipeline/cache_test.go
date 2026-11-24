package pipeline

import (
	"os"
	"testing"

	"mediad/internal/cache"
	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/model"
)

// TestPoisonedCacheEntryIsRejectedAndRecomputed seeds the cache with an entry
// whose content key matches a real frame but whose provenance names a different
// track. A hit must be refused, the frame recomputed, and the index must still
// describe the true frame.
func TestPoisonedCacheEntryIsRejectedAndRecomputed(t *testing.T) {
	dir := t.TempDir()
	p := gen(t, dir, 1, 30, 0, false)

	// Read frame 5's true payload from the source to know its content key.
	src, err := os.Open(p.Source())
	if err != nil {
		t.Fatal(err)
	}
	cur, err := ingest.NewCursor(src, nil)
	if err != nil {
		src.Close()
		t.Fatal(err)
	}
	var content [32]byte
	var payload []byte
	for {
		fr, err := cur.Next()
		if err != nil {
			break
		}
		if fr.Meta.FrameNo == 5 {
			content = fr.Digest
			if fb, rerr := ingest.ReadFrameAt(src, fr.Offset, uint64(fr.Size)); rerr == nil {
				payload = fb.Payload
			}
			break
		}
	}
	src.Close()
	if content == ([32]byte{}) || payload == nil {
		t.Fatal("frame 5 not found")
	}

	store, err := cache.Open(p.CacheDir())
	if err != nil {
		t.Fatal(err)
	}
	// Poison: same content digest, wrong track provenance.
	prov := cache.Provenance{
		TrackID: 999, FrameNo: 5,
		MetaDigest:    model.FrameMeta{TrackID: 999, FrameNo: 5}.Digest(),
		ContentDigest: content,
	}
	if err := store.Put(prov, payload); err != nil {
		t.Fatal(err)
	}

	res, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: 30})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.CacheInvalid != 1 {
		t.Fatalf("cache_invalid = %d, want 1 for the poisoned entry", res.CacheInvalid)
	}
	if res.Metrics.IndexEntries != 30 {
		t.Fatalf("index entries = %d, want 30", res.Metrics.IndexEntries)
	}
	// The indexed entry must bind the true frame, not the poisoned provenance.
	entries, _, err := index.Open(p.Index())
	if err != nil {
		t.Fatal(err)
	}
	e := index.LookupByFrame(entries, 5)
	if e == nil || e.TrackID != 1 {
		t.Fatalf("frame 5 entry = %+v, want track 1", e)
	}
}
