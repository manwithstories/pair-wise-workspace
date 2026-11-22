package main

import (
	"testing"

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

func TestChecksumExitCode(t *testing.T) {
	fe := fault.New(fault.KindChecksum, "cache", "mismatch")
	if code := exitFor(fe); code != 3 {
		t.Fatalf("exit code = %d want 3", code)
	}
}

func TestIOExitCode(t *testing.T) {
	fe := fault.New(fault.KindIO, "checkpoint", "boom")
	if code := exitFor(fe); code != 4 {
		t.Fatalf("exit code = %d want 4", code)
	}
}
