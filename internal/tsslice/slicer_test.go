package tsslice

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeStream renders one synthetic TS file: 25fps, keyframes every gopMs,
// PTS base ptsBase (90kHz ticks), duration durMs.
func writeStream(t *testing.T, path string, gopMs, durMs int, ptsBase int64) {
	t.Helper()
	var buf bytes.Buffer
	mux := NewMuxer(&buf)
	if err := mux.WritePATPMT(); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 900)
	for ms := 0; ms < durMs; ms += 40 {
		pts := ptsBase + int64(ms)*90
		key := ms%gopMs == 0
		if key {
			if err := mux.WritePATPMT(); err != nil {
				t.Fatal(err)
			}
		}
		if err := mux.WritePES(PIDVideo, 0xE0, pts, key, payload); err != nil {
			t.Fatal(err)
		}
		if err := mux.WritePES(PIDAudio, 0xC0, pts, false, payload[:200]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanFindsKeyframesAndPTS(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.ts")
	writeStream(t, f, 2000, 6000, 0)
	r, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var keys, ptsCount int
	var lastPTS int64
	err = Scan(r, func(info Info) error {
		if info.Kind == KindVideo && info.HasPTS {
			ptsCount++
			lastPTS = info.PTS
			if info.Keyframe {
				keys++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if keys != 3 {
		t.Fatalf("keyframes=%d want 3", keys)
	}
	if ptsCount != 150 {
		t.Fatalf("video PES=%d want 150", ptsCount)
	}
	if lastPTS != 5960*90 {
		t.Fatalf("lastPTS=%d want %d", lastPTS, 5960*90)
	}
}

func TestSliceEvenGOP(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ts")
	writeStream(t, in, 2000, 10000, 0)
	out := filepath.Join(dir, "out")
	res, err := Slice([]Input{{Path: in}}, out, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 5 {
		t.Fatalf("segments=%d want 5 (%+v)", len(res.Segments), res.Segments)
	}
	for i, s := range res.Segments {
		if s.DurationMs != 2000 {
			t.Fatalf("segment %d duration=%d want 2000", i, s.DurationMs)
		}
		if s.Seq != i {
			t.Fatalf("segment %d seq=%d", i, s.Seq)
		}
		if _, err := os.Stat(filepath.Join(out, s.File)); err != nil {
			t.Fatalf("missing output %s: %v", s.File, err)
		}
	}
	if res.RebaseEvents != 0 {
		t.Fatalf("rebase=%d want 0", res.RebaseEvents)
	}
}

func TestSliceOverlongGOPSplits(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ts")
	writeStream(t, in, 5000, 10000, 0) // 5s GOP, 2s target -> split in two
	out := filepath.Join(dir, "out")
	res, err := Slice([]Input{{Path: in}}, out, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 4 {
		t.Fatalf("segments=%d want 4 (%+v)", len(res.Segments), res.Segments)
	}
	for i, s := range res.Segments {
		if s.DurationMs != 2500 {
			t.Fatalf("segment %d duration=%d want 2500", i, s.DurationMs)
		}
	}
}

func TestSlicePTSRebase(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.ts")
	b := filepath.Join(dir, "b.ts")
	writeStream(t, a, 2000, 6000, 0)
	writeStream(t, b, 2000, 6000, 9000) // encoder reset: PTS back near zero
	out := filepath.Join(dir, "out")
	res, err := Slice([]Input{{Path: a}, {Path: b}}, out, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if res.RebaseEvents == 0 {
		t.Fatalf("expected rebase events")
	}
	for i, s := range res.Segments {
		if s.DurationMs <= 0 || s.DurationMs > 4000 {
			t.Fatalf("segment %d duration=%d out of bounds", i, s.DurationMs)
		}
	}
}

func TestSliceGapSlots(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.ts")
	b := filepath.Join(dir, "b.ts")
	writeStream(t, a, 2000, 4000, 0)
	writeStream(t, b, 2000, 4000, 4000*90)
	out := filepath.Join(dir, "out")
	res, err := Slice([]Input{{Path: a}, {Path: b, GapBefore: 2}}, out, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) != 1 || res.Gaps[0].Count != 2 {
		t.Fatalf("gaps=%+v want one gap of 2", res.Gaps)
	}
	// Sequences must skip the gap slots.
	wantSeq := []int{0, 1, 4, 5}
	if len(res.Segments) != len(wantSeq) {
		t.Fatalf("segments=%d want %d", len(res.Segments), len(wantSeq))
	}
	for i, s := range res.Segments {
		if s.Seq != wantSeq[i] {
			t.Fatalf("segment %d seq=%d want %d", i, s.Seq, wantSeq[i])
		}
	}
}
