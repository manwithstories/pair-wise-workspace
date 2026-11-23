package fingerprint_test

import (
	"bytes"
	"testing"

	"bcast/demux"
	"bcast/fingerprint"
	"bcast/internal/fixture"
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
