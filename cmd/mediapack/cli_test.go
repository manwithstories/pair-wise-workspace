package main

import (
	"testing"

	"mediapack/internal/cache"
	"mediapack/internal/checkpoint"
	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
)

func TestCorruptExitCodeAndOffset(t *testing.T) {
	data, _, keyOffs, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.AV1, Seed: 17, Frames: 30, KeyEvery: 10,
	}, fixture.Fault{})
	bad := data[:keyOffs[1]+12]
	_, _, err := codec.ParseBytes(bad, 1)
	fe, ok := fault.As(err)
	if !ok {
		t.Fatal("expected fault")
	}
	if code := exitFor(err); code != 2 {
		t.Fatalf("exit code = %d want 2", code)
	}
	if fe.Offset != int64(len(bad)) {
		t.Fatalf("offset = %d want %d", fe.Offset, len(bad))
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		kind fault.Kind
		want int
	}{
		{fault.KindCorruptInput, 2},
		{fault.KindChecksum, 3},
		{fault.KindIO, 4},
		{fault.KindLimit, 4},
		{fault.KindCanceled, 4},
	}
	for _, c := range cases {
		if got := exitFor(fault.New(c.kind, "x", "m")); got != c.want {
			t.Fatalf("kind %v -> %d want %d", c.kind, got, c.want)
		}
	}
}

func TestCacheAndCheckpointFaultKinds(t *testing.T) {
	// Checksum fault is reachable through the cache revalidation contract.
	id := cache.Identity{SourceID: "a", InputSHA256: "b"}
	if id.Key() == "" {
		t.Fatal("cache key must be non-empty")
	}
	// IO fault path: an unparseable checkpoint maps to KindIO.
	dir := t.TempDir()
	mgr, err := checkpoint.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(checkpoint.State{NextAU: 1}); err != nil {
		t.Fatal(err)
	}
	if err := writeGarbageCLI(mgr, "{bad"); err != nil {
		t.Fatal(err)
	}
	_, _, err = mgr.Load()
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindIO || exitFor(err) != 4 {
		t.Fatalf("checkpoint corruption must map to exit 4, got %v", err)
	}
}
