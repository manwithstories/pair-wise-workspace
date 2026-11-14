package codec

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func testFrame(width, height int) []byte {
	frame := make([]byte, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			frame[y*width+x] = byte((x*3 + y*5 + (x*y)%17) & 255)
		}
	}
	return frame
}

func TestDCTRoundTrip(t *testing.T) {
	var src [64]float64
	for i := range src {
		src[i] = float64((i*37)%101) - 50
	}
	var freq, restored [64]float64
	ForwardDCT(&src, &freq)
	InverseDCT(&freq, &restored)
	for i := range src {
		if math.Abs(src[i]-restored[i]) > 1e-9 {
			t.Fatalf("coefficient %d: got %f want %f", i, restored[i], src[i])
		}
	}
}

func TestFrameAndSegmentRoundTrip(t *testing.T) {
	width, height := 21, 13
	src := testFrame(width, height)
	payload, err := EncodeFrame(src, width, height, 30)
	if err != nil {
		t.Fatal(err)
	}
	payload2, err := EncodeFrame(src, width, height, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, payload2) {
		t.Fatal("frame encoding is not deterministic")
	}
	recon, err := DecodeFrame(payload, width, height, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(recon) != len(src) {
		t.Fatal("reconstructed frame size mismatch")
	}
	frames := [][]byte{src, src}
	seg, err := EncodeSegment(SegmentMeta{ID: 1, QP: 30, Width: width, Height: height, FPS: 24, Frames: len(frames)}, frames)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSegment(seg.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Meta.ID != 1 || decoded.Meta.QP != 30 || len(decoded.Frames) != 2 {
		t.Fatalf("unexpected decoded segment: %+v", decoded.Meta)
	}
	if decoded.CRC != seg.CRC || decoded.Bits != uint64(len(seg.Bytes))*8 {
		t.Fatal("decoded segment metadata mismatch")
	}
	seg.Bytes[len(seg.Bytes)/2] ^= 0x7f
	if _, err := DecodeSegment(seg.Bytes); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestQuantStepMonotonic(t *testing.T) {
	last := 0.0
	for qp := 0; qp <= 63; qp++ {
		step := QuantStep(qp)
		if step <= last {
			t.Fatalf("step is not increasing at qp=%d", qp)
		}
		last = step
	}
}
