package codec

import (
	"bytes"
	"math"
	"testing"

	"turncode/internal/yuv"
)

func TestDCTRoundTripAndZigzag(t *testing.T) {
	var in Block
	for i := range in {
		in[i] = float64((i*17)%251) - 125
	}
	coef := ForwardDCT(in)
	rec := InverseDCT(coef)
	for i := range in {
		if math.Abs(in[i]-rec[i]) > 1e-8 {
			t.Fatalf("DCT round trip mismatch at %d: %f != %f", i, in[i], rec[i])
		}
	}
	var q QuantBlock
	for i := range q {
		q[i] = i - 32
	}
	if got := Unzigzag(Zigzag(q)); got != q {
		t.Fatalf("zigzag round trip mismatch")
	}
}

func TestQuantizationClampsQP(t *testing.T) {
	var b Block
	b[0] = 1000
	q := Quantize(b, 100)
	if q[0] == 0 {
		t.Fatalf("DC unexpectedly zero")
	}
	if QStep(0) != QStep(1) || QStep(52) != QStep(51) {
		t.Fatalf("QP is not clamped")
	}
}

func TestExpGolombRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	bw := NewBitWriter(&buf)
	ues := []uint64{0, 1, 2, 7, 63, 1024, (1 << 20) - 1}
	sets := []int{0, 1, -1, 9, -9, 1234, -1234}
	for _, v := range ues {
		bw.WriteUE(v)
	}
	for _, v := range sets {
		bw.WriteSE(v)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	br := NewBitReader(buf.Bytes())
	for _, want := range ues {
		if got := br.ReadUE(); got != want {
			t.Fatalf("UE: got %d want %d", got, want)
		}
	}
	for _, want := range sets {
		if got := br.ReadSE(); got != want {
			t.Fatalf("SE: got %d want %d", got, want)
		}
	}
	if br.Err() != nil {
		t.Fatal(br.Err())
	}
}

func TestRecordRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, []byte("frame-a")); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(&buf, []byte("frame-b")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"frame-a", "frame-b"} {
		got, err := ReadRecord(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestCodecIandPReconstruction(t *testing.T) {
	const w, h = 32, 18
	f0, _ := yuv.NewFrame(w, h)
	f1, _ := yuv.NewFrame(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			f0.Y[y*w+x] = byte((x*7 + y*3) % 255)
			f1.Y[y*w+x] = f0.Y[y*w+x]
		}
	}
	for i := range f0.U {
		f0.U[i], f0.V[i] = byte(100+i%20), byte(140-i%20)
		f1.U[i], f1.V[i] = f0.U[i], f0.V[i]
	}
	enc, err := NewEncoder(w, h)
	if err != nil {
		t.Fatal(err)
	}
	p0, _, err := enc.EncodeFrame(f0, false, 24)
	if err != nil {
		t.Fatal(err)
	}
	p1, _, err := enc.EncodeFrame(f1, true, 24)
	if err != nil {
		t.Fatal(err)
	}
	rec0, qp, isP, err := DecodeFrame(p0, w, h, nil)
	if err != nil || qp != 24 || isP {
		t.Fatalf("decode I: qp=%d p=%v err=%v", qp, isP, err)
	}
	rec1, _, isP, err := DecodeFrame(p1, w, h, rec0)
	if err != nil || !isP {
		t.Fatalf("decode P: p=%v err=%v", isP, err)
	}
	_ = rec1
}
