package hash

import (
	"math"
	"testing"
)

func samplePlanes(n int, v float32) [][]float32 {
	return [][]float32{
		makeFill(n, v),
		makeFill(n, -v),
	}
}

func makeFill(n int, v float32) []float32 {
	p := make([]float32, n)
	for i := range p {
		p[i] = v
	}
	return p
}

func TestDigestStableAcrossRuns(t *testing.T) {
	seg := Segment{Index: 0, StartUS: 0, EndUS: 1_000_000}
	a := NewSegmentHasher(seg)
	b := NewSegmentHasher(seg)
	for _, h := range []*Hasher{a, b} {
		if err := h.AddAudio(samplePlanes(48000, 0.25)); err != nil {
			t.Fatal(err)
		}
		if err := h.AddRecord("subtitle", 200_000, []byte("hello")); err != nil {
			t.Fatal(err)
		}
	}
	ra, rb := a.Digest(seg), b.Digest(seg)
	if ra.SHA256 != rb.SHA256 {
		t.Fatalf("nondeterministic digest %s != %s", ra.SHA256, rb.SHA256)
	}
	if ra.Frames != 48000 || ra.Records != 1 || ra.Bytes <= 0 {
		t.Fatalf("report: %+v", ra)
	}
	if err := Compare([]Report{ra}, []Report{rb}); err != nil {
		t.Fatal(err)
	}
}

func TestDigestSensitiveToEveryField(t *testing.T) {
	base := func() Report {
		seg := Segment{Index: 3, StartUS: 1000, EndUS: 2000}
		h := NewSegmentHasher(seg)
		h.AddAudio(samplePlanes(16, 0.5))
		h.AddRecord("id3", 1500, []byte("x"))
		return h.Digest(seg)
	}
	want := base().SHA256

	// Segment boundary change.
	seg := Segment{Index: 3, StartUS: 1001, EndUS: 2000}
	h := NewSegmentHasher(seg)
	h.AddAudio(samplePlanes(16, 0.5))
	h.AddRecord("id3", 1500, []byte("x"))
	if h.Digest(seg).SHA256 == want {
		t.Fatal("boundary change not reflected")
	}
	// Sample change.
	h = NewSegmentHasher(seg)
	h.AddAudio(samplePlanes(16, 0.25))
	h.AddRecord("id3", 1500, []byte("x"))
	if h.Digest(seg).SHA256 == want {
		t.Fatal("sample change not reflected")
	}
	// Record position change.
	h = NewSegmentHasher(seg)
	h.AddAudio(samplePlanes(16, 0.5))
	h.AddRecord("id3", 1600, []byte("x"))
	if h.Digest(seg).SHA256 == want {
		t.Fatal("record position change not reflected")
	}
}

func TestFramingPreventsConcatenationCollision(t *testing.T) {
	seg := Segment{Index: 0, StartUS: 0, EndUS: 10}
	a := NewSegmentHasher(seg)
	a.AddRecord("k", 0, []byte("ab"))
	a.AddRecord("k", 0, []byte("c"))
	b := NewSegmentHasher(seg)
	b.AddRecord("k", 0, []byte("a"))
	b.AddRecord("k", 0, []byte("bc"))
	if a.Digest(seg).SHA256 == b.Digest(seg).SHA256 {
		t.Fatal("framing collision: payload split not encoded")
	}
}

func TestRejectsOversizeRecord(t *testing.T) {
	h := NewSegmentHasher(Segment{})
	if err := h.AddRecord("id3", 0, make([]byte, 1<<20+1)); err == nil {
		t.Fatal("oversize record accepted")
	}
}

func TestRejectsRaggedAudio(t *testing.T) {
	h := NewSegmentHasher(Segment{})
	if err := h.AddAudio([][]float32{make([]float32, 10), make([]float32, 9)}); err == nil {
		t.Fatal("ragged planes accepted")
	}
}

func TestCompareDetectsMismatch(t *testing.T) {
	a := []Report{{Segment: Segment{Index: 0, StartUS: 0, EndUS: 10}, SHA256: "aa"}}
	b := []Report{{Segment: Segment{Index: 0, StartUS: 0, EndUS: 10}, SHA256: "bb"}}
	if err := Compare(a, b); err == nil {
		t.Fatal("digest mismatch not detected")
	}
	c := []Report{{Segment: Segment{Index: 0, StartUS: 0, EndUS: 11}, SHA256: "aa"}}
	if err := Compare(a, c); err == nil {
		t.Fatal("boundary mismatch not detected")
	}
	if err := Compare(a, nil); err == nil {
		t.Fatal("count mismatch not detected")
	}
}

func TestSegmentValidation(t *testing.T) {
	if !(Segment{Index: 0, StartUS: 0, EndUS: 1}).Valid() {
		t.Fatal("valid segment rejected")
	}
	for _, s := range []Segment{
		{Index: -1},
		{Index: 0, StartUS: -1, EndUS: 1},
		{Index: 0, StartUS: 10, EndUS: 5},
	} {
		if s.Valid() {
			t.Fatalf("invalid segment accepted: %+v", s)
		}
	}
	if d := (Segment{StartUS: 500, EndUS: 1500}).Duration(); d != 1000 {
		t.Fatalf("duration %d", d)
	}
}

func TestDigestWriter(t *testing.T) {
	w := NewDigestWriter()
	w.Write([]byte("abc"))
	if w.Bytes() != 3 || math.IsNaN(0) {
		t.Fatalf("bytes %d", w.Bytes())
	}
	w2 := NewDigestWriter()
	w2.Write([]byte("abc"))
	if w.Digest() != w2.Digest() {
		t.Fatal("digest unstable")
	}
}
