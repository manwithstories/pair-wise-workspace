package fingerprint_test

import (
	"bytes"
	"errors"
	"testing"

	"bcast/demux"
	"bcast/fingerprint"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/segments"
)

func hashes(t *testing.T, data []byte) []string {
	t.Helper()
	f, err := demux.Parse(bytes.NewReader(data), int64(len(data)), "in")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	segs, err := segments.Cut(&f.File, 90000)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	out := make([]string, len(segs))
	for i, s := range segs {
		h, err := fingerprint.Hash(bytes.NewReader(data), s)
		if err != nil {
			t.Fatalf("hash seg %d: %v", i, err)
		}
		out[i] = h
	}
	return out
}

func TestDuplicateDetection(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	hs := hashes(t, a.Clean)
	if len(hs) != 12 {
		t.Fatalf("hashes=%d", len(hs))
	}
	groups := fingerprint.Dedup(hs)
	if len(groups) != 1 {
		t.Fatalf("dup groups=%d", len(groups))
	}
	for _, idxs := range groups {
		if len(idxs) != 2 || idxs[0] != 3 || idxs[1] != 7 {
			t.Fatalf("dup idxs=%v", idxs)
		}
	}
}

func TestHashDeterministic(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	ha, hb := hashes(t, a.Clean), hashes(t, b.Clean)
	for i := range ha {
		if ha[i] != hb[i] {
			t.Fatalf("seg %d hash unstable", i)
		}
	}
}

func TestIdempotencyKey(t *testing.T) {
	k1 := fingerprint.Key("in", "p", 3)
	k2 := fingerprint.Key("in", "p", 3)
	k3 := fingerprint.Key("in", "p", 4)
	if k1 != k2 || k1 == k3 {
		t.Fatal("key semantics broken")
	}
}

func TestHashRejectsTruncatedPayload(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	f, err := demux.Parse(bytes.NewReader(a.Clean), int64(len(a.Clean)), "clean")
	if err != nil {
		t.Fatal(err)
	}
	segs, err := segments.Cut(&f.File, 90000)
	if err != nil {
		t.Fatal(err)
	}
	// Present only a prefix: the last segment's payload runs past the end.
	cut := a.Clean[:len(a.Clean)-4096]
	_, err = fingerprint.Hash(bytes.NewReader(cut), segs[len(segs)-1])
	if !errors.Is(err, fingerprint.ErrShortPayload) {
		t.Fatalf("want ErrShortPayload, got %v", err)
	}
}

func TestHashIgnoresTimestampsKeepsContent(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	f, err := demux.Parse(bytes.NewReader(a.Clean), int64(len(a.Clean)), "clean")
	if err != nil {
		t.Fatal(err)
	}
	segs, err := segments.Cut(&f.File, 90000)
	if err != nil {
		t.Fatal(err)
	}
	// Segments 3 and 7 carry identical content at different positions; their
	// hashes must match. Shifting timestamps must not change a hash.
	h3, _ := fingerprint.Hash(bytes.NewReader(a.Clean), segs[3])
	h7, _ := fingerprint.Hash(bytes.NewReader(a.Clean), segs[7])
	if h3 != h7 {
		t.Fatal("duplicate content hashed differently")
	}
	moved := segs[3]
	moved.Video = append([]media.Sample(nil), segs[3].Video...)
	for i := range moved.Video {
		moved.Video[i].DTS.Value += 123456
	}
	hShift, _ := fingerprint.Hash(bytes.NewReader(a.Clean), moved)
	if hShift != h3 {
		t.Fatal("timestamp shift changed content hash")
	}
}
