package cache_test

import (
	"errors"
	"os"
	"testing"

	"mediad/internal/cache"
	"mediad/internal/model"
)

func metaDigest(seed byte) [32]byte {
	var d [32]byte
	d[0] = seed
	return d
}

func TestPutThenGetVerifiesProvenance(t *testing.T) {
	st, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("frame payload bytes")
	prov := cache.Provenance{
		TrackID: 1, FrameNo: 42,
		MetaDigest:    model.FrameMeta{TrackID: 1, FrameNo: 42}.Digest(),
		ContentDigest: model.DigestBytes(payload),
	}
	if err := st.Put(prov, payload); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := st.Get(prov)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q", got)
	}
}

func TestGetMissesWhenAbsent(t *testing.T) {
	st, _ := cache.Open(t.TempDir())
	_, err := st.Get(cache.Provenance{ContentDigest: model.DigestBytes([]byte("x"))})
	if !errors.Is(err, cache.ErrMiss) {
		t.Fatalf("err = %v, want ErrMiss", err)
	}
}

func TestHitRejectedWhenTrackDiffers(t *testing.T) {
	st, _ := cache.Open(t.TempDir())
	payload := []byte("shared content")
	content := model.DigestBytes(payload)
	md := metaDigest(1)
	if err := st.Put(cache.Provenance{TrackID: 1, FrameNo: 7, MetaDigest: md, ContentDigest: content}, payload); err != nil {
		t.Fatal(err)
	}
	// Same content key, different track: must not hit.
	_, err := st.Get(cache.Provenance{TrackID: 2, FrameNo: 7, MetaDigest: md, ContentDigest: content})
	if !errors.Is(err, cache.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestHitRejectedWhenFrameNumberDiffers(t *testing.T) {
	st, _ := cache.Open(t.TempDir())
	payload := []byte("shared content")
	content := model.DigestBytes(payload)
	md := metaDigest(2)
	if err := st.Put(cache.Provenance{TrackID: 3, FrameNo: 7, MetaDigest: md, ContentDigest: content}, payload); err != nil {
		t.Fatal(err)
	}
	_, err := st.Get(cache.Provenance{TrackID: 3, FrameNo: 8, MetaDigest: md, ContentDigest: content})
	if !errors.Is(err, cache.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestHitRejectedWhenMetaDigestDiffers(t *testing.T) {
	st, _ := cache.Open(t.TempDir())
	payload := []byte("shared content")
	content := model.DigestBytes(payload)
	if err := st.Put(cache.Provenance{TrackID: 3, FrameNo: 7, MetaDigest: metaDigest(1), ContentDigest: content}, payload); err != nil {
		t.Fatal(err)
	}
	_, err := st.Get(cache.Provenance{TrackID: 3, FrameNo: 7, MetaDigest: metaDigest(9), ContentDigest: content})
	if !errors.Is(err, cache.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestTamperedPayloadInvalidatesHit(t *testing.T) {
	dir := t.TempDir()
	st, _ := cache.Open(dir)
	payload := []byte("original bytes")
	content := model.DigestBytes(payload)
	prov := cache.Provenance{TrackID: 1, FrameNo: 1, MetaDigest: metaDigest(1), ContentDigest: content}
	if err := st.Put(prov, payload); err != nil {
		t.Fatal(err)
	}
	// Flip a payload byte on disk, keeping the record otherwise intact.
	path := dir + "/" + cache.Key(content) + ".mdc"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(prov); !errors.Is(err, cache.ErrInvalid) {
		t.Fatalf("tampered payload err = %v, want ErrInvalid", err)
	}
}

func TestPutRejectsWrongContentDigest(t *testing.T) {
	st, _ := cache.Open(t.TempDir())
	err := st.Put(cache.Provenance{TrackID: 1, FrameNo: 1, ContentDigest: model.DigestBytes([]byte("other"))}, []byte("payload"))
	if !errors.Is(err, cache.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestVerifiedCounters(t *testing.T) {
	var v cache.Verified
	v.Record(nil)
	v.Record(cache.ErrMiss)
	v.Record(cache.ErrInvalid)
	if v.Hits != 1 || v.Misses != 1 || v.Invalid != 1 || v.Recomputed != 2 {
		t.Fatalf("counters %+v", v)
	}
}
