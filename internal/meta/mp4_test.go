package meta

import (
	"bytes"
	"testing"

	"relaycheck/internal/media"
)

func TestMP4OkParsesAndReconciles(t *testing.T) {
	m, f := ReadMP4(BuildMP4Case("ok"))
	if f != nil {
		t.Fatal(f)
	}
	if len(m.Tracks) != 1 {
		t.Fatalf("tracks=%d", len(m.Tracks))
	}
	tr := m.Tracks[0]
	if tr.ID != 1 || tr.TimeScale != 48000 || tr.Duration != 192000 {
		t.Fatalf("bad track timing: %+v", tr)
	}
	if tr.Rotation.Degrees != 90 {
		t.Fatalf("rotation=%d want 90", tr.Rotation.Degrees)
	}
	d, f := m.Reconcile(Layer{}, Layer{})
	if f != nil {
		t.Fatal(f)
	}
	if d.Tracks[0].Color.Primaries != "bt2020" {
		t.Fatalf("track color should win: %+v", d.Tracks[0].Color)
	}
	if len(d.Conflicts) != 1 {
		t.Fatalf("want one container/track color conflict, got %d", len(d.Conflicts))
	}
}

func TestMP4AnomalyCases(t *testing.T) {
	cases := []struct {
		name string
		code media.Code
	}{
		{"illegal-rotation", media.CodeInvalidRotation},
		{"bad-crop", media.CodeInvalidCrop},
		{"unknown-color", media.CodeUnknownColor},
	}
	for _, c := range cases {
		m, rf := ReadMP4(BuildMP4Case(c.name))
		if rf != nil {
			t.Fatalf("%s: read failed early: %v", c.name, rf)
		}
		_, f := m.Reconcile(Layer{}, Layer{})
		if f == nil || f.Code != c.code {
			t.Fatalf("%s: reconcile code=%v want %s (%v)", c.name, f, c.code, f)
		}
	}
}

func TestMP4TruncatedReportsTrackAndRange(t *testing.T) {
	_, f := ReadMP4(BuildMP4Case("truncated-mp4"))
	if f == nil || f.Code != media.CodeParseError {
		t.Fatalf("want parse_error, got %v", f)
	}
	if f.TrackID != 1 {
		t.Fatalf("track_id=%d want 1", f.TrackID)
	}
	if f.Range == nil {
		t.Fatal("expected time_range on truncated fault")
	}
	if f.ExitCode() != 2 {
		t.Fatalf("exit=%d want 2", f.ExitCode())
	}
}

func TestMP4BadLengthAndOversizedSample(t *testing.T) {
	if _, f := ReadMP4(BuildMP4Case("bad-length")); f == nil || f.Code != media.CodeParseError {
		t.Fatalf("bad-length: %v", f)
	}
	if _, f := ReadMP4(BuildMP4Case("oversized-sample")); f == nil || f.Code != media.CodeBufferLimit {
		t.Fatalf("oversized-sample: %v", f)
	}
}

func TestDecodeBoxHeaderRejectsOverflowAndShort(t *testing.T) {
	if _, f := decodeBoxHeader([]byte{0, 0, 0, 8, 'f', 't'}, 0); f == nil {
		t.Fatal("short header must be rejected")
	}
	// size below header.
	if _, f := decodeBoxHeader([]byte{0, 0, 0, 4, 'f', 'r', 'e', 'e'}, 0); f == nil || f.Code != media.CodeParseError {
		t.Fatalf("sub-header size: %v", f)
	}
	// 64-bit largesize claiming beyond buffer.
	b := make([]byte, 16)
	b[3] = 1
	copy(b[4:8], "mdat")
	for i := 8; i < 16; i++ {
		b[i] = 0xff
	}
	if _, f := decodeBoxHeader(b, 0); f == nil || f.Code != media.CodeParseError {
		t.Fatalf("huge largesize must be safely rejected, got %v", f)
	}
}

func TestCheckStszRejectsCountOverflow(t *testing.T) {
	p := make([]byte, 12)
	p[8], p[9], p[10], p[11] = 0xff, 0xff, 0xff, 0xff // count huge, variable sizes
	if f := checkStsz(p); f == nil || f.Code != media.CodeParseError {
		t.Fatalf("count overflow/truncation: %v", f)
	}
}

func TestSampleBudgetEnforcesCap(t *testing.T) {
	b := NewSampleBudget(9) // argument is track id; limit is the 64 MiB cap
	if f := b.Reserve(MaxTrackBuffer + 1); f == nil || f.Code != media.CodeBufferLimit {
		t.Fatalf("oversized sample: %v", f)
	}
	small := &SampleBudget{TrackID: 1, Limit: 8}
	if f := small.Reserve(4); f != nil {
		t.Fatal(f)
	}
	if f := small.Reserve(4); f != nil {
		t.Fatal(f)
	}
	if f := small.Reserve(1); f == nil || f.Code != media.CodeBufferLimit {
		t.Fatalf("cumulative cap: %v", f)
	}
	small.Release(4)
	if f := small.Reserve(1); f != nil {
		t.Fatalf("reserve after release: %v", f)
	}
}

func TestMP4FixtureBytesAreDeterministic(t *testing.T) {
	if !bytes.Equal(BuildMP4Case("ok"), BuildMP4Case("ok")) {
		t.Fatal("mp4 fixture not deterministic")
	}
}
