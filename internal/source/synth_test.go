package source

import (
	"bytes"
	"testing"
)

func TestSyntheticFramesAreDeterministic(t *testing.T) {
	a, err := New("news", 10, 32, 32, 42)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := New("news", 10, 32, 32, 42)
	fa, _ := a.At(7)
	fb, _ := b.At(7)
	if !bytes.Equal(fa.Y, fb.Y) || !bytes.Equal(fa.U, fb.U) || !bytes.Equal(fa.V, fb.V) {
		t.Fatal("same seed produced different frames")
	}
	fc, _ := a.At(8)
	if bytes.Equal(fa.Y, fc.Y) {
		t.Fatal("motion did not change adjacent frames")
	}
}

func TestHardCutBoundaries(t *testing.T) {
	if !HardCut(60) || !HardCut(120) || HardCut(59) || HardCut(0) {
		t.Fatal("unexpected hard-cut schedule")
	}
}
