package model

import (
	"crypto/sha256"
	"testing"
)

func TestCanonicalBytesStableAndDistinct(t *testing.T) {
	base := FrameMeta{
		TrackID: 1, FrameNo: 7, CaptureUnits: 1700000000007,
		Width: 64, Height: 48, CaptureUPS: 1000, Timescale: 90000,
		Color: Color{Primaries: 1, Transfer: 13, Matrix: 6, Range: 1},
	}
	if a, b := base.Digest(), base.Digest(); a != b {
		t.Fatalf("digest not stable: %x vs %x", a, b)
	}
	if len(base.CanonicalBytes()) != len(base.CanonicalBytes()) {
		t.Fatal("canonical length varies")
	}
	// Every identity-bearing field must change the digest.
	variants := []FrameMeta{
		func() FrameMeta { m := base; m.TrackID++; return m }(),
		func() FrameMeta { m := base; m.FrameNo++; return m }(),
		func() FrameMeta { m := base; m.CaptureUnits++; return m }(),
		func() FrameMeta { m := base; m.Width++; return m }(),
		func() FrameMeta { m := base; m.Height++; return m }(),
		func() FrameMeta { m := base; m.CaptureUPS++; return m }(),
		func() FrameMeta { m := base; m.Timescale++; return m }(),
		func() FrameMeta { m := base; m.Color.Transfer++; return m }(),
		func() FrameMeta { m := base; m.Color.Range ^= 1; return m }(),
	}
	seen := map[[32]byte]int{base.Digest(): 0}
	for i, v := range variants {
		d := v.Digest()
		if d == base.Digest() {
			t.Fatalf("variant %d collides with base", i)
		}
		if prev, ok := seen[d]; ok {
			t.Fatalf("variant %d collides with variant %d", i, prev)
		}
		seen[d] = i
	}
}

func TestFrameStatusSemantics(t *testing.T) {
	cases := map[FrameStatus]struct {
		name   string
		usable bool
	}{
		FrameOK:            {"ok", true},
		FrameHashMismatch:  {"hash_mismatch", false},
		FrameImageInvalid:  {"image_invalid", false},
		FrameTruncatedTail: {"truncated_tail", false},
		FrameDuplicate:     {"duplicate", false},
		FrameConflict:      {"conflict", false},
	}
	for st, want := range cases {
		if st.String() != want.name {
			t.Fatalf("%d: name %q, want %q", st, st.String(), want.name)
		}
		if st.Usable() != want.usable {
			t.Fatalf("%s: usable %v, want %v", st, st.Usable(), want.usable)
		}
	}
}

func TestPayloadDigestHelper(t *testing.T) {
	a := sha256.Sum256([]byte("frame-a"))
	if DigestBytes([]byte("frame-a")) != a {
		t.Fatal("DigestBytes mismatch")
	}
	if DigestBytes([]byte("frame-b")) == a {
		t.Fatal("distinct payloads collided")
	}
}
