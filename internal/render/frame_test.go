package render

import (
	"bytes"
	"testing"
)

var testParams = Params{Width: 320, Height: 180, Seed: 41}

func TestFrameDeterministic(t *testing.T) {
	a := Frame(testParams, 7)
	b := Frame(testParams, 7)
	if !bytes.Equal(a, b) {
		t.Fatal("same index produced different pixels")
	}
	if SHA256(a) != SHA256(b) {
		t.Fatal("digests differ")
	}
}

func TestFrameIndexChangesPixels(t *testing.T) {
	a := Frame(testParams, 0)
	b := Frame(testParams, 1)
	if bytes.Equal(a, b) {
		t.Fatal("different indices produced identical frames")
	}
	if SHA256(a) == SHA256(b) {
		t.Fatal("digests collide across indices")
	}
}

func TestFrameSeedChangesPixels(t *testing.T) {
	p2 := testParams
	p2.Seed = 42
	if bytes.Equal(Frame(testParams, 0), Frame(p2, 0)) {
		t.Fatal("different seeds produced identical frames")
	}
}

func TestPSNRAboveThreshold(t *testing.T) {
	ref := Reference(testParams)
	for i := 0; i < 24; i++ {
		psnr := PSNR(Frame(testParams, i), ref)
		if psnr < 35 {
			t.Fatalf("frame %d PSNR=%.2f dB below 35", i, psnr)
		}
	}
}

func TestPSNRIdenticalIsCapped(t *testing.T) {
	ref := Reference(testParams)
	if got := PSNR(ref, ref); got != 99 {
		t.Fatalf("PSNR(identical)=%v", got)
	}
}
