package codec

import (
	"bytes"
	"testing"

	"rcbench/internal/quality"
	"rcbench/internal/source"
)

func TestDCTIDCTRoundtrip(t *testing.T) {
	var blk [64]float64
	for i := range blk {
		blk[i] = float64((i*37)%255) - 128
	}
	freq := FDCT(&blk)
	back := IDCT(freq)
	for i := range blk {
		if d := back[i] - blk[i]; d > 1e-9 || d < -1e-9 {
			t.Fatalf("roundtrip mismatch at %d: got %f want %f", i, back[i], blk[i])
		}
	}
}

func TestQTableMonotonic(t *testing.T) {
	prev := ScaleQTable(LumaBase, MinQP)
	for qp := MinQP + 1; qp <= MaxQP; qp++ {
		cur := ScaleQTable(LumaBase, qp)
		for i := range cur {
			if cur[i] < prev[i] {
				t.Fatalf("qp=%d table not monotonic at %d: %d < %d", qp, i, cur[i], prev[i])
			}
			if cur[i] < 1 {
				t.Fatalf("qp=%d quantizer below 1 at %d", qp, i)
			}
		}
		prev = cur
	}
}

func TestBitstreamRoundtrip(t *testing.T) {
	wr := &BitWriter{}
	wr.WriteBits(5, 3)
	wr.WriteUE(0)
	wr.WriteUE(63)
	wr.WriteSE(-7)
	wr.WriteSE(42)
	rd := NewBitReader(wr.Bytes(), wr.Bits())
	if v, _ := rd.ReadBits(3); v != 5 {
		t.Fatalf("bits: got %d want 5", v)
	}
	if v, _ := rd.ReadUE(); v != 0 {
		t.Fatalf("ue(0): got %d", v)
	}
	if v, _ := rd.ReadUE(); v != 63 {
		t.Fatalf("ue(63): got %d", v)
	}
	if v, _ := rd.ReadSE(); v != -7 {
		t.Fatalf("se(-7): got %d", v)
	}
	if v, _ := rd.ReadSE(); v != 42 {
		t.Fatalf("se(42): got %d", v)
	}
}

func genFrame(t *testing.T, w, h, index int) []byte {
	t.Helper()
	cfg := source.Config{Seed: 7, Frames: 32, Width: w, Height: h}
	buf := make([]byte, source.FrameSize(w, h))
	cfg.RenderFrame(buf, index)
	return buf
}

func TestLoopbackPSNRAtLowQP(t *testing.T) {
	w, h := 128, 128
	for _, qp := range []int{1, 10, 20} {
		src := genFrame(t, w, h, 3)
		payload, bits, err := EncodeFrame(src, w, h, qp)
		if err != nil {
			t.Fatal(err)
		}
		dec, gotQP, err := DecodeFrame(payload, bits, w, h)
		if err != nil {
			t.Fatal(err)
		}
		if gotQP != qp {
			t.Fatalf("qp header: got %d want %d", gotQP, qp)
		}
		if psnr := quality.FramePSNR(src, dec); psnr < 38 {
			t.Fatalf("qp=%d loopback PSNR %.2f dB below 38 dB", qp, psnr)
		}
	}
}

func TestBytesMonotonicInQP(t *testing.T) {
	w, h := 128, 128
	src := genFrame(t, w, h, 5)
	prev := -1
	first, last := 0, 0
	for qp := MinQP; qp <= MaxQP; qp++ {
		_, bits, err := EncodeFrame(src, w, h, qp)
		if err != nil {
			t.Fatal(err)
		}
		if prev >= 0 && bits > prev {
			t.Fatalf("bytes increased: qp=%d bits=%d prev=%d", qp, bits, prev)
		}
		if qp == MinQP {
			first = bits
		}
		last = bits
		prev = bits
	}
	if first <= last {
		t.Fatalf("expected qp=%d bytes %d > qp=%d bytes %d", MinQP, first, MaxQP, last)
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	w, h := 64, 64
	src := genFrame(t, w, h, 1)
	payload, bits, err := EncodeFrame(src, w, h, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeFrame(payload[:len(payload)/2], bits/2, w, h); err == nil {
		t.Fatal("expected error decoding truncated payload")
	}
}

func TestEncodeRejectsBadQP(t *testing.T) {
	src := genFrame(t, 64, 64, 0)
	if _, _, err := EncodeFrame(src, 64, 64, 0); err == nil {
		t.Fatal("expected error for qp=0")
	}
	if _, _, err := EncodeFrame(src, 64, 64, 52); err == nil {
		t.Fatal("expected error for qp=52")
	}
}

func TestFrameDeterminism(t *testing.T) {
	w, h := 64, 64
	src := genFrame(t, w, h, 2)
	p1, b1, _ := EncodeFrame(src, w, h, 30)
	p2, b2, _ := EncodeFrame(src, w, h, 30)
	if b1 != b2 || !bytes.Equal(p1, p2) {
		t.Fatal("encode not deterministic")
	}
}
