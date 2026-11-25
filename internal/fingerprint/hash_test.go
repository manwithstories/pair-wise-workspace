package fingerprint

import (
	"regexp"
	"testing"
)

func sampleWindow(id string) CanonicalWindow {
	geo := DefaultGeometry
	frames := make([][]byte, geo.FramesPerWindow)
	for i := range frames {
		f := make([]byte, geo.Width*geo.Height)
		for p := range f {
			f[p] = byte((p + i*7) % 256)
		}
		frames[i] = f
	}
	pcm := make([]int16, 480) // 10 ms at 48 kHz
	for i := range pcm {
		pcm[i] = int16(i % 1000)
	}
	return CanonicalWindow{
		SegmentID:  id,
		AudioRate:  48000,
		MonoPCM:    pcm,
		Geometry:   geo,
		GrayFrames: frames,
		ColorTags:  [4]uint16{2, 2, 1, 0},
	}
}

func TestFingerprintStableAndHex(t *testing.T) {
	w := sampleWindow("seg-001")
	fp1, err := Fingerprint(w)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := Fingerprint(w)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatal("fingerprint not stable across calls")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fp1) {
		t.Fatalf("fingerprint %q is not 64-char lowercase hex", fp1)
	}
}

func TestFingerprintContentSensitive(t *testing.T) {
	base := sampleWindow("a")
	fpBase, _ := Fingerprint(base)

	changedAudio := base
	changedAudio.MonoPCM = append([]int16(nil), base.MonoPCM...)
	changedAudio.MonoPCM[0] += 1
	if fp, _ := Fingerprint(changedAudio); fp == fpBase {
		t.Fatal("audio change did not alter fingerprint")
	}

	changedFrame := base
	changedFrame.GrayFrames = append([][]byte(nil), base.GrayFrames...)
	f0 := append([]byte(nil), base.GrayFrames[0]...)
	f0[0] ^= 0xff
	changedFrame.GrayFrames[0] = f0
	if fp, _ := Fingerprint(changedFrame); fp == fpBase {
		t.Fatal("video change did not alter fingerprint")
	}

	changedColor := base
	changedColor.ColorTags[0] = 9
	if fp, _ := Fingerprint(changedColor); fp == fpBase {
		t.Fatal("color tag change did not alter fingerprint")
	}
}

func TestFingerprintRejectsBadGeometry(t *testing.T) {
	w := sampleWindow("bad")
	w.Geometry.Width = 0
	if _, err := Fingerprint(w); err == nil {
		t.Fatal("zero-width geometry accepted")
	}
	w2 := sampleWindow("bad2")
	w2.GrayFrames = append(w2.GrayFrames, make([]byte, w2.Geometry.Width*w2.Geometry.Height))
	if _, err := Fingerprint(w2); err == nil {
		t.Fatal("too many frames accepted")
	}
	w3 := sampleWindow("bad3")
	w3.AudioRate = 0
	if _, err := Fingerprint(w3); err == nil {
		t.Fatal("zero audio rate accepted")
	}
}

func TestDeduperReferencesFirst(t *testing.T) {
	d := NewDeduper()
	w1 := sampleWindow("seg-001")
	dec1, err := d.Add(w1)
	if err != nil || dec1.Duplicate || dec1.CanonicalID != "seg-001" {
		t.Fatalf("first decision wrong: %+v %v", dec1, err)
	}
	// Same normalized content, different segment id -> duplicate reference.
	w2 := sampleWindow("seg-002")
	dec2, err := d.Add(w2)
	if err != nil {
		t.Fatal(err)
	}
	if !dec2.Duplicate || dec2.CanonicalID != "seg-001" || dec2.Fingerprint != dec1.Fingerprint {
		t.Fatalf("duplicate not referenced to first: %+v", dec2)
	}
	// Distinct content is not collapsed.
	w3 := sampleWindow("seg-003")
	w3.MonoPCM[0] = 9999
	dec3, _ := d.Add(w3)
	if dec3.Duplicate || dec3.CanonicalID != "seg-003" {
		t.Fatalf("distinct content collapsed: %+v", dec3)
	}
	if d.Hits() != 1 {
		t.Fatalf("hits = %d, want 1", d.Hits())
	}
	if fps := d.Fingerprints(); len(fps) != 2 {
		t.Fatalf("distinct fingerprints = %d, want 2", len(fps))
	}
}

func TestNormalizeMonoPCM(t *testing.T) {
	in := []int16{1, 2, 3}
	out := NormalizeMonoPCM(in, 5)
	if len(out) != 5 || out[0] != 1 || out[2] != 3 || out[3] != 0 || out[4] != 0 {
		t.Fatalf("pad wrong: %v", out)
	}
	out2 := NormalizeMonoPCM([]int16{1, 2, 3, 4, 5, 6}, 3)
	if len(out2) != 3 || out2[2] != 3 {
		t.Fatalf("trim wrong: %v", out2)
	}
	// Same input length/content must normalize identically.
	a := NormalizeMonoPCM(in, 5)
	b := NormalizeMonoPCM(append([]int16{}, in...), 5)
	if len(a) != len(b) {
		t.Fatal("determinism broken")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("normalization not deterministic")
		}
	}
}
